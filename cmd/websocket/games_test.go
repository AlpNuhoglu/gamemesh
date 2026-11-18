package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/ghosttag"
	"github.com/alpnuhoglu/gamemesh/internal/leaderboard"
	"github.com/alpnuhoglu/gamemesh/internal/matchmaking"
	"github.com/alpnuhoglu/gamemesh/internal/wsgateway"
	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
)

// This file is the end-to-end half of the concurrency proof: real sockets,
// the real hub and the real game, under -race. Every goroutine of the
// service takes part — two read pumps posting into the room, the room's own
// goroutine, two write pumps, a connection closing and another joining in
// the middle of a half — and the race detector watches all of them.
//
// It waits on frames, never on elapsed time: the timeouts only bound a
// failure.

// gameRooms is a RoomChecker whose every room is a Ghost Tag room of the
// given players.
type gameRooms struct{ players []string }

func (g gameRooms) Access(_ context.Context, _ string, playerID string) (wsgateway.RoomAccess, error) {
	return wsgateway.RoomAccess{
		Participant: slices.Contains(g.players, playerID),
		Game:        &wsgateway.GameSpec{Mode: matchmaking.ModeGhostTag, Players: g.players, CreatedAt: time.Now()},
	}, nil
}

type allSessions struct{}

func (allSessions) Exists(context.Context, string) (bool, error) { return true, nil }

// countingStore is the service's real score store (gameScores, on a
// miniredis) behind a counter of how often the game asks it.
type countingStore struct {
	calls atomic.Int32
	real  gameScores
}

func (s *countingStore) Settle(ctx context.Context, m ghosttag.Match, score func(map[string]ghosttag.Standing) map[string]ghosttag.Award) (ghosttag.Settlement, error) {
	s.calls.Add(1)
	return s.real.Settle(ctx, m, score)
}

type eventCounts struct {
	mu sync.Mutex
	n  map[string]int
}

func (e *eventCounts) Count(event string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.n[event]++
}

func (e *eventCounts) get(event string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.n[event]
}

// fastGame is the game with its clocks shortened: a match takes about a
// second. The grace is long, so the test's reconnect cannot lose a race
// against it.
func fastGame() ghosttag.Config {
	c := ghosttag.DefaultConfig()
	c.Tick = 2 * time.Millisecond
	c.THalf = 400 * time.Millisecond
	c.TShrink = 100 * time.Millisecond
	c.TCountdown = 50 * time.Millisecond
	c.TRoleSwap = 50 * time.Millisecond
	c.TGrace = 5 * time.Second
	c.TJoin = 5 * time.Second
	c.PressureInterval = 10 * time.Millisecond
	c.BotSightDelay = 50 * time.Millisecond
	c.BotReactionMin = 5 * time.Millisecond
	c.BotReactionMax = 10 * time.Millisecond
	return c
}

const frameTimeout = 10 * time.Second

// gameFrame is one Game frame as a client saw it.
type gameFrame struct {
	Room string
	Data map[string]any
	Raw  string
}

// gameClient is one player's connection: a reader that records Game frames
// and, on request, a sender that reports positions.
type gameClient struct {
	t    *testing.T
	id   string
	conn *websocket.Conn

	writeMu sync.Mutex // gorilla allows one writer at a time

	mu     sync.Mutex
	frames []gameFrame
	other  []string // frames that are not Game frames, by type
	change chan struct{}

	// homeX is the player's own x in the latest snap: where the sender
	// stays, so the two players never come within capture distance.
	homeX atomic.Int64

	stop     chan struct{}
	stopped  chan struct{}
	readDone chan struct{}
}

func dialGame(t *testing.T, url, id, token string) *gameClient {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url+"/ws?token="+token, nil)
	require.NoError(t, err)
	c := &gameClient{t: t, id: id, conn: conn, change: make(chan struct{}, 1), readDone: make(chan struct{})}
	go c.read()
	c.write(`{"action":"join","room":"g1"}`)
	return c
}

func (c *gameClient) write(msg string) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.WriteMessage(websocket.TextMessage, []byte(msg))
}

