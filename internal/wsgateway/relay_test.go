package wsgateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sendRelay feeds one raw client message through the relay path, as readPump
// does after decoding it.
func sendRelay(t *testing.T, c *Client, raw string) {
	t.Helper()
	var msg inboundMessage
	require.NoError(t, json.Unmarshal([]byte(raw), &msg))
	require.Equal(t, "relay", msg.Action)
	c.handleRelay(&msg, time.Now())
}

// drainFrames returns the raw frames queued for c.
func drainFrames(c *Client) []string {
	var frames []string
	for {
		select {
		case out := <-c.send:
			frames = append(frames, string(out.msg))
		default:
			return frames
		}
	}
}

func rejected(hub *Hub, reason string) float64 {
	return byReason(hub, "gamemesh_ws_relay_rejected_total", reason)
}

// byReason returns the value of the hub's metric series for reason, whatever
// the hub's service label, or -1 if there is none.
func byReason(hub *Hub, metric, reason string) float64 {
	prefix := metric + `{reason="` + reason + `",`
	for _, line := range strings.Split(scrape(hub), "\n") {
		if strings.HasPrefix(line, prefix) {
			f, _ := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
			return f
		}
	}
	return -1
}

// counter scrapes the hub's metrics and returns the value of one series.
func counter(hub *Hub, series string) float64 {
	for _, line := range strings.Split(scrape(hub), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			f, _ := strconv.ParseFloat(v, 64)
			return f
		}
	}
	return -1
}

// relayRoom registers clients for the given players, puts them all in
// room-1 and empties their queues of the join notices.
func relayRoom(hub *Hub, players ...string) []*Client {
	clients := make([]*Client, len(players))
	for i, p := range players {
		clients[i] = newHubClient(hub, p)
		hub.register(clients[i])
		hub.join(clients[i], "room-1")
	}
	for _, c := range clients {
		drainFrames(c)
	}
	return clients
}

// TestRelayDeliversToOtherMembers: the other member gets the server's
// envelope with the data byte-for-byte; the client cannot set type or from,
// and clients outside the room get nothing.
func TestRelayDeliversToOtherMembers(t *testing.T) {
	hub := newTestHub()
	c := relayRoom(hub, "alice", "bob")
	carol := newHubClient(hub, "carol")
	hub.register(carol)
	hub.join(carol, "room-2")
	drainFrames(carol)

	sendRelay(t, c[0], `{"action":"relay","room":"room-1","type":"MatchFound","from":"mallory","data":{"x": 1.50,"y":[2, 3],"s":"<&>"}}`)

	assert.Equal(t, []string{
		`{"type":"Relay","room":"room-1","from":"alice","data":{"x": 1.50,"y":[2, 3],"s":"<&>"}}`,
	}, drainFrames(c[1]))
	assert.Empty(t, drainFrames(carol))
}

// TestRelayDoesNotEchoToSender: the sending connection gets nothing back,
// while every other member does.
func TestRelayDoesNotEchoToSender(t *testing.T) {
	hub := newTestHub()
	c := relayRoom(hub, "alice", "bob", "dave")

	sendRelay(t, c[0], `{"action":"relay","room":"room-1","data":[1]}`)

	assert.Empty(t, drainFrames(c[0]))
	assert.Len(t, drainFrames(c[1]), 1)
	assert.Len(t, drainFrames(c[2]), 1)
}

// TestRelayRejectsNonMember: a client that never joined the room, or has
// left it, cannot send into it.
func TestRelayRejectsNonMember(t *testing.T) {
	hub := newTestHub()
	c := relayRoom(hub, "alice", "bob")
	mallory := newHubClient(hub, "mallory")
	hub.register(mallory)

	sendRelay(t, mallory, `{"action":"relay","room":"room-1","data":1}`)
	hub.leave(c[1], "room-1")
	drainFrames(c[0])
	sendRelay(t, c[1], `{"action":"relay","room":"room-1","data":2}`)

	assert.Empty(t, drainFrames(c[0]))
	assert.Equal(t, float64(2), rejected(hub, "not_member"))
}

