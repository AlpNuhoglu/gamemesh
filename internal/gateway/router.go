package gateway

import (
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/config"
	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
)

// RegisterRoutes wires every public route to its upstream service.
// Routes are declared explicitly (no blanket wildcards) so the gateway is
// also the authoritative, reviewable map of the public API surface.
//
// sessions enforces server-side revocation on protected routes; pass a
// cache-backed checker so the per-request check stays off the Redis hot path. A
// nil sessions disables revocation (JWT-only) — handy for tests.
func RegisterRoutes(r *gin.Engine, cfg *config.Config, tokens *auth.TokenManager, sessions middleware.SessionChecker, log *zap.Logger) {
	playerProxy := newProxy(cfg.PlayerServiceURL, log)
	matchProxy := newProxy(cfg.MatchmakingServiceURL, log)
	lbProxy := newProxy(cfg.LeaderboardServiceURL, log)
	wsProxy := newProxy(cfg.WebsocketServiceURL, log)

	authRequired := middleware.Auth(tokens, sessions)

	v1 := r.Group("/api/v1")

	// Public — authentication endpoints and leaderboard reads.
	v1.POST("/auth/register", playerProxy)
	v1.POST("/auth/login", playerProxy)
	v1.GET("/leaderboard", lbProxy)
	v1.GET("/leaderboard/top/:n", lbProxy)
	v1.GET("/leaderboard/rank/:player_id", lbProxy)

	// Protected — require a valid JWT.
	protected := v1.Group("", authRequired)
	protected.POST("/auth/logout", playerProxy)
	protected.GET("/players/:id", playerProxy) // ":id" accepts "me"
	protected.PUT("/players/:id", playerProxy)
	protected.POST("/score", lbProxy)
	protected.POST("/queue", matchProxy)
	protected.DELETE("/queue", matchProxy)
	protected.GET("/queue/status", matchProxy)
	protected.GET("/rooms/:id", matchProxy)

	// WebSocket upgrade — the WS service validates the token itself (it
	// arrives as a query parameter), so no Auth middleware here.
	r.GET("/ws", wsProxy)
}
