// Package store is the provisioner's own MongoDB access layer.
//
// The container-request queue used to live at the bus while this service was its
// only poller and status writer. The queue is provisioning's, so the writer moves
// here: the bus keeps the console's read-only view, and nothing else writes it.
package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Store provides access to the provisioner's collections.
type Store struct {
	mongo *mongo.Client
	db    *mongo.Database
}

// Open connects and waits for the server to answer, so a bad URI or bad
// credentials fail at startup rather than on the first request.
func Open(mongoURI string) (*Store, error) {
	if mongoURI == "" {
		return nil, errors.New("mongo uri is required")
	}
	name, err := databaseNameOf(mongoURI)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
	if err != nil {
		return nil, fmt.Errorf("connect mongo: %w", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("ping mongo: %w", err)
	}
	return &Store{mongo: client, db: client.Database(name)}, nil
}

func databaseNameOf(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("parse mongo uri: %w", err)
	}
	name := strings.Trim(u.Path, "/")
	if name == "" {
		return "", errors.New("mongo uri must name a database, e.g. mongodb://host:27017/nanosec")
	}
	if i := strings.IndexByte(name, '/'); i >= 0 {
		return "", fmt.Errorf("mongo uri names more than one database: %q", name)
	}
	return name, nil
}

// NewMongo wraps an existing client (tests use it).
func NewMongo(client *mongo.Client, database string) *Store {
	return &Store{mongo: client, db: client.Database(database)}
}

// Close releases the client.
func (s *Store) Close() error {
	if s.mongo == nil {
		return nil
	}
	return s.mongo.Disconnect(context.Background())
}

// DB exposes the database for the few queries the caller runs itself.
func (s *Store) DB() *mongo.Database { return s.db }

const opTimeout = 15 * time.Second

func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), opTimeout)
}

// retryOnDuplicateUpsert retries once on a duplicate-key error: MongoDB
// documents that concurrent upserts on a unique index can raise E11000.
func retryOnDuplicateUpsert(fn func() error) error {
	err := fn()
	if err != nil && mongo.IsDuplicateKeyError(err) {
		return fn()
	}
	return err
}
