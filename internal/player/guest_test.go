package player

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
)

const (
	testGuestTTL   = 2 * time.Hour
	testMemberTTL  = 24 * time.Hour
	testTokenIssue = "gamemesh"
)

// guestFixture is a service with guest accounts on, a clock the test moves
// and a token manager whose own lifetime is a day, so a guest token that got
// the wrong lifetime is visible.
type guestFixture struct {
	svc      *Service
	repo     *mockRepository
	sessions *fakeSessions
	tokens   *auth.TokenManager
	now      time.Time
}

func newGuestFixture(opts ...Option) *guestFixture {
	f := &guestFixture{
		repo:     newMockRepository(),
		sessions: newFakeSessions(),
		tokens:   auth.NewTokenManager("test-secret", testMemberTTL, testTokenIssue),
		now:      time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC),
	}
	f.svc = NewService(f.repo, f.sessions, f.tokens, zap.NewNop(), opts...)
	f.svc.now = func() time.Time { return f.now }
	return f
}

var guestNamePattern = regexp.MustCompile(`^[A-Z][a-z]+[A-Z][a-z]+[0-9]{4}$`)

func TestCreateGuest(t *testing.T) {
	f := newGuestFixture(WithGuests(testGuestTTL))
	ctx := context.Background()

	before := time.Now()
	guest, err := f.svc.CreateGuest(ctx)
	require.NoError(t, err)

	// The row: a guest, with a generated name, no real address, and the
	// secret stored only as a hash that the secret verifies.
	p := guest.Player
	stored, err := f.repo.GetByID(ctx, p.ID)
	require.NoError(t, err)
	assert.True(t, stored.IsGuest, "the account is not marked as a guest")
	assert.Regexp(t, guestNamePattern, stored.Username)
	assert.Equal(t, p.ID.String()+"@guest.invalid", stored.Email)
	assert.NotEqual(t, guest.Secret, stored.PasswordHash, "the secret is stored in clear")
	assert.True(t, auth.CheckPassword(stored.PasswordHash, guest.Secret), "the stored hash is not the secret's")
	require.NotNil(t, stored.LastSeenAt)
	assert.Equal(t, f.now, *stored.LastSeenAt)
	require.NotNil(t, stored.Stats)

	// The secret: 32 random bytes as unpadded base64url, 43 characters,
	// which bcrypt's 72-byte limit does not cut.
	raw, err := base64.RawURLEncoding.DecodeString(guest.Secret)
	require.NoError(t, err)
	assert.Len(t, raw, 32)
	assert.Len(t, guest.Secret, 43)

	// The token: a guest's, for the guest lifetime, with a session of the
	// same length.
	claims, err := f.tokens.Validate(guest.Token)
	require.NoError(t, err)
	assert.True(t, claims.Guest, "the token has no guest claim")
	assert.Equal(t, p.ID.String(), claims.PlayerID)
	assert.Equal(t, stored.Username, claims.Username)
	assert.WithinDuration(t, before.Add(testGuestTTL), claims.ExpiresAt.Time, 5*time.Second)
	assert.WithinDuration(t, claims.ExpiresAt.Time, guest.ExpiresAt, time.Second)
	assert.Equal(t, p.ID.String(), f.sessions.saved[claims.ID])
	assert.Equal(t, testGuestTTL, f.sessions.ttls[claims.ID], "the session does not end with the token")

	// The event: a registration marked as a guest's, without an address.
	require.Len(t, f.repo.events, 1)
	assert.Equal(t, events.TypePlayerRegistered, f.repo.events[0].Type)
	var payload events.PlayerRegisteredPayload
	require.NoError(t, json.Unmarshal(f.repo.events[0].Payload, &payload))
	assert.Equal(t, events.PlayerRegisteredPayload{PlayerID: p.ID.String(), Username: stored.Username, Guest: true}, payload)
}

func TestGuestSecretsDiffer(t *testing.T) {
	f := newGuestFixture(WithGuests(testGuestTTL))
	a, err := f.svc.CreateGuest(context.Background())
	require.NoError(t, err)
	b, err := f.svc.CreateGuest(context.Background())
	require.NoError(t, err)
	assert.NotEqual(t, a.Secret, b.Secret)
	assert.NotEqual(t, a.Player.ID, b.Player.ID)
}

// A generated name that is taken is replaced by another; the caller never
// sees the clash.
func TestCreateGuestRetriesATakenName(t *testing.T) {
	f := newGuestFixture(WithGuests(testGuestTTL))
	ctx := context.Background()
	_, err := f.svc.Register(ctx, RegisterInput{Username: "SwiftFox0001", Email: "a@example.com", Password: "password123"})
	require.NoError(t, err)

	names := []string{"SwiftFox0001", "SwiftFox0001", "QuietOwl0002"}
	calls := 0
	f.svc.guestName = func() (string, error) { calls++; return names[calls-1], nil }

	guest, err := f.svc.CreateGuest(ctx)
	require.NoError(t, err)
	assert.Equal(t, "QuietOwl0002", guest.Player.Username)
	assert.Equal(t, 3, calls)
}

