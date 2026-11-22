//go:build integration

package integration

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/alpnuhoglu/gamemesh/internal/outbox"
	"github.com/alpnuhoglu/gamemesh/internal/player"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
)

func deletedEvent(id uuid.UUID) (events.Event, error) {
	return events.New(events.TypePlayerDeleted, events.PlayerDeletedPayload{PlayerID: id.String()})
}

func count(t *testing.T, db *gorm.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Raw(query, args...).Scan(&n).Error)
	return n
}

// The SQL schema and the GORM model describe the same table: on a database
// created from schema.sql (how a fresh local stack starts), AutoMigrate must
// find the guest columns and their index already there, under the same
// names, and add nothing.
func TestGuestMigrationMatchesTheModel(t *testing.T) {
	ctx := context.Background()
	schema, err := filepath.Abs("../../scripts/db/schema.sql")
	require.NoError(t, err)
	pgC, err := tcpostgres.Run(ctx, "postgres:16.16-alpine@sha256:0adbf25a9e96d38823bb813f3ac0ed41fadb665c88f1df7fe6dbfb060cbdc106",
		tcpostgres.WithDatabase("gamemesh_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		tcpostgres.WithInitScripts(schema),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(context.Background()) })
	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)

	indexes := func() int64 {
		return count(t, db, `SELECT count(*) FROM pg_indexes WHERE tablename = 'players'`)
	}
	before := indexes()
	require.Equal(t, int64(1), count(t, db,
		`SELECT count(*) FROM pg_indexes WHERE tablename = 'players' AND indexname = 'idx_players_guest_last_seen'`),
		"schema.sql does not create the index")

	require.NoError(t, db.AutoMigrate(&player.Player{}, &player.Stats{}, &outbox.Event{}))
	assert.Equal(t, before, indexes(), "AutoMigrate added an index the SQL schema already has under another name")
	assert.Equal(t, int64(2), count(t, db,
		`SELECT count(*) FROM information_schema.columns WHERE table_name = 'players' AND column_name IN ('is_guest', 'last_seen_at')`))

	// And a row written before the columns existed is not a guest.
	require.NoError(t, db.Exec(`INSERT INTO players (username, email, password_hash) VALUES ('old', 'old@example.com', 'hash')`).Error)
	assert.Equal(t, int64(1), count(t, db, `SELECT count(*) FROM players WHERE username = 'old' AND NOT is_guest AND last_seen_at IS NULL`))
}

func TestTouchLastSeen(t *testing.T) {
	db := startPostgres(t)
	repo := newRepo(db)
	ctx := context.Background()

	p := &player.Player{Username: "SwiftFox4821", Email: "x@guest.invalid", PasswordHash: "hash", IsGuest: true}
	require.NoError(t, repo.Create(ctx, p))
	created, err := repo.GetByID(ctx, p.ID)
	require.NoError(t, err)
	assert.True(t, created.IsGuest)
	assert.Nil(t, created.LastSeenAt)

	at := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repo.TouchLastSeen(ctx, p.ID, at))
	touched, err := repo.GetByID(ctx, p.ID)
	require.NoError(t, err)
	require.NotNil(t, touched.LastSeenAt)
	assert.True(t, at.Equal(*touched.LastSeenAt))
	assert.True(t, created.UpdatedAt.Equal(touched.UpdatedAt), "taking a token counted as a profile change")

	assert.ErrorIs(t, repo.TouchLastSeen(ctx, uuid.New(), at), player.ErrNotFound)
}

func TestDeleteWithOutbox(t *testing.T) {
	db := startPostgres(t)
	repo := newRepo(db)
	ctx := context.Background()

	alice := &player.Player{Username: "alice", Email: "alice@example.com", PasswordHash: "hash", Stats: &player.Stats{Rank: 1000}}
	bob := &player.Player{Username: "bob", Email: "bob@example.com", PasswordHash: "hash", Stats: &player.Stats{Rank: 1000}}
	require.NoError(t, repo.Create(ctx, alice))
	require.NoError(t, repo.Create(ctx, bob))

	e, err := deletedEvent(alice.ID)
	require.NoError(t, err)
	require.NoError(t, repo.DeleteWithOutbox(ctx, alice.ID, e, events.TopicPlayer))

	_, err = repo.GetByID(ctx, alice.ID)
	assert.ErrorIs(t, err, player.ErrNotFound)
	assert.Equal(t, int64(0), count(t, db, `SELECT count(*) FROM player_stats WHERE player_id = ?`, alice.ID),
		"the deleted player's stats row is still there")
	assert.Equal(t, int64(1), count(t, db, `SELECT count(*) FROM player_stats WHERE player_id = ?`, bob.ID))
	assert.Equal(t, int64(1), count(t, db, `SELECT count(*) FROM outbox_events WHERE event_type = ?`, events.TypePlayerDeleted))

	// Deleting someone who is not there says so and announces nothing.
	err = repo.DeleteWithOutbox(ctx, alice.ID, e, events.TopicPlayer)
	assert.ErrorIs(t, err, player.ErrNotFound)
	assert.Equal(t, int64(1), count(t, db, `SELECT count(*) FROM outbox_events WHERE event_type = ?`, events.TypePlayerDeleted))
}

