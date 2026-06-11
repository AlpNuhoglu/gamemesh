package matchmaking

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/alpnuhoglu/gamemesh/pkg/httpx"
	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
)

// Handler exposes the matchmaking HTTP API (gateway strips /api/v1).
type Handler struct {
	svc *Service
}

// NewHandler constructs the handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes mounts the matchmaking endpoints.
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	r.POST("/queue", h.join)
	r.DELETE("/queue", h.leave)
	r.GET("/queue/status", h.status)
	r.GET("/rooms/:id", h.getRoom)
}

func callerID(c *gin.Context) (string, bool) {
	id := c.GetHeader(middleware.HeaderUserID)
	if id == "" {
		httpx.Error(c, http.StatusUnauthorized, "missing player identity")
		return "", false
	}
	return id, true
}

type joinRequest struct {
	Rank int `json:"rank" binding:"required,min=0,max=100000"`
}

func (h *Handler) join(c *gin.Context) {
	playerID, ok := callerID(c)
	if !ok {
		return
	}
	var req joinRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if err := h.svc.JoinQueue(c.Request.Context(), playerID, req.Rank); err != nil {
		httpx.Error(c, http.StatusInternalServerError, "failed to join queue")
		return
	}
	httpx.OK(c, gin.H{"status": "queued", "rank": req.Rank})
}

func (h *Handler) leave(c *gin.Context) {
	playerID, ok := callerID(c)
	if !ok {
		return
	}
	if err := h.svc.LeaveQueue(c.Request.Context(), playerID); err != nil {
		if errors.Is(err, ErrNotQueued) {
			httpx.Error(c, http.StatusNotFound, ErrNotQueued.Error())
			return
		}
		httpx.Error(c, http.StatusInternalServerError, "failed to leave queue")
		return
	}
	httpx.OK(c, gin.H{"status": "removed"})
}

func (h *Handler) status(c *gin.Context) {
	playerID, ok := callerID(c)
	if !ok {
		return
	}
	queued, size, err := h.svc.QueueStatus(c.Request.Context(), playerID)
	if err != nil {
		httpx.Error(c, http.StatusInternalServerError, "failed to fetch queue status")
		return
	}
	httpx.OK(c, gin.H{"queued": queued, "queue_size": size})
}

func (h *Handler) getRoom(c *gin.Context) {
	room, err := h.svc.GetRoom(c.Request.Context(), c.Param("id"))
	if err != nil {
		if errors.Is(err, ErrRoomNotFound) {
			httpx.Error(c, http.StatusNotFound, ErrRoomNotFound.Error())
			return
		}
		httpx.Error(c, http.StatusInternalServerError, "failed to fetch room")
		return
	}
	httpx.OK(c, room)
}
