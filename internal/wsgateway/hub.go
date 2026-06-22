// Package wsgateway maintains WebSocket connections, room subscriptions and
// fan-out of backend events (MatchFound, LeaderboardUpdated) to clients.
package wsgateway

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/presence"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
)

// Message is the wire format between the WS gateway and clients.
// Client → server: {"action": "join"|"leave", "room": "<id>"}
// Server → client: {"type": "...", "room": "...", "data": {...}}
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

	log *zap.Logger
	m   *metrics.Metrics

	// presence feeds player presence to the Presence Service. It is an injected
	// interface and never nil (NoopNotifier when no Presence Service is wired),
	// so the gateway stays decoupled from presence internals and runs standalone.
	presence presence.Notifier
}

// NewHub constructs an empty hub. A nil notifier becomes a no-op, so existing
// callers (and tests) that do not care about presence keep working unchanged.
func NewHub(log *zap.Logger, m *metrics.Metrics, notifier presence.Notifier) *Hub {
	if notifier == nil {
		notifier = presence.NoopNotifier{}
	}
	return &Hub{
		clients:  make(map[*Client]struct{}),
		byPlayer: make(map[string]map[*Client]struct{}),
		rooms:    make(map[string]map[*Client]struct{}),
		log:      log,
		m:        m,
		presence: notifier,
	}
}

func (h *Hub) register(c *Client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	if h.byPlayer[c.playerID] == nil {
		h.byPlayer[c.playerID] = make(map[*Client]struct{})
	}
	h.byPlayer[c.playerID][c] = struct{}{}
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

func (h *Hub) join(c *Client, room string) {
	h.mu.Lock()
	if h.rooms[room] == nil {
		h.rooms[room] = make(map[*Client]struct{})
	}
	h.rooms[room][c] = struct{}{}
	c.rooms[room] = struct{}{}
	h.mu.Unlock()

	h.BroadcastToRoom(room, marshal(Message{Type: "PlayerJoined", Room: room, Data: playerData(c.playerID)}))
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
