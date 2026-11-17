package matchmaking

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
)

const testBotWait = 5 * time.Second

// newGhostService is a service with the Ghost Tag queues on, and the
// miniredis behind it, so a test can set when a ticket joined.
func newGhostService(t *testing.T) (*Service, *capturingPublisher, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	pub := &capturingPublisher{}
	svc := NewService(
		NewQueue(rdb),
		NewRoomStore(rdb, time.Hour),
		pub,
		metrics.New("matchmaking-ghost-test"),
		zap.NewNop(),
		Config{
			RankWindow: 100, BatchSize: 1000, MaxQueueAge: 5 * time.Minute,
			GhostTag:         NewModeQueue(rdb, QueueGhostTag),
			GhostTagUnranked: NewModeQueue(rdb, QueueGhostTagUnranked),
			BotWait:          testBotWait,
		},
	)
	return svc, pub, mr
}

// joinedAgo rewrites when a Ghost Tag ticket joined, as if it had waited.
func joinedAgo(mr *miniredis.Miniredis, queue, playerID string, ago time.Duration) {
	mr.HSet("matchmaking:queue:"+queue+":joined", playerID, strconv.FormatInt(time.Now().Add(-ago).Unix(), 10))
}

// roomsCreated returns the rooms the published MatchFound events name.
func roomsCreated(t *testing.T, svc *Service, pub *capturingPublisher) []*Room {
	t.Helper()
	pub.mu.Lock()
	defer pub.mu.Unlock()
	var rooms []*Room
	for _, e := range pub.events {
		require.Equal(t, events.TypeMatchFound, e.Type)
		var p events.MatchFoundPayload
		require.NoError(t, json.Unmarshal(e.Payload, &p))
		room, err := svc.GetRoom(context.Background(), p.RoomID)
		require.NoError(t, err)
		assert.ElementsMatch(t, room.Players, p.Players, "MatchFound names the room's players")
		rooms = append(rooms, room)
	}
	return rooms
}

func queued(t *testing.T, q *Queue, playerID string) bool {
	t.Helper()
	ok, err := q.Contains(context.Background(), playerID)
	require.NoError(t, err)
	return ok
}

// TestGhostTagPairsIntoGameRooms: two Ghost Tag tickets in the rank window
// become one room that names the game; the general queue is not involved.
func TestGhostTagPairsIntoGameRooms(t *testing.T) {
	svc, pub, _ := newGhostService(t)
	ctx := context.Background()
	require.NoError(t, svc.JoinGhostTag(ctx, "alice", 1000, false))
	require.NoError(t, svc.JoinGhostTag(ctx, "bob", 1050, false))

	n, err := svc.MatchOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	rooms := roomsCreated(t, svc, pub)
	require.Len(t, rooms, 1)
	assert.Equal(t, ModeGhostTag, rooms[0].Mode)
	assert.False(t, rooms[0].Bot)
	assert.False(t, rooms[0].Unranked)
	assert.ElementsMatch(t, []string{"alice", "bob"}, rooms[0].Players)
	assert.False(t, queued(t, svc.cfg.GhostTag, "alice"), "dequeued from the Ghost Tag queue")
}

// TestGeneralRoomRecordIsUnchanged: a room of the general queue is stored
// exactly as before the mode fields existed, so the WebSocket service keeps
// treating it as an opaque-relay room.
func TestGeneralRoomRecordIsUnchanged(t *testing.T) {
	svc, pub, mr := newGhostService(t)
	ctx := context.Background()
	require.NoError(t, svc.JoinQueue(ctx, "alice", 1000))
	require.NoError(t, svc.JoinQueue(ctx, "bob", 1050))
	_, err := svc.MatchOnce(ctx)
	require.NoError(t, err)

	rooms := roomsCreated(t, svc, pub)
	require.Len(t, rooms, 1)
	raw, err := mr.Get("room:" + rooms[0].ID)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &fields))
	for _, k := range []string{"mode", "bot", "unranked"} {
		assert.NotContains(t, fields, k)
	}
}

// TestUnrankedPairsOnlyWithUnranked: an unranked ticket is never paired
// with a ranked one, however close their ranks; two unranked tickets make
// an unranked room.
func TestUnrankedPairsOnlyWithUnranked(t *testing.T) {
	svc, pub, _ := newGhostService(t)
	ctx := context.Background()
	require.NoError(t, svc.JoinGhostTag(ctx, "ranked", 1000, false))
	require.NoError(t, svc.JoinGhostTag(ctx, "bench1", 1001, true))

	n, err := svc.MatchOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "a ranked and an unranked ticket are not paired")

	require.NoError(t, svc.JoinGhostTag(ctx, "bench2", 1002, true))
	_, err = svc.MatchOnce(ctx)
	require.NoError(t, err)
	rooms := roomsCreated(t, svc, pub)
	require.Len(t, rooms, 1)
	assert.True(t, rooms[0].Unranked)
	assert.ElementsMatch(t, []string{"bench1", "bench2"}, rooms[0].Players)
	assert.True(t, queued(t, svc.cfg.GhostTag, "ranked"), "the ranked ticket still waits")
}

