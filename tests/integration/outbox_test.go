//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/outbox"
	"github.com/alpnuhoglu/gamemesh/internal/player"
	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
)

// Scenario 1: registering a player writes the business rows AND the outbox row
// in the same transaction. Both must be present after a successful register.
func TestOutboxRowWrittenWithBusinessRows(t *testing.T) {
	db := startPostgres(t)
	rdb := startRedis(t)
	ctx := context.Background()

	tokens := auth.NewTokenManager("integration-secret", 0, "gamemesh")
	svc := player.NewService(newRepo(db), player.NewSessionStore(rdb), tokens, zap.NewNop())

	p, err := svc.Register(ctx, player.RegisterInput{
		Username: "atomicuser", Email: "atomic@example.com", Password: "password123",
	})
	require.NoError(t, err)

	// Business rows committed.
	var players int64
	require.NoError(t, db.Model(&player.Player{}).Where("id = ?", p.ID).Count(&players).Error)
	assert.Equal(t, int64(1), players)

	// Outbox row committed in the same transaction, PENDING, correct type.
	var rows []outbox.Event
	require.NoError(t, db.Where("event_type = ?", events.TypePlayerRegistered).Find(&rows).Error)
	require.Len(t, rows, 1)
	assert.Equal(t, events.TopicPlayer, rows[0].Topic)
	assert.Equal(t, outbox.StatusPending, rows[0].Status)
	// The row id is the Event.ID (a generated UUID, the consumer dedup key) — NOT
	// the player id, which travels in the payload.
	assert.NotEqual(t, uuid.Nil, rows[0].ID, "outbox id is the generated event id")
	var payload events.PlayerRegisteredPayload
	require.NoError(t, json.Unmarshal(rows[0].Payload, &payload))
	assert.Equal(t, p.ID.String(), payload.PlayerID, "payload references the new player")
}

// Atomicity: when the business INSERT fails (duplicate username), the outbox row
// must NOT be left behind — the whole transaction rolls back.
func TestOutboxRollsBackWithBusinessFailure(t *testing.T) {
	db := startPostgres(t)
	rdb := startRedis(t)
	ctx := context.Background()

	tokens := auth.NewTokenManager("integration-secret", 0, "gamemesh")
	svc := player.NewService(newRepo(db), player.NewSessionStore(rdb), tokens, zap.NewNop())

	_, err := svc.Register(ctx, player.RegisterInput{
		Username: "dupe", Email: "dupe1@example.com", Password: "password123",
	})
	require.NoError(t, err)

	// Second register with the same username violates the unique constraint.
	_, err = svc.Register(ctx, player.RegisterInput{
		Username: "dupe", Email: "dupe2@example.com", Password: "password123",
	})
	require.ErrorIs(t, err, player.ErrDuplicate)

	// Exactly one outbox row (from the first, successful register) — the failed
	// transaction left no orphan event.
	var n int64
	require.NoError(t, db.Model(&outbox.Event{}).Count(&n).Error)
	assert.Equal(t, int64(1), n, "failed business tx must not leave an outbox row")
}

// The store's RunBatch claims pending rows, publishes them and marks them
// PUBLISHED — the relay's DB-backed path end to end.
func TestStoreRunBatchMarksPublished(t *testing.T) {
	db := startPostgres(t)
	ctx := context.Background()
	store := outbox.NewStore(db)

	// Seed a pending row directly via the publisher (no business tx needed here).
	e, err := events.New(events.TypePlayerRegistered, events.PlayerRegisteredPayload{PlayerID: "p1"})
	require.NoError(t, err)
	require.NoError(t, outbox.NewPublisher(store).Publish(ctx, events.TopicPlayer, e))

	pendingBefore, err := store.CountPending(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), pendingBefore)

	var publishedCount int
	// maxAttempts 0: dead-lettering disabled, exercises the plain publish path.
	n, dead, err := store.RunBatch(ctx, 10, 0, func(_ context.Context, rows []outbox.Event) ([]uuid.UUID, []uuid.UUID, error) {
		publishedCount = len(rows)
		ids := make([]uuid.UUID, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		return ids, nil, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 0, dead)
	assert.Equal(t, 1, publishedCount)

	pendingAfter, err := store.CountPending(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), pendingAfter, "row must be marked PUBLISHED")
}

// A row that keeps failing is dead-lettered (status FAILED) once attempt_count
// reaches maxAttempts, exercising the real SQL CASE update: it drops out of the
// PENDING poll and carries a last_error for operator triage.
func TestStoreRunBatchDeadLetters(t *testing.T) {
	db := startPostgres(t)
	ctx := context.Background()
	store := outbox.NewStore(db)

	e, err := events.New(events.TypePlayerRegistered, events.PlayerRegisteredPayload{PlayerID: "p1"})
	require.NoError(t, err)
	require.NoError(t, outbox.NewPublisher(store).Publish(ctx, events.TopicPlayer, e))

	// failPublish reports every row as failed so attempt_count climbs each batch.
	failPublish := func(_ context.Context, rows []outbox.Event) ([]uuid.UUID, []uuid.UUID, error) {
		ids := make([]uuid.UUID, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		return nil, ids, nil
	}

	// maxAttempts 2: first failure bumps to 1 (stays PENDING), second reaches 2
	// and dead-letters.
	_, dead, err := store.RunBatch(ctx, 10, 2, failPublish)
	require.NoError(t, err)
	assert.Equal(t, 0, dead, "not dead-lettered before max attempts")
	pending, err := store.CountPending(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), pending)

	_, dead, err = store.RunBatch(ctx, 10, 2, failPublish)
	require.NoError(t, err)
	assert.Equal(t, 1, dead, "row dead-lettered on reaching max attempts")
	pending, err = store.CountPending(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), pending, "FAILED row must leave the PENDING set")

	// The row now carries FAILED status and a last_error for triage.
	var row outbox.Event
	require.NoError(t, db.WithContext(ctx).Where("id = ?", e.ID).First(&row).Error)
	assert.Equal(t, outbox.StatusFailed, row.Status)
	assert.NotEmpty(t, row.LastError)

	// A further batch is a no-op: the poison row is never polled again.
	n, _, err := store.RunBatch(ctx, 10, 2, failPublish)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}
