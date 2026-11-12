package store

import (
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// RunnerStaleTimeout is how long a runner may go without a heartbeat before the
// sweeper marks it offline.
const RunnerStaleTimeout = 2 * time.Minute

// runnersCollection is the runner registry. The prefix and the writer agree now:
// this service registers itself and reports its heartbeat, and the console reads
// the fleet through the bus.
const runnersCollection = "provisioner_runners"

// storedRunner is the document shape. The writes below build their $set by hand
// (they only ever touch a few fields); this is what the reads decode into.
type storedRunner struct {
	ID            string    `bson:"_id"`
	Hostname      string    `bson:"hostname"`
	DockerHost    string    `bson:"docker_host"`
	Status        string    `bson:"status"`
	LastHeartbeat time.Time `bson:"last_heartbeat"`
	RegisteredAt  time.Time `bson:"registered_at"`
}

// RegisterRunner inserts or updates this runner's row. It runs at startup, so the
// row carries the host facts; the heartbeat keeps it fresh afterwards.
func (s *Store) RegisterRunner(id, hostname, dockerHost string) error {
	ctx, cancel := opCtx()
	defer cancel()

	now := time.Now().UTC()
	// registered_at is $setOnInsert and not $set: it is when the runner was first
	// seen, and a redeploy must not rewrite it.
	err := retryOnDuplicateUpsert(func() error {
		_, err := s.db.Collection(runnersCollection).UpdateOne(ctx,
			bson.D{{Key: "_id", Value: id}},
			bson.D{
				{Key: "$set", Value: bson.D{
					{Key: "hostname", Value: hostname},
					{Key: "docker_host", Value: dockerHost},
					{Key: "status", Value: "online"},
					{Key: "last_heartbeat", Value: now},
				}},
				{Key: "$setOnInsert", Value: bson.D{{Key: "registered_at", Value: now}}},
			},
			options.UpdateOne().SetUpsert(true),
		)
		return err
	})
	if err != nil {
		return fmt.Errorf("register runner: %w", err)
	}
	return nil
}

// TouchRunner reports a heartbeat and brings the row back online. No upsert: a
// heartbeat for an unknown id is a row that was pruned on purpose, and the next
// restart registers it again. Zero matches is not an error.
func (s *Store) TouchRunner(id string) error {
	ctx, cancel := opCtx()
	defer cancel()

	_, err := s.db.Collection(runnersCollection).UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: "online"},
			{Key: "last_heartbeat", Value: time.Now().UTC()},
		}}},
	)
	if err != nil {
		return fmt.Errorf("touch runner: %w", err)
	}
	return nil
}

// MarkStaleRunnersOffline marks runners whose heartbeat is older than the
// staleness window as offline. It never deletes rows — operators prune those.
func (s *Store) MarkStaleRunnersOffline() error {
	ctx, cancel := opCtx()
	defer cancel()

	_, err := s.db.Collection(runnersCollection).UpdateMany(ctx,
		bson.D{
			{Key: "status", Value: "online"},
			{Key: "last_heartbeat", Value: bson.D{{Key: "$lt", Value: time.Now().UTC().Add(-RunnerStaleTimeout)}}},
		},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "offline"}}}},
	)
	if err != nil {
		return fmt.Errorf("mark stale runners offline: %w", err)
	}
	return nil
}

// DeleteRunner removes a runner row (used to prune dead runners by hand).
func (s *Store) DeleteRunner(id string) error {
	ctx, cancel := opCtx()
	defer cancel()

	if _, err := s.db.Collection(runnersCollection).DeleteOne(ctx, bson.D{{Key: "_id", Value: id}}); err != nil {
		return fmt.Errorf("delete runner %q: %w", id, err)
	}
	return nil
}
