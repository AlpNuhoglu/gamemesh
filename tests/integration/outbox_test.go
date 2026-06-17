//go:build integration

package integration

import (
	"context"
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
	var rows []outbox.OutboxEvent
	require.NoError(t, db.Where("event_type = ?", events.TypePlayerRegistered).Find(&rows).Error)
	require.Len(t, rows, 1)
	assert.Equal(t, events.TopicPlayer, rows[0].Topic)
	assert.Equal(t, outbox.StatusPending, rows[0].Status)
	assert.Equal(t, p.ID.String(), rows[0].ID.String(), "outbox id == event id (the dedup key)")
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
	require.NoError(t, db.Model(&outbox.OutboxEvent{}).Count(&n).Error)
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
	n, err := store.RunBatch(ctx, 10, func(_ context.Context, rows []outbox.OutboxEvent) ([]uuid.UUID, []uuid.UUID, error) {
		publishedCount = len(rows)
		ids := make([]uuid.UUID, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		return ids, nil, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 1, publishedCount)

	pendingAfter, err := store.CountPending(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), pendingAfter, "row must be marked PUBLISHED")
}