// TestRelayRejectsOversizeAndInvalid: data over 1024 bytes is too large;
// missing or null data and a missing room are invalid.
func TestRelayRejectsOversizeAndInvalid(t *testing.T) {
	hub := newTestHub()
	c := relayRoom(hub, "alice", "bob")

	atLimit := `"` + strings.Repeat("x", maxRelayData-2) + `"`
	sendRelay(t, c[0], `{"action":"relay","room":"room-1","data":`+atLimit+`}`)
	require.Len(t, drainFrames(c[1]), 1, "exactly 1024 bytes is allowed")

	sendRelay(t, c[0], `{"action":"relay","room":"room-1","data":"x`+atLimit[1:]+`}`)
	sendRelay(t, c[0], `{"action":"relay","room":"room-1"}`)
	sendRelay(t, c[0], `{"action":"relay","room":"room-1","data":null}`)
	sendRelay(t, c[0], `{"action":"relay","data":1}`)

	assert.Empty(t, drainFrames(c[1]))
	assert.Equal(t, float64(1), rejected(hub, "too_large"))
	assert.Equal(t, float64(3), rejected(hub, "invalid"))
}

// TestRelayRateLimitPerConnection: beyond the burst a connection's relays
// are rejected, while another connection keeps its own budget.
func TestRelayRateLimitPerConnection(t *testing.T) {
	hub := newTestHub()
	hub.SetRelayLimit(0.001, 3) // effectively no refill during the test
	c := relayRoom(hub, "alice", "bob")

	for range 5 {
		sendRelay(t, c[0], `{"action":"relay","room":"room-1","data":1}`)
	}
	sendRelay(t, c[1], `{"action":"relay","room":"room-1","data":2}`)

	assert.Len(t, drainFrames(c[1]), 3)
	assert.Len(t, drainFrames(c[0]), 1, "bob's relay is not limited by alice's bucket")
	assert.Equal(t, float64(2), rejected(hub, "rate_limited"))
}

// TestRelaySlowReceiverDropIsCounted: a receiver whose buffer is full sheds
// relay frames, counted, without blocking the sender.
func TestRelaySlowReceiverDropIsCounted(t *testing.T) {
	hub := newTestHub()
	c := relayRoom(hub, "alice", "slow")

	for range sendBuffer + 5 {
		sendRelay(t, c[0], `{"action":"relay","room":"room-1","data":1}`)
	}

	assert.Len(t, drainFrames(c[1]), sendBuffer)
	assert.Equal(t, float64(5), counter(hub, `gamemesh_ws_relay_dropped_total{service="ws-hub-test"}`))
	assert.Zero(t, rejected(hub, "rate_limited"))
}

// TestRelayOverWebSocket: end to end through readPump and writePump; the
// peer receives the frame from the sender's JWT player ID, and every stage is
// timed once.
func TestRelayOverWebSocket(t *testing.T) {
	srv, tokens, hub := newWSServer(t)
	alice, bob := uuid.New(), uuid.New()
	roomsOf(hub).add("room-1", alice.String(), bob.String())
	conns := make([]*websocket.Conn, 0, 2)
	for _, id := range []uuid.UUID{alice, bob} {
		token, _, err := tokens.Generate(id, "player")
		require.NoError(t, err)
		conn := dialToken(t, srv, token)
		require.NoError(t, conn.WriteJSON(map[string]string{"action": "join", "room": "room-1"}))
		readUntil(t, conn, "PlayerJoined") // its own join: now a member
		conns = append(conns, conn)
	}

	require.NoError(t, conns[0].WriteMessage(websocket.TextMessage,
		[]byte(`{"action":"relay","room":"room-1","from":"forged","data":{"seq":1}}`)))

	assert.Equal(t, `{"type":"Relay","room":"room-1","from":"`+alice.String()+`","data":{"seq":1}}`,
		readUntil(t, conns[1], "Relay"))
	for _, stage := range []string{"process", "queue", "write", "total"} {
		want := `gamemesh_ws_relay_duration_seconds_count{service="ws-handler-test",stage="` + stage + `"} 1`
		assert.Eventually(t, func() bool { return strings.Contains(scrape(hub), want) },
			time.Second, 10*time.Millisecond, stage)
	}
}

// readUntil reads frames from conn until one has the given type and returns
// it raw.
func readUntil(t *testing.T, conn *websocket.Conn, typ string) string {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	for {
		_, raw, err := conn.ReadMessage()
		require.NoError(t, err)
		var msg Message
		require.NoError(t, json.Unmarshal(raw, &msg))
		if msg.Type == typ {
			return string(raw)
		}
	}
}

func scrape(hub *Hub) string {
	w := httptest.NewRecorder()
	hub.m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return w.Body.String()
}
