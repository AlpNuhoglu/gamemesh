// Package gateway implements the public API gateway: JWT termination,
// per-IP rate limiting, request logging and reverse-proxying to internal
// services.
package gateway

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
)

// newProxy builds a gin handler that strips /api/v1 and forwards to target.
// The authenticated identity travels on X-User-ID / X-Token-JTI headers —
// internal services trust these because they are only reachable inside the
// cluster network. httputil.ReverseProxy also transparently proxies WebSocket
// upgrades (Go >= 1.12), so /ws can be routed here too.
func newProxy(target string, log *zap.Logger) gin.HandlerFunc {
	u, err := url.Parse(target)
	if err != nil {
		log.Fatal("invalid upstream URL", zap.String("target", target), zap.Error(err))
	}
	proxy := httputil.NewSingleHostReverseProxy(u)

	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		director(r)
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api/v1")
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Error("upstream unavailable",
			zap.String("target", target),
			zap.String("path", r.URL.Path),
			zap.Error(err))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"upstream unavailable"}`))
	}

	return func(c *gin.Context) {
		// Never trust identity headers from the outside world: strip anything
		// the client sent, then inject the values derived from the JWT.
		c.Request.Header.Del(middleware.HeaderUserID)
		c.Request.Header.Del("X-Token-JTI")
		if uid := c.GetString(middleware.CtxUserID); uid != "" {
			c.Request.Header.Set(middleware.HeaderUserID, uid)
		}
		if jti := c.GetString(middleware.CtxTokenJTI); jti != "" {
			c.Request.Header.Set("X-Token-JTI", jti)
		}
		if rid := c.GetString(middleware.CtxRequestID); rid != "" {
			c.Request.Header.Set(middleware.HeaderRequestID, rid)
		}
		proxy.ServeHTTP(c.Writer, c.Request)
	}
}
