package store

import (
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TunnelStaleTimeout is how long a tunnel may go without a heartbeat before the
// row is pruned. It is deliberately short: a tunnel row only says "this
// container is connected right now", and the agent reconnects within 30s.
const TunnelStaleTimeout = 30 * time.Second

// tunnelsCollection is the tunnel registry. It used to live only in the bus's
// memory, which meant a second bus replica could not reach a container that had
// connected to the first one. The row is a liveness marker plus the runner that
// holds the connection (stage 6 needs it to forward across replicas).
const tunnelsCollection = "provisioner_tunnels"

// Tunnel is one registered hypervisor tunnel.
type Tunnel struct {
	Name     string `bson:"_id"`
	UserID   string `bson:"user_id"`
	RunnerID string `bson:"runner_id"`
	// RunnerURL is where the holding runner accepts forwarded requests, so a
	// replica that does not hold this tunnel can relay to it.
	RunnerURL    string    `bson:"runner_url,omitempty"`
	RegisteredAt time.Time `bson:"registered_at"`
	HeartbeatAt  time.Time `bson:"heartbeat_at"`
}

// UpsertTunnel records a tunnel connection. registered_at is $setOnInsert: it is
// when the container first connected, and a reconnect must not rewrite it.
func (s *Store) UpsertTunnel(name, userID, runnerID, runnerURL string) error {
	ctx, cancel := opCtx()
	defer cancel()

	now := time.Now().UTC()
	err := retryOnDuplicateUpsert(func() error {
		_, err := s.db.Collection(tunnelsCollection).UpdateOne(ctx,
			bson.D{{Key: "_id", Value: name}},
			bson.D{
				{Key: "$set", Value: bson.D{
					{Key: "user_id", Value: userID},
					{Key: "runner_id", Value: runnerID},
					{Key: "runner_url", Value: runnerURL},
					{Key: "heartbeat_at", Value: now},
				}},
				{Key: "$setOnInsert", Value: bson.D{{Key: "registered_at", Value: now}}},
			},
			options.UpdateOne().SetUpsert(true),
		)
		return err
	})
	if err != nil {
		return fmt.Errorf("upsert tunnel %q: %w", name, err)
	}
	return nil
}

// TouchTunnel refreshes a tunnel's heartbeat. No upsert: a heartbeat for an
// unknown name is a row that was pruned on purpose, and the next reconnect
// registers it again. Zero matches is not an error.
func (s *Store) TouchTunnel(name string) error {
	ctx, cancel := opCtx()
	defer cancel()

	_, err := s.db.Collection(tunnelsCollection).UpdateOne(ctx,
		bson.D{{Key: "_id", Value: name}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "heartbeat_at", Value: time.Now().UTC()}}}},
	)
	if err != nil {
		return fmt.Errorf("touch tunnel %q: %w", name, err)
	}
	return nil
}

// DeleteTunnel removes a tunnel row when its connection closes.
func (s *Store) DeleteTunnel(name string) error {
	ctx, cancel := opCtx()
	defer cancel()

	if _, err := s.db.Collection(tunnelsCollection).DeleteOne(ctx, bson.D{{Key: "_id", Value: name}}); err != nil {
		return fmt.Errorf("delete tunnel %q: %w", name, err)
	}
	return nil
}

// MarkStaleTunnels prunes rows whose heartbeat is older than the staleness
// window and reports how many went. Unlike runners there is no "offline"
// state to keep: a stale tunnel row describes a connection that no longer
// exists, and keeping it would make a dead container look reachable.
func (s *Store) MarkStaleTunnels(now time.Time, timeout time.Duration) (int64, error) {
	ctx, cancel := opCtx()
	defer cancel()

	res, err := s.db.Collection(tunnelsCollection).DeleteMany(ctx,
		bson.D{{Key: "heartbeat_at", Value: bson.D{{Key: "$lt", Value: now.Add(-timeout)}}}},
	)
	if err != nil {
		return 0, fmt.Errorf("prune stale tunnels: %w", err)
	}
	return res.DeletedCount, nil
}

// Tunnel reads one row (used by the internal forwarding surface to decide
// between "not connected" and "connected somewhere else").
func (s *Store) Tunnel(name string) (*Tunnel, error) {
	ctx, cancel := opCtx()
	defer cancel()

	var t Tunnel
	err := s.db.Collection(tunnelsCollection).FindOne(ctx, bson.D{{Key: "_id", Value: name}}).Decode(&t)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, fmt.Errorf("read tunnel %q: %w", name, err)
	}
	return &t, nil
}

// ListTunnels returns every registered tunnel, ordered by name. It backs the
// operator-facing diagnostics (which container is connected, and where).
func (s *Store) ListTunnels() ([]Tunnel, error) {
	ctx, cancel := opCtx()
	defer cancel()

	cursor, err := s.db.Collection(tunnelsCollection).Find(ctx, bson.D{},
		options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("list tunnels: %w", err)
	}
	tunnels := []Tunnel{}
	if err := cursor.All(ctx, &tunnels); err != nil {
		return nil, fmt.Errorf("decode tunnels: %w", err)
	}
	return tunnels, nil
}
