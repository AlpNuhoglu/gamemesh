package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// player is one bench account and its connection.
type player struct {
	idx      int
	username string
	email    string
	id       string // player UUID, from login
	token    string
	jti      string

	registered bool        // the account exists in Postgres
	queued     atomic.Bool // POST /queue succeeded and no MatchFound seen since
	conn       *wsConn
}

// serverMsg is the server → client envelope. Data stays raw.
type serverMsg struct {
	Type string          `json:"type"`
	Room string          `json:"room"`
	From string          `json:"from"`
	Data json.RawMessage `json:"data"`
}

// matchNote is a MatchFound seen by one of our connections.
type matchNote struct {
	by      *player
	room    string
	players []string
}

// joinNote is a PlayerJoined seen by one of our connections.
type joinNote struct {
	room     string
	playerID string
}

// wsConn wraps one WebSocket. Exactly one goroutine (readLoop) reads; data
// frames are written by one goroutine at a time (setup, then the sender);
// pings and the close frame use WriteControl, which gorilla allows
// concurrently with everything else.
type wsConn struct {
	p       *player
	ws      *websocket.Conn
	matches chan<- matchNote // shared by all connections
	joins   chan joinNote
	recv    atomic.Pointer[receiver] // set while this connection is a receiver
	done    chan struct{}            // closed when readLoop returns

	mu        sync.Mutex
	readErr   error
	pingsSent int
	rtts      []time.Duration
}

func wsEndpoint(base, token string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/") + "/ws")
	if err != nil {
		return "", err
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", fmt.Errorf("-ws must start with ws:// or wss://, got %q", base)
	}
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// dial opens the WebSocket, installs the pong handler and starts the reader.
func dial(ctx context.Context, api *apiClient, base string, p *player, matches chan<- matchNote) (*wsConn, error) {
	endpoint, err := wsEndpoint(base, p.token)
	if err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	var ws *websocket.Conn
	backoff := 250 * time.Millisecond
	for attempt := 1; ; attempt++ {
		if err := api.lim.Wait(ctx); err != nil { // the gateway rate-limits upgrades too
			return nil, err
		}
		var resp *http.Response
		ws, resp, err = dialer.DialContext(ctx, endpoint, nil)
		if err == nil {
			break
		}
		status := 0
		if resp != nil {
			status = resp.StatusCode
			_ = resp.Body.Close()
		}
		// Never echo endpoint: it carries the token.
		if (status != 0 && status != 429 && status != 502 && status != 503) || attempt == 5 || ctx.Err() != nil {
			return nil, fmt.Errorf("websocket dial for %s failed (status %d): %w", p.username, status, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	c := &wsConn{
		p:       p,
		ws:      ws,
		matches: matches,
		joins:   make(chan joinNote, 16),
		done:    make(chan struct{}),
	}
	ws.SetPongHandler(c.onPong)
	go c.readLoop()
	return c, nil
}

// readLoop is the only reader. The receive time is taken the moment
// ReadMessage returns, before anything is decoded.
func (c *wsConn) readLoop() {
	defer close(c.done)
	for {
		_, raw, err := c.ws.ReadMessage()
		now := mono()
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			c.mu.Unlock()
			return
		}
		var m serverMsg
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		switch m.Type {
		case "Relay":
			if r := c.recv.Load(); r != nil {
				r.record(m, now)
			}
		case "MatchFound":
			var d struct {
				RoomID  string   `json:"room_id"`
				Players []string `json:"players"`
			}
			if json.Unmarshal(m.Data, &d) != nil {
				continue
			}
			room := m.Room
			if room == "" {
				room = d.RoomID
			}
			select {
			case c.matches <- matchNote{by: c.p, room: room, players: d.Players}:
			default: // the pairing loop is gone or swamped; nothing to do with it
			}
		case "PlayerJoined":
			var d struct {
				PlayerID string `json:"player_id"`
			}
			if json.Unmarshal(m.Data, &d) != nil {
				continue
			}
			select {
			case c.joins <- joinNote{room: m.Room, playerID: d.PlayerID}:
			default:
			}
		}
	}
}

// ping sends one WebSocket ping carrying the monotonic send time.
func (c *wsConn) ping() error {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(mono())) // #nosec G115 -- monotonic offset is never negative
	c.mu.Lock()
	c.pingsSent++
	c.mu.Unlock()
	return c.ws.WriteControl(websocket.PingMessage, buf[:], time.Now().Add(5*time.Second))
}

// onPong runs inside ReadMessage on the reader goroutine.
func (c *wsConn) onPong(appData string) error {
	now := mono()
	if len(appData) != 8 {
		return nil
	}
	sent := time.Duration(binary.BigEndian.Uint64([]byte(appData))) // #nosec G115 -- round-trips our own value
	if sent <= 0 || sent > now {
		return nil
	}
	c.mu.Lock()
	c.rtts = append(c.rtts, now-sent)
	c.mu.Unlock()
	return nil
}

func (c *wsConn) pingStats() (sent int, rtts []time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pingsSent, append([]time.Duration(nil), c.rtts...)
}

// sendJSON writes a data frame. Only one goroutine may call it at a time.
func (c *wsConn) sendJSON(v any) error {
	_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.ws.WriteJSON(v)
}

// alive reports whether the reader is still running.
func (c *wsConn) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *wsConn) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

// close performs the closing handshake: send a normal-closure frame, wait for
// the server's answer (which ends readLoop), then drop the TCP connection.
func (c *wsConn) close() {
	if c == nil {
		return
	}
	_ = c.ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "latencybench done"),
		time.Now().Add(2*time.Second))
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
	}
	_ = c.ws.Close()
	<-c.done
}

// waitJoined waits until this connection sees PlayerJoined for playerID in
// room. The hub broadcasts PlayerJoined to the room after adding the joiner,
// so a player seeing its own join proves it is a member.
func (c *wsConn) waitJoined(ctx context.Context, room, playerID string, timeout time.Duration) error {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case n := <-c.joins:
			if n.room == room && n.playerID == playerID {
				return nil
			}
		case <-c.done:
			return fmt.Errorf("%s: connection closed while joining room: %v", c.p.username, c.err())
		case <-t.C:
			return fmt.Errorf("%s: no PlayerJoined for room %s within %s", c.p.username, room, timeout)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
