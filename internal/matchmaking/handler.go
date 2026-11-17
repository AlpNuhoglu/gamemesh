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

// joinRequest is the body of POST /queue. Without mode it joins the
// general queue, as it always has. mode "ghosttag" joins the Ghost Tag
// queue; unranked asks for matches that do not score, which only another
// unranked ticket (or the bot) is paired with.
type joinRequest struct {
	Rank     int    `json:"rank" binding:"required,min=0,max=100000"`
	Mode     string `json:"mode" binding:"omitempty,oneof=ghosttag"`
	Unranked bool   `json:"unranked"`
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
	if req.Unranked && req.Mode == "" {
		// Ignoring it would queue the player for matches they asked not to have.
		httpx.Error(c, http.StatusBadRequest, "invalid request: unranked needs a mode")
		return
	}
	var err error
	if req.Mode == ModeGhostTag {
		err = h.svc.JoinGhostTag(c.Request.Context(), playerID, req.Rank, req.Unranked)
	} else {
		err = h.svc.JoinQueue(c.Request.Context(), playerID, req.Rank)
	}
	switch {
	case errors.Is(err, ErrModeDisabled):
		httpx.Error(c, http.StatusBadRequest, ErrModeDisabled.Error())
		return
	case err != nil:
		httpx.Error(c, http.StatusInternalServerError, "failed to join queue")
		return
	}
	body := gin.H{"status": "queued", "rank": req.Rank}
	if req.Mode != "" {
		body["mode"], body["unranked"] = req.Mode, req.Unranked
	}
	httpx.OK(c, body)
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

// getRoom returns a room to its own players only. Anyone else gets the same
// 404 as for a room that does not exist, so the answer does not tell a
// stranger whether a room ID is live; the refusal is counted.
func (h *Handler) getRoom(c *gin.Context) {
	playerID, ok := callerID(c)
	if !ok {
		return
	}
	room, err := h.svc.GetRoom(c.Request.Context(), c.Param("id"))
	if err != nil {
		if errors.Is(err, ErrRoomNotFound) {
			httpx.Error(c, http.StatusNotFound, ErrRoomNotFound.Error())
			return
		}
		httpx.Error(c, http.StatusInternalServerError, "failed to fetch room")
		return
	}
	if !room.HasPlayer(playerID) {
		h.svc.m.MatchmakingRoomDeniedTotal.Inc()
		httpx.Error(c, http.StatusNotFound, ErrRoomNotFound.Error())
		return
	}
	httpx.OK(c, room)
}
