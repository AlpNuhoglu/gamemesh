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

	// Event-transport instruments. Shared by every transport (RedisBus,
	// NATSBus) so dashboards work regardless of EVENT_BUS. Labelled by topic so
	// matchmaking and leaderboard traffic can be split; result distinguishes
	// ack/nak/error on the consume path.
	EventsPublishedTotal    *prometheus.CounterVec
	EventsConsumedTotal     *prometheus.CounterVec
	EventsFailedTotal       *prometheus.CounterVec
	EventProcessingDuration *prometheus.HistogramVec

	// Presence instruments. OnlinePlayers is a gauge of players whose presence
	// record is non-offline; the rest track the presence write path. (An expired
	// counter is intentionally absent: TTL expiry is passive Redis behaviour with
	// no code path to observe in this milestone.)
	PresenceOnlinePlayers         prometheus.Gauge
	PresenceStateTransitionsTotal *prometheus.CounterVec
	PresenceHeartbeatTotal        prometheus.Counter
	PresenceInvalidTransitions    prometheus.Counter

	// Transactional outbox relay instruments. Pending is a gauge of the current
	// backlog (PENDING rows); the rest track the relay's publish path.
	OutboxEventsPending           prometheus.Gauge
	OutboxEventsPublishedTotal    prometheus.Counter
	OutboxPublishFailuresTotal    prometheus.Counter
	OutboxEventsDeadLetteredTotal prometheus.Counter
	OutboxPublishDuration         prometheus.Histogram
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
		EventsPublishedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "gamemesh_events_published_total",
			Help:        "Total events published to the event bus.",
			ConstLabels: constLabels,
		}, []string{"topic", "type"}),
		EventsConsumedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "gamemesh_events_consumed_total",
			Help:        "Total events successfully consumed and acknowledged.",
			ConstLabels: constLabels,
		}, []string{"topic", "type"}),
		EventsFailedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "gamemesh_events_failed_total",
			Help:        "Total event publish/consume failures.",
			ConstLabels: constLabels,
		}, []string{"topic", "stage"}),
		EventProcessingDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:        "gamemesh_event_processing_duration_seconds",
			Help:        "Handler processing latency for consumed events.",
			ConstLabels: constLabels,
			Buckets:     []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"topic", "type"}),
		PresenceOnlinePlayers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "gamemesh_presence_online_players",
			Help:        "Players currently tracked as non-offline by this presence replica.",
			ConstLabels: constLabels,
		}),
		PresenceStateTransitionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "gamemesh_presence_state_transitions_total",
			Help:        "Total presence state transitions, labelled by from/to state.",
			ConstLabels: constLabels,
		}, []string{"from", "to"}),
		PresenceHeartbeatTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "gamemesh_presence_heartbeat_total",
			Help:        "Total presence heartbeats processed.",
			ConstLabels: constLabels,
		}),
		PresenceInvalidTransitions: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "gamemesh_presence_invalid_transitions_total",
			Help:        "Total presence transitions rejected as invalid (e.g. OFFLINE->IN_MATCH).",
			ConstLabels: constLabels,
		}),
		OutboxEventsPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "gamemesh_outbox_events_pending",
			Help:        "Outbox rows awaiting publication (the relay backlog).",
			ConstLabels: constLabels,
		}),
		OutboxEventsPublishedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "gamemesh_outbox_events_published_total",
			Help:        "Total outbox rows successfully relayed to the event bus.",
			ConstLabels: constLabels,
		}),
		OutboxPublishFailuresTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "gamemesh_outbox_publish_failures_total",
			Help:        "Total outbox publish attempts that failed (row stays PENDING and is retried).",
			ConstLabels: constLabels,
		}),
		OutboxEventsDeadLetteredTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "gamemesh_outbox_events_dead_lettered_total",
			Help:        "Total outbox rows moved to FAILED after exceeding max publish attempts (poison rows; need operator attention).",
			ConstLabels: constLabels,
		}),
		OutboxPublishDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:        "gamemesh_outbox_publish_duration_seconds",
			Help:        "Latency of publishing a single outbox row to the event bus.",
			ConstLabels: constLabels,
			Buckets:     []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}),
	}

	reg.MustRegister(
		m.RequestsTotal, m.ErrorsTotal, m.RequestDuration,
		m.WSConnections, m.MatchmakingQueueSize, m.MatchesCreated, m.LeaderboardUpdates,
		m.EventsPublishedTotal, m.EventsConsumedTotal, m.EventsFailedTotal, m.EventProcessingDuration,
		m.PresenceOnlinePlayers, m.PresenceStateTransitionsTotal, m.PresenceHeartbeatTotal, m.PresenceInvalidTransitions,
		m.OutboxEventsPending, m.OutboxEventsPublishedTotal, m.OutboxPublishFailuresTotal,
		m.OutboxEventsDeadLetteredTotal, m.OutboxPublishDuration,
	)
	return m
}

// Handler returns the /metrics HTTP handler for this service's registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
