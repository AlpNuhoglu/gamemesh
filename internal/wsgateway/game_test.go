package wsgateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
)

// fakeSession is a GameSession that records what the hub does with it.
type fakeSession struct {
	out  GameTransport
	done chan struct{}
	end  chan struct{} // close to end Run as a match that is over

	mu        sync.Mutex
	posts     []fakePost
	notified  []string
	full      bool // Post reports a full inbox
	cancelled bool // Run ended because its context was cancelled
	// connectedAtCancel is what Connected said for watch at that moment.
	watch             string
	connectedAtCancel bool
	onNotify          func(playerID string)
	panicInRun        bool
}

type fakePost struct {
	playerID string
	data     string
	arrived  time.Time
}

func (s *fakeSession) Run(ctx context.Context) {
	defer close(s.done)
	if s.panicInRun {
		panic("game bug")
	}
	select {
	case <-ctx.Done():
		connected := s.watch != "" && s.out.Connected(s.watch)
		s.mu.Lock()
		s.cancelled, s.connectedAtCancel = true, connected
		s.mu.Unlock()
	case <-s.end:
	}
}

func (s *fakeSession) Post(playerID string, data []byte, arrived time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.full {
		return false
	}
	s.posts = append(s.posts, fakePost{playerID, string(data), arrived})
	return true
}

func (s *fakeSession) Notify(playerID string) {
	s.mu.Lock()
	s.notified = append(s.notified, playerID)
	fn := s.onNotify
	s.mu.Unlock()
	if fn != nil {
		fn(playerID)
	}
}

func (s *fakeSession) Done() <-chan struct{} { return s.done }

func (s *fakeSession) posted() []fakePost {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakePost(nil), s.posts...)
}

func (s *fakeSession) notices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.notified...)
}

// finish ends the session as a finished match and waits until the hub has
// seen it.
func (s *fakeSession) finish(t *testing.T, hub *Hub, room string) {
	t.Helper()
	close(s.end)
	require.Eventually(t, func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return hub.games[room].finished
	}, time.Second, time.Millisecond)
}

// fakeHost is a GameHost that hands out fakeSessions.
type fakeHost struct {
	mu       sync.Mutex
	started  []string // rooms, in order
	specs    map[string]GameSpec
	sessions map[string]*fakeSession
	err      error
	prepare  func(*fakeSession)
}

func (f *fakeHost) Start(room string, spec GameSpec, out GameTransport) (GameSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	s := &fakeSession{out: out, done: make(chan struct{}), end: make(chan struct{})}
	if f.prepare != nil {
		f.prepare(s)
	}
	f.started = append(f.started, room)
	f.specs[room] = spec
	f.sessions[room] = s
	return s, nil
}

func (f *fakeHost) session(room string) *fakeSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[room]
}

func (f *fakeHost) starts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...)
}

// addGame creates a validated room with the given players, as matchmaking
// would, created at the given age.
func (f *fakeRooms) addGame(room string, age time.Duration, players ...string) {
	f.add(room, players...)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.games[room] = &GameSpec{Mode: "test", Players: players, CreatedAt: time.Now().Add(-age)}
}

const gameStartWindow = time.Minute

// newGameHub builds a test hub that runs validated rooms, with game-1
// (alice, bob) just created. room-1 stays an opaque-relay room.
func newGameHub(t *testing.T) (*Hub, *fakeHost) {
	t.Helper()
	hub := newTestHub()
	host := &fakeHost{specs: map[string]GameSpec{}, sessions: map[string]*fakeSession{}}
	hub.SetGames(context.Background(), host, gameStartWindow)
	roomsOf(hub).addGame("game-1", 0, "alice", "bob")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		hub.stopGames(ctx)
	})
	return hub, host
}

// gameRoomOf joins a client per player into game-1 and empties their queues
// of the join notices.
func gameRoomOf(t *testing.T, hub *Hub, players ...string) []*Client {
	t.Helper()
	clients := make([]*Client, len(players))
	for i, p := range players {
		clients[i] = newHubClient(hub, p)
		hub.register(clients[i])
		require.True(t, hub.join(clients[i], "game-1"), "%s joins game-1", p)
	}
	for _, c := range clients {
		drainFrames(c)
	}
	return clients
}

