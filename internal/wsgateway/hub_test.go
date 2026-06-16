package wsgateway

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
)

// newHubClient builds a client without a real socket: hub operations only
// touch the send channel, never the conn.
func newHubClient(hub *Hub, playerID string) *Client {
	return newClient(playerID, nil, hub, zap.NewNop())
}

func newTestHub() *Hub {
	return NewHub(zap.NewNop(), metrics.New("ws-hub-test"))
}

func drain(c *Client) []Message {
	var msgs []Message
	for {
		select {
		case raw := <-c.send:
			var m Message
			_ = json.Unmarshal(raw, &m)
			msgs = append(msgs, m)
		default:
			return msgs
		}
	}
}

func TestJoinBroadcastsPlayerJoined(t *testing.T) {
	hub := newTestHub()
	alice := newHubClient(hub, "alice")
	bob := newHubClient(hub, "bob")
	hub.register(alice)
	hub.register(bob)

	hub.join(alice, "room-1")
	hub.join(bob, "room-1")

	// Bob's join is seen by alice (and bob himself, already in the room).
	aliceMsgs := drain(alice)
	require.NotEmpty(t, aliceMsgs)
	last := aliceMsgs[len(aliceMsgs)-1]
	assert.Equal(t, "PlayerJoined", last.Type)
	assert.Equal(t, "room-1", last.Room)
	assert.Contains(t, string(last.Data), "bob")
}

func TestLeaveBroadcastsPlayerLeft(t *testing.T) {
	hub := newTestHub()
	alice := newHubClient(hub, "alice")
	bob := newHubClient(hub, "bob")
	hub.register(alice)
	hub.register(bob)
	hub.join(alice, "room-1")
	hub.join(bob, "room-1")
	drain(alice)

	hub.leave(bob, "room-1")
	msgs := drain(alice)
	require.NotEmpty(t, msgs)
	assert.Equal(t, "PlayerLeft", msgs[len(msgs)-1].Type)
}

func TestUnregisterNotifiesRoomsAndUpdatesGauge(t *testing.T) {
	hub := newTestHub()
	alice := newHubClient(hub, "alice")
	bob := newHubClient(hub, "bob")
	hub.register(alice)
	hub.register(bob)
	hub.join(alice, "room-1")
	hub.join(bob, "room-1")
	drain(alice)

	hub.unregister(bob)
	msgs := drain(alice)
	require.NotEmpty(t, msgs)
	assert.Equal(t, "PlayerLeft", msgs[len(msgs)-1].Type)

	// Unregistering twice must be a no-op (no panic on double close).
	assert.NotPanics(t, func() { hub.unregister(bob) })
}

func TestSendToPlayerHitsAllConnections(t *testing.T) {
	hub := newTestHub()
	tab1 := newHubClient(hub, "alice")
	tab2 := newHubClient(hub, "alice") // second tab, same player
	other := newHubClient(hub, "bob")
	hub.register(tab1)
	hub.register(tab2)
	hub.register(other)

	hub.SendToPlayer("alice", []byte(`{"type":"MatchFound"}`))
	assert.Len(t, drain(tab1), 1)
	assert.Len(t, drain(tab2), 1)
	assert.Empty(t, drain(other))
}

func TestBroadcastAll(t *testing.T) {
	hub := newTestHub()
	a := newHubClient(hub, "a")
	b := newHubClient(hub, "b")
	hub.register(a)
	hub.register(b)

	hub.BroadcastAll([]byte(`{"type":"LeaderboardUpdated"}`))
	assert.Len(t, drain(a), 1)
	assert.Len(t, drain(b), 1)
}

func TestSlowClientDoesNotBlock(t *testing.T) {
	hub := newTestHub()
	slow := newHubClient(hub, "slow")
	hub.register(slow)

	// Overflow the send buffer; trySend must drop, not block.
	msg := []byte(`{"type":"LeaderboardUpdated"}`)
	for i := 0; i < sendBuffer*2; i++ {
		hub.BroadcastAll(msg)
	}
	assert.Len(t, drain(slow), sendBuffer)
}

func TestBridgeDispatch(t *testing.T) {
	hub := newTestHub()
	alice := newHubClient(hub, "alice")
	carol := newHubClient(hub, "carol")
	hub.register(alice)
	hub.register(carol)

	bridge := NewBridge(hub, nil, zap.NewNop())

	// MatchFound goes only to the matched players.
	e, err := events.New(events.TypeMatchFound, events.MatchFoundPayload{
		RoomID: "room-9", Players: []string{"alice", "bob"},
	})
	require.NoError(t, err)
	bridge.dispatch(context.Background(), e)
	aliceMsgs := drain(alice)
	require.Len(t, aliceMsgs, 1)
	assert.Equal(t, events.TypeMatchFound, aliceMsgs[0].Type)
	assert.Equal(t, "room-9", aliceMsgs[0].Room)
	assert.Empty(t, drain(carol))

	// LeaderboardUpdated broadcasts to everyone.
	e, err = events.New(events.TypeLeaderboardUpdated, events.LeaderboardUpdatedPayload{
		PlayerID: "p", Score: 1, Rank: 1,
	})
	require.NoError(t, err)
	bridge.dispatch(context.Background(), e)
	assert.Len(t, drain(alice), 1)
	assert.Len(t, drain(carol), 1)

	// Unknown events are ignored.
	bridge.dispatch(context.Background(), events.Event{Type: "SomethingElse"})
	assert.Empty(t, drain(alice))
}