func TestCreateGuestGivesUpOnNames(t *testing.T) {
	f := newGuestFixture(WithGuests(testGuestTTL))
	ctx := context.Background()
	_, err := f.svc.Register(ctx, RegisterInput{Username: "SwiftFox0001", Email: "a@example.com", Password: "password123"})
	require.NoError(t, err)

	calls := 0
	f.svc.guestName = func() (string, error) { calls++; return "SwiftFox0001", nil }

	_, err = f.svc.CreateGuest(ctx)
	require.ErrorIs(t, err, ErrGuestName)
	assert.Equal(t, 5, calls, "the retries are not bounded at five")
	assert.Len(t, f.repo.players, 1, "a failed creation left a row")
	assert.Empty(t, f.sessions.saved, "a failed creation left a session")
}

// A failure that is not a taken name is not retried.
func TestCreateGuestDoesNotRetryOtherFailures(t *testing.T) {
	f := newGuestFixture(WithGuests(testGuestTTL))
	boom := errors.New("database down")
	f.repo.createErr = boom
	calls := 0
	f.svc.guestName = func() (string, error) { calls++; return "SwiftFox0001", nil }

	_, err := f.svc.CreateGuest(context.Background())
	require.ErrorIs(t, err, boom)
	assert.Equal(t, 1, calls)
}

// Renewal is the ordinary login, with the generated name and the secret, and
// what comes back is again a guest's token.
func TestGuestRenewsThroughLogin(t *testing.T) {
	f := newGuestFixture(WithGuests(testGuestTTL))
	ctx := context.Background()
	guest, err := f.svc.CreateGuest(ctx)
	require.NoError(t, err)
	created := f.now

	f.now = f.now.Add(26 * time.Hour) // the next day
	before := time.Now()
	session, err := f.svc.Login(ctx, guest.Player.Username, guest.Secret)
	require.NoError(t, err)
	assert.Equal(t, guest.Player.ID, session.Player.ID, "renewal answered with another account")

	claims, err := f.tokens.Validate(session.Token)
	require.NoError(t, err)
	assert.True(t, claims.Guest, "a renewed guest token lost the guest claim")
	assert.WithinDuration(t, before.Add(testGuestTTL), claims.ExpiresAt.Time, 5*time.Second,
		"a renewed guest token got a registered player's lifetime")
	assert.Equal(t, testGuestTTL, f.sessions.ttls[claims.ID])

	stored, err := f.repo.GetByID(ctx, guest.Player.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.LastSeenAt)
	assert.Equal(t, f.now, *stored.LastSeenAt, "renewal did not record the activity the sweep reads")
	assert.NotEqual(t, created, *stored.LastSeenAt)

	_, err = f.svc.Login(ctx, guest.Player.Username, guest.Secret+"x")
	assert.ErrorIs(t, err, ErrInvalidCredentials)
	_, err = f.svc.Login(ctx, guest.Player.Username, "")
	assert.ErrorIs(t, err, ErrInvalidCredentials)
}

// Not recording the activity must not cost the guest its token.
func TestGuestRenewalSurvivesAFailedTouch(t *testing.T) {
	f := newGuestFixture(WithGuests(testGuestTTL))
	ctx := context.Background()
	guest, err := f.svc.CreateGuest(ctx)
	require.NoError(t, err)

	f.repo.touchErr = errors.New("database down")
	session, err := f.svc.Login(ctx, guest.Player.Username, guest.Secret)
	require.NoError(t, err)
	assert.NotEmpty(t, session.Token)
}

// A registered player's login is what it was: no guest claim, the manager's
// lifetime, no activity record.
func TestRegisteredLoginIsNotAGuestSession(t *testing.T) {
	f := newGuestFixture(WithGuests(testGuestTTL))
	ctx := context.Background()
	_, err := f.svc.Register(ctx, RegisterInput{Username: "alice", Email: "alice@example.com", Password: "password123"})
	require.NoError(t, err)

	before := time.Now()
	session, err := f.svc.Login(ctx, "alice", "password123")
	require.NoError(t, err)
	claims, err := f.tokens.Validate(session.Token)
	require.NoError(t, err)
	assert.False(t, claims.Guest)
	assert.WithinDuration(t, before.Add(testMemberTTL), claims.ExpiresAt.Time, 5*time.Second)
	assert.WithinDuration(t, claims.ExpiresAt.Time, session.ExpiresAt, time.Second)
	assert.Equal(t, testMemberTTL, f.sessions.ttls[claims.ID])
	assert.Nil(t, session.Player.LastSeenAt)
}

