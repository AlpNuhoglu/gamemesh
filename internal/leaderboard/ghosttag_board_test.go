package leaderboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
)

// boardNow is the handler's clock in these tests: its UTC day is the daily
// board's.
var boardNow = time.Date(2026, 11, 22, 23, 30, 0, 0, time.UTC)

func newBoardRouter(t *testing.T) (*gin.Engine, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	svc := NewService(NewStore(rdb), &nopPublisher{}, metrics.New("lb-board-test"), zap.NewNop())
	h := NewHandler(svc, NewGhostTagStore(rdb))
	h.now = func() time.Time { return boardNow }
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.RegisterRoutes(r)
	return r, mr
}

// board reads a board and returns its status and rows, each row as the
// raw JSON object so that a field that should not be there shows.
func board(t *testing.T, r *gin.Engine, query string) (int, string, []map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/leaderboard/ghosttag"+query, nil))
	if w.Code != http.StatusOK {
		return w.Code, "", nil
	}
	var body struct {
		Board   string           `json:"board"`
		Entries []map[string]any `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	require.NotNil(t, body.Entries, "entries is a list, never null: %s", w.Body.String())
	return w.Code, body.Board, body.Entries
}

func row(name string, score float64, rank int) map[string]any {
	return map[string]any{"name": name, "score": score, "rank": float64(rank)}
}

// TestGhostTagBoardShowsNameScoreRankOnly: the all-time board, best first,
// each row only a name, a score and a rank; a player with no stored name
// shows the placeholder; the player ID and every other stats field stay
// out.
func TestGhostTagBoardShowsNameScoreRankOnly(t *testing.T) {
	r, mr := newBoardRouter(t)
	_, _ = mr.ZAdd(GhostTagAllTimeKey, 61, "id-alice")
	_, _ = mr.ZAdd(GhostTagAllTimeKey, 140, "id-bob")
	_, _ = mr.ZAdd(GhostTagAllTimeKey, -25, "id-carol")
	mr.HSet(GhostTagStatsKey("id-alice"), "name", "SwiftFox4821", "wins", "1", "win_streak", "1", "fastest_capture_ms", "28400")
	mr.HSet(GhostTagStatsKey("id-bob"), "name", "bob")
	mr.HSet(GhostTagStatsKey("id-carol"), "wins", "0") // stats, but no name

	code, name, rows := board(t, r, "")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "alltime", name, "the default board") //nolint:misspell // the API's spelling (section 4)
	assert.Equal(t, []map[string]any{
		row("bob", 140, 1),
		row("SwiftFox4821", 61, 2),
		row(GhostTagUnnamed, -25, 3),
	}, rows)
}

// TestGhostTagBoardDailyIsToday: board=daily is the board of the handler's
// UTC day, not another day's, nor the all-time one.
func TestGhostTagBoardDailyIsToday(t *testing.T) {
	r, mr := newBoardRouter(t)
	_, _ = mr.ZAdd(GhostTagAllTimeKey, 500, "id-old")
	_, _ = mr.ZAdd("lb:ghosttag:daily:2026-11-21", 90, "id-yesterday")
	_, _ = mr.ZAdd("lb:ghosttag:daily:2026-11-22", 45, "id-today")
	mr.HSet(GhostTagStatsKey("id-today"), "name", "today")

	code, name, rows := board(t, r, "?board=daily")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "daily", name)
	assert.Equal(t, []map[string]any{row("today", 45, 1)}, rows)

	_, _, rows = board(t, r, "?board=alltime") //nolint:misspell // the API's spelling (section 4)
	assert.Equal(t, []map[string]any{row(GhostTagUnnamed, 500, 1)}, rows)
}

// TestGhostTagBoardLimit: 10 rows by default, as many as asked up to 50,
// and an empty board is an empty list.
func TestGhostTagBoardLimit(t *testing.T) {
	r, mr := newBoardRouter(t)
	_, _, rows := board(t, r, "")
	assert.Empty(t, rows, "nobody has played")

	for i := range 60 {
		_, _ = mr.ZAdd(GhostTagAllTimeKey, float64(i), fmt.Sprintf("id-%02d", i))
	}
	_, _, rows = board(t, r, "")
	assert.Len(t, rows, 10)
	assert.Equal(t, 59.0, rows[0]["score"])
	assert.Equal(t, 10.0, rows[9]["rank"])
	_, _, rows = board(t, r, "?limit=3")
	assert.Len(t, rows, 3)
	_, _, rows = board(t, r, "?limit=50")
	assert.Len(t, rows, 50)
	assert.Equal(t, 50.0, rows[49]["rank"])
}

// TestGhostTagBoardRejects: an unknown board and a limit that is not a
// number in [1, 50] are refused.
func TestGhostTagBoardRejects(t *testing.T) {
	r, _ := newBoardRouter(t)
	for _, q := range []string{"?board=weekly", "?board=", "?limit=0", "?limit=51", "?limit=-1", "?limit=ten"} {
		code, _, _ := board(t, r, q)
		assert.Equal(t, http.StatusBadRequest, code, q)
	}
}

// TestGhostTagBoardFailsWithoutRedis: a store failure is a 500, not an
// empty board.
func TestGhostTagBoardFailsWithoutRedis(t *testing.T) {
	r, mr := newBoardRouter(t)
	_, _ = mr.ZAdd(GhostTagAllTimeKey, 1, "id-a")
	mr.Close()
	code, _, _ := board(t, r, "")
	assert.Equal(t, http.StatusInternalServerError, code)
}

// TestGhostTagBoardFailsOnAnUnreadableName: an error reading a name (here a
// stats key of the wrong type) fails the read rather than showing the
// placeholder for a player who has a name.
func TestGhostTagBoardFailsOnAnUnreadableName(t *testing.T) {
	r, mr := newBoardRouter(t)
	_, _ = mr.ZAdd(GhostTagAllTimeKey, 1, "id-a")
	require.NoError(t, mr.Set(GhostTagStatsKey("id-a"), "not a hash"))
	code, _, _ := board(t, r, "")
	assert.Equal(t, http.StatusInternalServerError, code)
}
