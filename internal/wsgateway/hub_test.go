package wsgateway

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
)

// newHubClient builds a client without a real socket: hub operations only
// touch the send channel, never the conn.
func newHubClient(hub *Hub, playerID string) *Client {
	return newClient(playerID, "", time.Time{}, nil, hub, zap.NewNop())
}

// fakeRooms is an in-memory RoomChecker: room ID -> its players. err, when
// set, makes every lookup fail; block makes every lookup wait for its
// context. calls counts lookups.
type fakeRooms struct {
	mu    sync.Mutex
	rooms map[string][]string
	err   error
	block bool
	calls int
}

func (f *fakeRooms) IsParticipant(ctx context.Context, room, playerID string) (bool, error) {
	f.mu.Lock()
	f.calls++
	players, found := f.rooms[room]
	err, block := f.err, f.block
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return false, ctx.Err()
	}
	if err != nil {
		return false, err
	}
	if !found {
		return false, ErrRoomNotFound
	}
	return slices.Contains(players, playerID), nil
}

// add creates room with the given players, as matchmaking would.
func (f *fakeRooms) add(room string, players ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rooms[room] = players
}

func (f *fakeRooms) lookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newFakeRooms() *fakeRooms { return &fakeRooms{rooms: map[string][]string{}} }

// roomsOf returns the fake RoomChecker a test hub was built with.
func roomsOf(hub *Hub) *fakeRooms { return hub.participants.(*fakeRooms) }

// newTestHub builds a hub whose rooms are the ones the hub tests use:
// room-1 and room-2 with the players that join them.
func newTestHub() *Hub {
	rooms := newFakeRooms()
	rooms.add("room-1", "alice", "bob", "dave", "slow")
	rooms.add("room-2", "carol")
	return NewHub(zap.NewNop(), metrics.New("ws-hub-test"), nil, rooms)
}

// membersOf returns the player IDs of room's members on this hub.
func membersOf(hub *Hub, room string) []string {
	hub.mu.RLock()
	defer hub.mu.RUnlock()
	var ids []string
	for c := range hub.rooms[room] {
		ids = append(ids, c.playerID)
	}
	return ids
}

func joinRejected(hub *Hub, reason string) float64 {
	return byReason(hub, "gamemesh_ws_join_rejected_total", reason)
}

func drain(c *Client) []Message {
	var msgs []Message
	for {
		select {
		case out := <-c.send:
			var m Message
			_ = json.Unmarshal(out.msg, &m)
			msgs = append(msgs, m)
		default:
			return msgs
		}
	}
}

func TestJoinBroadcastsPlayerJoined(t *testing.T) {
	hub := newTestHub()
	alice := newHubClient(hub, "alice")
	bob := newHubClient(hub, "bob")
	hub.register(alice)
	hub.register(bob)

	hub.join(alice, "room-1")
	hub.join(bob, "room-1")

	// Bob's join is seen by alice (and bob himself, already in the room).
	aliceMsgs := drain(alice)
	require.NotEmpty(t, aliceMsgs)
	last := aliceMsgs[len(aliceMsgs)-1]
	assert.Equal(t, "PlayerJoined", last.Type)
	assert.Equal(t, "room-1", last.Room)
	assert.Contains(t, string(last.Data), "bob")
}

func TestLeaveBroadcastsPlayerLeft(t *testing.T) {
	hub := newTestHub()
	alice := newHubClient(hub, "alice")
	bob := newHubClient(hub, "bob")
	hub.register(alice)
	hub.register(bob)
	hub.join(alice, "room-1")
	hub.join(bob, "room-1")
	drain(alice)

	hub.leave(bob, "room-1")
	msgs := drain(alice)
	require.NotEmpty(t, msgs)
	assert.Equal(t, "PlayerLeft", msgs[len(msgs)-1].Type)
}

