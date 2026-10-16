package wsgateway

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/httpx"
	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
	"github.com/alpnuhoglu/gamemesh/pkg/tracing"
)

// Handler upgrades HTTP connections to WebSockets.
//
// Unlike REST services, this service validates JWTs itself: browsers cannot
// set an Authorization header on a WebSocket upgrade, so the token arrives as
// a ?token= query parameter (an Authorization header is also accepted for
// non-browser clients). Like the gateway, it also requires the token's session
// to still exist, so a logged-out token or one with a forged JTI is refused.
type Handler struct {
	hub      *Hub
	tokens   *auth.TokenManager
	sessions middleware.SessionChecker
	upgrader websocket.Upgrader
	log      *zap.Logger
}

// sessionCheckTimeout bounds the session lookup during the handshake, so a
// stalled Redis or an exhausted connection pool answers 503 quickly instead
// of holding the upgrade open; clients retry with jitter.
const sessionCheckTimeout = 2 * time.Second

// NewHandler constructs the handler. sessions must not be cached: the lookup
// happens once per connection, so revocation takes effect immediately.
// allowedOrigins mirrors the HTTP CORS configuration for the upgrade
// handshake's Origin check.
func NewHandler(hub *Hub, tokens *auth.TokenManager, sessions middleware.SessionChecker, allowedOrigins []string, log *zap.Logger) *Handler {
	allowAll := len(allowedOrigins) == 1 && allowedOrigins[0] == "*"
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[o] = struct{}{}
	}
	return &Handler{
		hub:      hub,
		tokens:   tokens,
		sessions: sessions,
		log:      log,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				if allowAll {
					return true
				}
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true // non-browser client
				}
				_, ok := allowed[origin]
				return ok
			},
		},
	}
}

// RegisterRoutes mounts the /ws endpoint.
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	r.GET("/ws", h.serveWS)
}

func (h *Handler) serveWS(c *gin.Context) {
	tokenString := c.Query("token")
	if tokenString == "" {
		if header := c.GetHeader("Authorization"); len(header) > 7 && header[:7] == "Bearer " {
			tokenString = header[7:]
		}
	}
	if tokenString == "" {
		httpx.Error(c, http.StatusUnauthorized, "missing token")
		return
	}
	claims, err := h.tokens.Validate(tokenString)
	if err != nil {
		httpx.Error(c, http.StatusUnauthorized, "invalid or expired token")
		return
	}
	// A valid signature is not enough: the session must still exist. On a
	// lookup error we reject rather than fail open, as the gateway does.
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionCheckTimeout)
	active, err := h.sessions.Exists(ctx, claims.ID)
	cancel()
	if err != nil {
		h.log.Warn("websocket session check failed", zap.Error(err))
		httpx.Error(c, http.StatusServiceUnavailable, "session check unavailable")
		return
	}
	if !active {
		httpx.Error(c, http.StatusUnauthorized, "token revoked")
		return
	}

	// Trace connection establishment only — the long-lived read/write pumps are
	// intentionally NOT traced (per-frame spans would grow without bound).
	_, span := tracing.Tracer().Start(c.Request.Context(), "ws.connect")
	defer span.End()

	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade already wrote the HTTP error response.
		tracing.RecordError(span, err)
		h.log.Warn("websocket upgrade failed", zap.Error(err))
		return
	}

	client := newClient(claims.PlayerID, conn, h.hub, h.log)
	h.hub.register(client)
	h.log.Info("websocket connected",
		append([]zap.Field{zap.String("player_id", claims.PlayerID)},
			tracing.LogFields(c.Request.Context())...)...)

	go client.writePump()
	go client.readPump()
}
