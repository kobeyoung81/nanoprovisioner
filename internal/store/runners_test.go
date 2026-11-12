package store

// The runner registry belongs to this service: it registers itself, reports its
// heartbeat, and sweeps rows that stopped reporting. These tests run against a
// real MongoDB so the sweeper's filter is evaluated by the server that will
// evaluate it in production — a mock would only assert the filter the test
// itself wrote.

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"gitee.com/kobeyoung81/nanorunner/migrations"
)

// storedRunnerDoc mirrors the document so a test can look at fields the store's
// API does not return.
type storedRunnerDoc struct {
	ID            string    `bson:"_id"`
	Hostname      string    `bson:"hostname"`
	DockerHost    string    `bson:"docker_host"`
	Status        string    `bson:"status"`
	LastHeartbeat time.Time `bson:"last_heartbeat"`
	RegisteredAt  time.Time `bson:"registered_at"`
}

func readRunner(t *testing.T, st *Store, id string) storedRunnerDoc {
	t.Helper()
	var doc storedRunnerDoc
	if err := st.DB().Collection(runnersCollection).
		FindOne(context.Background(), bson.D{{Key: "_id", Value: id}}).Decode(&doc); err != nil {
		t.Fatalf("read runner %s: %v", id, err)
	}
	return doc
}

// seedRunnerRow writes a runner document directly, which is the only way to give
// it a heartbeat in the past: the API always stamps "now".
func seedRunnerRow(t *testing.T, st *Store, id, status string, heartbeat time.Time) {
	t.Helper()
	if _, err := st.DB().Collection(runnersCollection).InsertOne(context.Background(), bson.D{
		{Key: "_id", Value: id},
		{Key: "hostname", Value: id + ".host"},
		{Key: "docker_host", Value: "local"},
		{Key: "status", Value: status},
		{Key: "last_heartbeat", Value: heartbeat},
		{Key: "registered_at", Value: heartbeat},
	}); err != nil {
		t.Fatalf("seed runner %s: %v", id, err)
	}
}

func TestRegisterRunner_InsertsOnline(t *testing.T) {
	st := testStore(t)

	if err := st.RegisterRunner("runner-1", "host-a", "local"); err != nil {
		t.Fatalf("RegisterRunner: %v", err)
	}

	doc := readRunner(t, st, "runner-1")
	if doc.Hostname != "host-a" || doc.DockerHost != "local" {
		t.Fatalf("registration did not store the runner: %+v", doc)
	}
	if doc.Status != "online" {
		t.Fatalf("a new runner is not online: %q", doc.Status)
	}
	if doc.RegisteredAt.IsZero() || doc.LastHeartbeat.IsZero() {
		t.Fatalf("timestamps were not set: %+v", doc)
	}
}

// TestRegisterRunner_KeepsRegistrationTime pins registered_at as "first seen": a
// redeployed runner updates the host facts and the heartbeat, not the date it
// first appeared.
func TestRegisterRunner_KeepsRegistrationTime(t *testing.T) {
	st := testStore(t)

	if err := st.RegisterRunner("runner-1", "host-a", "local"); err != nil {
		t.Fatalf("first register: %v", err)
	}
	first := readRunner(t, st, "runner-1")

	time.Sleep(10 * time.Millisecond)
	if err := st.RegisterRunner("runner-1", "host-b", "remote"); err != nil {
		t.Fatalf("second register: %v", err)
	}

	doc := readRunner(t, st, "runner-1")
	if doc.Hostname != "host-b" || doc.DockerHost != "remote" {
		t.Fatalf("re-registration did not update the runner: %+v", doc)
	}
	if !doc.RegisteredAt.Equal(first.RegisteredAt) {
		t.Fatalf("registered_at moved on re-registration: %v → %v", first.RegisteredAt, doc.RegisteredAt)
	}
	if !doc.LastHeartbeat.After(first.LastHeartbeat) {
		t.Fatalf("last_heartbeat was not refreshed: %v → %v", first.LastHeartbeat, doc.LastHeartbeat)
	}
}

// TestTouchRunner_RevivesAnOfflineRunner: a runner that was swept offline and
// then heartbeats again has to come back, or the fleet view keeps showing a
// ghost.
func TestTouchRunner_RevivesAnOfflineRunner(t *testing.T) {
	st := testStore(t)
	seedRunnerRow(t, st, "runner-1", "offline", time.Now().UTC().Add(-time.Hour))

	if err := st.TouchRunner("runner-1"); err != nil {
		t.Fatalf("TouchRunner: %v", err)
	}

	doc := readRunner(t, st, "runner-1")
	if doc.Status != "online" {
		t.Fatalf("an offline runner did not come back online: %+v", doc)
	}
	if time.Since(doc.LastHeartbeat) > time.Minute {
		t.Fatalf("the heartbeat was not refreshed: %+v", doc)
	}
}

