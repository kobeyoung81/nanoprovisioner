package store

// The console's reads of this module's collections.
//
// The fleet page, the runtime snapshot and the overview's counters used to read
// provisioner_runners and provisioner_container_requests straight out of the shared
// database (they were the bus's reads, before the queue moved here). The collections
// are this module's, so it answers instead.

import (
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// RunnerSummary is one row of the fleet view.
type RunnerSummary struct {
	ID            string    `json:"id"`
	Hostname      string    `json:"hostname"`
	DockerHost    string    `json:"docker_host"`
	Status        string    `json:"status"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	RegisteredAt  time.Time `json:"registered_at"`
}

// ListRunners returns the whole fleet, most recently registered first.
func (s *Store) ListRunners() ([]RunnerSummary, error) {
	ctx, cancel := opCtx()
	defer cancel()

	cur, err := s.db.Collection(runnersCollection).Find(ctx, bson.D{},
		options.Find().SetSort(bson.D{{Key: "registered_at", Value: -1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("list runners: %w", err)
	}
	defer func() { _ = cur.Close(ctx) }()

	out := []RunnerSummary{}
	for cur.Next(ctx) {
		var doc storedRunner
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode runner: %w", err)
		}
		out = append(out, RunnerSummary{
			ID: doc.ID, Hostname: doc.Hostname, DockerHost: doc.DockerHost,
			Status: doc.Status, LastHeartbeat: doc.LastHeartbeat, RegisteredAt: doc.RegisteredAt,
		})
	}
	return out, cur.Err()
}

// CountOnlineRunners counts the runners the fleet view calls online.
func (s *Store) CountOnlineRunners() (int, error) {
	ctx, cancel := opCtx()
	defer cancel()

	count, err := s.db.Collection(runnersCollection).CountDocuments(ctx,
		bson.D{{Key: "status", Value: "online"}})
	if err != nil {
		return 0, fmt.Errorf("count online runners: %w", err)
	}
	return int(count), nil
}

// ContainerSummary is one row of the console's container list.
type ContainerSummary struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Level     string    `json:"level"`
	Status    string    `json:"status"`
	RunnerID  string    `json:"runner_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListContainerSummaries returns the queue's rows newest first, optionally for one
// owner.
func (s *Store) ListContainerSummaries(userID string, limit int) ([]ContainerSummary, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	ctx, cancel := opCtx()
	defer cancel()

	filter := bson.D{}
	if userID != "" {
		filter = bson.D{{Key: "user_id", Value: userID}}
	}
	cur, err := s.db.Collection(containerRequestsCollection).Find(ctx, filter,
		options.Find().
			SetSort(bson.D{{Key: "updated_at", Value: -1}, {Key: "_id", Value: -1}}).
			SetLimit(int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	defer func() { _ = cur.Close(ctx) }()

	out := []ContainerSummary{}
	for cur.Next(ctx) {
		var doc storedContainerRequest
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode container request: %w", err)
		}
		out = append(out, ContainerSummary{
			ID: doc.ID.Hex(), UserID: doc.UserID, Level: doc.Level,
			Status: doc.Status, RunnerID: doc.RunnerID, UpdatedAt: doc.UpdatedAt,
		})
	}
	return out, cur.Err()
}

// CountContainersByStatus returns the buckets the console adds up. "Active" is a sum
// of statuses the caller decides, so a new status does not need a new number here.
func (s *Store) CountContainersByStatus() (map[string]int, error) {
	ctx, cancel := opCtx()
	defer cancel()

	cur, err := s.db.Collection(containerRequestsCollection).Aggregate(ctx, bson.A{
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$status"},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
	})
	if err != nil {
		return nil, fmt.Errorf("count containers: %w", err)
	}
	defer func() { _ = cur.Close(ctx) }()

	out := map[string]int{}
	for cur.Next(ctx) {
		var row struct {
			Status string `bson:"_id"`
			Count  int    `bson:"count"`
		}
		if err := cur.Decode(&row); err != nil {
			return nil, fmt.Errorf("decode container count: %w", err)
		}
		out[row.Status] = row.Count
	}
	return out, cur.Err()
}