// TestValidatedRoomStartsOneSession: the first join starts the room's
// session with the record's spec, the second attaches to it, and each
// arrival is announced to the session.
func TestValidatedRoomStartsOneSession(t *testing.T) {
	hub, host := newGameHub(t)
	gameRoomOf(t, hub, "alice", "bob")

	assert.Equal(t, []string{"game-1"}, host.starts(), "one session for the room, not one per join")
	spec := host.specs["game-1"]
	assert.Equal(t, "test", spec.Mode)
	assert.Equal(t, []string{"alice", "bob"}, spec.Players)
	assert.Equal(t, []string{"alice", "bob"}, host.session("game-1").notices())
	assert.ElementsMatch(t, []string{"alice", "bob"}, membersOf(hub, "game-1"))
	assert.Equal(t, float64(1), counter(hub, `gamemesh_ws_game_rooms{service="ws-hub-test"}`))
}

// TestValidatedRoomRelayGoesToTheGameOnly: in a validated room a relay is
// handed to the session, with the identity of the connection and the data as
// sent, and is NOT fanned out. Passing it on as well would let a client
// reach the other player around the game.
func TestValidatedRoomRelayGoesToTheGameOnly(t *testing.T) {
	hub, host := newGameHub(t)
	clients := gameRoomOf(t, hub, "alice", "bob")
	alice, bob := clients[0], clients[1]

	readAt := time.Now()
	var msg inboundMessage
	msg.Action, msg.Room, msg.Data = "relay", "game-1", []byte(`{"t":"pos","x":1.5,  "from":"mallory"}`)
	alice.handleRelay(&msg, readAt)

	posts := host.session("game-1").posted()
	require.Len(t, posts, 1)
	assert.Equal(t, "alice", posts[0].playerID, "the sender is the connection's player, whatever data claims")
	assert.Equal(t, `{"t":"pos","x":1.5,  "from":"mallory"}`, posts[0].data, "data reaches the game byte-for-byte")
	assert.True(t, posts[0].arrived.Equal(readAt), "with the time it was read from the socket")
	assert.Empty(t, drainFrames(bob), "nothing is relayed past the game")
	assert.Empty(t, drainFrames(alice))
}

// TestValidatedRoomKeepsTheRelayChecks: membership, size and rate limit come
// before the game, as for any relay.
func TestValidatedRoomKeepsTheRelayChecks(t *testing.T) {
	hub, host := newGameHub(t)
	hub.SetRelayLimit(1, 1) // one message, then the bucket is empty
	clients := gameRoomOf(t, hub, "alice", "bob")
	alice := clients[0]
	carol := newHubClient(hub, "carol")
	hub.register(carol)

	sendRelay(t, carol, `{"action":"relay","room":"game-1","data":{"t":"pos"}}`)
	assert.Equal(t, float64(1), rejected(hub, "not_member"))

	sendRelay(t, alice, `{"action":"relay","room":"game-1","data":"`+strings.Repeat("x", maxRelayData)+`"}`)
	assert.Equal(t, float64(1), rejected(hub, "too_large"))

	sendRelay(t, alice, `{"action":"relay","room":"game-1","data":{"t":"pos","seq":1}}`) // the only token
	sendRelay(t, alice, `{"action":"relay","room":"game-1","data":{"t":"pos","seq":2}}`)
	assert.Equal(t, float64(1), rejected(hub, "rate_limited"))

	posts := host.session("game-1").posted()
	require.Len(t, posts, 1, "only the message that passed every check reached the game")
	assert.Equal(t, `{"t":"pos","seq":1}`, posts[0].data)
}

