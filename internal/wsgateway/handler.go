package wsgateway

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/httpx"
)

// Handler upgrades HTTP connections to WebSockets.
//
// Unlike REST services, this service validates JWTs itself: browsers cannot
// set an Authorization header on a WebSocket upgrade, so the token arrives as
// a ?token= query parameter (an Authorization header is also accepted for
// non-browser clients).
type Handler struct {
	hub      *Hub
	tokens   *auth.TokenManager
	upgrader websocket.Upgrader
	log      *zap.Logger
}

// NewHandler constructs the handler. allowedOrigins mirrors the HTTP CORS
// configuration for the upgrade handshake's Origin check.
func NewHandler(hub *Hub, tokens *auth.TokenManager, allowedOrigins []string, log *zap.Logger) *Handler {
	allowAll := len(allowedOrigins) == 1 && allowedOrigins[0] == "*"
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[o] = struct{}{}
	}
	return &Handler{
		hub:    hub,
		tokens: tokens,
		log:    log,
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

	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade already wrote the HTTP error response.
		h.log.Warn("websocket upgrade failed", zap.Error(err))
		return
	}

	client := newClient(claims.PlayerID, conn, h.hub, h.log)
	h.hub.register(client)
	h.log.Info("websocket connected", zap.String("player_id", claims.PlayerID))

	go client.writePump()
	go client.readPump()
}
