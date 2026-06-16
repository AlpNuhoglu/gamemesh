// Package middleware provides the gin middleware chain shared by all
// services: request IDs, structured request logging, Prometheus metrics,
// CORS, per-IP rate limiting and JWT authentication.
package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/httpx"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/tracing"
)

// Context keys used across services.
const (
	CtxRequestID = "request_id"
	CtxUserID    = "user_id"
	CtxUsername  = "username"
	CtxTokenJTI  = "token_jti"

	HeaderRequestID = "X-Request-ID"
	HeaderUserID    = "X-User-ID"
)

// RequestID ensures every request carries an ID, generating one when absent.
// Downstream services receive it via the X-Request-ID header so a single
// request can be traced across service boundaries.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(HeaderRequestID)
		if id == "" {
			id = uuid.NewString()
		}
		c.Set(CtxRequestID, id)
		c.Writer.Header().Set(HeaderRequestID, id)
		c.Next()
	}
}

// Logger emits one structured log line per request with request ID, user ID
// (when authenticated), latency and error details.
func Logger(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		fields := []zap.Field{
			zap.String("request_id", c.GetString(CtxRequestID)),
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.Int("status", c.Writer.Status()),
			zap.Duration("latency", time.Since(start)),
			zap.String("client_ip", c.ClientIP()),
		}
		if uid := c.GetString(CtxUserID); uid != "" {
			fields = append(fields, zap.String("user_id", uid))
		} else if uid := c.GetHeader(HeaderUserID); uid != "" {
			fields = append(fields, zap.String("user_id", uid))
		}
		// Correlate logs with traces: attach trace_id/span_id when a recording
		// span is active (otelgin runs before this middleware), so operators
		// can pivot from a log line straight to the trace in Jaeger.
		fields = append(fields, tracing.LogFields(c.Request.Context())...)
		if len(c.Errors) > 0 {
			fields = append(fields, zap.String("error", c.Errors.String()))
			log.Error("http request", fields...)
			return
		}
		log.Info("http request", fields...)
	}
}

// Metrics records request count, error count and latency per route. It uses
// the matched route template (c.FullPath) rather than the raw URL to keep
// label cardinality bounded.
func Metrics(m *metrics.Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		path := c.FullPath()
		if path == "" {
			path = "unmatched"
		}
		status := strconv.Itoa(c.Writer.Status())
		m.RequestsTotal.WithLabelValues(c.Request.Method, path, status).Inc()
		m.RequestDuration.WithLabelValues(c.Request.Method, path).Observe(time.Since(start).Seconds())
		if c.Writer.Status() >= http.StatusBadRequest {
			m.ErrorsTotal.WithLabelValues(c.Request.Method, path, status).Inc()
		}
	}
}

// CORS applies a configurable allow-list of origins.
func CORS(allowedOrigins []string) gin.HandlerFunc {
	allowAll := len(allowedOrigins) == 1 && allowedOrigins[0] == "*"
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[o] = struct{}{}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			if allowAll {
				c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
			} else if _, ok := allowed[origin]; ok {
				c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
				c.Writer.Header().Set("Vary", "Origin")
			}
			c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			c.Writer.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID")
			c.Writer.Header().Set("Access-Control-Max-Age", "86400")
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// ipLimiter tracks a token bucket per client IP. Entries idle for more than
// ten minutes are evicted by a background janitor to bound memory. For
// multi-replica gateways this would move to a Redis-based limiter; the
// interface boundary (gin.HandlerFunc) stays the same.
type ipLimiter struct {
	mu       sync.Mutex
	limiters map[string]*ipEntry
	rps      rate.Limit
	burst    int
}

type ipEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func (l *ipLimiter) get(ip string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.limiters[ip]
	if !ok {
		e = &ipEntry{limiter: rate.NewLimiter(l.rps, l.burst)}
		l.limiters[ip] = e
	}
	e.lastSeen = time.Now()
	return e.limiter
}

func (l *ipLimiter) janitor() {
	for range time.Tick(time.Minute) {
		l.mu.Lock()
		for ip, e := range l.limiters {
			if time.Since(e.lastSeen) > 10*time.Minute {
				delete(l.limiters, ip)
			}
		}
		l.mu.Unlock()
	}
}

// RateLimit rejects clients exceeding rps (with the given burst) per IP.
func RateLimit(rps float64, burst int) gin.HandlerFunc {
	l := &ipLimiter{
		limiters: make(map[string]*ipEntry),
		rps:      rate.Limit(rps),
		burst:    burst,
	}
	go l.janitor()

	return func(c *gin.Context) {
		if !l.get(c.ClientIP()).Allow() {
			httpx.Error(c, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		c.Next()
	}
}

// Auth validates the Bearer token and stores the caller identity in the
// context. Used by the gateway (and the WebSocket service directly, since
// browsers cannot set headers on WS upgrade requests).
func Auth(tm *auth.TokenManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			httpx.Error(c, http.StatusUnauthorized, "missing bearer token")
			return
		}
		claims, err := tm.Validate(strings.TrimPrefix(header, prefix))
		if err != nil {
			httpx.Error(c, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		c.Set(CtxUserID, claims.PlayerID)
		c.Set(CtxUsername, claims.Username)
		c.Set(CtxTokenJTI, claims.ID)
		c.Next()
	}
}
