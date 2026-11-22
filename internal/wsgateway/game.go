package wsgateway

// Validated rooms: rooms whose traffic the server opens, checks and answers
// with decisions of its own, instead of passing it on unread.
//
//	client → server: {"action":"relay","room":"<id>","data":<game payload>}
//	server → room:   {"type":"Game","room":"<id>","data":<game payload>}
//
// A room is a validated room when matchmaking's record of it says so
// (GameSpec, through the RoomChecker); a client cannot choose. Its inbound
// relays pass the same size, rate and membership checks as any relay and are
// then handed to the room's GameSession instead of being fanned out, so in a
// validated room nothing a client sends reaches another client unread.
//
// This file knows no game. The session is an interface the service wires in
// (GameHost), and this package gives it a GameTransport to answer through.
//
// Like the relay, the path is in memory and at-most-once, and stays on this
// replica. Nothing here touches the event bus or a store.
//
// Locking: a session runs on its own goroutine and comes back into the hub
// through its GameTransport, which takes the hub's read lock. The hub
// therefore never makes a call into a session that can block (Notify) while
// it holds its own lock; it collects the sessions to notify under the lock
// and calls them after releasing it. Post never blocks and is called under
// the read lock, together with the membership check.

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
)

// gameType is the type of every frame a validated room sends. It is not
// relayType on purpose: a Relay frame is another player's bytes, unread; a
// Game frame is the server's own statement. Like relayType it is server-only
// and must not be a durable event's name (boundary_test.go checks).
const gameType = "Game"

// GameSpec is what matchmaking's room record says about a validated room.
type GameSpec struct {
	// Mode names the game, e.g. "ghosttag". The GameHost decides what it
	// can run.
	Mode string
	// Players are the room's human players.
	Players []string
	// Bot says the room's second player is the server's bot.
	Bot bool
	// Unranked says the match must not produce points.
	Unranked bool
	// CreatedAt is when matchmaking created the room. A session can only be
	// started for a room younger than the hub's start window.
	CreatedAt time.Time
}

// GameTransport is how a session reaches the room's connections. The hub
// provides one per room. payload is a JSON value, sent as the data of a Game
// frame byte-for-byte; it may be shared by several recipients and must not
// be modified after the call. No method blocks on a slow connection: a frame
// for a connection whose queue is full is dropped.
type GameTransport interface {
	// Broadcast sends payload to every connection in the room.
	Broadcast(payload []byte)
	// BroadcastExcept sends payload to every connection in the room except
	// playerID's. arrived is when the inbound message that caused it was
	// read from its socket; the frame is then timed through the server like
	// a relay. The zero time means no inbound message caused it.
	BroadcastExcept(playerID string, payload []byte, arrived time.Time)
	// SendTo sends payload to every connection playerID has in the room.
	SendTo(playerID string, payload []byte)
	// Connected reports whether playerID has a connection in the room now.
	Connected(playerID string) bool
	// Name is the username of the JWT one of playerID's connections in the
	// room was opened with, or "" if the player has none. A player's
	// connections normally share it; if they do not (the account was
	// renamed between two tokens), it is one of them.
	Name(playerID string) string
}

// GameSession is one validated room's game. Run is its goroutine; the other
// methods are called from connection goroutines.
type GameSession interface {
	// Run runs the game until it is over or ctx is cancelled. A cancelled
	// ctx means the server is shutting down, which is not a result: the
	// session must stop without recording one.
	Run(ctx context.Context)
	// Post hands the session the data of one inbound relay, read from
	// playerID's socket at arrived. It must not block; false means the
	// session dropped it.
	Post(playerID string, data []byte, arrived time.Time) bool
	// Notify tells the session that playerID's presence in the room may
	// have changed; the session asks GameTransport.Connected for the answer.
	// It may block until the session has taken the notice or is done.
	Notify(playerID string)
	// Done is closed when Run has returned.
	Done() <-chan struct{}
}

// GameHost starts the sessions of validated rooms. The hub calls Start while
// it holds its lock, so Start must only build the session: no I/O, no call
// on out, no goroutine that calls out before Run does. An error (an unknown
// mode, a spec the game refuses) rejects the join.
type GameHost interface {
	Start(room string, spec GameSpec, out GameTransport) (GameSession, error)
}

// gameMetrics are the validated rooms' instruments, resolved once.
type gameMetrics struct {
	stageMetrics
	dropped prometheus.Counter
	rooms   prometheus.Gauge
	panics  prometheus.Counter
}

// GameEventRoomPanic is the event a room that panicked is counted under in
// WSGameEventsTotal, by the hub or by the session that recovered itself.
const GameEventRoomPanic = "room_panic"

