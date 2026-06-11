package player

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/auth"
)

// mockRepository is an in-memory Repository for unit tests.
type mockRepository struct {
	players map[uuid.UUID]*Player
}

func newMockRepository() *mockRepository {
	return &mockRepository{players: make(map[uuid.UUID]*Player)}
}

func (r *mockRepository) Create(_ context.Context, p *Player) error {
	for _, existing := range r.players {
		if existing.Username == p.Username || existing.Email == p.Email {
			return ErrDuplicate
		}
	}
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	r.players[p.ID] = p
	return nil
}

func (r *mockRepository) GetByID(_ context.Context, id uuid.UUID) (*Player, error) {
	if p, ok := r.players[id]; ok {
		return p, nil
	}
	return nil, ErrNotFound
}

func (r *mockRepository) GetByUsername(_ context.Context, username string) (*Player, error) {
	for _, p := range r.players {
		if p.Username == username {
			return p, nil
		}
	}
	return nil, ErrNotFound
}

func (r *mockRepository) GetByEmail(_ context.Context, email string) (*Player, error) {
	for _, p := range r.players {
		if p.Email == email {
			return p, nil
		}
	}
	return nil, ErrNotFound
}

func (r *mockRepository) Update(_ context.Context, p *Player) error {
	if _, ok := r.players[p.ID]; !ok {
		return ErrNotFound
	}
	r.players[p.ID] = p
	return nil
}

// fakeSessions records session operations.
type fakeSessions struct {
	saved   map[string]string
	deleted []string
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{saved: make(map[string]string)}
}

func (s *fakeSessions) Save(_ context.Context, jti, playerID string, _ time.Duration) error {
	s.saved[jti] = playerID
	return nil
}

func (s *fakeSessions) Delete(_ context.Context, jti string) error {
	s.deleted = append(s.deleted, jti)
	delete(s.saved, jti)
	return nil
}

func (s *fakeSessions) Exists(_ context.Context, jti string) (bool, error) {
	_, ok := s.saved[jti]
	return ok, nil
}

func newTestService() (*Service, *mockRepository, *fakeSessions) {
	repo := newMockRepository()
	sessions := newFakeSessions()
	tokens := auth.NewTokenManager("test-secret", time.Hour, "gamemesh")
	return NewService(repo, sessions, tokens, zap.NewNop()), repo, sessions
}

func TestRegisterHashesPassword(t *testing.T) {
	svc, _, _ := newTestService()

	p, err := svc.Register(context.Background(), RegisterInput{
		Username: "alice", Email: "alice@example.com", Password: "password123",
	})
	require.NoError(t, err)
	assert.NotEqual(t, "password123", p.PasswordHash)
	assert.True(t, auth.CheckPassword(p.PasswordHash, "password123"))
	require.NotNil(t, p.Stats)
	assert.Equal(t, 1000, p.Stats.Rank, "new players start at default rank")
}

func TestRegisterDuplicate(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()

	_, err := svc.Register(ctx, RegisterInput{Username: "alice", Email: "alice@example.com", Password: "password123"})
	require.NoError(t, err)

	_, err = svc.Register(ctx, RegisterInput{Username: "alice", Email: "other@example.com", Password: "password123"})
	assert.ErrorIs(t, err, ErrDuplicate)
}

func TestLoginSuccessCachesSession(t *testing.T) {
	svc, _, sessions := newTestService()
	ctx := context.Background()

	registered, err := svc.Register(ctx, RegisterInput{Username: "alice", Email: "alice@example.com", Password: "password123"})
	require.NoError(t, err)

	// By username.
	token, p, err := svc.Login(ctx, "alice", "password123")
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	assert.Equal(t, registered.ID, p.ID)
	assert.Len(t, sessions.saved, 1)

	// By email.
	_, _, err = svc.Login(ctx, "alice@example.com", "password123")
	require.NoError(t, err)
	assert.Len(t, sessions.saved, 2)
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()

	_, err := svc.Register(ctx, RegisterInput{Username: "alice", Email: "alice@example.com", Password: "password123"})
	require.NoError(t, err)

	_, _, errWrongPassword := svc.Login(ctx, "alice", "wrong-password")
	_, _, errNoUser := svc.Login(ctx, "nobody", "password123")

	// Same error for both prevents user enumeration.
	assert.ErrorIs(t, errWrongPassword, ErrInvalidCredentials)
	assert.ErrorIs(t, errNoUser, ErrInvalidCredentials)
}

func TestLogoutDeletesSession(t *testing.T) {
	svc, _, sessions := newTestService()
	ctx := context.Background()

	_, err := svc.Register(ctx, RegisterInput{Username: "alice", Email: "alice@example.com", Password: "password123"})
	require.NoError(t, err)
	_, _, err = svc.Login(ctx, "alice", "password123")
	require.NoError(t, err)

	var jti string
	for k := range sessions.saved {
		jti = k
	}
	require.NoError(t, svc.Logout(ctx, jti))
	assert.Empty(t, sessions.saved)
}

func TestUpdateProfile(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()

	p, err := svc.Register(ctx, RegisterInput{Username: "alice", Email: "alice@example.com", Password: "password123"})
	require.NoError(t, err)

	newName := "alicia"
	updated, err := svc.UpdateProfile(ctx, p.ID, UpdateInput{Username: &newName})
	require.NoError(t, err)
	assert.Equal(t, "alicia", updated.Username)
	assert.Equal(t, "alice@example.com", updated.Email, "unset fields stay unchanged")

	_, err = svc.UpdateProfile(ctx, uuid.New(), UpdateInput{Username: &newName})
	assert.ErrorIs(t, err, ErrNotFound)
}
