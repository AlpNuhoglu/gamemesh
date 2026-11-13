// Package wsgateway maintains WebSocket connections, room subscriptions,
// fan-out of backend events (MatchFound, LeaderboardUpdated) to clients, and
// an in-memory relay of ephemeral game traffic between the members of a room
// (relay.go), which never goes through the event bus.
package wsgateway

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/alpnuhoglu/gamemesh/internal/presence"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
)

// Message is the wire format between the WS gateway and clients.
//
//	Client → server: {"action": "join"|"leave", "room": "<id>"}
//	                 {"action": "relay", "room": "<id>", "data": <any JSON value>}
//	Server → client: {"type": "...", "room": "...", "data": {...}}
//	                 {"type": "Relay", "room": "<id>", "from": "<player id>", "data": <as sent>}
//	                 {"type": "JoinRejected", "room": "<id>"}
//
// A join is accepted only if matchmaking's record of the room exists and
// lists the connection's player (see RoomChecker); the room is then told
// PlayerJoined. Otherwise the sender alone gets JoinRejected, with no reason,
// so a stranger cannot learn whether a room exists, and its memberships are
// unchanged. Joins draw from the same token bucket as relays, one bucket per
// player shared by all of the player's connections.
//
// A relay goes to the room's other members only, and type and from are set
// by the server. Its frame is built in relay.go rather than from Message, so
// data is passed through byte-for-byte.
type Message struct {
	Type string          `json:"type,omitempty"`
	Room string          `json:"room,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Hub tracks connected clients and their room memberships. All maps are
// guarded by one RWMutex — broadcast paths take the read lock, membership
// changes the write lock. For horizontal scale, each WS replica runs its own
// hub and receives every event via Redis Pub/Sub, so clients can land on any
// replica.
type Hub struct {
	mu       sync.RWMutex
	clients  map[*Client]struct{}
	byPlayer map[string]map[*Client]struct{}
	rooms    map[string]map[*Client]struct{}

	// buckets holds each connected player's relay token bucket, which all of
	// the player's connections on this replica share (relays and joins draw
	// from it), so a second connection does not double the allowance. A
	// bucket is created with the player's first connection and deleted with
	// their last, in register and unregister under mu, so none outlives its
	// player and there is nothing idle to sweep. The consequence is that a
	// player who drops every connection and reconnects starts with a full
	// bucket; the per-IP limit at the gateway and the session sweep still
	// apply. Each replica has its own buckets. Connections hold a pointer to
	// theirs, so the hot path takes no lock: rate.Limiter is safe for
	// concurrent use.
	buckets map[string]*rate.Limiter

	log *zap.Logger
	m   *metrics.Metrics

	// presence feeds player presence to the Presence Service. It is an injected
	// interface and never nil (NoopNotifier when no Presence Service is wired),
	// so the gateway stays decoupled from presence internals and runs standalone.
	presence presence.Notifier

	// participants decides who may join a room (see join). Never nil.
	participants RoomChecker
	joinM        joinMetrics

	// Room relay (relay.go): the token bucket settings new player buckets
	// get, and the relay instruments. Read-only once connections are served.
	relayRate  rate.Limit
	relayBurst int
	relayM     relayMetrics
}

// ErrRoomNotFound is what a RoomChecker returns for a room that does not
// exist, or no longer does.
var ErrRoomNotFound = errors.New("room not found")

// RoomChecker answers whether a player may join a room. In the service it
// reads matchmaking's room record (cmd/websocket); it is an interface so this
// package depends on neither Redis nor matchmaking.
type RoomChecker interface {
	// IsParticipant reports whether playerID is one of the room's players,
	// or returns ErrRoomNotFound if there is no such room.
	IsParticipant(ctx context.Context, room, playerID string) (bool, error)
}

// noRoomChecker is what a hub built without a RoomChecker gets: every join
// fails as if the lookup were down, so leaving the checker out closes rooms
// rather than opening them.
type noRoomChecker struct{}

func (noRoomChecker) IsParticipant(context.Context, string, string) (bool, error) {
	return false, errors.New("no room checker configured")
}

// roomCheckTimeout bounds the room lookup of one join. It runs on the
// connection's read loop, so a stalled Redis holds that connection's inbound
// messages for at most this long, and the join is then rejected.
const roomCheckTimeout = 2 * time.Second

// joinMetrics holds the rejection counters resolved once, which also makes
// every reason's series exist from the start.
type joinMetrics struct {
	notParticipant, roomNotFound, unavailable, rateLimited prometheus.Counter
}

func newJoinMetrics(m *metrics.Metrics) joinMetrics {
	reason := m.WSJoinRejectedTotal.WithLabelValues
	return joinMetrics{
		notParticipant: reason("not_participant"),
		roomNotFound:   reason("room_not_found"),
		unavailable:    reason("unavailable"),
		rateLimited:    reason("rate_limited"),
	}
}

// NewHub constructs an empty hub. A nil notifier becomes a no-op, so existing
// callers (and tests) that do not care about presence keep working unchanged.
// A nil rooms checker is not a way to switch the join check off: every join
// is then rejected as "unavailable".
func NewHub(log *zap.Logger, m *metrics.Metrics, notifier presence.Notifier, rooms RoomChecker) *Hub {
	if notifier == nil {
		notifier = presence.NoopNotifier{}
	}
	if rooms == nil {
		rooms = noRoomChecker{}
	}
	return &Hub{
		clients:  make(map[*Client]struct{}),
		byPlayer: make(map[string]map[*Client]struct{}),
		rooms:    make(map[string]map[*Client]struct{}),
		buckets:  make(map[string]*rate.Limiter),
		log:      log,
		m:        m,
		presence: notifier,

		participants: rooms,
		joinM:        newJoinMetrics(m),

		relayRate:  defaultRelayRate,
		relayBurst: defaultRelayBurst,
		relayM:     newRelayMetrics(m),
	}
}

func (h *Hub) register(c *Client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	if h.byPlayer[c.playerID] == nil {
		h.byPlayer[c.playerID] = make(map[*Client]struct{})
		h.buckets[c.playerID] = rate.NewLimiter(h.relayRate, h.relayBurst)
	}
	h.byPlayer[c.playerID][c] = struct{}{}
	c.relay.limiter = h.buckets[c.playerID]
	h.m.WSConnections.Set(float64(len(h.clients)))
	h.mu.Unlock()

	// Each connection is one presence connection (the Presence Service does the
	// multi-device counting). Fire async with a fresh context so a slow/absent
	// Presence Service never blocks accepting the WebSocket.
	h.notifyPresence("connect", c.playerID, h.presence.Connect)
}

func (h *Hub) unregister(c *Client) {
	h.mu.Lock()
	var left []string
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		delete(h.byPlayer[c.playerID], c)
		if len(h.byPlayer[c.playerID]) == 0 {
			delete(h.byPlayer, c.playerID)
			delete(h.buckets, c.playerID)
		}
		for room := range c.rooms {
			delete(h.rooms[room], c)
			if len(h.rooms[room]) == 0 {
				delete(h.rooms, room)
			} else {
				left = append(left, room)
			}
		}
		close(c.send)
	}
	h.m.WSConnections.Set(float64(len(h.clients)))
	h.mu.Unlock()

	// Drop one presence connection. The Presence Service only flips the player to
	// OFFLINE when their last connection closes, so multi-device players stay
	// online here too.
	h.notifyPresence("disconnect", c.playerID, h.presence.Disconnect)

	// Notify remaining occupants outside the lock.
	for _, room := range left {
		h.BroadcastToRoom(room, marshal(Message{Type: "PlayerLeft", Room: room, Data: playerData(c.playerID)}))
	}
}

// CloseAll tells every connected client that the server is going away
// (close code 1001, "server shutting down") and waits until they have
// disconnected or ctx is done; connections still open then are closed
// outright. It runs as a shutdown hook, because http.Server.Shutdown does
// not track hijacked WebSocket connections and clients would otherwise see
// 1006 (abnormal closure).
func (h *Hub) CloseAll(ctx context.Context) {
	h.mu.RLock()
	clients := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		if c.conn != nil {
			clients = append(clients, c)
		}
	}
	h.mu.RUnlock()

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(writeWait)
	}
	frame := websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down")
	for _, c := range clients {
		// WriteControl may run concurrently with the client's writePump.
		_ = c.conn.WriteControl(websocket.CloseMessage, frame, deadline)
	}

	// Clients answer with their own close frame; readPump then unregisters
	// them. Poll rather than use a WaitGroup, since a connection still being
	// upgraded may register while we wait.
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for h.count() > 0 {
		select {
		case <-ctx.Done():
			for _, c := range clients {
				_ = c.conn.Close() // unblocks readPump, which unregisters
			}
			h.log.Warn("websocket clients closed forcibly", zap.Int("clients", len(clients)))
			return
		case <-tick.C:
		}
	}
	h.log.Info("websocket clients closed", zap.Int("clients", len(clients)))
}

func (h *Hub) count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// handleJoin processes one inbound join that ReadMessage returned at readAt.
// A join costs a room lookup on this read loop, so it draws a token from the
// player's relay bucket and is rejected when the bucket is empty.
func (c *Client) handleJoin(room string, readAt time.Time) {
	if !c.relay.limiter.AllowN(readAt, 1) {
		c.hub.rejectJoin(c, room, c.hub.joinM.rateLimited)
		return
	}
	c.hub.join(c, room)
}

// join adds c to room and tells the room, if c's player is one of the room's
// participants; otherwise it rejects the join. It reports whether c joined.
// The lookup runs before the hub lock is taken, so a slow lookup holds up
// only this connection.
func (h *Hub) join(c *Client, room string) bool {
	if reason := h.checkJoin(c.playerID, room); reason != nil {
		h.rejectJoin(c, room, reason)
		return false
	}

	h.mu.Lock()
	if h.rooms[room] == nil {
		h.rooms[room] = make(map[*Client]struct{})
	}
	h.rooms[room][c] = struct{}{}
	c.rooms[room] = struct{}{}
	h.mu.Unlock()

	h.BroadcastToRoom(room, marshal(Message{Type: "PlayerJoined", Room: room, Data: playerData(c.playerID)}))
	return true
}

// checkJoin asks the RoomChecker whether playerID may join room, and returns
// nil if it may, or the counter of the reason it may not. Failing to get an
// answer in time is a rejection.
func (h *Hub) checkJoin(playerID, room string) prometheus.Counter {
	ctx, cancel := context.WithTimeout(context.Background(), roomCheckTimeout)
	defer cancel()
	ok, err := h.participants.IsParticipant(ctx, room, playerID)
	switch {
	case errors.Is(err, ErrRoomNotFound):
		return h.joinM.roomNotFound
	case err != nil:
		h.log.Warn("room membership check failed", zap.String("player_id", playerID), zap.Error(err))
		return h.joinM.unavailable
	case !ok:
		return h.joinM.notParticipant
	}
	return nil
}

// rejectJoin counts a rejected join and tells the sender alone, without a
// reason. Only c's readPump calls it, and only readPump unregisters c, so c's
// send channel is still open.
func (h *Hub) rejectJoin(c *Client, room string, reason prometheus.Counter) {
	reason.Inc()
	c.trySend(marshal(Message{Type: joinRejectedType, Room: room}))
}

func (h *Hub) leave(c *Client, room string) {
	h.mu.Lock()
	delete(c.rooms, room)
	if members, ok := h.rooms[room]; ok {
		delete(members, c)
		if len(members) == 0 {
			delete(h.rooms, room)
		}
	}
	h.mu.Unlock()

	h.BroadcastToRoom(room, marshal(Message{Type: "PlayerLeft", Room: room, Data: playerData(c.playerID)}))
}

// BroadcastToRoom sends raw bytes to every client subscribed to a room.
func (h *Hub) BroadcastToRoom(room string, msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.rooms[room] {
		c.trySend(msg)
	}
}

// BroadcastAll sends raw bytes to every connected client.
func (h *Hub) BroadcastAll(msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		c.trySend(msg)
	}
}

// SendToPlayer delivers to every connection of one player (multiple tabs).
func (h *Hub) SendToPlayer(playerID string, msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.byPlayer[playerID] {
		c.trySend(msg)
	}
}

// notifyPresence runs a presence lifecycle call asynchronously with its own
// short-lived context. Async + fresh context means the closing connection's
// cancelled context cannot cancel the call, and a slow Presence Service cannot
// block WS connect/disconnect handling. Failures are logged, never fatal —
// presence is re-derived from the next heartbeat.
func (h *Hub) notifyPresence(op, playerID string, fn func(context.Context, string) error) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := fn(ctx, playerID); err != nil {
			h.log.Warn("presence notify failed",
				zap.String("op", op), zap.String("player_id", playerID), zap.Error(err))
		}
	}()
}

// RunHeartbeat refreshes presence for every locally-connected player every
// interval, keeping their presence:{id} TTL alive. It runs until ctx is
// cancelled. One ticker for the whole replica (rather than per-connection)
// keeps the load proportional to distinct players, not connections. Any WS
// replica can heartbeat any of its players — no sticky sessions required.
func (h *Hub) RunHeartbeat(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ids := h.connectedPlayers()
			if len(ids) == 0 {
				continue
			}
			// One bulk call for the whole replica instead of a goroutine + HTTP
			// request per player. Fire async with a bounded timeout derived from
			// the loop ctx, so a slow Presence Service cannot stall the ticker yet
			// in-flight beats are cancelled on shutdown. Failures are logged, not
			// fatal — presence self-heals from the next beat / TTL. The closure
			// captures the batch once per tick, not once per player.
			go func(batch []string) {
				cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				if err := h.presence.HeartbeatMany(cctx, batch); err != nil {
					h.log.Warn("bulk presence heartbeat failed",
						zap.Int("count", len(batch)), zap.Error(err))
				}
			}(ids)
		}
	}
}

// connectedPlayers snapshots the distinct player IDs with at least one live
// connection on this replica.
func (h *Hub) connectedPlayers() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ids := make([]string, 0, len(h.byPlayer))
	for id := range h.byPlayer {
		ids = append(ids, id)
	}
	return ids
}

func playerData(playerID string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"player_id": playerID})
	return raw
}

func marshal(m Message) []byte {
	raw, _ := json.Marshal(m)
	return raw
}