// TestGameTransportFrames: what the session sends goes out as Game frames
// with the payload untouched, to exactly the connections each method names.
func TestGameTransportFrames(t *testing.T) {
	hub, host := newGameHub(t)
	clients := gameRoomOf(t, hub, "alice", "bob", "alice") // alice has two tabs
	alice, bob, alice2 := clients[0], clients[1], clients[2]
	out := host.session("game-1").out

	out.Broadcast([]byte(`{"t":"snap",  "r":500}`))
	want := `{"type":"Game","room":"game-1","data":{"t":"snap",  "r":500}}`
	for _, c := range clients {
		assert.Equal(t, []string{want}, drainFrames(c), "Broadcast reaches every connection")
	}

	out.BroadcastExcept("alice", []byte(`{"t":"pos","from":"alice"}`), time.Now())
	assert.Equal(t, []string{`{"type":"Game","room":"game-1","data":{"t":"pos","from":"alice"}}`}, drainFrames(bob))
	assert.Empty(t, drainFrames(alice), "the sender gets no echo")
	assert.Empty(t, drainFrames(alice2), "on none of the sender's connections")

	out.SendTo("alice", []byte(`{"t":"clamp"}`))
	assert.Len(t, drainFrames(alice), 1)
	assert.Len(t, drainFrames(alice2), 1)
	assert.Empty(t, drainFrames(bob), "SendTo reaches one player only")
}

// TestGameTransportTimesAnswersApart: a position answer is timed in the
// validated room's histogram, and the relay's histogram does not move.
func TestGameTransportTimesAnswersApart(t *testing.T) {
	hub, host := newGameHub(t)
	gameRoomOf(t, hub, "alice", "bob")
	out := host.session("game-1").out
	const game = `gamemesh_ws_game_duration_seconds_count{service="ws-hub-test",stage="process"}`
	const relay = `gamemesh_ws_relay_duration_seconds_count{service="ws-hub-test",stage="process"}`

	out.BroadcastExcept("alice", []byte(`{"t":"pos"}`), time.Now().Add(-time.Millisecond))
	assert.Equal(t, float64(1), counter(hub, game))
	assert.Equal(t, float64(0), counter(hub, relay))

	// A bot's position was caused by no inbound message: nothing to time.
	out.BroadcastExcept("bot", []byte(`{"t":"pos"}`), time.Time{})
	assert.Equal(t, float64(1), counter(hub, game))
}

// TestGameTransportShedsForASlowConnection: a full send queue costs that
// connection the frame, counted, and holds nobody else up.
func TestGameTransportShedsForASlowConnection(t *testing.T) {
	hub, host := newGameHub(t)
	clients := gameRoomOf(t, hub, "alice", "bob")
	bob := clients[1]
	out := host.session("game-1").out

	for range sendBuffer + 3 {
		out.BroadcastExcept("alice", []byte(`{"t":"pos"}`), time.Now())
	}
	assert.Len(t, drainFrames(bob), sendBuffer)
	assert.Equal(t, float64(3), counter(hub, `gamemesh_ws_game_dropped_total{service="ws-hub-test"}`))
}

// TestGameTransportConnected: a player is connected while they have at least
// one connection in the room, and every change is announced to the session.
func TestGameTransportConnected(t *testing.T) {
	hub, host := newGameHub(t)
	clients := gameRoomOf(t, hub, "alice", "bob", "alice")
	alice, bob, alice2 := clients[0], clients[1], clients[2]
	s := host.session("game-1")
	require.Equal(t, []string{"alice", "bob", "alice"}, s.notices())

	hub.unregister(alice)
	assert.True(t, s.out.Connected("alice"), "the second tab covers the first")
	hub.leave(alice2, "game-1")
	assert.False(t, s.out.Connected("alice"))
	assert.True(t, s.out.Connected("bob"))

	hub.unregister(bob) // the last member: the room is empty, the session is still told
	assert.False(t, s.out.Connected("bob"))
	assert.Equal(t, []string{"alice", "bob", "alice", "alice", "alice", "bob"}, s.notices())

	hub.leave(alice2, "game-1") // not a member any more: nothing to announce
	assert.Len(t, s.notices(), 6)
}

