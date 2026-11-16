package ghosttag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Session tests run on real time with a fast Config. None of them sleeps:
// each waits for something observable (a recorded message, a counted
// event, Done) and fails if it does not come within waitLimit. An absence
// (no store call, no match_end) is asserted only after Done has closed,
// when the loop can no longer produce anything.

const waitLimit = 10 * time.Second

// fastConfig is a whole match in well under a second.
func fastConfig() Config {
	c := DefaultConfig()
	c.Tick = time.Millisecond
	c.THalf = 300 * time.Millisecond
	c.TShrink = 100 * time.Millisecond
	c.TCountdown = 20 * time.Millisecond
	c.TRoleSwap = 20 * time.Millisecond
	c.TGrace = 60 * time.Millisecond
	c.TJoin = 2 * time.Second
	c.PressureInterval = 5 * time.Millisecond
	c.BotSightDelay = 50 * time.Millisecond
	c.BotReactionMin = 5 * time.Millisecond
	c.BotReactionMax = 10 * time.Millisecond
	c.SettleTimeout = 2 * time.Second
	return c
}

// signal is a change notification: every record pokes it, and a waiter
// re-checks its condition on each poke.
type signal chan struct{}

func (s signal) poke() {
	select {
	case s <- struct{}{}:
	default:
	}
}

// fakeTransport records what the session sends and answers Connected from
// flags the test sets. sendCalls is deliberately unsynchronised: only the
// session's goroutine may call the send methods, and if anything else did,
// the race detector would report it here.
type fakeTransport struct {
	mu        sync.Mutex
	msgs      []sent
	connected map[string]bool
	changed   signal
	sendCalls int

	// panicOn, if set, panics on a broadcast whose payload contains it.
	panicOn string
	// panics counts the panics it raised.
	panics atomic.Int32
}

func newFakeTransport(connected ...string) *fakeTransport {
	f := &fakeTransport{connected: map[string]bool{}, changed: make(signal, 1)}
	for _, id := range connected {
		f.connected[id] = true
	}
	return f
}

func (f *fakeTransport) record(how, who string, payload []byte) {
	f.sendCalls++
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		panic(fmt.Sprintf("invalid JSON %q", payload))
	}
	f.mu.Lock()
	f.msgs = append(f.msgs, sent{how: how, who: who, raw: string(payload), body: body})
	f.mu.Unlock()
	f.changed.poke()
}

func (f *fakeTransport) Broadcast(p []byte) {
	f.mu.Lock()
	trigger := f.panicOn != "" && strings.Contains(string(p), f.panicOn)
	f.mu.Unlock()
	if trigger {
		f.panics.Add(1)
		panic("fake transport: " + f.panicOn)
	}
	f.record("all", "", p)
}
func (f *fakeTransport) BroadcastExcept(id string, p []byte, _ time.Time) { f.record("except", id, p) }
func (f *fakeTransport) SendTo(id string, p []byte)                       { f.record("to", id, p) }

func (f *fakeTransport) Connected(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected[id]
}

func (f *fakeTransport) setConnected(id string, c bool) {
	f.mu.Lock()
	f.connected[id] = c
	f.mu.Unlock()
}

func (f *fakeTransport) snapshot() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.msgs...)
}

func (f *fakeTransport) ofType(t string) []sent {
	var out []sent
	for _, m := range f.snapshot() {
		if m.typ() == t {
			out = append(out, m)
		}
	}
	return out
}