func (c *gameClient) read() {
	defer close(c.readDone)
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var env struct {
			Type string          `json:"type"`
			Room string          `json:"room"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		c.mu.Lock()
		if env.Type != "Game" {
			c.other = append(c.other, env.Type)
			c.mu.Unlock()
			continue
		}
		f := gameFrame{Room: env.Room, Raw: string(raw)}
		_ = json.Unmarshal(env.Data, &f.Data)
		if f.Data["t"] == "snap" {
			for _, p := range f.Data["players"].([]any) {
				if p := p.(map[string]any); p["id"] == c.id {
					c.homeX.Store(int64(p["x"].(float64)))
				}
			}
		}
		c.frames = append(c.frames, f)
		c.mu.Unlock()
		select {
		case c.change <- struct{}{}:
		default:
		}
	}
}

// waitFor returns the first frame, at or after index from, that match
// accepts, and the index after it.
func (c *gameClient) waitFor(from int, what string, match func(gameFrame) bool) (gameFrame, int) {
	c.t.Helper()
	deadline := time.After(frameTimeout)
	for {
		c.mu.Lock()
		for i := from; i < len(c.frames); i++ {
			if match(c.frames[i]) {
				f := c.frames[i]
				c.mu.Unlock()
				return f, i + 1
			}
		}
		c.mu.Unlock()
		select {
		case <-c.change:
		case <-c.readDone:
			c.t.Fatalf("%s: connection closed while waiting for %s", c.id, what)
		case <-deadline:
			c.t.Fatalf("%s: no %s within %s", c.id, what, frameTimeout)
		}
	}
}

func isType(t string) func(gameFrame) bool {
	return func(f gameFrame) bool { return f.Data["t"] == t }
}

func isSnap(state string) func(gameFrame) bool {
	return func(f gameFrame) bool { return f.Data["t"] == "snap" && f.Data["state"] == state }
}

func (c *gameClient) all(t string) []gameFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []gameFrame
	for _, f := range c.frames {
		if f.Data["t"] == t {
			out = append(out, f)
		}
	}
	return out
}

// otherTypes returns the types of the frames that were not Game frames.
func (c *gameClient) otherTypes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.other...)
}

// startSending reports a position every few milliseconds, circling close to
// where the latest snap put the player, with a ping now and then. The circle
// turns 0.005 rad a packet, 1.7 rad/s at the nominal 3 ms, so it stays under
// the runner's turn limit (OmegaMax·dt) unless the sender's timer bunches
// two packets within about a millisecond. The test asserts nothing about
// clamps either way.
func (c *gameClient) startSending(firstSeq int) {
	c.stop, c.stopped = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(c.stopped)
		tick := time.NewTicker(3 * time.Millisecond)
		defer tick.Stop()
		for seq := firstSeq; ; seq++ {
			select {
			case <-c.stop:
				return
			case <-tick.C:
			}
			a := float64(seq) / 200
			x, y := float64(c.homeX.Load())+8*math.Cos(a), 8*math.Sin(a)
			c.write(fmt.Sprintf(`{"action":"relay","room":"g1","data":{"t":"pos","seq":%d,"cts":%d,"x":%.2f,"y":%.2f,"vx":10,"vy":10}}`, seq, seq, x, y))
			if seq%50 == 0 {
				c.write(`{"action":"relay","room":"g1","data":{"t":"ping","cts":7}}`)
			}
		}
	}()
}

func (c *gameClient) stopSending() {
	close(c.stop)
	<-c.stopped
}

// TestGhostTagMatchOverRealSockets plays one whole match between two
// connections through the real hub: join, countdown, a half in which one
// player's connection drops and a new one takes its place, the role swap,
// the second half, the settlement and match_end.
func TestGhostTagMatchOverRealSockets(t *testing.T) {
	alice, bob := uuid.New(), uuid.New()
	ids := []string{alice.String(), bob.String()}
	scores, mr, announced := newGameScores(t)
	store := &countingStore{real: scores}
	events := &eventCounts{n: map[string]int{}}

	hub := wsgateway.NewHub(zap.NewNop(), metrics.New("ws-game-test"), nil, gameRooms{players: ids})
	hub.SetRelayLimit(5000, 5000) // the test sends faster than a client may
	ctx, cancel := context.WithCancel(context.Background())
	hub.SetGames(ctx, gameHost{cfg: fastGame(), scores: store, events: events, log: zap.NewNop()}, time.Minute)

	tokens := auth.NewTokenManager("ws-game-test-secret", time.Hour, "gamemesh")
	origins, err := middleware.ParseOrigins([]string{"*"})
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	wsgateway.NewHandler(hub, tokens, allSessions{}, origins, zap.NewNop()).RegisterRoutes(engine)
	srv := httptest.NewServer(engine)
	t.Cleanup(func() {
		cancel()
		stopCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		hub.CloseAll(stopCtx)
		srv.Close()
	})
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	aliceToken, _, err := tokens.Generate(alice, "alice")
	require.NoError(t, err)
	bobToken, _, err := tokens.Generate(bob, "bob")
	require.NoError(t, err)

	a := dialGame(t, url, ids[0], aliceToken)
	b := dialGame(t, url, ids[1], bobToken)

	// The first half starts once both have joined.
	_, aAt := a.waitFor(0, "snap HALF_1_ACTIVE", isSnap("HALF_1_ACTIVE"))
	b.waitFor(0, "snap HALF_1_ACTIVE", isSnap("HALF_1_ACTIVE"))
	a.startSending(1)
	b.startSending(1)

	// Each sees the other's validated positions, in Game frames.
	pos, aAt := a.waitFor(aAt, "a position of bob", isType("pos"))
	assert.Equal(t, "g1", pos.Room)
	assert.Equal(t, ids[1], pos.Data["from"], "from is the sender's identity from its JWT")
	assert.NotContains(t, pos.Data, "seq", "no client claim is passed on")
	assert.NotContains(t, pos.Data, "cts")
	assert.Contains(t, pos.Raw, `{"type":"Game","room":"g1","data":{"t":"pos","from":"`+ids[1]+`"`)
	b.waitFor(0, "a position of alice", isType("pos"))

	// Bob's connection drops in the middle of the half: the room stops.
	b.stopSending()
	_ = b.conn.Close()
	grace, aAt := a.waitFor(aAt, "snap DISCONNECT_GRACE", isSnap("DISCONNECT_GRACE"))
	remAtGrace := grace.Data["rem_ms"].(float64)

	// A new connection of bob joins: the half goes on from where it stopped.
	b2 := dialGame(t, url, ids[1], bobToken)
	resumed, aAt := a.waitFor(aAt, "snap HALF_1_ACTIVE after the return", isSnap("HALF_1_ACTIVE"))
	assert.InDelta(t, remAtGrace, resumed.Data["rem_ms"].(float64), 1, "the half clock did not run during the grace")
	b2.waitFor(0, "snap HALF_1_ACTIVE for the new connection", isSnap("HALF_1_ACTIVE"))
	b2.startSending(1) // a new client instance counts from 1 again

	// Both halves run out; nobody came near the other.
	_, aAt = a.waitFor(aAt, "half_end 1", isType("half_end"))
	_, aAt = a.waitFor(aAt, "snap HALF_2_ACTIVE", isSnap("HALF_2_ACTIVE"))
	b2.waitFor(0, "a position of alice after the return", isType("pos"))
	_, aAt = a.waitFor(aAt, "half_end 2", isType("half_end"))
	end, _ := a.waitFor(aAt, "match_end", isType("match_end"))
	endB, _ := b2.waitFor(0, "match_end", isType("match_end"))
	a.stopSending()
	b2.stopSending()

	halves := a.all("half_end")
	require.Len(t, halves, 2)
	for i, h := range halves {
		assert.Equal(t, float64(i+1), h.Data["half"])
		assert.Equal(t, "TIMEOUT", h.Data["reason"])
	}
	assert.NotEqual(t, halves[0].Data["hunter"], halves[1].Data["hunter"], "the roles swapped")

	require.Len(t, a.all("match_end"), 1)
	assert.Equal(t, end.Raw, endB.Raw, "both players got the same match_end")
	assert.Equal(t, true, end.Data["ranked"])
	deltas := end.Data["deltas"].(map[string]any)
	assert.Len(t, deltas, 2)
	assert.Contains(t, deltas, ids[0])
	assert.Contains(t, deltas, ids[1])
	assert.Equal(t, int32(1), store.calls.Load(), "the match was settled exactly once")
	// What the players were shown is what was written, and announced.
	for _, id := range ids {
		written, err := mr.ZScore(leaderboard.GhostTagAllTimeKey, id)
		require.NoError(t, err, "%s is on the Ghost Tag board", id)
		assert.Equal(t, deltas[id], written, "%s: match_end shows what Redis holds", id)
	}
	assert.Len(t, announced.payloads(t), 2, "each player's standing was announced")
	assert.Equal(t, 1, events.get(ghosttag.EventMatchCompleted))
	assert.Zero(t, events.get(ghosttag.EventRoomPanic))
	assert.NotEmpty(t, a.all("pong"), "a ping is answered")
	for _, c := range []*gameClient{a, b, b2} {
		assert.NotContains(t, c.otherTypes(), "Relay", "nothing a client sent was passed on unread")
	}
}

// TestGameHostRejectsUnknownMode: a room of a game this service does not
// run is not started (and the hub then rejects the join).
func TestGameHostRejectsUnknownMode(t *testing.T) {
	h := gameHost{cfg: ghosttag.DefaultConfig(), log: zap.NewNop()}
	s, err := h.Start("r", wsgateway.GameSpec{Mode: "chess", Players: []string{"a", "b"}}, nil)
	require.Error(t, err)
	assert.Nil(t, s)

	// A spec the game refuses comes back as an error and a nil interface,
	// not as a nil session inside a non-nil one.
	s, err = h.Start("r", wsgateway.GameSpec{Mode: matchmaking.ModeGhostTag, Players: []string{"a"}}, nil)
	require.Error(t, err)
	assert.True(t, s == nil)
}

// TestGameEventsAreCounted: the game's telemetry lands in the service's
// metric, by event.
func TestGameEventsAreCounted(t *testing.T) {
	m := metrics.New("ws-game-events-test")
	e := gameEvents{total: m.WSGameEventsTotal}
	e.Count(ghosttag.EventJoinTimeout)
	e.Count(ghosttag.EventJoinTimeout)
	e.Count(ghosttag.EventBothDisconnected)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	assert.Contains(t, body, `gamemesh_ws_game_events_total{event="join_timeout",service="ws-game-events-test"} 2`)
	assert.Contains(t, body, `gamemesh_ws_game_events_total{event="both_disconnected",service="ws-game-events-test"} 1`)
}