// The rules of the sweep, against the real SQL. Every account that must
// survive is here next to one that must not.
func TestDeleteSweptRules(t *testing.T) {
	db := startPostgres(t)
	repo := newRepo(db)
	ctx := context.Background()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	at := func(ago time.Duration) *time.Time { v := now.Add(-ago); return &v }

	add := func(username, email string, guest bool, created time.Duration, seen *time.Time) uuid.UUID {
		p := &player.Player{
			Username: username, Email: email, PasswordHash: "hash", IsGuest: guest,
			CreatedAt: now.Add(-created), LastSeenAt: seen, Stats: &player.Stats{Rank: 1000},
		}
		require.NoError(t, repo.Create(ctx, p))
		return p.ID
	}
	exists := func(id uuid.UUID) bool {
		return count(t, db, `SELECT count(*) FROM players WHERE id = ?`, id) == 1
	}

	idleGuest := add("QuietOwl0001", "a@guest.invalid", true, 90*day, at(31*day))
	neverSeen := add("CalmElk0002", "b@guest.invalid", true, 45*day, nil) // judged by its creation
	activeGuest := add("SwiftFox0003", "c@guest.invalid", true, 90*day, at(29*day))
	oldMember := add("alice", "alice@example.com", false, 400*day, at(400*day))
	oldBench := add("bench7f3a1", "bench_7f3a_1@latencybench.invalid", false, 2*day, nil)
	newBench := add("bench9c2b1", "bench_9c2b_1@latencybench.invalid", false, time.Hour, nil)
	// One condition of the bench rule each, never both.
	benchNameOnly := add("benchwarmer", "sam@example.com", false, 400*day, nil)
	benchMailOnly := add("carol", "carol@latencybench.invalid", false, 400*day, nil)
	// A guest whose placeholder happens to look like a bench address is a
	// guest: the bench rule does not take it, the guest rule judges it.
	guestLikeBench := add("benchguest", "bench_g_1@latencybench.invalid", true, 400*day, at(time.Hour))

	guestCutoff := now.Add(-30 * day)
	benchCutoff := now.Add(-day)

	ids, err := repo.DeleteSwept(ctx, player.SweepInactiveGuests, guestCutoff, 100, events.TopicPlayer, deletedEvent)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{idleGuest, neverSeen}, ids)

	ids, err = repo.DeleteSwept(ctx, player.SweepBenchAccounts, benchCutoff, 100, events.TopicPlayer, deletedEvent)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{oldBench}, ids)

	for name, id := range map[string]uuid.UUID{
		"a guest seen 29 days ago":              activeGuest,
		"a registered player of 400 days":       oldMember,
		"a bench account an hour old":           newBench,
		"a player whose name starts with bench": benchNameOnly,
		"a player with a bench-domain address":  benchMailOnly,
		"an active guest with a bench address":  guestLikeBench,
	} {
		assert.True(t, exists(id), "%s was deleted", name)
	}
	assert.Equal(t, int64(3), count(t, db, `SELECT count(*) FROM outbox_events WHERE event_type = ?`, events.TypePlayerDeleted),
		"one event for each deleted account")
	assert.Equal(t, int64(6), count(t, db, `SELECT count(*) FROM player_stats`), "stats rows of deleted accounts remain")
}

// The limit is in the statement: of five accounts the rule selects, a limit
// of two deletes the two oldest, and a limit of zero or less deletes none.
func TestDeleteSweptHonoursItsLimit(t *testing.T) {
	db := startPostgres(t)
	repo := newRepo(db)
	ctx := context.Background()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	var byAge []uuid.UUID // oldest first
	for i := 0; i < 5; i++ {
		seen := now.Add(-time.Duration(60-i) * day)
		p := &player.Player{
			Username: "QuietOwl000" + string(rune('0'+i)), Email: string(rune('a'+i)) + "@guest.invalid",
			PasswordHash: "hash", IsGuest: true, CreatedAt: seen, LastSeenAt: &seen,
		}
		require.NoError(t, repo.Create(ctx, p))
		byAge = append(byAge, p.ID)
	}
	cutoff := now.Add(-30 * day)

	for _, limit := range []int{0, -1} {
		ids, err := repo.DeleteSwept(ctx, player.SweepInactiveGuests, cutoff, limit, events.TopicPlayer, deletedEvent)
		require.NoError(t, err)
		assert.Empty(t, ids, "a limit of %d deleted accounts", limit)
	}
	require.Equal(t, int64(5), count(t, db, `SELECT count(*) FROM players`))

	ids, err := repo.DeleteSwept(ctx, player.SweepInactiveGuests, cutoff, 2, events.TopicPlayer, deletedEvent)
	require.NoError(t, err)
	assert.ElementsMatch(t, byAge[:2], ids, "not the two oldest")
	assert.Equal(t, int64(3), count(t, db, `SELECT count(*) FROM players`))

	// An event that cannot be built undoes the whole batch: no account is
	// deleted without its announcement.
	boom := errors.New("no event")
	_, err = repo.DeleteSwept(ctx, player.SweepInactiveGuests, cutoff, 100, events.TopicPlayer,
		func(uuid.UUID) (events.Event, error) { return events.Event{}, boom })
	require.ErrorIs(t, err, boom)
	assert.Equal(t, int64(3), count(t, db, `SELECT count(*) FROM players`), "accounts were deleted without their events")

	// An unknown rule deletes nothing.
	_, err = repo.DeleteSwept(ctx, player.SweepRule(99), cutoff, 100, events.TopicPlayer, deletedEvent)
	require.Error(t, err)
	assert.Equal(t, int64(3), count(t, db, `SELECT count(*) FROM players`))
}