// With guest accounts off, none can be made and an existing one cannot log
// in: the flag closes the whole path, not only the creation.
func TestGuestsOff(t *testing.T) {
	on := newGuestFixture(WithGuests(testGuestTTL))
	ctx := context.Background()
	guest, err := on.svc.CreateGuest(ctx)
	require.NoError(t, err)

	off := NewService(on.repo, on.sessions, on.tokens, zap.NewNop())
	_, err = off.CreateGuest(ctx)
	assert.ErrorIs(t, err, ErrGuestsDisabled)
	_, err = off.Login(ctx, guest.Player.Username, guest.Secret)
	assert.ErrorIs(t, err, ErrInvalidCredentials)
}

func TestGuestCannotChangeItsProfile(t *testing.T) {
	f := newGuestFixture(WithGuests(testGuestTTL))
	ctx := context.Background()
	guest, err := f.svc.CreateGuest(ctx)
	require.NoError(t, err)
	name := guest.Player.Username
	eventsBefore := len(f.repo.events)

	rude := "SomethingRude"
	_, err = f.svc.UpdateProfile(ctx, guest.Player.ID, UpdateInput{Username: &rude})
	require.ErrorIs(t, err, ErrGuestProfile)
	address := "someone@example.com"
	_, err = f.svc.UpdateProfile(ctx, guest.Player.ID, UpdateInput{Email: &address})
	require.ErrorIs(t, err, ErrGuestProfile)

	stored, err := f.repo.GetByID(ctx, guest.Player.ID)
	require.NoError(t, err)
	assert.Equal(t, name, stored.Username)
	assert.Len(t, f.repo.events, eventsBefore, "a refused change still announced an update")
}

// Every name the generator can make must be a valid username: letters and
// digits only, within the column's length. All 2,304 word pairs are checked.
func TestGuestNamesAreValidUsernames(t *testing.T) {
	seen := map[string]bool{}
	for _, list := range [][]string{guestAdjectives, guestAnimals} {
		for _, word := range list {
			assert.Regexp(t, `^[A-Z][a-z]+$`, word)
			assert.False(t, seen[word], "%q is listed twice", word)
			seen[word] = true
		}
	}
	longest := 0
	for _, adjective := range guestAdjectives {
		for _, animal := range guestAnimals {
			longest = max(longest, len(adjective)+len(animal)+4)
		}
	}
	assert.LessOrEqual(t, longest, 32, "a generated name does not fit the username column")

	for i := 0; i < 200; i++ {
		name, err := newGuestName()
		require.NoError(t, err)
		assert.Regexp(t, guestNamePattern, name)
	}
}

func newGuestRouter(t *testing.T, enabled bool) (*gin.Engine, *guestFixture) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	f := newGuestFixture(WithGuests(testGuestTTL))
	h := NewHandler(f.svc)
	r := gin.New()
	h.RegisterRoutes(r)
	if enabled {
		h.RegisterGameClientRoutes(r)
	}
	return r, f
}

func TestGuestEndpoint(t *testing.T) {
	r, f := newGuestRouter(t, true)

	w := doReq(r, http.MethodPost, "/auth/guest", "", nil)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var body struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
		Secret    string    `json:"secret"`
		Player    struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			IsGuest  bool   `json:"is_guest"`
		} `json:"player"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.NotEmpty(t, body.Token)
	assert.Len(t, body.Secret, 43)
	assert.False(t, body.ExpiresAt.IsZero())
	assert.Regexp(t, guestNamePattern, body.Player.Username)
	assert.True(t, body.Player.IsGuest)
	// Neither the placeholder address nor the hash leaves the service.
	assert.NotContains(t, w.Body.String(), "guest.invalid")
	assert.NotContains(t, w.Body.String(), `"email"`)
	stored, err := f.repo.GetByUsername(context.Background(), body.Player.Username)
	require.NoError(t, err)
	assert.NotContains(t, w.Body.String(), stored.PasswordHash)

	// The answer is enough to renew: the login takes the name and the secret.
	login, err := json.Marshal(map[string]string{"identifier": body.Player.Username, "password": body.Secret})
	require.NoError(t, err)
	w = doReq(r, http.MethodPost, "/auth/login", string(login), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"expires_at"`)
	assert.NotContains(t, w.Body.String(), "guest.invalid")
	assert.NotContains(t, w.Body.String(), `"secret"`, "the secret is given once, at creation")

	// A guest's own profile has no address either.
	w = doReq(r, http.MethodGet, "/players/me", "", map[string]string{middleware.HeaderUserID: body.Player.ID})
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "guest.invalid")

	// And the name stays the server's.
	w = doReq(r, http.MethodPut, "/players/"+body.Player.ID, `{"username":"SomethingRude"}`,
		map[string]string{middleware.HeaderUserID: body.Player.ID})
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// Without the release flag the route does not exist.
func TestGuestEndpointIsAbsentWithoutTheFlag(t *testing.T) {
	r, f := newGuestRouter(t, false)
	w := doReq(r, http.MethodPost, "/auth/guest", "", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Empty(t, f.repo.players)
}
