package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"gitee.com/kobeyoung81/nanoprotocol"
)

// Errors the callers map onto HTTP status codes.
var (
	// ErrContainerNotFound reports that no request matched.
	ErrContainerNotFound = errors.New("container request not found")
	// ErrContainerAlreadyRequested reports an active request that already exists.
	ErrContainerAlreadyRequested = errors.New("container already requested")
)

// containerRequestsCollection is the provisioning queue. The provisioner is its
// only writer; the console reads it through the bus.
const containerRequestsCollection = "provisioner_container_requests"

// accountsCollection is the account collection. This service does not own it —
// these are the two read-only facts it needs (the tier that sizes a container and
// the name the container runs as).
const accountsCollection = "auth_users"

// storedContainerRequest is one queue document.
type storedContainerRequest struct {
	ID        bson.ObjectID `bson:"_id,omitempty"`
	UserID    string        `bson:"user_id"`
	BindingID *int64        `bson:"binding_id"`
	AgentID   string        `bson:"agent_id,omitempty"`
	Level     string        `bson:"level"`
	Status    string        `bson:"status"`
	RunnerID  string        `bson:"runner_id,omitempty"`
	CreatedAt time.Time     `bson:"created_at"`
	UpdatedAt time.Time     `bson:"updated_at"`
	// DisposedAt records when the container was destroyed; the queue keeps the
	// row so a re-provision can revive it instead of inserting a second one.
	DisposedAt *time.Time `bson:"disposed_at,omitempty"`
}

func (d storedContainerRequest) request() nanoprotocol.ContainerRequest {
	req := nanoprotocol.ContainerRequest{
		UserID: d.UserID, AgentID: d.AgentID,
		Level: d.Level, Status: d.Status,
	}
	if d.BindingID != nil {
		req.BindingID = *d.BindingID
	}
	return req
}

func (s *Store) containerRequests() *mongo.Collection {
	return s.db.Collection(containerRequestsCollection)
}

func nullableInt64(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

// containerIdentity is the filter naming the request a (user, binding) pair owns.
// A binding of 0 means the user-level container, which is stored as NULL — and a
// null comparison also matches a document without the field.
func containerIdentity(userID string, bindingID int64) bson.D {
	return bson.D{
		{Key: "user_id", Value: userID},
		{Key: "binding_id", Value: nullableInt64(bindingID)},
	}
}

// CreateContainerRequest enqueues a provisioning request. One non-terminal
// request per (user, binding) is allowed: every agent of a user shares a
// container, so agent-level deduplication is deliberately absent.
func (s *Store) CreateContainerRequest(userID string, bindingID int64, agentID, level string) error {
	if level == "" {
		level = "free"
	}
	ctx, cancel := opCtx()
	defer cancel()

	identity := containerIdentity(userID, bindingID)

	existing, err := s.containerRequests().CountDocuments(ctx, append(identity,
		bson.E{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{
			"pending", "creating", "running", "suspended", "disposing",
		}}}}))
	if err != nil {
		return fmt.Errorf("check existing container request: %w", err)
	}
	if existing > 0 {
		return ErrContainerAlreadyRequested
	}

	now := time.Now().UTC()
	// A disposed request still holds the slot; revive it rather than inserting a
	// second row (the binding index would refuse one anyway).
	revived, err := s.containerRequests().UpdateMany(ctx,
		append(identity, bson.E{Key: "status", Value: "disposed"}),
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: "pending"},
			{Key: "level", Value: level},
			{Key: "agent_id", Value: agentID},
			{Key: "updated_at", Value: now},
		}}, {Key: "$unset", Value: bson.D{{Key: "runner_id", Value: ""}, {Key: "disposed_at", Value: ""}}}},
	)
	if err != nil {
		return fmt.Errorf("revive disposed container request: %w", err)
	}
	if revived.MatchedCount > 0 {
		return nil
	}

	if _, err := s.containerRequests().InsertOne(ctx, storedContainerRequest{
		UserID:    userID,
		BindingID: nullableInt64(bindingID),
		AgentID:   agentID,
		Level:     level,
		Status:    "pending",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		return fmt.Errorf("insert container request: %w", err)
	}
	return nil
}

