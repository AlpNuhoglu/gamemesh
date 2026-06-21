package presence

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/alpnuhoglu/gamemesh/pkg/httpx"
)

// Handler exposes the internal presence HTTP API. These endpoints are called by
// other cluster services (the WS gateway for connection lifecycle, matchmaking
// for queue/match transitions, friend/social services for lookups) — they are
// not public, so they trust the cluster network and the player IDs supplied in
// the request, matching the rest of GameMesh's internal-service model.
type Handler struct {
	svc *Service
}

// NewHandler constructs the handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// RegisterRoutes mounts the presence endpoints.
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	// Public-ish reads (still internal).
	r.GET("/presence/:id", h.get)
	r.POST("/presence/friends", h.friends)
	r.PUT("/presence/state", h.setState)
	// Connection lifecycle, driven by the WS gateway notifier.
	r.POST("/presence/connect", h.connect)
	r.POST("/presence/disconnect", h.disconnect)
	r.POST("/presence/heartbeat", h.heartbeat)
}

type playerRequest struct {
	PlayerID string `json:"player_id" binding:"required"`
}

type stateRequest struct {
	PlayerID string `json:"player_id" binding:"required"`
	State    State  `json:"state" binding:"required"`
}

type friendsRequest struct {
	// IDs is the friend list to look up. POST (not GET) so large lists are not
	// constrained by URL length limits.
	IDs []string `json:"ids" binding:"required"`
}

func (h *Handler) get(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		httpx.Error(c, http.StatusBadRequest, "missing player id")
		return
	}
	rec, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		httpx.Error(c, http.StatusInternalServerError, "failed to fetch presence")
		return
	}
	httpx.OK(c, presenceResponse(id, rec))
}

func (h *Handler) friends(c *gin.Context) {
	var req friendsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	friends, err := h.svc.Friends(c.Request.Context(), req.IDs)
	if err != nil {
		httpx.Error(c, http.StatusInternalServerError, "failed to fetch friend presence")
		return
	}
	httpx.OK(c, gin.H{"friends": friends})
}

func (h *Handler) setState(c *gin.Context) {
	var req stateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	rec, err := h.svc.SetState(c.Request.Context(), req.PlayerID, req.State)
	if err != nil {
		if errors.Is(err, ErrInvalidTransition) {
			httpx.Error(c, http.StatusBadRequest, "invalid presence transition to "+string(req.State))
			return
		}
		httpx.Error(c, http.StatusInternalServerError, "failed to set presence state")
		return
	}
	httpx.OK(c, presenceResponse(req.PlayerID, rec))
}

func (h *Handler) connect(c *gin.Context)    { h.lifecycle(c, h.svc.Connect) }
func (h *Handler) disconnect(c *gin.Context) { h.lifecycle(c, h.svc.Disconnect) }
func (h *Handler) heartbeat(c *gin.Context)  { h.lifecycle(c, h.svc.Heartbeat) }

// lifecycle is the shared body for connect/disconnect/heartbeat: bind the
// player id, run the op, return the resulting record. All three ops share the
// same (ctx, playerID) -> (Record, error) shape.
func (h *Handler) lifecycle(c *gin.Context, op func(ctx context.Context, playerID string) (Record, error)) {
	var req playerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	rec, err := op(c.Request.Context(), req.PlayerID)
	if err != nil {
		httpx.Error(c, http.StatusInternalServerError, "failed to update presence")
		return
	}
	httpx.OK(c, presenceResponse(req.PlayerID, rec))
}

func presenceResponse(playerID string, rec Record) gin.H {
	return gin.H{
		"player_id":        playerID,
		"state":            rec.State,
		"last_seen":        rec.LastSeen,
		"connection_count": rec.ConnectionCount,
		"updated_at":       rec.UpdatedAt,
	}
}