func TestUnregisterNotifiesRoomsAndUpdatesGauge(t *testing.T) {
	hub := newTestHub()
	alice := newHubClient(hub, "alice")
	bob := newHubClient(hub, "bob")
	hub.register(alice)
	hub.register(bob)
	hub.join(alice, "room-1")
	hub.join(bob, "room-1")
	drain(alice)

	hub.unregister(bob)
	msgs := drain(alice)
	require.NotEmpty(t, msgs)
	assert.Equal(t, "PlayerLeft", msgs[len(msgs)-1].Type)

	// Unregistering twice must be a no-op (no panic on double close).
	assert.NotPanics(t, func() { hub.unregister(bob) })
}

func TestSendToPlayerHitsAllConnections(t *testing.T) {
	hub := newTestHub()
	tab1 := newHubClient(hub, "alice")
	tab2 := newHubClient(hub, "alice") // second tab, same player
	other := newHubClient(hub, "bob")
	hub.register(tab1)
	hub.register(tab2)
	hub.register(other)

	hub.SendToPlayer("alice", []byte(`{"type":"MatchFound"}`))
	assert.Len(t, drain(tab1), 1)
	assert.Len(t, drain(tab2), 1)
	assert.Empty(t, drain(other))
}

func TestBroadcastAll(t *testing.T) {
	hub := newTestHub()
	a := newHubClient(hub, "a")
	b := newHubClient(hub, "b")
	hub.register(a)
	hub.register(b)

	hub.BroadcastAll([]byte(`{"type":"LeaderboardUpdated"}`))
	assert.Len(t, drain(a), 1)
	assert.Len(t, drain(b), 1)
}

func TestSlowClientDoesNotBlock(t *testing.T) {
	hub := newTestHub()
	slow := newHubClient(hub, "slow")
	hub.register(slow)

	// Overflow the send buffer; trySend must drop, not block.
	msg := []byte(`{"type":"LeaderboardUpdated"}`)
	for i := 0; i < sendBuffer*2; i++ {
		hub.BroadcastAll(msg)
	}
	assert.Len(t, drain(slow), sendBuffer)
}

func TestBridgeDispatch(t *testing.T) {
	hub := newTestHub()
	alice := newHubClient(hub, "alice")
	carol := newHubClient(hub, "carol")
	hub.register(alice)
	hub.register(carol)

	bridge := NewBridge(hub, nil, zap.NewNop())

	// MatchFound goes only to the matched players.
	e, err := events.New(events.TypeMatchFound, events.MatchFoundPayload{
		RoomID: "room-9", Players: []string{"alice", "bob"},
	})
	require.NoError(t, err)
	bridge.dispatch(context.Background(), e)
	aliceMsgs := drain(alice)
	require.Len(t, aliceMsgs, 1)
	assert.Equal(t, events.TypeMatchFound, aliceMsgs[0].Type)
	assert.Equal(t, "room-9", aliceMsgs[0].Room)
	assert.Empty(t, drain(carol))

	// LeaderboardUpdated broadcasts to everyone.
	e, err = events.New(events.TypeLeaderboardUpdated, events.LeaderboardUpdatedPayload{
		PlayerID: "p", Score: 1, Rank: 1,
	})
	require.NoError(t, err)
	bridge.dispatch(context.Background(), e)
	assert.Len(t, drain(alice), 1)
	assert.Len(t, drain(carol), 1)

	// Unknown events are ignored.
	bridge.dispatch(context.Background(), events.Event{Type: "SomethingElse"})
	assert.Empty(t, drain(alice))
}

// assertJoinRejected checks that c's only queued message is the bare
// JoinRejected reply for room, and that c is not a member of room.
func assertJoinRejected(t *testing.T, hub *Hub, c *Client, room string) {
	t.Helper()
	assert.Equal(t, []string{`{"type":"JoinRejected","room":"` + room + `"}`}, drainFrames(c))
	assert.NotContains(t, membersOf(hub, room), c.playerID)
}

// TestJoinRejectsUnknownRoom: a room matchmaking has no record of (never
// created, or expired) cannot be joined, and the reply does not say why.
func TestJoinRejectsUnknownRoom(t *testing.T) {
	hub := newTestHub()
	alice := newHubClient(hub, "alice")
	hub.register(alice)

	assert.False(t, hub.join(alice, "no-such-room"))

	assertJoinRejected(t, hub, alice, "no-such-room")
	assert.NotContains(t, hub.rooms, "no-such-room", "a rejected join creates no room")
	assert.Equal(t, float64(1), joinRejected(hub, "room_not_found"))
	assert.Zero(t, joinRejected(hub, "not_participant"))
}

