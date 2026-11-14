package store

// The console's container list. It used to be the bus's read of this module's
// queue; the queue is this module's, so it answers — and the answer has to carry
// everything the console and the agent-facing shape show, including which agent and
// which channel binding the container belongs to.

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func seedQueueRow(t *testing.T, st *Store, userID, agentID, level, status string, bindingID int64, touched time.Time) {
	t.Helper()
	doc := bson.D{
		{Key: "user_id", Value: userID},
		{Key: "agent_id", Value: agentID},
		{Key: "level", Value: level},
		{Key: "status", Value: status},
		{Key: "created_at", Value: touched},
		{Key: "updated_at", Value: touched},
	}
	if bindingID != 0 {
		doc = append(doc, bson.E{Key: "binding_id", Value: bindingID})
	}
	if _, err := st.DB().Collection(containerRequestsCollection).InsertOne(context.Background(), doc); err != nil {
		t.Fatalf("seed queue row: %v", err)
	}
}

func TestListContainerSummariesCarriesTheOwnerAndTheAgent(t *testing.T) {
	st := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	seedQueueRow(t, st, "u-1", "u-1-default", "pro", "running", 7, base.Add(-time.Hour))
	seedQueueRow(t, st, "u-1", "", "pro", "pending", 0, base)
	seedQueueRow(t, st, "u-2", "u-2-default", "free", "running", 0, base)

	rows, err := st.ListContainerSummaries("u-1", 10)
	if err != nil {
		t.Fatalf("ListContainerSummaries: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected u-1's two rows, got %+v", rows)
	}
	// Newest first.
	if rows[0].Status != "pending" || rows[1].Status != "running" {
		t.Fatalf("unexpected order: %+v", rows)
	}
	if rows[1].AgentID != "u-1-default" || rows[1].BindingID != 7 {
		t.Fatalf("the row lost its agent or binding: %+v", rows[1])
	}
	if rows[0].AgentID != "" || rows[0].BindingID != 0 {
		t.Fatalf("an unbound row invented an agent: %+v", rows[0])
	}
	if rows[0].CreatedAt.IsZero() || rows[0].UpdatedAt.IsZero() {
		t.Fatalf("timestamps missing: %+v", rows[0])
	}

	// An empty user means the runtime snapshot's view: everybody's.
	all, err := st.ListContainerSummaries("", 10)
	if err != nil {
		t.Fatalf("ListContainerSummaries(all): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected all three rows, got %+v", all)
	}
}
