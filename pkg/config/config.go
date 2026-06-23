// Package config loads service configuration from environment variables.
// Every value has a sane development default so a single service can be run
// locally with zero setup, while production overrides everything via env.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration shared across services. Services
// only read the fields they need; keeping one struct avoids config drift
// between five small services.
type Config struct {
	ServiceName string
	Env         string
	HTTPPort    string

	PostgresDSN string

	RedisAddr     string
	RedisPassword string
	RedisDB       int

	// Event transport selection. EventBus picks the messaging implementation
	// ("redis" or "nats") at startup; services keep depending only on the
	// events.Publisher/Subscriber interfaces, never on the concrete transport.
	EventBus     string
	NATSURL      string
	EventWorkers int

	// Transactional outbox relay tuning. The relay polls outbox_events and
	// publishes committed rows to NATS. OutboxEnabled gates the relay loop;
	// the player service always writes to the outbox regardless.
	OutboxEnabled      bool
	OutboxBatchSize    int
	OutboxPollInterval time.Duration
	OutboxWorkers      int
	// OutboxMaxAttempts dead-letters a row (status FAILED) once its publish
	// attempts reach this count, so a poison row stops being retried forever.
	// 0 (the default) disables dead-lettering: rows retry indefinitely.
	OutboxMaxAttempts int

	JWTSecret string
	JWTExpiry time.Duration
	JWTIssuer string

	PlayerServiceURL      string
	MatchmakingServiceURL string
	LeaderboardServiceURL string
	WebsocketServiceURL   string

	RateLimitRPS   float64
	RateLimitBurst int

	AllowedOrigins []string

	MatchInterval    time.Duration
	MatchRankWindow  int
	MatchBatchSize   int64
	MatchMaxQueueAge time.Duration
	RoomTTL          time.Duration

	// Presence tuning. PresenceTTL is the expiry on presence:{id}; if heartbeats
	// stop for longer, the key vanishes and the player is OFFLINE. The heartbeat
	// interval is advisory for WS replicas (the source of heartbeats) and kept at
	// ~1/3 of the TTL so two missed beats still leave the player online.
	PresenceServiceURL        string
	PresenceTTL               time.Duration
	PresenceHeartbeatInterval time.Duration

	AutoMigrate bool

	ServiceVersion string

	OTelEnabled      bool
	OTelServiceName  string
	OTelEndpoint     string
	OTelSampler      string
	OTelSamplerRatio float64

	// Observability sampling. Logs and traces are both observability load, so
	// their sampling knobs live together. Logger sampling thins the flood of
	// successful/fast 2xx access logs (errors and slow requests always bypass the
	// sampler); high-volume trace sampling drops most consumer spans for chatty
	// event types while ALWAYS keeping the trace-context propagation intact.
	LogSampleInitial           int           // first N identical entries/sec logged in full
	LogSampleThereafter        int           // then 1 in M of the rest
	LogSlowRequestThreshold    time.Duration // requests slower than this always log
	TraceHighVolumeEvents      []string      // event types whose consumer spans are sampled down
	TraceHighVolumeSampleRatio float64       // keep-ratio for those high-volume spans

	// Session revocation cache. The gateway checks the session store on every
	// authenticated request to honour server-side logout; an in-process positive
	// cache keeps that off the Redis hot path. SessionCacheTTL is how long a
	// "valid" verdict is trusted before re-checking Redis — i.e. the worst-case
	// window a revoked token stays usable (eventual consistency, accepted).
	SessionCacheEnabled bool
	SessionCacheTTL     time.Duration
}