// TestBotRoomAfterTheWait: a ticket left alone gets a room with the bot
// once it has waited BotWait, and not before.
func TestBotRoomAfterTheWait(t *testing.T) {
	svc, pub, mr := newGhostService(t)
	ctx := context.Background()
	require.NoError(t, svc.JoinGhostTag(ctx, "early", 1000, false))
	require.NoError(t, svc.JoinGhostTag(ctx, "late", 5000, false)) // out of early's window
	joinedAgo(mr, QueueGhostTag, "early", 2*time.Second)
	joinedAgo(mr, QueueGhostTag, "late", testBotWait+time.Second)

	n, err := svc.MatchOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	rooms := roomsCreated(t, svc, pub)
	require.Len(t, rooms, 1)
	assert.Equal(t, []string{"late"}, rooms[0].Players, "only the human; the bot is the server's")
	assert.True(t, rooms[0].Bot)
	assert.Equal(t, ModeGhostTag, rooms[0].Mode)
	assert.Equal(t, map[string]int{"late": 5000}, rooms[0].Ranks)
	assert.False(t, queued(t, svc.cfg.GhostTag, "late"))
	assert.True(t, queued(t, svc.cfg.GhostTag, "early"), "2 s is not long enough")
}

// TestAHumanBeatsTheBot: two tickets that have both waited past BotWait
// and are in each other's window play each other, not the bot.
func TestAHumanBeatsTheBot(t *testing.T) {
	svc, pub, mr := newGhostService(t)
	ctx := context.Background()
	require.NoError(t, svc.JoinGhostTag(ctx, "alice", 1000, false))
	require.NoError(t, svc.JoinGhostTag(ctx, "bob", 1050, false))
	joinedAgo(mr, QueueGhostTag, "alice", time.Minute)
	joinedAgo(mr, QueueGhostTag, "bob", time.Minute)

	_, err := svc.MatchOnce(ctx)
	require.NoError(t, err)
	rooms := roomsCreated(t, svc, pub)
	require.Len(t, rooms, 1)
	assert.False(t, rooms[0].Bot)
	assert.Len(t, rooms[0].Players, 2)
}

// TestUnrankedBotRoomIsUnranked: a bench ticket left alone gets a bot room
// that is marked unranked as well.
func TestUnrankedBotRoomIsUnranked(t *testing.T) {
	svc, pub, mr := newGhostService(t)
	ctx := context.Background()
	require.NoError(t, svc.JoinGhostTag(ctx, "bench", 1000, true))
	joinedAgo(mr, QueueGhostTagUnranked, "bench", time.Minute)

	_, err := svc.MatchOnce(ctx)
	require.NoError(t, err)
	rooms := roomsCreated(t, svc, pub)
	require.Len(t, rooms, 1)
	assert.True(t, rooms[0].Bot)
	assert.True(t, rooms[0].Unranked)
}

// TestOneQueueAtATime: joining one queue takes the player out of the
// others, so no tick can put them in two rooms; leaving finds them in any.
func TestOneQueueAtATime(t *testing.T) {
	svc, _, _ := newGhostService(t)
	ctx := context.Background()
	require.NoError(t, svc.JoinQueue(ctx, "alice", 1000))
	require.NoError(t, svc.JoinGhostTag(ctx, "alice", 1000, false))
	assert.False(t, queued(t, svc.queue, "alice"))
	assert.True(t, queued(t, svc.cfg.GhostTag, "alice"))

	require.NoError(t, svc.JoinGhostTag(ctx, "alice", 1000, true))
	assert.False(t, queued(t, svc.cfg.GhostTag, "alice"))
	assert.True(t, queued(t, svc.cfg.GhostTagUnranked, "alice"))

	in, size, err := svc.QueueStatus(ctx, "alice")
	require.NoError(t, err)
	assert.True(t, in)
	assert.Equal(t, int64(1), size, "the depth of the queue alice is in")

	require.NoError(t, svc.LeaveQueue(ctx, "alice"))
	assert.False(t, queued(t, svc.cfg.GhostTagUnranked, "alice"))
	assert.ErrorIs(t, svc.LeaveQueue(ctx, "alice"), ErrNotQueued)
}

// TestGhostTagOffWithoutItsQueues: a service built without the Ghost Tag
// queues refuses the mode instead of putting the ticket somewhere else.
func TestGhostTagOffWithoutItsQueues(t *testing.T) {
	svc, _ := newTestService(t)
	assert.ErrorIs(t, svc.JoinGhostTag(context.Background(), "alice", 1000, false), ErrModeDisabled)
}

// TestQueueEndpointModes: the HTTP shape of the modes.
func TestQueueEndpointModes(t *testing.T) {
	svc, _, _ := newGhostService(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(svc).RegisterRoutes(r)

	w := doReq(r, http.MethodPost, "/queue", `{"rank":1000,"mode":"ghosttag","unranked":true}`, "alice")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"mode":"ghosttag"`)
	assert.Contains(t, w.Body.String(), `"unranked":true`)
	assert.True(t, queued(t, svc.cfg.GhostTagUnranked, "alice"))

	w = doReq(r, http.MethodPost, "/queue", `{"rank":1000,"mode":"chess"}`, "bob")
	assert.Equal(t, http.StatusBadRequest, w.Code, "an unknown mode")

	w = doReq(r, http.MethodPost, "/queue", `{"rank":1000,"unranked":true}`, "bob")
	assert.Equal(t, http.StatusBadRequest, w.Code, "unranked without a mode")
	assert.False(t, queued(t, svc.queue, "bob"))

	w = doReq(r, http.MethodPost, "/queue", `{"rank":1000}`, "carol")
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "mode", "the general queue answers as before")

	off, _ := newHandlerRouter(t)
	w = doReq(off, http.MethodPost, "/queue", `{"rank":1000,"mode":"ghosttag"}`, "dave")
	assert.Equal(t, http.StatusBadRequest, w.Code, "the mode is off on this service")
}