// EnsureContainerRequest enqueues a request unless one is already active, using
// the user's tier as the resource level. It is idempotent.
func (s *Store) EnsureContainerRequest(userID, agentID string) error {
	tier, _, err := s.userTier(context.Background(), userID)
	if err != nil {
		return err
	}
	if tier == "" {
		tier = "free"
	}
	if err := s.CreateContainerRequest(userID, 0, agentID, tier); err != nil {
		if errors.Is(err, ErrContainerAlreadyRequested) {
			return nil
		}
		return err
	}
	return nil
}

// PollPendingContainers returns up to 10 pending, disposing or restarting
// requests and claims them for this runner. A pending request is claimed by
// moving to 'creating'; the other two keep their status and only take the runner.
func (s *Store) PollPendingContainers(runnerID string) ([]nanoprotocol.ContainerRequest, error) {
	ctx, cancel := opCtx()
	defer cancel()

	cur, err := s.containerRequests().Find(ctx,
		bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"pending", "disposing", "restarting"}}}}},
		options.Find().
			SetSort(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}).
			SetLimit(10),
	)
	if err != nil {
		return nil, fmt.Errorf("query pending containers: %w", err)
	}
	defer func() { _ = cur.Close(ctx) }()

	var candidates []storedContainerRequest
	for cur.Next(ctx) {
		var doc storedContainerRequest
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode container request: %w", err)
		}
		candidates = append(candidates, doc)
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("iterate container requests: %w", err)
	}

	requests := make([]nanoprotocol.ContainerRequest, 0, len(candidates))
	for _, doc := range candidates {
		// The claim is conditional on the status the poll saw, so two runners
		// racing for the same request resolve to one winner and the loser does
		// not receive it.
		claimed, err := s.claimContainerRequest(ctx, doc, runnerID)
		if err != nil {
			return nil, err
		}
		if !claimed {
			continue
		}
		request := doc.request()
		name, err := s.displayNameOf(ctx, doc.UserID)
		if err != nil {
			return nil, err
		}
		request.DisplayName = name
		requests = append(requests, request)
	}
	return requests, nil
}

func (s *Store) claimContainerRequest(ctx context.Context, doc storedContainerRequest, runnerID string) (bool, error) {
	filter := bson.D{
		{Key: "_id", Value: doc.ID},
		{Key: "status", Value: doc.Status},
	}
	set := bson.D{{Key: "runner_id", Value: runnerID}, {Key: "updated_at", Value: time.Now().UTC()}}
	if doc.Status == "pending" {
		set = append(set, bson.E{Key: "status", Value: "creating"})
	}

	res, err := s.containerRequests().UpdateOne(ctx, filter, bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return false, fmt.Errorf("claim container request %s: %w", doc.UserID, err)
	}
	return res.MatchedCount > 0, nil
}

// DisposeContainerRequest marks a user's active request as 'disposing' so the
// runner deprovisions it.
func (s *Store) DisposeContainerRequest(userID string, bindingID int64) error {
	ctx, cancel := opCtx()
	defer cancel()

	filter := containerIdentity(userID, bindingID)
	if bindingID > 0 {
		filter = bson.D{{Key: "binding_id", Value: bindingID}}
	}
	filter = append(filter, bson.E{Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{"disposing", "disposed"}}}})

	res, err := s.containerRequests().UpdateMany(ctx, filter,
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: "disposing"},
			{Key: "updated_at", Value: time.Now().UTC()},
		}}})
	if err != nil {
		return fmt.Errorf("dispose container request: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrContainerNotFound
	}
	return nil
}

