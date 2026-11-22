package player

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
)

// eventsOfType returns the payloads of the recorded events of one type.
func eventsOfType(t *testing.T, repo *mockRepository, eventType string) []json.RawMessage {
	t.Helper()
	var out []json.RawMessage
	for _, e := range repo.events {
		if e.Type == eventType {
			out = append(out, e.Payload)
		}
	}
	return out
}

func TestDeleteAccount(t *testing.T) {
	f := newGuestFixture(WithGuests(testGuestTTL))
	ctx := context.Background()
	alice, err := f.svc.Register(ctx, RegisterInput{Username: "alice", Email: "alice@example.com", Password: "password123"})
	require.NoError(t, err)
	bob, err := f.svc.Register(ctx, RegisterInput{Username: "bob", Email: "bob@example.com", Password: "password123"})
	require.NoError(t, err)
	// Alice is logged in twice (two devices); Bob once.
	_, err = f.svc.Login(ctx, "alice", "password123")
	require.NoError(t, err)
	_, err = f.svc.Login(ctx, "alice", "password123")
	require.NoError(t, err)
	_, err = f.svc.Login(ctx, "bob", "password123")
	require.NoError(t, err)
	require.Len(t, f.sessions.saved, 3)

	require.NoError(t, f.svc.DeleteAccount(ctx, alice.ID))

	_, err = f.repo.GetByID(ctx, alice.ID)
	assert.ErrorIs(t, err, ErrNotFound, "the row is still there")
	_, err = f.repo.GetByID(ctx, bob.ID)
	assert.NoError(t, err, "someone else's account was deleted")

	// The deletion is announced, with the id and nothing else.
	deleted := eventsOfType(t, f.repo, events.TypePlayerDeleted)
	require.Len(t, deleted, 1)
	assert.JSONEq(t, `{"player_id":"`+alice.ID.String()+`"}`, string(deleted[0]))

	// Both of Alice's sessions are gone and each is announced, so her open
	// WebSocket connections close; Bob's session is untouched.
	require.Len(t, f.sessions.saved, 1)
	for _, owner := range f.sessions.saved {
		assert.Equal(t, bob.ID.String(), owner)
	}
	assert.Len(t, eventsOfType(t, f.repo, events.TypeSessionRevoked), 2)
}

// An attempt that deleted the row and then could not revoke the sessions
// reports the failure, and repeating it finishes the job.
func TestDeleteAccountIsFinishedByARetry(t *testing.T) {
	f := newGuestFixture(WithGuests(testGuestTTL))
	ctx := context.Background()
	alice, err := f.svc.Register(ctx, RegisterInput{Username: "alice", Email: "alice@example.com", Password: "password123"})
	require.NoError(t, err)
	_, err = f.svc.Login(ctx, "alice", "password123")
	require.NoError(t, err)

	redisDown := errors.New("redis down")
	f.sessions.deleteAllErr = redisDown
	err = f.svc.DeleteAccount(ctx, alice.ID)
	require.ErrorIs(t, err, redisDown, "the caller was told the account is gone while its session still works")
	require.Len(t, f.sessions.saved, 1)

	f.sessions.deleteAllErr = nil
	err = f.svc.DeleteAccount(ctx, alice.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Empty(t, f.sessions.saved, "the retry did not revoke the session the first attempt left")
}

func TestDeleteAccountEndpoint(t *testing.T) {
	r, f := newGuestRouter(t, true)
	ctx := context.Background()
	alice, err := f.svc.Register(ctx, RegisterInput{Username: "alice", Email: "alice@example.com", Password: "password123"})
	require.NoError(t, err)
	bob, err := f.svc.Register(ctx, RegisterInput{Username: "bob", Email: "bob@example.com", Password: "password123"})
	require.NoError(t, err)
	as := func(id uuid.UUID) map[string]string { return map[string]string{middleware.HeaderUserID: id.String()} }

	// Nobody deletes someone else, and nobody without an identity deletes
	// anyone.
	w := doReq(r, http.MethodDelete, "/players/"+bob.ID.String(), "", as(alice.ID))
	assert.Equal(t, http.StatusForbidden, w.Code)
	w = doReq(r, http.MethodDelete, "/players/"+bob.ID.String(), "", nil)
	assert.Equal(t, http.StatusForbidden, w.Code)
	_, err = f.repo.GetByID(ctx, bob.ID)
	require.NoError(t, err)

	w = doReq(r, http.MethodDelete, "/players/me", "", as(alice.ID))
	assert.Equal(t, http.StatusNoContent, w.Code)
	_, err = f.repo.GetByID(ctx, alice.ID)
	assert.ErrorIs(t, err, ErrNotFound)

	w = doReq(r, http.MethodDelete, "/players/me", "", as(alice.ID))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteAccountEndpointIsAbsentWithoutTheFlag(t *testing.T) {
	r, f := newGuestRouter(t, false)
	alice, err := f.svc.Register(context.Background(), RegisterInput{Username: "alice", Email: "alice@example.com", Password: "password123"})
	require.NoError(t, err)

	w := doReq(r, http.MethodDelete, "/players/me", "", map[string]string{middleware.HeaderUserID: alice.ID.String()})
	assert.Equal(t, http.StatusNotFound, w.Code)
	_, err = f.repo.GetByID(context.Background(), alice.ID)
	assert.NoError(t, err)
}
