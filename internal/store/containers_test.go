package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// testStore builds a throwaway database. What these tests pin is queue behaviour
// (the conditional claim, the revive-instead-of-insert rule, the level refresh),
// so they need the real server; without a URI they skip.
func testStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("NANOSEC_PROVISIONER_TEST_URI")
	if uri == "" {
		uri = os.Getenv("NANOSEC_ADMIN_TEST_URI")
	}
	if uri == "" {
		t.Skip("NANOSEC_PROVISIONER_TEST_URI / NANOSEC_ADMIN_TEST_URI not set")
	}
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse uri: %v", err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	u.Path = "/provisioner_test_" + hex.EncodeToString(b[:])

	st, err := Open(u.String())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		_ = st.DB().Drop(context.Background())
		_ = st.Close()
	})
	return st
}

func seedUser(t *testing.T, st *Store, userID, tier, displayName string) {
	t.Helper()
	if _, err := st.DB().Collection(accountsCollection).InsertOne(context.Background(), bson.D{
		{Key: "_id", Value: userID}, {Key: "tier", Value: tier}, {Key: "display_name", Value: displayName},
	}); err != nil {
		t.Fatalf("seed user %s: %v", userID, err)
	}
}

// The queue's whole job: one active request per user, claimed once, driven to
// running, disposable, and revivable with the current tier.
func TestQueueLifecycle(t *testing.T) {
	st := testStore(t)
	seedUser(t, st, "u1", "pro", "Alice")

	if err := st.EnsureContainerRequest("u1", "agent-1"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// Idempotent: a second ensure is not an error.
	if err := st.EnsureContainerRequest("u1", "agent-1"); err != nil {
		t.Fatalf("second ensure: %v", err)
	}

	// The tier sizes the container.
	var doc storedContainerRequest
	if err := st.DB().Collection(containerRequestsCollection).FindOne(context.Background(),
		bson.D{{Key: "user_id", Value: "u1"}}).Decode(&doc); err != nil {
		t.Fatalf("read request: %v", err)
	}
	if doc.Level != "pro" || doc.Status != "pending" {
		t.Fatalf("request = %+v", doc)
	}

	requests, err := st.PollPendingContainers("runner-1")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(requests) != 1 || requests[0].UserID != "u1" || requests[0].DisplayName != "Alice" {
		t.Fatalf("polled = %+v", requests)
	}
	// Claimed: a second poll finds nothing.
	if again, err := st.PollPendingContainers("runner-2"); err != nil || len(again) != 0 {
		t.Fatalf("second poll = %+v, %v", again, err)
	}

	if err := st.UpdateContainerStatus("u1", 0, "running"); err != nil {
		t.Fatalf("running: %v", err)
	}

	// A tier change is picked up by a restart, which re-sizes the container.
	if _, err := st.DB().Collection(accountsCollection).UpdateOne(context.Background(),
		bson.D{{Key: "_id", Value: "u1"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "tier", Value: "ultra"}}}}); err != nil {
		t.Fatalf("change tier: %v", err)
	}
	if err := st.RestartContainerRequest("u1"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if err := st.DB().Collection(containerRequestsCollection).FindOne(context.Background(),
		bson.D{{Key: "user_id", Value: "u1"}}).Decode(&doc); err != nil {
		t.Fatalf("read request: %v", err)
	}
	if doc.Status != "restarting" || doc.Level != "ultra" {
		t.Fatalf("after restart = %+v", doc)
	}

	// Dispose, then revive: the same row comes back rather than a second one.
	if err := st.UpdateContainerStatus("u1", 0, "running"); err != nil {
		t.Fatalf("running again: %v", err)
	}
	if err := st.DisposeContainerRequest("u1", 0); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	if err := st.UpdateContainerStatus("u1", 0, "disposed"); err != nil {
		t.Fatalf("disposed: %v", err)
	}
	if err := st.ReprovisionContainerRequest("u1"); err != nil {
		t.Fatalf("reprovision: %v", err)
	}
	count, err := st.DB().Collection(containerRequestsCollection).CountDocuments(context.Background(),
		bson.D{{Key: "user_id", Value: "u1"}})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("rows = %d, want 1 (a disposed request is revived, not duplicated)", count)
	}
}

// A request that is not there is reported as such: the API turns it into a 404
// rather than a silent success.
func TestMissingRequestIsReported(t *testing.T) {
	st := testStore(t)
	if err := st.DisposeContainerRequest("nobody", 0); err != ErrContainerNotFound {
		t.Fatalf("dispose = %v, want ErrContainerNotFound", err)
	}
	if err := st.RestartContainerRequest("nobody"); err != ErrContainerNotFound {
		t.Fatalf("restart = %v, want ErrContainerNotFound", err)
	}
	if err := st.ReprovisionContainerRequest("nobody"); err != ErrContainerNotFound {
		t.Fatalf("reprovision = %v, want ErrContainerNotFound", err)
	}
}

// An active request blocks a second one for the same (user, binding).
func TestOneActiveRequestPerUser(t *testing.T) {
	st := testStore(t)
	seedUser(t, st, "u1", "free", "Alice")

	if err := st.CreateContainerRequest("u1", 0, "a1", "free"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.CreateContainerRequest("u1", 0, "a2", "free"); err != ErrContainerAlreadyRequested {
		t.Fatalf("second create = %v, want ErrContainerAlreadyRequested", err)
	}
}