// RestartContainerRequest marks a running request as 'restarting'. The rebuilt
// container follows the user's current tier rather than the level recorded at
// the previous provision.
func (s *Store) RestartContainerRequest(userID string) error {
	ctx, cancel := opCtx()
	defer cancel()

	tier, found, err := s.userTier(ctx, userID)
	if err != nil {
		return err
	}
	if !found {
		return ErrContainerNotFound
	}

	set := bson.D{
		{Key: "status", Value: "restarting"},
		{Key: "updated_at", Value: time.Now().UTC()},
	}
	if tier != "" {
		set = append(set, bson.E{Key: "level", Value: tier})
	}

	res, err := s.containerRequests().UpdateMany(ctx,
		bson.D{{Key: "user_id", Value: userID}, {Key: "status", Value: "running"}},
		bson.D{{Key: "$set", Value: set}},
	)
	if err != nil {
		return fmt.Errorf("restart container request: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrContainerNotFound
	}
	return nil
}

// ReprovisionContainerRequest revives a disposed or failed request so a fresh
// container is built from the current image. The level is refreshed from the
// user's tier: the stored one may predate a tier change, and the runner sizes the
// container from it.
func (s *Store) ReprovisionContainerRequest(userID string) error {
	ctx, cancel := opCtx()
	defer cancel()

	tier, found, err := s.userTier(ctx, userID)
	if err != nil {
		return err
	}
	if !found {
		return ErrContainerNotFound
	}

	set := bson.D{
		{Key: "status", Value: "pending"},
		{Key: "updated_at", Value: time.Now().UTC()},
	}
	if tier != "" {
		set = append(set, bson.E{Key: "level", Value: tier})
	}

	res, err := s.containerRequests().UpdateMany(ctx,
		bson.D{
			{Key: "user_id", Value: userID},
			{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"disposed", "failed"}}}},
		},
		bson.D{
			{Key: "$set", Value: set},
			{Key: "$unset", Value: bson.D{{Key: "runner_id", Value: ""}, {Key: "disposed_at", Value: ""}}},
		},
	)
	if err != nil {
		return fmt.Errorf("reprovision container request: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrContainerNotFound
	}
	return nil
}

// UpdateContainerStatus records what the runner did. Once a container is being
// disposed, only the move to 'disposed' is honoured, so a racing provision
// completion cannot undo a deprovision.
func (s *Store) UpdateContainerStatus(userID string, bindingID int64, status string) error {
	ctx, cancel := opCtx()
	defer cancel()

	filter := containerIdentity(userID, bindingID)
	if bindingID > 0 {
		filter = bson.D{{Key: "binding_id", Value: bindingID}}
	}
	if status != "disposed" {
		filter = append(filter, bson.E{Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{"disposing", "disposed"}}}})
	}

	res, err := s.containerRequests().UpdateMany(ctx, filter,
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: status},
			{Key: "updated_at", Value: time.Now().UTC()},
		}}})
	if err != nil {
		return fmt.Errorf("update container status: %w", err)
	}
	if res.MatchedCount == 0 {
		return fmt.Errorf("container request not found for user %s binding %d", userID, bindingID)
	}
	return nil
}

// userTier reads a user's tier, and whether the user exists at all.
func (s *Store) userTier(ctx context.Context, userID string) (string, bool, error) {
	var doc struct {
		Tier string `bson:"tier"`
	}
	err := s.db.Collection(accountsCollection).FindOne(ctx, bson.D{{Key: "_id", Value: userID}},
		options.FindOne().SetProjection(bson.D{{Key: "tier", Value: 1}})).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read user tier: %w", err)
	}
	return doc.Tier, true, nil
}

// displayNameOf reads the account name a container runs as.
func (s *Store) displayNameOf(ctx context.Context, userID string) (string, error) {
	var doc struct {
		DisplayName string `bson:"display_name"`
	}
	err := s.db.Collection(accountsCollection).FindOne(ctx, bson.D{{Key: "_id", Value: userID}},
		options.FindOne().SetProjection(bson.D{{Key: "display_name", Value: 1}})).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read container owner %s: %w", userID, err)
	}
	return doc.DisplayName, nil
}
