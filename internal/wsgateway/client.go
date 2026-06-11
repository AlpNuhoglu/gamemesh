package wsgateway

import (
	"encoding/json"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
	maxMsgSize = 4096
	sendBuffer = 64
)

// Client is one WebSocket connection. The standard gorilla pattern applies:
// exactly one reader goroutine (readPump) and one writer goroutine
// (writePump) per connection; all sends go through the buffered send channel.
type Client struct {
	playerID string
	conn     *websocket.Conn
	send     chan []byte
	hub      *Hub
	rooms    map[string]struct{}
	log      *zap.Logger
}

func newClient(playerID string, conn *websocket.Conn, hub *Hub, log *zap.Logger) *Client {
	return &Client{
		playerID: playerID,
		conn:     conn,
		send:     make(chan []byte, sendBuffer),
		hub:      hub,
		rooms:    make(map[string]struct{}),
		log:      log,
	}
}

// trySend drops the message if the client's buffer is full — a slow consumer
// must not block broadcasts to everyone else (backpressure by shedding).
func (c *Client) trySend(msg []byte) {
	select {
	case c.send <- msg:
	default:
		c.log.Warn("dropping message to slow client", zap.String("player_id", c.playerID))
	}
}

type inboundMessage struct {
	Action string `json:"action"`
	Room   string `json:"room"`
}

func (c *Client) readPump() {
	defer func() {
		c.hub.unregister(c)
		_ = c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMsgSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				c.log.Warn("websocket closed unexpectedly", zap.Error(err), zap.String("player_id", c.playerID))
			}
			return
		}
		var msg inboundMessage
		if err := json.Unmarshal(raw, &msg); err != nil || msg.Room == "" {
			continue
		}
		switch msg.Action {
		case "join":
			c.hub.join(c, msg.Room)
		case "leave":
			c.hub.leave(c, msg.Room)
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