// waitFor blocks until cond holds on what f recorded.
func (f *fakeTransport) waitFor(t *testing.T, what string, cond func([]sent) bool) {
	t.Helper()
	deadline := time.After(waitLimit)
	for !cond(f.snapshot()) {
		select {
		case <-f.changed:
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func hasSnap(state string) func([]sent) bool {
	return func(ms []sent) bool {
		for _, m := range ms {
			if m.typ() == "snap" && m.body["state"] == state {
				return true
			}
		}
		return false
	}
}

// syncCounts is an Observer safe for the session's and the posters'
// goroutines.
type syncCounts struct {
	mu sync.Mutex
	n  map[string]int
}

func newSyncCounts() *syncCounts { return &syncCounts{n: map[string]int{}} }

func (c *syncCounts) Count(e string) {
	c.mu.Lock()
	c.n[e]++
	c.mu.Unlock()
}

func (c *syncCounts) get(e string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[e]
}

// fakeStore settles by calling score with the given standings, as a real
// store does inside its transaction, unless err or unranked say otherwise.
type fakeStore struct {
	calls    atomic.Int32
	prior    map[string]Standing
	err      error
	unranked bool
	match    atomic.Pointer[Match]
}

func (s *fakeStore) Settle(_ context.Context, m Match, score func(map[string]Standing) map[string]Award) (Settlement, error) {
	s.calls.Add(1)
	s.match.Store(&m)
	if s.err != nil {
		return Settlement{}, s.err
	}
	awards := score(s.prior)
	if s.unranked {
		// A store that wrongly returns awards with Ranked false: the
		// session must still show zeros.
		return Settlement{Ranked: false, Awards: awards}, nil
	}
	return Settlement{Ranked: true, Awards: awards}, nil
}

type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logs) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

type sessionRig struct {
	s     *Session
	tr    *fakeTransport
	store *fakeStore
	obs   *syncCounts
	log   *logs
}

func newRig(t *testing.T, cfg Config, spec Spec, tr *fakeTransport, store *fakeStore) *sessionRig {
	t.Helper()
	rig := &sessionRig{tr: tr, store: store, obs: newSyncCounts(), log: &logs{}}
	spec.Logf = rig.log.logf
	var ss ScoreStore
	if store != nil {
		ss = store
	}
	s, err := NewSession(cfg, spec, tr, ss, rig.obs)
	require.NoError(t, err)
	rig.s = s
	return rig
}

func (r *sessionRig) run(ctx context.Context) {
	go r.s.Run(ctx)
}

func (r *sessionRig) waitDone(t *testing.T) {
	t.Helper()
	select {
	case <-r.s.Done():
	case <-time.After(waitLimit):
		t.Fatal("the session did not finish")
	}
}

func (r *sessionRig) matchEnd(t *testing.T) map[string]any {
	t.Helper()
	ends := r.tr.ofType("match_end")
	require.Len(t, ends, 1, "exactly one match_end")
	assert.Equal(t, "all", ends[0].how)
	return ends[0].body
}

func pvp() Spec { return Spec{RoomID: "room-s", Players: []string{"a", "b"}} }

// TestSessionRankedDrawSettlesOnce: two players who never move draw on
// equal pressure (450 at the spawns). The store is called once, with the
// room, both players and the end time, and match_end carries the deltas it
// returned for both: +20 each.
func TestSessionRankedDrawSettlesOnce(t *testing.T) {
	store := &fakeStore{prior: map[string]Standing{"a": {WinStreak: 2}}}
	rig := newRig(t, fastConfig(), pvp(), newFakeTransport("a", "b"), store)
	rig.run(context.Background())
	rig.waitDone(t)

	assert.Equal(t, int32(1), store.calls.Load())
	m := store.match.Load()
	require.NotNil(t, m)
	assert.Equal(t, "room-s", m.RoomID)
	assert.ElementsMatch(t, []string{"a", "b"}, m.Players)
	assert.False(t, m.EndedAt.IsZero())

	end := rig.matchEnd(t)
	assert.Equal(t, map[string]any{
		"t": "match_end", "winner": "", "is_draw": true, "ranked": true,
		"deltas": map[string]any{"a": 20.0, "b": 20.0},
	}, end)
	assert.Equal(t, 1, rig.obs.get(EventRanked))
	assert.Equal(t, 1, rig.obs.get(EventRolesDraw))
	assert.Len(t, rig.tr.ofType("half_end"), 2)
}

// TestSessionBotMatchNeverReachesTheStore: a bot match is played out and
// reported unranked with zeros for the human and the bot; the store is
// never called.
func TestSessionBotMatchNeverReachesTheStore(t *testing.T) {
	store := &fakeStore{}
	rig := newRig(t, fastConfig(), Spec{RoomID: "room-bot", Players: []string{"h"}, Bot: true}, newFakeTransport("h"), store)
	rig.run(context.Background())
	rig.waitDone(t)

	assert.Zero(t, store.calls.Load())
	end := rig.matchEnd(t)
	assert.Equal(t, false, end["ranked"])
	assert.Equal(t, map[string]any{"h": 0.0, "bot": 0.0}, end["deltas"])
	assert.NotEmpty(t, rig.tr.ofType("pos"), "the bot played")
}

// TestSessionUnrankedRoomsShowZeros: a room created unranked never reaches
// the store; a match the store declines (same pair, same day) shows zeros
// even if the store returned awards.
func TestSessionUnrankedRoomsShowZeros(t *testing.T) {
	store := &fakeStore{}
	spec := pvp()
	spec.Unranked = true
	rig := newRig(t, fastConfig(), spec, newFakeTransport("a", "b"), store)
	rig.run(context.Background())
	rig.waitDone(t)
	assert.Zero(t, store.calls.Load())
	end := rig.matchEnd(t)
	assert.Equal(t, false, end["ranked"])
	assert.Equal(t, map[string]any{"a": 0.0, "b": 0.0}, end["deltas"])

	samePair := &fakeStore{unranked: true}
	rig = newRig(t, fastConfig(), pvp(), newFakeTransport("a", "b"), samePair)
	rig.run(context.Background())
	rig.waitDone(t)
	assert.Equal(t, int32(1), samePair.calls.Load())
	end = rig.matchEnd(t)
	assert.Equal(t, false, end["ranked"])
	assert.Equal(t, map[string]any{"a": 0.0, "b": 0.0}, end["deltas"])
	assert.Zero(t, rig.obs.get(EventRanked))
}

// TestSessionSettleFailureIsUnranked: a store error shows no points, is
// counted and logged with the result.
func TestSessionSettleFailureIsUnranked(t *testing.T) {
	store := &fakeStore{err: errors.New("redis down")}
	rig := newRig(t, fastConfig(), pvp(), newFakeTransport("a", "b"), store)
	rig.run(context.Background())
	rig.waitDone(t)

	end := rig.matchEnd(t)
	assert.Equal(t, false, end["ranked"])
	assert.Equal(t, map[string]any{"a": 0.0, "b": 0.0}, end["deltas"])
	assert.Equal(t, true, end["is_draw"], "the result stands; only the points are withheld")
	assert.Equal(t, 1, rig.obs.get(EventSettleFailed))
	assert.Contains(t, rig.log.all(), "redis down")
	assert.Contains(t, rig.log.all(), "outcome:1")
}

// TestSessionShutdownWritesNothing: cancelling the context in the middle of
// a half ends the session with no forfeit, no store call and no match_end.
func TestSessionShutdownWritesNothing(t *testing.T) {
	store := &fakeStore{}
	rig := newRig(t, fastConfig(), pvp(), newFakeTransport("a", "b"), store)
	ctx, cancel := context.WithCancel(context.Background())
	rig.run(ctx)
	rig.tr.waitFor(t, "HALF_1_ACTIVE", hasSnap("HALF_1_ACTIVE"))

	cancel()
	rig.waitDone(t)

	assert.Zero(t, store.calls.Load())
	assert.Empty(t, rig.tr.ofType("match_end"))
	assert.Zero(t, rig.obs.get(EventForfeit))
	assert.Contains(t, rig.log.all(), "cancelled in HALF_1_ACTIVE")
}

// TestSessionJoinTimeout: a player who never connects cancels the match;
// the one who did is told, unranked, no winner, and nobody forfeits.
func TestSessionJoinTimeout(t *testing.T) {
	cfg := fastConfig()
	cfg.TJoin = 50 * time.Millisecond
	store := &fakeStore{}
	rig := newRig(t, cfg, pvp(), newFakeTransport("a"), store)
	rig.run(context.Background())
	rig.waitDone(t)

	end := rig.matchEnd(t)
	assert.Equal(t, map[string]any{
		"t": "match_end", "winner": "", "is_draw": false, "ranked": false,
		"deltas": map[string]any{"a": 0.0, "b": 0.0},
	}, end)
	assert.Equal(t, 1, rig.obs.get(EventJoinTimeout))
	assert.Zero(t, rig.obs.get(EventForfeit))
	assert.Zero(t, store.calls.Load())
}

// TestSessionForfeit: a player who drops and does not come back loses 25
// and their streak; the one who stayed gets 40 and keeps theirs.
func TestSessionForfeit(t *testing.T) {
	store := &fakeStore{prior: map[string]Standing{"a": {WinStreak: 2}, "b": {WinStreak: 3}}}
	tr := newFakeTransport("a", "b")
	rig := newRig(t, fastConfig(), pvp(), tr, store)
	rig.run(context.Background())
	tr.waitFor(t, "HALF_1_ACTIVE", hasSnap("HALF_1_ACTIVE"))

	tr.setConnected("b", false)
	rig.s.Notify("b")
	rig.waitDone(t)

	end := rig.matchEnd(t)
	assert.Equal(t, "a", end["winner"])
	assert.Equal(t, map[string]any{"a": 40.0, "b": -25.0}, end["deltas"])
	assert.Equal(t, 1, rig.obs.get(EventForfeit))
	assert.Zero(t, rig.obs.get(EventFirstHunterWon)+rig.obs.get(EventSecondHunterWon)+rig.obs.get(EventRolesDraw))
}

// TestSessionPresenceIsLevelTriggered: Notify carries no state; the
// session asks the transport. A drop sends the room to the grace, the
// return resumes it, and a Notify that changes nothing still sends the
// player the state.
func TestSessionPresenceIsLevelTriggered(t *testing.T) {
	tr := newFakeTransport("a", "b")
	rig := newRig(t, fastConfig(), pvp(), tr, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig.run(ctx)
	tr.waitFor(t, "HALF_1_ACTIVE", hasSnap("HALF_1_ACTIVE"))

	tr.setConnected("b", false)
	rig.s.Notify("b")
	tr.waitFor(t, "the grace", hasSnap("DISCONNECT_GRACE"))
	tr.setConnected("b", true)
	rig.s.Notify("b")
	tr.waitFor(t, "the half resumed", func(ms []sent) bool {
		return hasSnapAfter(ms, "DISCONNECT_GRACE", "HALF_1_ACTIVE")
	})

	rig.s.Notify("a")
	tr.waitFor(t, "a snap to a alone", func(ms []sent) bool {
		for _, m := range ms {
			if m.how == "to" && m.who == "a" && m.typ() == "snap" {
				return true
			}
		}
		return false
	})
	cancel()
	rig.waitDone(t)
}

// hasSnapAfter reports a snap of state second after one of state first.
func hasSnapAfter(ms []sent, first, second string) bool {
	seen := false
	for _, m := range ms {
		if m.typ() != "snap" {
			continue
		}
		if m.body["state"] == first {
			seen = true
		} else if seen && m.body["state"] == second {
			return true
		}
	}
	return false
}

// TestSessionServerTsNeverGoesBackwards: a packet that was read earlier but
// reaches the inbox later (two readers racing) is stamped no earlier than
// the one before it.
func TestSessionServerTsNeverGoesBackwards(t *testing.T) {
	tr := newFakeTransport("a", "b")
	rig := newRig(t, fastConfig(), pvp(), tr, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig.run(ctx)
	tr.waitFor(t, "HALF_1_ACTIVE", hasSnap("HALF_1_ACTIVE"))

	now := time.Now()
	require.True(t, rig.s.Post("a", []byte(`{"t":"pos","seq":1,"x":0,"y":10}`), now))
	require.True(t, rig.s.Post("a", []byte(`{"t":"pos","seq":2,"x":0,"y":20}`), now.Add(-500*time.Millisecond)))
	tr.waitFor(t, "two positions", func(ms []sent) bool { return countType(ms, "pos") >= 2 })

	pos := tr.ofType("pos")
	assert.GreaterOrEqual(t, pos[1].body["sts"].(float64), pos[0].body["sts"].(float64))
	cancel()
	rig.waitDone(t)
}

func countType(ms []sent, t string) int {
	n := 0
	for _, m := range ms {
		if m.typ() == t {
			n++
		}
	}
	return n
}

// TestSessionPingPong: a ping is answered to its sender alone, with its cts
// and the server's sts; an unknown type is counted as invalid.
func TestSessionPingPong(t *testing.T) {
	tr := newFakeTransport("a", "b")
	rig := newRig(t, fastConfig(), pvp(), tr, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.True(t, rig.s.Post("b", []byte(`{"t":"relay","data":1}`), time.Now()))
	require.True(t, rig.s.Post("a", []byte(`{"t":"ping","cts":173168}`), time.Now()))
	rig.run(ctx)
	tr.waitFor(t, "a pong", func(ms []sent) bool { return countType(ms, "pong") == 1 })

	pong := tr.ofType("pong")[0]
	assert.Equal(t, "to", pong.how)
	assert.Equal(t, "a", pong.who)
	assert.Equal(t, 173168.0, pong.body["cts"])
	cancel()
	rig.waitDone(t)
	assert.Equal(t, 1, rig.obs.get(EventInvalidPayload))
}

// TestSessionPostDropsWhenTheInboxIsFull: Post never blocks; past the
// inbox's size it drops and counts, and after the match it refuses quietly.
func TestSessionPostDropsWhenTheInboxIsFull(t *testing.T) {
	cfg := fastConfig()
	cfg.InboxSize = 2
	rig := newRig(t, cfg, pvp(), newFakeTransport(), nil)
	payload := []byte(`{"t":"ping","cts":1}`)
	assert.True(t, rig.s.Post("a", payload, time.Now()))
	assert.True(t, rig.s.Post("a", payload, time.Now()))
	assert.False(t, rig.s.Post("a", payload, time.Now()))
	assert.Equal(t, 1, rig.obs.get(EventInboxDropped))

	ctx, cancel := context.WithCancel(context.Background())
	rig.run(ctx)
	cancel()
	rig.waitDone(t)
	assert.False(t, rig.s.Post("a", payload, time.Now()))
	assert.Equal(t, 1, rig.obs.get(EventInboxDropped), "not counted once the session is over")
	rig.s.Notify("a") // returns: the session is done
}

// TestSessionRoomPanicIsVoidAndUnranked: the transport panics when the
// first half ends. The session recovers, counts room_panic, logs the panic
// with its stack, never calls the store, sends one void match_end, and
// closes Done.
func TestSessionRoomPanicIsVoidAndUnranked(t *testing.T) {
	store := &fakeStore{}
	tr := newFakeTransport("a", "b")
	tr.panicOn = `"t":"half_end"`
	rig := newRig(t, fastConfig(), pvp(), tr, store)
	rig.run(context.Background())
	rig.waitDone(t)

	require.Equal(t, int32(1), tr.panics.Load(), "the fake did panic")
	assert.Equal(t, 1, rig.obs.get(EventRoomPanic))
	assert.Zero(t, store.calls.Load())
	end := rig.matchEnd(t)
	assert.Equal(t, map[string]any{
		"t": "match_end", "winner": "", "is_draw": false, "ranked": false,
		"deltas": map[string]any{"a": 0.0, "b": 0.0},
	}, end)
	assert.Contains(t, rig.log.all(), "fake transport")
	assert.Contains(t, rig.log.all(), "goroutine", "the stack is logged")
}

// TestSessionPanicInTheVoidBroadcastIsSwallowed: if the void match_end
// panics too, the session still ends cleanly.
func TestSessionPanicInTheVoidBroadcastIsSwallowed(t *testing.T) {
	store := &fakeStore{}
	tr := newFakeTransport("a", "b")
	tr.panicOn = `"t":` // every broadcast
	rig := newRig(t, fastConfig(), pvp(), tr, store)
	rig.run(context.Background())
	rig.waitDone(t)

	assert.Equal(t, int32(2), tr.panics.Load(), "the first broadcast and the void match_end")
	assert.Equal(t, 1, rig.obs.get(EventRoomPanic))
	assert.Zero(t, store.calls.Load())
}

// TestNewSessionRejects: specs the room cannot be built from.
func TestNewSessionRejects(t *testing.T) {
	tr := newFakeTransport()
	for name, spec := range map[string]Spec{
		"no room":           {Players: []string{"a", "b"}},
		"one human":         {RoomID: "r", Players: []string{"a"}},
		"same human twice":  {RoomID: "r", Players: []string{"a", "a"}},
		"empty id":          {RoomID: "r", Players: []string{"a", ""}},
		"bot and two":       {RoomID: "r", Players: []string{"a", "b"}, Bot: true},
		"human is the bot":  {RoomID: "r", Players: []string{"bot"}, Bot: true},
		"human is bot id x": {RoomID: "r", Players: []string{"x"}, Bot: true, BotID: "x"},
	} {
		_, err := NewSession(DefaultConfig(), spec, tr, nil, nil)
		assert.Error(t, err, name)
	}
	_, err := NewSession(DefaultConfig(), pvp(), nil, nil, nil)
	assert.Error(t, err, "nil transport")
	bad := DefaultConfig()
	bad.Alpha = 1
	_, err = NewSession(bad, pvp(), tr, nil, nil)
	assert.Error(t, err, "invalid config")
}