// TestJoinRejectsNonParticipantInHub: a player the room record does not
// list is rejected, and the members are not told PlayerJoined.
func TestJoinRejectsNonParticipantInHub(t *testing.T) {
	hub := newTestHub()
	alice := newHubClient(hub, "alice")
	mallory := newHubClient(hub, "mallory")
	hub.register(alice)
	hub.register(mallory)
	require.True(t, hub.join(alice, "room-1"))
	drainFrames(alice)

	assert.False(t, hub.join(mallory, "room-1"))

	assertJoinRejected(t, hub, mallory, "room-1")
	assert.Equal(t, []string{"alice"}, membersOf(hub, "room-1"))
	assert.Empty(t, drainFrames(alice), "no PlayerJoined for a rejected join")
	assert.Equal(t, float64(1), joinRejected(hub, "not_participant"))
}

// TestJoinFailsClosedWhenLookupFails: if the room record cannot be read,
// even a listed participant is rejected rather than let in unchecked.
func TestJoinFailsClosedWhenLookupFails(t *testing.T) {
	hub := newTestHub()
	roomsOf(hub).err = errors.New("redis: connection refused")
	alice := newHubClient(hub, "alice")
	hub.register(alice)

	assert.False(t, hub.join(alice, "room-1"))

	assertJoinRejected(t, hub, alice, "room-1")
	assert.Equal(t, float64(1), joinRejected(hub, "unavailable"))
}

// TestJoinFailsClosedWhenLookupTimesOut: a lookup that never answers is cut
// off after roomCheckTimeout and the join is rejected.
func TestJoinFailsClosedWhenLookupTimesOut(t *testing.T) {
	hub := newTestHub()
	roomsOf(hub).block = true
	alice := newHubClient(hub, "alice")
	hub.register(alice)

	start := time.Now()
	assert.False(t, hub.join(alice, "room-1"))
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, roomCheckTimeout)
	assert.Less(t, elapsed, roomCheckTimeout+time.Second)
	assertJoinRejected(t, hub, alice, "room-1")
	assert.Equal(t, float64(1), joinRejected(hub, "unavailable"))
}

// TestHubWithoutRoomCheckerRejectsJoins: leaving the checker out does not
// switch the check off.
func TestHubWithoutRoomCheckerRejectsJoins(t *testing.T) {
	hub := NewHub(zap.NewNop(), metrics.New("ws-hub-test"), nil, nil)
	alice := newHubClient(hub, "alice")
	hub.register(alice)

	assert.False(t, hub.join(alice, "room-1"))

	assertJoinRejected(t, hub, alice, "room-1")
	assert.Equal(t, float64(1), joinRejected(hub, "unavailable"))
}

// TestJoinIsRateLimited: joins draw from the relay token bucket; once it is
// empty a join is rejected without a room lookup.
func TestJoinIsRateLimited(t *testing.T) {
	hub := newTestHub()
	hub.SetRelayLimit(0.001, 2) // effectively no refill during the test
	alice := newHubClient(hub, "alice")
	hub.register(alice)

	now := time.Now()
	alice.handleJoin("room-2", now) // not a participant: rejected, but a token spent
	alice.handleJoin("room-2", now)
	drainFrames(alice)
	alice.handleJoin("room-1", now) // would be accepted, but the bucket is empty

	assertJoinRejected(t, hub, alice, "room-1")
	assert.Equal(t, 2, roomsOf(hub).lookups(), "a rate-limited join does not reach the room store")
	assert.Equal(t, float64(1), joinRejected(hub, "rate_limited"))

	sendRelay(t, alice, `{"action":"relay","room":"room-1","data":1}`)
	assert.Equal(t, float64(1), rejected(hub, "rate_limited"), "joins and relays share the bucket")
}