// Load reads configuration for the named service from the environment.
func Load(serviceName string) *Config {
	return &Config{
		ServiceName: serviceName,
		Env:         getEnv("APP_ENV", "development"),
		HTTPPort:    getEnv("HTTP_PORT", defaultPort(serviceName)),

		PostgresDSN: getEnv("POSTGRES_DSN",
			"host=localhost user=gamemesh password=gamemesh-dev-password dbname=gamemesh port=5432 sslmode=disable"),

		RedisAddr:     getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword: getEnv("REDIS_PASSWORD", ""),
		RedisDB:       getEnvInt("REDIS_DB", 0),

		// Default to redis so a bare `go run` of a single service keeps working
		// without NATS; docker-compose sets EVENT_BUS=nats explicitly.
		EventBus:     getEnv("EVENT_BUS", "redis"),
		NATSURL:      getEnv("NATS_URL", "nats://localhost:4222"),
		EventWorkers: getEnvInt("EVENT_WORKERS", 8),

		OutboxEnabled:      getEnvBool("OUTBOX_ENABLED", true),
		OutboxBatchSize:    getEnvInt("OUTBOX_BATCH_SIZE", 100),
		OutboxPollInterval: getEnvDuration("OUTBOX_POLL_INTERVAL", time.Second),
		OutboxWorkers:      getEnvInt("OUTBOX_WORKERS", 4),
		OutboxMaxAttempts:  getEnvInt("OUTBOX_MAX_ATTEMPTS", 0),

		JWTSecret: getEnv("JWT_SECRET", "insecure-dev-secret-do-not-use-in-prod"),
		JWTExpiry: getEnvDuration("JWT_EXPIRY", 24*time.Hour),
		JWTIssuer: getEnv("JWT_ISSUER", "gamemesh"),

		PlayerServiceURL:      getEnv("PLAYER_SERVICE_URL", "http://localhost:8081"),
		MatchmakingServiceURL: getEnv("MATCHMAKING_SERVICE_URL", "http://localhost:8082"),
		LeaderboardServiceURL: getEnv("LEADERBOARD_SERVICE_URL", "http://localhost:8083"),
		WebsocketServiceURL:   getEnv("WEBSOCKET_SERVICE_URL", "http://localhost:8084"),
		PresenceServiceURL:    getEnv("PRESENCE_SERVICE_URL", "http://localhost:8086"),

		RateLimitRPS:   getEnvFloat("RATE_LIMIT_RPS", 50),
		RateLimitBurst: getEnvInt("RATE_LIMIT_BURST", 100),

		AllowedOrigins: splitCSV(getEnv("ALLOWED_ORIGINS", "*")),

		MatchInterval:    time.Duration(getEnvInt("MATCH_INTERVAL_SECONDS", 5)) * time.Second,
		MatchRankWindow:  getEnvInt("MATCH_RANK_WINDOW", 100),
		MatchBatchSize:   int64(getEnvInt("MATCH_BATCH_SIZE", 1000)),
		MatchMaxQueueAge: getEnvDuration("MATCH_MAX_QUEUE_AGE", 5*time.Minute),
		RoomTTL:          getEnvDuration("ROOM_TTL", time.Hour),

		PresenceTTL:               getEnvDuration("PRESENCE_TTL", 45*time.Second),
		PresenceHeartbeatInterval: getEnvDuration("PRESENCE_HEARTBEAT_INTERVAL", 15*time.Second),

		AutoMigrate: getEnvBool("AUTO_MIGRATE", true),

		ServiceVersion: getEnv("SERVICE_VERSION", "dev"),

		// Distributed tracing. Defaults target a local OTel Collector
		// (docker-compose) and always-sample so every dev request is visible.
		// OTEL_SERVICE_NAME falls back to the service's own name so traces are
		// correctly attributed without any extra config.
		OTelEnabled:      getEnvBool("OTEL_ENABLED", true),
		OTelServiceName:  getEnv("OTEL_SERVICE_NAME", serviceName),
		OTelEndpoint:     getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317"),
		OTelSampler:      getEnv("OTEL_TRACES_SAMPLER", "parentbased_always_on"),
		OTelSamplerRatio: getEnvFloat("OTEL_TRACES_SAMPLER_ARG", 1.0),

		// Observability sampling (logs + traces, kept side by side).
		LogSampleInitial:           getEnvInt("LOG_SAMPLE_INITIAL", 100),
		LogSampleThereafter:        getEnvInt("LOG_SAMPLE_THEREAFTER", 100),
		LogSlowRequestThreshold:    getEnvDuration("LOG_SLOW_REQUEST_THRESHOLD", time.Second),
		TraceHighVolumeEvents:      splitCSV(getEnv("TRACE_HIGHVOLUME_EVENTS", "LeaderboardUpdated")),
		TraceHighVolumeSampleRatio: getEnvFloat("TRACE_HIGHVOLUME_SAMPLE_RATIO", 0.01),

		SessionCacheEnabled: getEnvBool("SESSION_CACHE_ENABLED", true),
		SessionCacheTTL:     getEnvDuration("SESSION_CACHE_TTL", 5*time.Second),
	}
}

func defaultPort(serviceName string) string {
	switch serviceName {
	case "gateway":
		return "8080"
	case "player":
		return "8081"
	case "matchmaking":
		return "8082"
	case "leaderboard":
		return "8083"
	case "websocket":
		return "8084"
	case "outbox-relay":
		return "8085"
	case "presence":
		return "8086"
	default:
		return "8080"
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
