package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewIsIsolatedPerInstance(t *testing.T) {
	// Two instances must not collide (each has its own registry).
	assert.NotPanics(t, func() {
		New("svc-a")
		New("svc-b")
	})
}

func TestHandlerExposesInstruments(t *testing.T) {
	m := New("svc-test")
	m.RequestsTotal.WithLabelValues("GET", "/x", "200").Inc()
	m.WSConnections.Set(7)
	m.MatchmakingQueueSize.Set(3)
	m.MatchesCreated.Inc()
	m.LeaderboardUpdates.Inc()
	m.RequestDuration.WithLabelValues("GET", "/x").Observe(0.05)
	m.ErrorsTotal.WithLabelValues("GET", "/x", "500").Inc()

	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()

	for _, metric := range []string{
		"gamemesh_http_requests_total",
		"gamemesh_http_errors_total",
		"gamemesh_http_request_duration_seconds",
		`gamemesh_websocket_active_connections{service="svc-test"} 7`,
		`gamemesh_matchmaking_queue_size{service="svc-test"} 3`,
		`gamemesh_matchmaking_matches_total{service="svc-test"} 1`,
		`gamemesh_leaderboard_updates_total{service="svc-test"} 1`,
	} {
		assert.Contains(t, body, metric)
	}
	assert.Contains(t, body, `service="svc-test"`)
}
