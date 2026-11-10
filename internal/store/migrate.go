package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// MigrationInfo describes one migration and whether it has been applied.
type MigrationInfo struct {
	Name      string
	Applied   bool
	AppliedAt time.Time
}

const migrationsCollection = "sys_migrations"

// migrationService scopes this service's records inside the shared
// sys_migrations collection: every module keeps its bookkeeping in the same
// place, and the id carries the owner so two modules' filenames cannot be
// mistaken for each other.
const migrationService = "portal"

// Migrate applies every pending manifest, in filename order.
//
// Manifests are declarative: they describe the indexes (and optionally a
// validator) a collection must have. Creating an index that already exists is a
// no-op in MongoDB, so a partially applied manifest is harmless; the record is
// still written so the CLI can report what ran and when.
func (s *Store) Migrate(manifests map[string]string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	applied, err := s.appliedMigrations(ctx)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(manifests))
	for name := range manifests {
		names = append(names, name)
	}
	sort.Strings(names)

	var ran []string
	for _, name := range names {
		if _, ok := applied[name]; ok {
			continue
		}
		slog.Info("applying mongo migration", "name", name)
		spec, err := parseMongoMigration(manifests[name])
		if err != nil {
			return ran, fmt.Errorf("migration %s: %w", name, err)
		}
		if err := s.applyMongoMigration(ctx, spec); err != nil {
			return ran, fmt.Errorf("migration %s: %w", name, err)
		}
		if err := s.recordMigration(ctx, name); err != nil {
			return ran, fmt.Errorf("migration %s: record: %w", name, err)
		}
		ran = append(ran, name)
	}
	return ran, nil
}

// MigrationStatus reports every manifest and whether it has been applied.
func (s *Store) MigrationStatus(manifests map[string]string) ([]MigrationInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	applied, err := s.appliedMigrations(ctx)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(manifests))
	for name := range manifests {
		names = append(names, name)
	}
	sort.Strings(names)

	infos := make([]MigrationInfo, len(names))
	for i, name := range names {
		at, ok := applied[name]
		infos[i] = MigrationInfo{Name: name, Applied: ok, AppliedAt: at}
	}
	return infos, nil
}

// appliedMigrations reads the names already applied by this service.
func (s *Store) appliedMigrations(ctx context.Context) (map[string]time.Time, error) {
	cur, err := s.db.Collection(migrationsCollection).Find(ctx,
		bson.D{{Key: "service", Value: migrationService}})
	if err != nil {
		// A collection that does not exist yet is not an error: Find simply
		// returns an empty cursor.
		return nil, fmt.Errorf("read %s: %w", migrationsCollection, err)
	}
	defer func() { _ = cur.Close(ctx) }()

	out := make(map[string]time.Time)
	for cur.Next(ctx) {
		var doc struct {
			ID        string    `bson:"_id"`
			AppliedAt time.Time `bson:"applied_at"`
		}
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode migration record: %w", err)
		}
		out[strings.TrimPrefix(doc.ID, migrationService+":")] = doc.AppliedAt
	}
	return out, cur.Err()
}

func (s *Store) recordMigration(ctx context.Context, name string) error {
	_, err := s.db.Collection(migrationsCollection).InsertOne(ctx, bson.D{
		{Key: "_id", Value: migrationService + ":" + name},
		{Key: "service", Value: migrationService},
		{Key: "applied_at", Value: time.Now().UTC()},
	})
	return err
}