func newGameMetrics(m *metrics.Metrics) gameMetrics {
	stage := m.WSGameDuration.WithLabelValues
	return gameMetrics{
		stageMetrics: stageMetrics{
			process: stage("process"),
			queue:   stage("queue"),
			write:   stage("write"),
			total:   stage("total"),
		},
		dropped: m.WSGameDroppedTotal,
		rooms:   m.WSGameRooms,
		panics:  m.WSGameEventsTotal.WithLabelValues(GameEventRoomPanic),
	}
}

// gameRoom is one validated room on this hub: the session, and the
// GameTransport the session answers through. It stays in Hub.games after
// its session has ended (finished), so that the room can neither be started
// again nor fall back to being an opaque-relay room; pruneGames removes it.
type gameRoom struct {
	hub     *Hub
	room    string
	prefix  []byte // the Game frame up to the data value
	session GameSession

	// finished and finishedAt are guarded by Hub.mu.
	finished   bool
	finishedAt time.Time
}

// SetGames turns validated rooms on: host starts their sessions, which run
// until ctx is cancelled or CloseAll stops them. A room can be started only
// within startWindow of its creation (0 means any age): matchmaking keeps a
// room's record for much longer than a match lasts, and without the window a
// player rejoining a finished room after a restart of this service would
// start the match again. Call it before serving. Without it, a join to a
// validated room is rejected rather than served as an opaque-relay room.
func (h *Hub) SetGames(ctx context.Context, host GameHost, startWindow time.Duration) {
	h.gameHost = host
	h.gameWindow = startWindow
	h.gameCtx, h.gameCancel = context.WithCancel(ctx)
}

// joinGame adds c to a validated room, starting the room's session if c is
// the first to arrive, and reports whether c joined. The caller has checked
// that c's player is one of the room's participants.
func (h *Hub) joinGame(c *Client, room string, spec GameSpec) bool {
	if h.gameHost == nil {
		h.log.Warn("join to a validated room, but this hub runs no games", zap.String("mode", spec.Mode))
		h.rejectJoin(c, room, h.joinM.unavailable)
		return false
	}

	h.mu.Lock()
	g := h.games[room]
	var reject prometheus.Counter
	var startErr error
	switch {
	case g != nil && g.finished:
		reject = h.joinM.roomClosed
	case g == nil && h.gameWindow > 0 && time.Since(spec.CreatedAt) > h.gameWindow:
		reject = h.joinM.roomClosed
	case g == nil:
		g = &gameRoom{hub: h, room: room, prefix: gameFramePrefix(room)}
		if g.session, startErr = h.gameHost.Start(room, spec, g); startErr != nil {
			reject = h.joinM.unavailable
			break
		}
		h.games[room] = g
		h.gameM.rooms.Inc()
		h.gameWG.Add(1)
		go h.runGame(g)
	}
	if reject == nil {
		h.addToRoom(c, room)
	}
	h.mu.Unlock()

	if reject != nil {
		if startErr != nil {
			h.log.Warn("could not start validated room", zap.String("mode", spec.Mode), zap.Error(startErr))
		}
		h.rejectJoin(c, room, reject)
		return false
	}
	h.BroadcastToRoom(room, marshal(Message{Type: "PlayerJoined", Room: room, Data: playerData(c.playerID)}))
	// Outside the lock: Notify may block, and the session answers it by
	// calling back into the hub.
	g.session.Notify(c.playerID)
	return true
}

// runGame is a session's goroutine. When the session ends the room is marked
// finished; its members stay in it and can leave, but what they relay is
// rejected.
//
// A panic in a game must not take the service, and every other room, down
// with it: it ends that room, without a result. A session is expected to
// recover on its own, where it can still tell its players that the match is
// void; this is the backstop for a panic that gets past it, and it can only
// close the room, since the hub does not speak the game's protocol. Either
// way nothing is scored from a room whose state can no longer be trusted,
// and the panic is counted under the same event.
func (h *Hub) runGame(g *gameRoom) {
	defer h.gameWG.Done()
	defer func() {
		if r := recover(); r != nil {
			h.gameM.panics.Inc()
			h.log.Error("validated room panicked", zap.String("room", g.room), zap.Any("panic", r), zap.Stack("stack"))
		}
		h.mu.Lock()
		g.finished, g.finishedAt = true, time.Now()
		h.gameM.rooms.Dec()
		h.pruneGames(g.finishedAt)
		h.mu.Unlock()
	}()
	g.session.Run(h.gameCtx)
}

