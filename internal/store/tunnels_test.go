package store

// The tunnel registry used to be the bus's in-memory map. It lives here now so a
// container's connection is visible from any replica. These tests run against a
// real MongoDB because the pruning filter and the $setOnInsert upsert have to be
// evaluated by the server that will evaluate them in production.

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"gitee.com/kobeyoung81/nanorunner/migrations"
)

func readTunnel(t *testing.T, st *Store, name string) Tunnel {
	t.Helper()
	got, err := st.Tunnel(name)
	if err != nil {
		t.Fatalf("read tunnel %s: %v", name, err)
	}
	if got == nil {
		t.Fatalf("tunnel %s is missing", name)
	}
	return *got
}

// seedTunnelRow writes a row directly so a test can age its heartbeat; the API
// always stamps "now".
func seedTunnelRow(t *testing.T, st *Store, name string, heartbeat time.Time) {
	t.Helper()
	if _, err := st.DB().Collection(tunnelsCollection).InsertOne(context.Background(), bson.D{
		{Key: "_id", Value: name},
		{Key: "user_id", Value: "user-1"},
		{Key: "runner_id", Value: "runner-1"},
		{Key: "registered_at", Value: heartbeat},
		{Key: "heartbeat_at", Value: heartbeat},
	}); err != nil {
		t.Fatalf("seed tunnel %s: %v", name, err)
	}
}

func TestUpsertTunnel_KeepsRegistrationTime(t *testing.T) {
	st := testStore(t)

	if err := st.UpsertTunnel("hypervisor-user-1", "user-1", "runner-1", "http://runner-1:8080"); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	first := readTunnel(t, st, "hypervisor-user-1")
	if first.UserID != "user-1" || first.RunnerID != "runner-1" {
		t.Fatalf("the connection facts were not stored: %+v", first)
	}
	if first.RegisteredAt.IsZero() || first.HeartbeatAt.IsZero() {
		t.Fatalf("timestamps were not set: %+v", first)
	}

	time.Sleep(10 * time.Millisecond)
	// A reconnect can land on another runner; registered_at must stay "first seen".
	if err := st.UpsertTunnel("hypervisor-user-1", "user-1", "runner-2", "http://runner-2:8080"); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	again := readTunnel(t, st, "hypervisor-user-1")
	if !again.RegisteredAt.Equal(first.RegisteredAt) {
		t.Fatalf("registered_at moved on reconnect: %v → %v", first.RegisteredAt, again.RegisteredAt)
	}
	if !again.HeartbeatAt.After(first.HeartbeatAt) {
		t.Fatalf("heartbeat_at was not refreshed: %v → %v", first.HeartbeatAt, again.HeartbeatAt)
	}
	if again.RunnerID != "runner-2" || again.RunnerURL != "http://runner-2:8080" {
		t.Fatalf("the reconnect did not move the runner: %+v", again)
	}
}

func TestTouchTunnel_RefreshesAndIgnoresUnknown(t *testing.T) {
	st := testStore(t)
	seedTunnelRow(t, st, "hypervisor-user-1", time.Now().UTC().Add(-time.Minute))

	if err := st.TouchTunnel("hypervisor-user-1"); err != nil {
		t.Fatalf("TouchTunnel: %v", err)
	}
	if got := readTunnel(t, st, "hypervisor-user-1"); time.Since(got.HeartbeatAt) > time.Minute {
		t.Fatalf("the heartbeat was not refreshed: %+v", got)
	}

	// No upsert: a heartbeat must not resurrect a name that was pruned.
	if err := st.TouchTunnel("hypervisor-unknown"); err != nil {
		t.Fatalf("TouchTunnel on an unknown tunnel: %v", err)
	}
	if got, err := st.Tunnel("hypervisor-unknown"); err != nil || got != nil {
		t.Fatalf("a heartbeat created a tunnel row: %+v (err %v)", got, err)
	}
}

func TestDeleteTunnel(t *testing.T) {
	st := testStore(t)
	seedTunnelRow(t, st, "hypervisor-user-1", time.Now().UTC())

	if err := st.DeleteTunnel("hypervisor-user-1"); err != nil {
		t.Fatalf("DeleteTunnel: %v", err)
	}
	if got, err := st.Tunnel("hypervisor-user-1"); err != nil || got != nil {
		t.Fatalf("the tunnel survived the delete: %+v (err %v)", got, err)
	}
	// Deleting it again is not an error.
	if err := st.DeleteTunnel("hypervisor-user-1"); err != nil {
		t.Fatalf("DeleteTunnel on a missing tunnel: %v", err)
	}
}

// TestMarkStaleTunnels pins both halves of the filter: the stale row goes, the
// fresh one stays. A filter comparing the wrong direction would prune every live
// connection, and a container would look offline while it is connected.
func TestMarkStaleTunnels(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	seedTunnelRow(t, st, "hypervisor-stale", now.Add(-TunnelStaleTimeout-time.Minute))
	seedTunnelRow(t, st, "hypervisor-fresh", now)

	n, err := st.MarkStaleTunnels(now, TunnelStaleTimeout)
	if err != nil {
		t.Fatalf("MarkStaleTunnels: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly one pruned tunnel, got %d", n)
	}
	if got, _ := st.Tunnel("hypervisor-stale"); got != nil {
		t.Fatalf("a stale tunnel survived: %+v", got)
	}
	if got, _ := st.Tunnel("hypervisor-fresh"); got == nil {
		t.Fatal("a fresh tunnel was pruned")
	}
}

func TestListTunnels_OrderedByName(t *testing.T) {
	st := testStore(t)
	seedTunnelRow(t, st, "hypervisor-b", time.Now().UTC())
	seedTunnelRow(t, st, "hypervisor-a", time.Now().UTC())

	got, err := st.ListTunnels()
	if err != nil {
		t.Fatalf("ListTunnels: %v", err)
	}
	if len(got) != 2 || got[0].Name != "hypervisor-a" || got[1].Name != "hypervisor-b" {
		t.Fatalf("tunnels are not ordered by name: %+v", got)
	}
}

// TestMigrations_DeclareTunnelIndexes pins the manifest to the two queries that
// matter: the sweeper filters heartbeat_at, and cross-replica forwarding finds a
// tunnel by the runner that holds it.
func TestMigrations_DeclareTunnelIndexes(t *testing.T) {
	st := testStore(t)
	manifests, err := migrations.Load()
	if err != nil {
		t.Fatalf("load manifests: %v", err)
	}
	if _, err := st.Migrate(manifests); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	want := map[string]bson.D{
		"heartbeat": {{Key: "heartbeat_at", Value: int32(1)}},
		"runner_id": {{Key: "runner_id", Value: int32(1)}},
	}
	cursor, err := st.DB().Collection(tunnelsCollection).Indexes().List(context.Background())
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	var indexes []struct {
		Name string `bson:"name"`
		Key  bson.D `bson:"key"`
	}
	if err := cursor.All(context.Background(), &indexes); err != nil {
		t.Fatalf("decode indexes: %v", err)
	}
	have := map[string]bson.D{}
	for _, idx := range indexes {
		have[idx.Name] = idx.Key
	}
	for name, keys := range want {
		got, ok := have[name]
		if !ok {
			t.Fatalf("index %q is missing (have %v)", name, have)
		}
		if len(got) != len(keys) {
			t.Fatalf("index %q has %d keys, want %d: %v", name, len(got), len(keys), got)
		}
		for i := range keys {
			if got[i].Key != keys[i].Key || got[i].Value != keys[i].Value {
				t.Fatalf("index %q key %d = %v, want %v", name, i, got[i], keys[i])
			}
		}
	}
}
