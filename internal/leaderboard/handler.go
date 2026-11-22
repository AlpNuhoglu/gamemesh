package leaderboard

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alpnuhoglu/gamemesh/pkg/httpx"
	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
)

const maxTopN = 1000

// The Ghost Tag board read: the two boards, and limit's default and
// ceiling.
const (
	ghostBoardAllTime = "alltime" //nolint:misspell // the query value of the design doc (section 4)
	ghostBoardDaily   = "daily"
	ghostDefaultLimit = 10
	ghostMaxLimit     = 50
)

// Handler exposes the leaderboard HTTP API.
type Handler struct {
	svc   *Service
	ghost *GhostTagStore
	// now picks the day of the daily Ghost Tag board.
	now func() time.Time
}

// NewHandler constructs the handler. ghost serves the Ghost Tag boards.
func NewHandler(svc *Service, ghost *GhostTagStore) *Handler {
	return &Handler{svc: svc, ghost: ghost, now: time.Now}
}

// RegisterRoutes mounts the leaderboard endpoints (gateway strips /api/v1).
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	r.POST("/score", h.submitScore)
	r.GET("/leaderboard", h.page)
	r.GET("/leaderboard/top/:n", h.top)
	r.GET("/leaderboard/rank/:player_id", h.rank)
	r.GET("/leaderboard/ghosttag", h.ghostTag)
}

// ghostTag is the public read of a Ghost Tag board: ?board= is the
// all-time board (ghostBoardAllTime, the default) or today's UTC day
// (ghostBoardDaily), ?limit= is 10 by default and at most 50. A row is a
// name, a score and a rank only.
func (h *Handler) ghostTag(c *gin.Context) {
	board := c.DefaultQuery("board", ghostBoardAllTime)
	if board != ghostBoardAllTime && board != ghostBoardDaily {
		httpx.Error(c, http.StatusBadRequest, "board must be "+ghostBoardDaily+" or "+ghostBoardAllTime)
		return
	}
	limit, err := strconv.ParseInt(c.DefaultQuery("limit", strconv.Itoa(ghostDefaultLimit)), 10, 64)
	if err != nil || limit <= 0 || limit > ghostMaxLimit {
		httpx.Error(c, http.StatusBadRequest, "limit must be between 1 and 50")
		return
	}
	rows, err := h.ghost.GhostTagTop(c.Request.Context(), board == ghostBoardDaily, h.now(), limit)
	if err != nil {
		httpx.Error(c, http.StatusInternalServerError, "failed to fetch leaderboard")
		return
	}
	httpx.OK(c, gin.H{"board": board, "entries": rows})
}

type scoreRequest struct {
	Score     float64 `json:"score" binding:"required"`
	Increment bool    `json:"increment"`
}

func (h *Handler) submitScore(c *gin.Context) {
	playerID := c.GetHeader(middleware.HeaderUserID)
	if playerID == "" {
		httpx.Error(c, http.StatusUnauthorized, "missing player identity")
		return
	}
	var req scoreRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	entry, err := h.svc.SubmitScore(c.Request.Context(), playerID, req.Score, req.Increment)
	if err != nil {
		httpx.Error(c, http.StatusInternalServerError, "failed to update score")
		return
	}
	httpx.OK(c, entry)
}

func (h *Handler) page(c *gin.Context) {
	offset, _ := strconv.ParseInt(c.DefaultQuery("offset", "0"), 10, 64)
	limit, _ := strconv.ParseInt(c.DefaultQuery("limit", "50"), 10, 64)
	if offset < 0 || limit <= 0 || limit > maxTopN {
		httpx.Error(c, http.StatusBadRequest, "offset must be >= 0 and 0 < limit <= 1000")
		return
	}
	entries, total, err := h.svc.Page(c.Request.Context(), offset, limit)
	if err != nil {
		httpx.Error(c, http.StatusInternalServerError, "failed to fetch leaderboard")
		return
	}
	httpx.OK(c, gin.H{"entries": entries, "total": total, "offset": offset, "limit": limit})
}

func (h *Handler) top(c *gin.Context) {
	n, err := strconv.ParseInt(c.Param("n"), 10, 64)
	if err != nil || n <= 0 || n > maxTopN {
		httpx.Error(c, http.StatusBadRequest, "n must be between 1 and 1000")
		return
	}
	entries, err := h.svc.Top(c.Request.Context(), n)
	if err != nil {
		httpx.Error(c, http.StatusInternalServerError, "failed to fetch leaderboard")
		return
	}
	httpx.OK(c, gin.H{"entries": entries})
}

func (h *Handler) rank(c *gin.Context) {
	entry, err := h.svc.Rank(c.Request.Context(), c.Param("player_id"))
	if err != nil {
		if errors.Is(err, ErrPlayerNotRanked) {
			httpx.Error(c, http.StatusNotFound, ErrPlayerNotRanked.Error())
			return
		}
		httpx.Error(c, http.StatusInternalServerError, "failed to fetch rank")
		return
	}
	httpx.OK(c, entry)
}