// gameTombstone is how long a finished room is remembered after its last
// member has left, when the start window does not say (0, any age).
const gameTombstone = time.Hour

// pruneGames forgets finished rooms that nobody is in any more and that are
// too old to be started again. The caller holds the write lock. It runs when
// a session ends, which bounds the map by the rooms that finished since.
func (h *Hub) pruneGames(now time.Time) {
	keep := h.gameWindow
	if keep <= 0 {
		keep = gameTombstone
	}
	for room, g := range h.games {
		if g.finished && len(h.rooms[room]) == 0 && now.Sub(g.finishedAt) > keep {
			delete(h.games, room)
		}
	}
}

// stopGames ends every session without a result and waits for them, until
// ctx is done. CloseAll calls it before it closes a single connection, so no
// room ever sees the server's own shutdown as its players disconnecting.
func (h *Hub) stopGames(ctx context.Context) {
	if h.gameCancel == nil {
		return
	}
	h.gameCancel()
	stopped := make(chan struct{})
	go func() {
		h.gameWG.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-ctx.Done():
		h.log.Warn("validated rooms still running at shutdown")
	}
}

// gamesToNotify returns the live validated rooms among rooms. The caller
// holds the lock, and calls notifyGames after releasing it.
func (h *Hub) gamesToNotify(rooms map[string]struct{}) []*gameRoom {
	if len(h.games) == 0 {
		return nil
	}
	var games []*gameRoom
	for room := range rooms {
		if g := h.games[room]; g != nil && !g.finished {
			games = append(games, g)
		}
	}
	return games
}

// notifyGames tells each session that playerID's presence may have changed.
// It must be called without the hub's lock (see the top of this file).
func notifyGames(games []*gameRoom, playerID string) {
	for _, g := range games {
		g.session.Notify(playerID)
	}
}

func gameFramePrefix(room string) []byte {
	r, _ := json.Marshal(room)
	p := append([]byte(`{"type":"`+gameType+`","room":`), r...)
	return append(p, `,"data":`...)
}

// frame wraps payload in the room's Game frame, byte-for-byte.
func (g *gameRoom) frame(payload []byte) []byte {
	f := make([]byte, 0, len(g.prefix)+len(payload)+1)
	f = append(f, g.prefix...)
	f = append(f, payload...)
	return append(f, '}')
}

// Broadcast implements GameTransport. It carries the room's discrete
// messages (a snapshot, the end of a half or of the match), so a frame shed
// for a slow connection is logged, as for a backend event.
func (g *gameRoom) Broadcast(payload []byte) {
	g.hub.BroadcastToRoom(g.room, g.frame(payload))
}

// BroadcastExcept implements GameTransport. It is the per-packet path: a
// frame shed for a slow connection is counted, not logged, and the time from
// arrived to here is the validated room's process stage, the counterpart of
// the relay's.
func (g *gameRoom) BroadcastExcept(playerID string, payload []byte, arrived time.Time) {
	h := g.hub
	out := outbound{msg: g.frame(payload), readAt: arrived, game: true}
	h.mu.RLock()
	out.queuedAt = time.Now()
	for c := range h.rooms[g.room] {
		if c.playerID == playerID {
			continue
		}
		select {
		case c.send <- out:
		default:
			h.gameM.dropped.Inc()
		}
	}
	h.mu.RUnlock()
	if !arrived.IsZero() {
		h.gameM.process.Observe(out.queuedAt.Sub(arrived).Seconds())
	}
}

// SendTo implements GameTransport.
func (g *gameRoom) SendTo(playerID string, payload []byte) {
	h := g.hub
	frame := g.frame(payload)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.rooms[g.room] {
		if c.playerID == playerID {
			c.trySend(frame)
		}
	}
}

// Connected implements GameTransport.
func (g *gameRoom) Connected(playerID string) bool {
	h := g.hub
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.rooms[g.room] {
		if c.playerID == playerID {
			return true
		}
	}
	return false
}

// Name implements GameTransport.
func (g *gameRoom) Name(playerID string) string {
	h := g.hub
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.rooms[g.room] {
		if c.playerID == playerID && c.name != "" {
			return c.name
		}
	}
	return ""
}

// gameState is the hub's validated-room state, embedded in Hub. games is
// guarded by Hub.mu; the rest is set by SetGames before connections are
// served and read-only afterwards.
type gameState struct {
	games      map[string]*gameRoom
	gameHost   GameHost
	gameWindow time.Duration
	gameCtx    context.Context
	gameCancel context.CancelFunc
	gameWG     sync.WaitGroup
	gameM      gameMetrics
}