// TestNotifyIsCalledWithoutTheHubLock: a session answers a notice by asking
// the hub who is connected, which takes the hub's read lock. If the hub
// called Notify while holding its write lock, that would never return.
func TestNotifyIsCalledWithoutTheHubLock(t *testing.T) {
	hub, host := newGameHub(t)
	var mu sync.Mutex
	var seen []bool
	host.prepare = func(s *fakeSession) {
		s.onNotify = func(playerID string) {
			connected := s.out.Connected(playerID)
			mu.Lock()
			seen = append(seen, connected)
			mu.Unlock()
		}
	}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		alice := newHubClient(hub, "alice")
		hub.register(alice)
		hub.join(alice, "game-1")
		bob := newHubClient(hub, "bob")
		hub.register(bob)
		hub.join(bob, "game-1")
		hub.leave(bob, "game-1")
		hub.unregister(alice)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("deadlock: the hub called Notify while holding its lock")
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []bool{true, true, false, false}, seen,
		"each notice already sees the change it announces")
}

// TestFinishedRoomIsClosed: when the match is over the room takes no more
// game traffic, cannot be started again, and does not become an
// opaque-relay room for the members still in it.
func TestFinishedRoomIsClosed(t *testing.T) {
	hub, host := newGameHub(t)
	clients := gameRoomOf(t, hub, "alice", "bob")
	alice, bob := clients[0], clients[1]
	host.session("game-1").finish(t, hub, "game-1")
	assert.Equal(t, float64(0), counter(hub, `gamemesh_ws_game_rooms{service="ws-hub-test"}`))

	sendRelay(t, alice, `{"action":"relay","room":"game-1","data":{"t":"pos"}}`)
	assert.Equal(t, float64(1), rejected(hub, "room_closed"))
	assert.Empty(t, host.session("game-1").posted())
	assert.Empty(t, drainFrames(bob), "a finished room is not an opaque-relay room")

	hub.unregister(bob)
	drainFrames(alice)
	bob2 := newHubClient(hub, "bob")
	hub.register(bob2)
	assert.False(t, hub.join(bob2, "game-1"), "a finished room cannot be rejoined")
	assert.Equal(t, float64(1), joinRejected(hub, "room_closed"))
	assert.Equal(t, []string{"game-1"}, host.starts(), "and the match is not started again")
	assert.NotContains(t, membersOf(hub, "game-1"), "bob")
}

// TestRoomPastTheStartWindowIsNotStarted: matchmaking keeps a room's record
// far longer than a match lasts. A room nobody started in time stays closed,
// so a restart of this service cannot replay matches from old records.
func TestRoomPastTheStartWindowIsNotStarted(t *testing.T) {
	hub, host := newGameHub(t)
	roomsOf(hub).addGame("old", gameStartWindow+time.Second, "alice", "bob")
	roomsOf(hub).addGame("young", gameStartWindow-10*time.Second, "alice", "bob")
	alice := newHubClient(hub, "alice")
	hub.register(alice)

	assert.False(t, hub.join(alice, "old"))
	assert.Equal(t, float64(1), joinRejected(hub, "room_closed"))
	assert.True(t, hub.join(alice, "young"))
	assert.Equal(t, []string{"young"}, host.starts())
}

// TestValidatedRoomWithoutAHostIsRejected: a hub that runs no games refuses
// a validated room. Serving it as an opaque-relay room would switch the
// validation off by configuration.
func TestValidatedRoomWithoutAHostIsRejected(t *testing.T) {
	hub := newTestHub()
	roomsOf(hub).addGame("game-1", 0, "alice", "bob")
	alice := newHubClient(hub, "alice")
	hub.register(alice)

	assert.False(t, hub.join(alice, "game-1"))
	assert.Equal(t, float64(1), joinRejected(hub, "unavailable"))
	assert.Empty(t, membersOf(hub, "game-1"))
}

// TestGameThatCannotStartRejectsTheJoin: a mode the host does not know.
func TestGameThatCannotStartRejectsTheJoin(t *testing.T) {
	hub, host := newGameHub(t)
	host.err = errors.New("unknown mode")
	alice := newHubClient(hub, "alice")
	hub.register(alice)

	assert.False(t, hub.join(alice, "game-1"))
	assert.Equal(t, float64(1), joinRejected(hub, "unavailable"))
	assert.Empty(t, membersOf(hub, "game-1"))
	assert.Equal(t, float64(0), counter(hub, `gamemesh_ws_game_rooms{service="ws-hub-test"}`))
}

