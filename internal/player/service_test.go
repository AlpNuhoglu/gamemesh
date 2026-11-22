package player

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
)

// mockRepository is an in-memory Repository for unit tests. The *WithOutbox
// methods capture the emitted event so tests can assert the outbox payload
// without a database (the real atomicity is covered by the integration tests).
type mockRepository struct {
	players    map[uuid.UUID]*Player
	events     []events.Event
	publishErr error
	touchErr   error
	createErr  error
	sweepErr   error
	// sweeps records each DeleteSwept call; swept, when set, is told too.
	sweeps []sweepCall
	swept  chan sweepCall
}

type sweepCall struct {
	rule   SweepRule
	cutoff time.Time
	limit  int
}

func (r *mockRepository) DeleteWithOutbox(_ context.Context, id uuid.UUID, e events.Event, _ string) error {
	if _, ok := r.players[id]; !ok {
		return ErrNotFound
	}
	delete(r.players, id)
	r.events = append(r.events, e)
	return nil
}

// DeleteSwept applies the same rules as the SQL (which the integration tests
// check against Postgres), oldest first, up to limit.
func (r *mockRepository) DeleteSwept(_ context.Context, rule SweepRule, cutoff time.Time, limit int, _ string, event func(uuid.UUID) (events.Event, error)) ([]uuid.UUID, error) {
	call := sweepCall{rule: rule, cutoff: cutoff, limit: limit}
	r.sweeps = append(r.sweeps, call)
	if r.swept != nil {
		r.swept <- call
	}
	if r.sweepErr != nil {
		return nil, r.sweepErr
	}
	var matched []*Player
	for _, p := range r.players {
		seen := p.CreatedAt
		if p.LastSeenAt != nil {
			seen = *p.LastSeenAt
		}
		switch rule {
		case SweepInactiveGuests:
			if p.IsGuest && seen.Before(cutoff) {
				matched = append(matched, p)
			}
		case SweepBenchAccounts:
			if !p.IsGuest && strings.HasSuffix(p.Email, "@latencybench.invalid") &&
				strings.HasPrefix(p.Username, "bench") && p.CreatedAt.Before(cutoff) {
				matched = append(matched, p)
			}
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].CreatedAt.Before(matched[j].CreatedAt) })
	var ids []uuid.UUID
	for _, p := range matched {
		if len(ids) == limit {
			break
		}
		e, err := event(p.ID)
		if err != nil {
			return nil, err
		}
		r.events = append(r.events, e)
		delete(r.players, p.ID)
		ids = append(ids, p.ID)
	}
	return ids, nil
}

func (r *mockRepository) PublishEvent(_ context.Context, e events.Event, _ string) error {
	if r.publishErr != nil {
		return r.publishErr
	}
	r.events = append(r.events, e)
	return nil
}

func newMockRepository() *mockRepository {
	return &mockRepository{players: make(map[uuid.UUID]*Player)}
}

