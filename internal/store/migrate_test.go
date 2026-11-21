package store

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"gitee.com/kobeyoung81/nanorunner/migrations"
)

// A replacement without a name would silently do nothing: there would be
// nothing to drop and no index to name in the log.
func TestParseMongoMigrationRequiresReplaceIndexNames(t *testing.T) {
	_, err := parseMongoMigration(`{"collections":{"c":{"replaceIndexes":[{"keys":[["a",1]]}]}}}`)
	if err == nil {
		t.Fatal("a nameless replaceIndexes entry was accepted")
	}
}

// migrateAll applies every manifest, the way the service does at startup.
func migrateAll(t *testing.T, st *Store) {
	t.Helper()
	manifests, err := migrations.Load()
	if err != nil {
		t.Fatalf("load manifests: %v", err)
	}
	if _, err := st.Migrate(manifests); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

// The live defect behind 0004: every user-level container request is stored with
// an explicit null binding_id, and a plain unique index admits one null value
// platform-wide. The first account to get a container then blocks every other
// account, whose insert fails with E11000 and whose agent stays in provisioning.
//
// This is a behaviour test, not a shape test: it fails if the partial filter is
// dropped, and it also fails if a deployment keeps the legacy index around.
func TestContainerRequests_UserLevelRowsDoNotCollide(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	// Reproduce the legacy index first, so the test also covers a database that
	// already carries it (the upgrade path), not only a fresh install.
	if _, err := st.DB().Collection(containerRequestsCollection).Indexes().CreateOne(ctx,
		mongo.IndexModel{
			Keys:    bson.D{{Key: "binding_id", Value: int32(1)}},
			Options: options.Index().SetName("binding_unique").SetUnique(true).SetSparse(true),
		}); err != nil {
		t.Fatalf("create legacy index: %v", err)
	}

	migrateAll(t, st)

	for _, user := range []string{"u1", "u2", "u3"} {
		if err := st.CreateContainerRequest(user, 0, "agent-"+user, "free"); err != nil {
			t.Fatalf("user-level request for %s: %v", user, err)
		}
	}

	// A real binding is still globally unique: two channels cannot share one.
	if err := st.CreateContainerRequest("u1", 7, "agent-u1", "free"); err != nil {
		t.Fatalf("first binding request: %v", err)
	}
	if err := st.CreateContainerRequest("u2", 7, "agent-u2", "free"); err == nil {
		t.Fatal("binding 7 was accepted twice; the unique constraint is gone")
	}
}
