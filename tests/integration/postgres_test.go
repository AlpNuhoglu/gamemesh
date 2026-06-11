//go:build integration

// Integration tests run against real PostgreSQL/Redis via Testcontainers.
// Execute with: go test -tags integration ./tests/integration/...
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/alpnuhoglu/gamemesh/internal/player"
)

func startPostgres(t *testing.T) *gorm.DB {
	t.Helper()
	ctx := context.Background()

	pgC, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("gamemesh_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(context.Background()) })

	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&player.Player{}, &player.PlayerStats{}))
	return db
}

func TestPlayerRepositoryCRUD(t *testing.T) {
	db := startPostgres(t)
	repo := player.NewRepository(db)
	ctx := context.Background()

	p := &player.Player{
		Username:     "alice",
		Email:        "alice@example.com",
		PasswordHash: "hash",
		Stats:        &player.PlayerStats{Rank: 1000},
	}
	require.NoError(t, repo.Create(ctx, p))

	// Fetch by ID with stats preloaded.
	fetched, err := repo.GetByID(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, "alice", fetched.Username)
	require.NotNil(t, fetched.Stats)
	assert.Equal(t, 1000, fetched.Stats.Rank)

	// Lookups used by login.
	byName, err := repo.GetByUsername(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, p.ID, byName.ID)
	byEmail, err := repo.GetByEmail(ctx, "alice@example.com")
	require.NoError(t, err)
	assert.Equal(t, p.ID, byEmail.ID)

	// Update round-trips.
	fetched.Username = "alicia"
	require.NoError(t, repo.Update(ctx, fetched))
	updated, err := repo.GetByID(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, "alicia", updated.Username)
}

func TestPlayerRepositoryConstraints(t *testing.T) {
	db := startPostgres(t)
	repo := player.NewRepository(db)
	ctx := context.Background()

	require.NoError(t, repo.Create(ctx, &player.Player{
		Username: "bob", Email: "bob@example.com", PasswordHash: "hash",
	}))

	// Unique username violation surfaces as the domain error.
	err := repo.Create(ctx, &player.Player{
		Username: "bob", Email: "bob2@example.com", PasswordHash: "hash",
	})
	assert.ErrorIs(t, err, player.ErrDuplicate)

	// Unique email violation too.
	err = repo.Create(ctx, &player.Player{
		Username: "bobby", Email: "bob@example.com", PasswordHash: "hash",
	})
	assert.ErrorIs(t, err, player.ErrDuplicate)

	// Missing row maps to ErrNotFound.
	_, err = repo.GetByUsername(ctx, "ghost")
	assert.ErrorIs(t, err, player.ErrNotFound)
}
