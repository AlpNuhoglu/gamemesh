package leaderboard

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/alpnuhoglu/gamemesh/pkg/httpx"
	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
)

const maxTopN = 1000

// Handler exposes the leaderboard HTTP API.
type Handler struct {
	svc *Service
}

// NewHandler constructs the handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes mounts the leaderboard endpoints (gateway strips /api/v1).
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	r.POST("/score", h.submitScore)
	r.GET("/leaderboard", h.page)
	r.GET("/leaderboard/top/:n", h.top)
	r.GET("/leaderboard/rank/:player_id", h.rank)
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
