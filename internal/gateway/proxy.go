// Package gateway implements the public API gateway: JWT termination,
// per-IP rate limiting, request logging and reverse-proxying to internal
// services.
package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
)

// statusClientClosedRequest is nginx's 499: the client closed the request
// before the answer. Not in net/http, not a standard code; used only in logs
// and metrics, since the client is gone.
const statusClientClosedRequest = 499

// upstreamTransport carries every proxied request; one idle pool per
// upstream. Go's default keeps 2 idle connections per host, so under
// concurrency the proxy closed almost every connection after its response
// and dialled a new one for the next: each left a socket in TIME_WAIT, and at
// about 770 requests/s the gateway ran out of local ports and answered 502
// (connect: cannot assign requested address, finding #96). 256 per upstream
// covers the concurrency the gateway sees; there is no cap on open
// connections, so a burst is never queued behind the pool.
var upstreamTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 1024
	t.MaxIdleConnsPerHost = 256
	t.IdleConnTimeout = 90 * time.Second
	return t
}()

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
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(u)
			// SetURL points Host at the upstream; keep the inbound Host, as
			// NewSingleHostReverseProxy did.
			r.Out.Host = r.In.Host
			r.Out.URL.Path = strings.TrimPrefix(r.Out.URL.Path, "/api/v1")
			if r.Out.URL.Path == "" {
				r.Out.URL.Path = "/"
			}
			// Rewrite has already dropped the client's Forwarded and
			// X-Forwarded-* headers; forward the client IP the handler resolved.
			r.Out.Header.Set("X-Forwarded-For", r.In.Header.Get("X-Forwarded-For"))
		},
		// Wrap the transport so the outbound request to the downstream service
		// is a client span and the W3C traceparent header is injected
		// automatically. Combined with otelgin on the downstream, gateway ->
		// service -> store shows up as a single distributed trace.
		Transport: otelhttp.NewTransport(upstreamTransport),
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// The client gave up (closed the page or the connection, or hit its
		// own timeout): not the upstream's failure, and nobody reads the
		// answer. It was a 502, so the status page's error rate (5xx) counted
		// users giving up as server errors (finding #97). 499 is nginx's
		// "client closed request"; it stays visible in requests_total.
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			w.WriteHeader(statusClientClosedRequest)
			return
		}
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
		// The same goes for the client address: forward the IP resolved
		// against TRUSTED_PROXIES (see server.NewEngine), never the
		// client's own X-Forwarded-For / X-Real-IP.
		c.Request.Header.Del("X-Real-IP")
		c.Request.Header.Set("X-Forwarded-For", c.ClientIP())
		proxy.ServeHTTP(c.Writer, c.Request)
	}
}
