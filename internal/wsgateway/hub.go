// Package wsgateway maintains WebSocket connections, room subscriptions and
// fan-out of backend events (MatchFound, LeaderboardUpdated) to clients.
package wsgateway

import (
	"encoding/json"
	"sync"

	"go.uber.org/zap"

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
}

// NewHub constructs an empty hub.
func NewHub(log *zap.Logger, m *metrics.Metrics) *Hub {
	return &Hub{
		clients:  make(map[*Client]struct{}),
		byPlayer: make(map[string]map[*Client]struct{}),
		rooms:    make(map[string]map[*Client]struct{}),
		log:      log,
		m:        m,
	}
}

func (h *Hub) register(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = struct{}{}
	if h.byPlayer[c.playerID] == nil {
		h.byPlayer[c.playerID] = make(map[*Client]struct{})
	}
	h.byPlayer[c.playerID][c] = struct{}{}
	h.m.WSConnections.Set(float64(len(h.clients)))
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

func playerData(playerID string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"player_id": playerID})
	return raw
}

func marshal(m Message) []byte {
	raw, _ := json.Marshal(m)
	return raw
}
