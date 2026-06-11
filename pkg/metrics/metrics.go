// Package metrics defines the Prometheus instruments shared by all services.
// Each service gets its own registry (rather than the global default) so unit
// tests can construct multiple instances without duplicate-registration
// panics, and so only intentional metrics are exposed.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics bundles every instrument a service may use. Gauges/counters that a
// given service never touches simply stay at zero and cost nothing.
type Metrics struct {
	registry *prometheus.Registry

	RequestsTotal   *prometheus.CounterVec
	ErrorsTotal     *prometheus.CounterVec
	RequestDuration *prometheus.HistogramVec

	WSConnections        prometheus.Gauge
	MatchmakingQueueSize prometheus.Gauge
	MatchesCreated       prometheus.Counter
	LeaderboardUpdates   prometheus.Counter
}

// New creates and registers all instruments, labelled with the service name
// as a const label so dashboards can aggregate or split per service.
func New(service string) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	constLabels := prometheus.Labels{"service": service}

	m := &Metrics{
		registry: reg,
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "gamemesh_http_requests_total",
			Help:        "Total HTTP requests processed.",
			ConstLabels: constLabels,
		}, []string{"method", "path", "status"}),
		ErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "gamemesh_http_errors_total",
			Help:        "Total HTTP responses with status >= 400.",
			ConstLabels: constLabels,
		}, []string{"method", "path", "status"}),
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:        "gamemesh_http_request_duration_seconds",
			Help:        "HTTP request latency.",
			ConstLabels: constLabels,
			Buckets:     []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"method", "path"}),
		WSConnections: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "gamemesh_websocket_active_connections",
			Help:        "Currently open WebSocket connections.",
			ConstLabels: constLabels,
		}),
		MatchmakingQueueSize: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "gamemesh_matchmaking_queue_size",
			Help:        "Players currently waiting in the matchmaking queue.",
			ConstLabels: constLabels,
		}),
		MatchesCreated: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "gamemesh_matchmaking_matches_total",
			Help:        "Total matches created.",
			ConstLabels: constLabels,
		}),
		LeaderboardUpdates: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "gamemesh_leaderboard_updates_total",
			Help:        "Total leaderboard score updates.",
			ConstLabels: constLabels,
		}),
	}

	reg.MustRegister(
		m.RequestsTotal, m.ErrorsTotal, m.RequestDuration,
		m.WSConnections, m.MatchmakingQueueSize, m.MatchesCreated, m.LeaderboardUpdates,
	)
	return m
}

// Handler returns the /metrics HTTP handler for this service's registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