func (r *mockRepository) Create(_ context.Context, p *Player) error {
	if r.createErr != nil {
		return r.createErr
	}
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

func (r *mockRepository) TouchLastSeen(_ context.Context, id uuid.UUID, at time.Time) error {
	if r.touchErr != nil {
		return r.touchErr
	}
	p, ok := r.players[id]
	if !ok {
		return ErrNotFound
	}
	p.LastSeenAt = &at
	return nil
}

func (r *mockRepository) Update(_ context.Context, p *Player) error {
	if _, ok := r.players[p.ID]; !ok {
		return ErrNotFound
	}
	r.players[p.ID] = p
	return nil
}

func (r *mockRepository) CreateWithOutbox(ctx context.Context, p *Player, e events.Event, _ string) error {
	if err := r.Create(ctx, p); err != nil {
		return err
	}
	r.events = append(r.events, e)
	return nil
}

func (r *mockRepository) UpdateWithOutbox(ctx context.Context, p *Player, e events.Event, _ string) error {
	if err := r.Update(ctx, p); err != nil {
		return err
	}
	r.events = append(r.events, e)
	return nil
}

// fakeSessions records session operations.
type fakeSessions struct {
	saved        map[string]string
	deleted      []string
	deletedAll   []string
	ttls         map[string]time.Duration
	deleteAllErr error
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{saved: make(map[string]string), ttls: make(map[string]time.Duration)}
}

func (s *fakeSessions) Save(_ context.Context, jti, playerID string, ttl time.Duration) error {
	s.saved[jti] = playerID
	s.ttls[jti] = ttl
	return nil
}

func (s *fakeSessions) Delete(_ context.Context, jti string) error {
	s.deleted = append(s.deleted, jti)
	delete(s.saved, jti)
	return nil
}

func (s *fakeSessions) DeleteAll(_ context.Context, playerID string) ([]string, error) {
	if s.deleteAllErr != nil {
		return nil, s.deleteAllErr
	}
	var jtis []string
	for jti, owner := range s.saved {
		if owner == playerID {
			jtis = append(jtis, jti)
			delete(s.saved, jti)
		}
	}
	s.deletedAll = append(s.deletedAll, playerID)
	return jtis, nil
}

func (s *fakeSessions) Exists(_ context.Context, jti string) (bool, error) {
	_, ok := s.saved[jti]
	return ok, nil
}

func (s *fakeSessions) ExistsMany(_ context.Context, jtis []string) ([]bool, error) {
	out := make([]bool, len(jtis))
	for i, jti := range jtis {
		_, out[i] = s.saved[jti]
	}
	return out, nil
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

func TestRegisterEmitsOutboxEvent(t *testing.T) {
	svc, repo, _ := newTestService()

	p, err := svc.Register(context.Background(), RegisterInput{
		Username: "alice", Email: "alice@example.com", Password: "password123",
	})
	require.NoError(t, err)

	require.Len(t, repo.events, 1, "register must emit exactly one event through the outbox")
	e := repo.events[0]
	assert.Equal(t, events.TypePlayerRegistered, e.Type)
	var payload events.PlayerRegisteredPayload
	require.NoError(t, json.Unmarshal(e.Payload, &payload))
	assert.Equal(t, p.ID.String(), payload.PlayerID)
	assert.Equal(t, "alice", payload.Username)
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
	session, err := svc.Login(ctx, "alice", "password123")
	require.NoError(t, err)
	assert.NotEmpty(t, session.Token)
	assert.Equal(t, registered.ID, session.Player.ID)
	assert.Len(t, sessions.saved, 1)

	// By email.
	_, err = svc.Login(ctx, "alice@example.com", "password123")
	require.NoError(t, err)
	assert.Len(t, sessions.saved, 2)
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()

	_, err := svc.Register(ctx, RegisterInput{Username: "alice", Email: "alice@example.com", Password: "password123"})
	require.NoError(t, err)

	_, errWrongPassword := svc.Login(ctx, "alice", "wrong-password")
	_, errNoUser := svc.Login(ctx, "nobody", "password123")

	// Same error for both prevents user enumeration.
	assert.ErrorIs(t, errWrongPassword, ErrInvalidCredentials)
	assert.ErrorIs(t, errNoUser, ErrInvalidCredentials)
}

func TestLogoutDeletesSession(t *testing.T) {
	svc, _, sessions := newTestService()
	ctx := context.Background()

	_, err := svc.Register(ctx, RegisterInput{Username: "alice", Email: "alice@example.com", Password: "password123"})
	require.NoError(t, err)
	_, err = svc.Login(ctx, "alice", "password123")
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

// loginJTI registers and logs in alice and returns her session's JTI.
func loginJTI(t *testing.T, svc *Service, sessions *fakeSessions) string {
	t.Helper()
	ctx := context.Background()
	_, err := svc.Register(ctx, RegisterInput{Username: "alice", Email: "alice@example.com", Password: "password123"})
	require.NoError(t, err)
	_, err = svc.Login(ctx, "alice", "password123")
	require.NoError(t, err)
	for jti := range sessions.saved {
		return jti
	}
	t.Fatal("no session saved")
	return ""
}

func TestLogoutAnnouncesRevocation(t *testing.T) {
	svc, repo, sessions := newTestService()
	jti := loginJTI(t, svc, sessions)

	require.NoError(t, svc.Logout(context.Background(), jti))

	last := repo.events[len(repo.events)-1]
	assert.Equal(t, events.TypeSessionRevoked, last.Type)
	var p events.SessionRevokedPayload
	require.NoError(t, json.Unmarshal(last.Payload, &p))
	assert.Equal(t, jti, p.JTI)
}

// TestLogoutSucceedsWhenAnnouncementFails: the revocation is the Redis delete;
// a failed announcement is covered by the WebSocket sweep.
func TestLogoutSucceedsWhenAnnouncementFails(t *testing.T) {
	svc, repo, sessions := newTestService()
	jti := loginJTI(t, svc, sessions)
	repo.publishErr = errors.New("postgres down")

	require.NoError(t, svc.Logout(context.Background(), jti))
	assert.Contains(t, sessions.deleted, jti)
}