// TestCloseAllStopsGamesBeforeConnections: at shutdown every session is
// cancelled while its players are still connected. A session that first saw
// its players' connections close would take the shutdown for both of them
// leaving, and record a result for a match the server itself ended.
func TestCloseAllStopsGamesBeforeConnections(t *testing.T) {
	hub, host := newGameHub(t)
	host.prepare = func(s *fakeSession) { s.watch = "alice" }
	clients := gameRoomOf(t, hub, "alice", "bob")
	s := host.session("game-1")

	// The test's clients have no socket, so CloseAll has nothing to close;
	// they disconnect once it has had time to stop the games.
	go func() {
		<-s.Done()
		for _, c := range clients {
			hub.unregister(c)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hub.CloseAll(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	assert.True(t, s.cancelled, "the session was stopped by its context")
	assert.True(t, s.connectedAtCancel, "while its players were still connected")
}

// TestPanickingGameEndsOnlyItsRoom: a bug in one room's game closes that
// room; the hub, and with it every other room, keeps running. The logged
// error is what shows the panic was caught: without the recover the process
// dies, but possibly only after this test has returned.
func TestPanickingGameEndsOnlyItsRoom(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	rooms := newFakeRooms()
	rooms.add("room-1", "bob")
	rooms.addGame("game-1", 0, "alice", "bob")
	hub := NewHub(zap.New(core), metrics.New("ws-hub-test"), nil, rooms)
	host := &fakeHost{specs: map[string]GameSpec{}, sessions: map[string]*fakeSession{}}
	host.prepare = func(s *fakeSession) { s.panicInRun = true }
	hub.SetGames(context.Background(), host, gameStartWindow)

	alice := newHubClient(hub, "alice")
	hub.register(alice)
	require.True(t, hub.join(alice, "game-1"))

	require.Eventually(t, func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		return hub.games["game-1"].finished
	}, time.Second, time.Millisecond)
	require.Equal(t, 1, logs.FilterMessage("validated room panicked").Len(), "the panic was recovered and logged")
	assert.Equal(t, float64(1), counter(hub, `gamemesh_ws_game_events_total{event="room_panic",service="ws-hub-test"}`))
	frames := drainFrames(alice)
	require.Len(t, frames, 1, "the hub sends nothing in the game's name")
	assert.Contains(t, frames[0], `"type":"PlayerJoined"`)

	bob := newHubClient(hub, "bob")
	hub.register(bob)
	assert.True(t, hub.join(bob, "room-1"), "the hub still serves other rooms")
}

// TestFinishedRoomsAreForgotten: a finished room is remembered while it has
// members or could still be started again, and not for ever.
func TestFinishedRoomsAreForgotten(t *testing.T) {
	hub, host := newGameHub(t)
	clients := gameRoomOf(t, hub, "alice", "bob")
	host.session("game-1").finish(t, hub, "game-1")

	prune := func(after time.Duration) bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		hub.pruneGames(time.Now().Add(after))
		return hub.games["game-1"] != nil
	}
	assert.True(t, prune(2*gameStartWindow), "members are still in it")
	hub.unregister(clients[0])
	hub.unregister(clients[1])
	assert.True(t, prune(gameStartWindow/2), "its record could still start it again")
	assert.False(t, prune(2*gameStartWindow))
}

// TestOpaqueRoomIsUntouchedByGames: on a hub that runs games, a room whose
// record names no game is relayed exactly as before.
func TestOpaqueRoomIsUntouchedByGames(t *testing.T) {
	hub, host := newGameHub(t)
	alice, bob := newHubClient(hub, "alice"), newHubClient(hub, "bob")
	for _, c := range []*Client{alice, bob} {
		hub.register(c)
		require.True(t, hub.join(c, "room-1"))
		drainFrames(c)
	}
	drainFrames(alice)

	sendRelay(t, alice, `{"action":"relay","room":"room-1","data":{"t":"pos"}}`)
	assert.Equal(t, []string{`{"type":"Relay","room":"room-1","from":"alice","data":{"t":"pos"}}`}, drainFrames(bob))
	assert.Empty(t, host.starts())
}
