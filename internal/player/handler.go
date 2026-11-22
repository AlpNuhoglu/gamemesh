package player

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/alpnuhoglu/gamemesh/pkg/httpx"
	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
)

// Handler exposes the player HTTP API. Routes are registered without the
// /api/v1 prefix; the API gateway strips it before proxying.
//
// Trust model: the gateway terminates JWT auth and forwards the caller's
// identity as X-User-ID. Services are reachable only on the cluster-internal
// network, so they trust that header. The WebSocket service is the one
// exception (it validates JWTs itself, see internal/wsgateway).
type Handler struct {
	svc *Service
}

// NewHandler constructs the handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes mounts all player endpoints.
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	r.POST("/auth/register", h.register)
	r.POST("/auth/login", h.login)
	r.POST("/auth/logout", h.logout)
	r.GET("/players/:id", h.getProfile)
	r.PUT("/players/:id", h.updateProfile)
}

// RegisterGameClientRoutes mounts what exists only with the release flag
// (GAME_CLIENT_ENABLED) on.
func (h *Handler) RegisterGameClientRoutes(r *gin.Engine) {
	r.POST("/auth/guest", h.createGuest)
	r.DELETE("/players/:id", h.deleteAccount)
}

// deleteAccount deletes the caller's own account, registered or guest. The
// token is the proof: whoever holds a valid session of the account may end
// it.
func (h *Handler) deleteAccount(c *gin.Context) {
	id, ok := resolveID(c)
	if !ok {
		return
	}
	if caller := c.GetHeader(middleware.HeaderUserID); caller != id.String() {
		httpx.Error(c, http.StatusForbidden, "cannot delete another player's account")
		return
	}
	if err := h.svc.DeleteAccount(c.Request.Context(), id); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.Error(c, http.StatusNotFound, ErrNotFound.Error())
			return
		}
		httpx.Error(c, http.StatusInternalServerError, "could not delete the account")
		return
	}
	c.Status(http.StatusNoContent)
}

type registerRequest struct {
	Username string `json:"username" binding:"required,alphanum,min=3,max=32"`
	Email    string `json:"email" binding:"required,email,max=255"`
	Password string `json:"password" binding:"required,min=8,max=72"`
}

func (h *Handler) register(c *gin.Context) {
	var req registerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	p, err := h.svc.Register(c.Request.Context(), RegisterInput(req))
	if err != nil {
		if errors.Is(err, ErrDuplicate) {
			httpx.Error(c, http.StatusConflict, ErrDuplicate.Error())
			return
		}
		httpx.Error(c, http.StatusInternalServerError, "registration failed")
		return
	}
	httpx.Created(c, p)
}

type loginRequest struct {
	// Identifier accepts username or email.
	Identifier string `json:"identifier" binding:"required,max=255"`
	Password   string `json:"password" binding:"required,max=72"`
}

type loginResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Player    *Player   `json:"player"`
}

// guestResponse is a login answer plus the secret, which is given once.
type guestResponse struct {
	loginResponse
	Secret string `json:"secret"`
}

func (h *Handler) login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	session, err := h.svc.Login(c.Request.Context(), req.Identifier, req.Password)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			httpx.Error(c, http.StatusUnauthorized, ErrInvalidCredentials.Error())
			return
		}
		httpx.Error(c, http.StatusInternalServerError, "login failed")
		return
	}
	httpx.OK(c, loginAnswer(session))
}

func loginAnswer(s *Session) loginResponse {
	return loginResponse{Token: s.Token, ExpiresAt: s.ExpiresAt, Player: view(s.Player, true)}
}

// createGuest makes a guest account: no body, no form. The per-IP hourly
// limit on it lives at the gateway, which is where the client's address is
// known.
func (h *Handler) createGuest(c *gin.Context) {
	guest, err := h.svc.CreateGuest(c.Request.Context())
	if err != nil {
		switch {
		case errors.Is(err, ErrGuestsDisabled):
			httpx.Error(c, http.StatusNotFound, "not found")
		case errors.Is(err, ErrGuestName):
			httpx.Error(c, http.StatusServiceUnavailable, "could not create a guest account, try again")
		default:
			httpx.Error(c, http.StatusInternalServerError, "could not create a guest account")
		}
		return
	}
	httpx.Created(c, guestResponse{loginResponse: loginAnswer(&guest.Session), Secret: guest.Secret})
}

func (h *Handler) logout(c *gin.Context) {
	jti := c.GetHeader("X-Token-JTI")
	if jti == "" {
		httpx.Error(c, http.StatusBadRequest, "missing session")
		return
	}
	if err := h.svc.Logout(c.Request.Context(), jti); err != nil {
		httpx.Error(c, http.StatusInternalServerError, "logout failed")
		return
	}
	httpx.OK(c, gin.H{"status": "logged out"})
}

// resolveID supports "/players/me" by substituting the authenticated caller's
// ID forwarded by the gateway.
func resolveID(c *gin.Context) (uuid.UUID, bool) {
	raw := c.Param("id")
	if raw == "me" {
		raw = c.GetHeader(middleware.HeaderUserID)
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		httpx.Error(c, http.StatusBadRequest, "invalid player id")
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) getProfile(c *gin.Context) {
	id, ok := resolveID(c)
	if !ok {
		return
	}
	p, err := h.svc.GetProfile(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.Error(c, http.StatusNotFound, ErrNotFound.Error())
			return
		}
		httpx.Error(c, http.StatusInternalServerError, "failed to fetch profile")
		return
	}
	httpx.OK(c, view(p, c.GetHeader(middleware.HeaderUserID) == p.ID.String()))
}

// view is the player as the API shows them. The e-mail address is shown to
// its owner only: any signed-in caller can ask for any player's profile, and
// player ids are not secret (a match shows both players theirs), so the
// address used to be readable by whoever a player had been matched with. A
// guest has no address at all; the placeholder in its row is never shown.
func view(p *Player, own bool) *Player {
	if own && !p.IsGuest {
		return p
	}
	public := *p
	public.Email = ""
	return &public
}

type updateRequest struct {
	Username *string `json:"username" binding:"omitempty,alphanum,min=3,max=32"`
	Email    *string `json:"email" binding:"omitempty,email,max=255"`
}

func (h *Handler) updateProfile(c *gin.Context) {
	id, ok := resolveID(c)
	if !ok {
		return
	}
	// Authorization: players may only update their own profile.
	if caller := c.GetHeader(middleware.HeaderUserID); caller != id.String() {
		httpx.Error(c, http.StatusForbidden, "cannot update another player's profile")
		return
	}
	var req updateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	p, err := h.svc.UpdateProfile(c.Request.Context(), id, UpdateInput(req))
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			httpx.Error(c, http.StatusNotFound, ErrNotFound.Error())
		case errors.Is(err, ErrGuestProfile):
			httpx.Error(c, http.StatusForbidden, ErrGuestProfile.Error())
		case errors.Is(err, ErrDuplicate):
			httpx.Error(c, http.StatusConflict, ErrDuplicate.Error())
		default:
			httpx.Error(c, http.StatusInternalServerError, "update failed")
		}
		return
	}
	httpx.OK(c, view(p, true))
}
