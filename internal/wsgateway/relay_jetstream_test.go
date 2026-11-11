package wsgateway

// EXPERIMENT (exp/relay-jetstream, never merged): the relay through a real
// (embedded) JetStream, NATSBus and Bridge, end to end over WebSocket.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
)

func TestRelayThroughJetStream(t *testing.T) {
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(ns.Shutdown)

	srv, tokens, hub := newWSServer(t)
	bus, err := events.NewNATSBus(ns.ClientURL(), "websocket", 8, hub.m, zap.NewNop(), events.WithBroadcast())
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })
	hub.SetRelayBus(bus)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = NewBridge(hub, bus, zap.NewNop()).Run(ctx) }()
	time.Sleep(200 * time.Millisecond) // let the consumers start

	alice := uuid.New()
	conns := make([]*websocket.Conn, 0, 2)
	for _, id := range []uuid.UUID{alice, uuid.New()} {
		token, _, err := tokens.Generate(id, "player")
		require.NoError(t, err)
		conn := dialToken(t, srv, token)
		require.NoError(t, conn.WriteJSON(map[string]string{"action": "join", "room": "room-1"}))
		readUntil(t, conn, "PlayerJoined")
		conns = append(conns, conn)
	}
	readUntil(t, conns[0], "PlayerJoined") // bob's join, seen by alice

	data := `{"x": 1.50,"s":"<&>"}`
	require.NoError(t, conns[0].WriteMessage(websocket.TextMessage,
		[]byte(`{"action":"relay","room":"room-1","from":"forged","data":`+data+`}`)))

	assert.Equal(t, `{"type":"Relay","room":"room-1","from":"`+alice.String()+`","data":`+data+`}`,
		readUntil(t, conns[1], "Relay"), "frame byte-for-byte after the round trip")
	for _, stage := range []string{"process", "queue", "write", "total"} {
		want := `gamemesh_ws_relay_duration_seconds_count{service="ws-handler-test",stage="` + stage + `"} 1`
		assert.Eventually(t, func() bool { return strings.Contains(scrape(hub), want) },
			2*time.Second, 10*time.Millisecond, stage)
	}

	// No echo to the sender.
	require.NoError(t, conns[0].SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	_, raw, err := conns[0].ReadMessage()
	assert.Error(t, err, "sender got %s", raw)
}