// TestTouchRunner_UnknownRunnerIsSilent: a heartbeat for a row pruned on purpose
// is not an error (the next restart registers it again).
func TestTouchRunner_UnknownRunnerIsSilent(t *testing.T) {
	st := testStore(t)

	if err := st.TouchRunner("nobody"); err != nil {
		t.Fatalf("TouchRunner on an unknown runner: %v", err)
	}
}

// TestMarkStaleRunnersOffline pins both halves of the sweeper's filter: the
// stale runner flips, the fresh one does not. A filter that compared the wrong
// direction would mark the whole fleet offline.
func TestMarkStaleRunnersOffline(t *testing.T) {
	st := testStore(t)
	seedRunnerRow(t, st, "stale", "online", time.Now().UTC().Add(-RunnerStaleTimeout-time.Minute))
	seedRunnerRow(t, st, "fresh", "online", time.Now().UTC())
	seedRunnerRow(t, st, "already-offline", "offline", time.Now().UTC().Add(-time.Hour))

	if err := st.MarkStaleRunnersOffline(); err != nil {
		t.Fatalf("MarkStaleRunnersOffline: %v", err)
	}

	if doc := readRunner(t, st, "stale"); doc.Status != "offline" {
		t.Fatalf("a stale runner is still online: %+v", doc)
	}
	if doc := readRunner(t, st, "fresh"); doc.Status != "online" {
		t.Fatalf("a fresh runner was marked offline: %+v", doc)
	}
	if doc := readRunner(t, st, "already-offline"); doc.Status != "offline" {
		t.Fatalf("an offline runner changed on its own: %+v", doc)
	}
	// It sweeps, it never deletes.
	if n, err := st.DB().Collection(runnersCollection).CountDocuments(context.Background(), bson.D{}); err != nil || n != 3 {
		t.Fatalf("expected all 3 runners to survive the sweep, got %d (err %v)", n, err)
	}
}

func TestDeleteRunner(t *testing.T) {
	st := testStore(t)
	seedRunnerRow(t, st, "runner-1", "online", time.Now().UTC())

	if err := st.DeleteRunner("runner-1"); err != nil {
		t.Fatalf("DeleteRunner: %v", err)
	}

	n, err := st.DB().Collection(runnersCollection).CountDocuments(context.Background(), bson.D{})
	if err != nil {
		t.Fatalf("count runners: %v", err)
	}
	if n != 0 {
		t.Fatalf("runner survived the delete: %d documents", n)
	}
	// Deleting it again is not an error.
	if err := st.DeleteRunner("runner-1"); err != nil {
		t.Fatalf("DeleteRunner on a missing runner: %v", err)
	}
}

// TestMigrations_DeclareRunnerIndexes pins the manifest against the queries the
// two readers run: the sweeper filters (status, last_heartbeat), and the console's
// fleet list sorts by registered_at descending.
func TestMigrations_DeclareRunnerIndexes(t *testing.T) {
	st := testStore(t)
	manifests, err := migrations.Load()
	if err != nil {
		t.Fatalf("load manifests: %v", err)
	}
	if _, err := st.Migrate(manifests); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	want := map[string]bson.D{
		"status_last_heartbeat": {{Key: "status", Value: int32(1)}, {Key: "last_heartbeat", Value: int32(1)}},
		"registered_at_desc":    {{Key: "registered_at", Value: int32(-1)}},
	}

	cur, err := st.DB().Collection(runnersCollection).Indexes().List(ctx)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	type indexDoc struct {
		Name string `bson:"name"`
		Key  bson.D `bson:"key"`
	}
	seen := map[string]indexDoc{}
	for cur.Next(ctx) {
		var doc indexDoc
		if err := cur.Decode(&doc); err != nil {
			t.Fatalf("decode index: %v", err)
		}
		seen[doc.Name] = doc
	}
	for name, keys := range want {
		got, ok := seen[name]
		if !ok {
			t.Fatalf("index %s is missing from the applied manifest", name)
		}
		if len(got.Key) != len(keys) {
			t.Fatalf("index %s has %d keys, want %d", name, len(got.Key), len(keys))
		}
		for i := range keys {
			if got.Key[i] != keys[i] {
				t.Fatalf("index %s key %d = %v, want %v", name, i, got.Key[i], keys[i])
			}
		}
	}
}
