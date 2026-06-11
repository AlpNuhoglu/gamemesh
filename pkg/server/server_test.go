package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/config"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
)

func TestNewEngineHealthAndMetrics(t *testing.T) {
	cfg := config.Load("testsvc")
	engine := NewEngine(cfg, zap.NewNop(), metrics.New("testsvc"))

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"testsvc"`)

	w = httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "go_goroutines")
}

func TestRunFailsFastOnBadPort(t *testing.T) {
	cfg := config.Load("testsvc")
	engine := NewEngine(cfg, zap.NewNop(), metrics.New("testsvc-run"))
	err := Run(engine, "999999", zap.NewNop())
	assert.Error(t, err)
}

func TestShutdownContext(t *testing.T) {
	ctx, cancel := ShutdownContext()
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("context must not be cancelled before a signal")
	default:
	}
}