// applyMongoMigration creates the indexes (and validators) a manifest declares.
func (s *Store) applyMongoMigration(ctx context.Context, spec mongoMigration) error {
	// Sorted so the same manifest always touches collections in the same order:
	// a failing migration then stops at a reproducible point.
	colls := make([]string, 0, len(spec.Collections))
	for name := range spec.Collections {
		colls = append(colls, name)
	}
	sort.Strings(colls)

	for _, name := range colls {
		cs := spec.Collections[name]

		// Create the collection explicitly when a validator is declared:
		// createIndexes on a missing collection would otherwise create it
		// without the validator, and the next run would silently skip it.
		if len(cs.Validator) > 0 {
			opts := options.CreateCollection()
			opts.SetValidator(cs.Validator)
			if cs.ValidationLevel != "" {
				opts.SetValidationLevel(cs.ValidationLevel)
			}
			if cs.ValidationAction != "" {
				opts.SetValidationAction(cs.ValidationAction)
			}
			if err := s.db.CreateCollection(ctx, name, opts); err != nil && !isNamespaceExists(err) {
				return fmt.Errorf("create collection %s: %w", name, err)
			}
		}

		if len(cs.Indexes) == 0 {
			continue
		}
		models := make([]mongo.IndexModel, 0, len(cs.Indexes))
		for _, ix := range cs.Indexes {
			opts := options.Index()
			if ix.Name != "" {
				opts.SetName(ix.Name)
			}
			if ix.Unique {
				opts.SetUnique(true)
			}
			if ix.Sparse {
				opts.SetSparse(true)
			}
			if ix.ExpireAfterSeconds != nil {
				opts.SetExpireAfterSeconds(*ix.ExpireAfterSeconds)
			}
			if len(ix.PartialFilter) > 0 {
				var filter bson.D
				if err := bson.UnmarshalExtJSON(ix.PartialFilter, true, &filter); err != nil {
					return fmt.Errorf("collection %s index %s: partialFilterExpression: %w", name, ix.Name, err)
				}
				opts.SetPartialFilterExpression(filter)
			}
			// bson.D, not the decoder's named slice: the driver marshals an
			// index key document by type, and a named []bson.E has no BSON
			// marshaller of its own.
			models = append(models, mongo.IndexModel{Keys: bson.D(ix.Keys), Options: opts})
		}
		if _, err := s.db.Collection(name).Indexes().CreateMany(ctx, models); err != nil {
			return fmt.Errorf("create indexes on %s: %w", name, err)
		}
	}
	return nil
}

// mongoMigration is one manifest: the shape a set of collections must have.
type mongoMigration struct {
	Collections map[string]collectionSpec `json:"collections"`
}

type collectionSpec struct {
	Indexes          []indexSpec     `json:"indexes"`
	Validator        json.RawMessage `json:"validator,omitempty"`
	ValidationLevel  string          `json:"validationLevel,omitempty"`
	ValidationAction string          `json:"validationAction,omitempty"`
}

type indexSpec struct {
	Name               string          `json:"name,omitempty"`
	Keys               indexKeys       `json:"keys"`
	Unique             bool            `json:"unique,omitempty"`
	Sparse             bool            `json:"sparse,omitempty"`
	ExpireAfterSeconds *int32          `json:"expireAfterSeconds,omitempty"`
	PartialFilter      json.RawMessage `json:"partialFilterExpression,omitempty"`
}

// indexKeys decodes `[["field", 1], ["other", -1], ["body", "text"]]`.
//
// An ordered list of pairs rather than a JSON object on purpose: index key order
// is part of the index's identity and of what queries it can serve.
type indexKeys []bson.E

func (k *indexKeys) UnmarshalJSON(data []byte) error {
	var pairs []json.RawMessage
	if err := json.Unmarshal(data, &pairs); err != nil {
		return fmt.Errorf("index keys must be a list of [field, direction] pairs: %w", err)
	}
	out := make(indexKeys, 0, len(pairs))
	for _, raw := range pairs {
		var pair []json.RawMessage
		if err := json.Unmarshal(raw, &pair); err != nil || len(pair) != 2 {
			return fmt.Errorf("index key entry must be [field, direction]: %s", string(raw))
		}
		var field string
		if err := json.Unmarshal(pair[0], &field); err != nil {
			return fmt.Errorf("index key field must be a string: %s", string(pair[0]))
		}
		value, err := indexDirection(pair[1])
		if err != nil {
			return fmt.Errorf("index key %q: %w", field, err)
		}
		out = append(out, bson.E{Key: field, Value: value})
	}
	if len(out) == 0 {
		return fmt.Errorf("index must name at least one key")
	}
	*k = out
	return nil
}

// indexDirection narrows JSON numbers to int32 (the driver rejects float64
// directions) and passes index types through.
func indexDirection(raw json.RawMessage) (any, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "text", "hashed", "2d", "2dsphere", "geoHaystack", "wildcard":
			return s, nil
		default:
			return nil, fmt.Errorf("unsupported index type %q", s)
		}
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("direction must be 1, -1 or an index type: %s", string(raw))
	}
	if f != 1 && f != -1 {
		return nil, fmt.Errorf("direction must be 1 or -1, got %v", f)
	}
	return int32(f), nil
}

// parseMongoMigration decodes one manifest and refuses the mistakes that would
// otherwise fail halfway through applying it.
func parseMongoMigration(manifest string) (mongoMigration, error) {
	var spec mongoMigration
	dec := json.NewDecoder(strings.NewReader(manifest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return spec, fmt.Errorf("manifest: %w", err)
	}
	if len(spec.Collections) == 0 {
		return spec, fmt.Errorf("manifest declares no collections")
	}
	return spec, nil
}

// isNamespaceExists reports MongoDB's "namespace exists" error (48).
func isNamespaceExists(err error) bool {
	var ce mongo.CommandError
	return errors.As(err, &ce) && ce.Code == 48
}
