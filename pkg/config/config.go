// Package config loads service configuration from environment variables.
// Every value has a sane development default so a single service can be run
// locally with zero setup, while production overrides everything via env.
package config

import (
	"errors"
	"fmt"
	"net/url"
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
	// RedisPoolSize caps the go-redis connection pool. 0 (the default) keeps
	// go-redis's own sizing of 10*GOMAXPROCS, which scales the pool with the
	// cores available to the process — deliberate on go-redis's part, and
	// usually what you want, since a smaller pod also serves proportionally less
	// traffic. Worth knowing when tuning: the effective pool therefore differs
	// between a 14-core host (140) and a 2-core CPU limit (20), so set this
	// explicitly if a deployment needs a pool decoupled from its core count.
	RedisPoolSize int

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

	// TrustedProxies lists the proxies (IPs or CIDRs) whose X-Forwarded-For /
	// X-Real-IP headers are believed when resolving the client IP that rate
	// limiting and access logs key on. Empty (the default) trusts no proxy, so
	// the peer address is the client. Behind an ingress, set it to the ingress
	// pods' range (k3s: 10.42.0.0/16), or every client shares one IP.
	TrustedProxies []string

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

// version is the commit the binary was built from, set at link time:
//
//	go build -ldflags "-X github.com/alpnuhoglu/gamemesh/pkg/config.version=<sha>"
//
// The Dockerfile does this from its VERSION build arg. SERVICE_VERSION, when
// set, still takes precedence.
var version = "dev"

// Load reads configuration for the named service from the environment.
func Load(serviceName string) *Config {
	return &Config{
		ServiceName: serviceName,
		Env:         getEnv("APP_ENV", "development"),
		HTTPPort:    getEnv("HTTP_PORT", defaultPort(serviceName)),

		PostgresDSN: getEnv("POSTGRES_DSN",
			"host=localhost user=gamemesh password=gamemesh-dev-password dbname=gamemesh port=5432 sslmode=disable connect_timeout=5"),

		RedisAddr:     getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword: getEnv("REDIS_PASSWORD", ""),
		RedisDB:       getEnvInt("REDIS_DB", 0),
		// 0 hands sizing back to go-redis (10*GOMAXPROCS). Measured at 250/500/
		// 1000 VUs, an explicit 256 changed neither throughput nor p95, and the
		// pool never exceeded 135 connections — so this is a tuning knob for
		// deployments that need it, not a default worth overriding.
		RedisPoolSize: getEnvInt("REDIS_POOL_SIZE", 0),

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

		TrustedProxies: splitCSV(getEnv("TRUSTED_PROXIES", "")),

		AllowedOrigins: splitCSV(getEnv("ALLOWED_ORIGINS", "*")),

		MatchInterval:    time.Duration(getEnvInt("MATCH_INTERVAL_SECONDS", 5)) * time.Second,
		MatchRankWindow:  getEnvInt("MATCH_RANK_WINDOW", 100),
		MatchBatchSize:   int64(getEnvInt("MATCH_BATCH_SIZE", 1000)),
		MatchMaxQueueAge: getEnvDuration("MATCH_MAX_QUEUE_AGE", 5*time.Minute),
		RoomTTL:          getEnvDuration("ROOM_TTL", time.Hour),

		PresenceTTL:               getEnvDuration("PRESENCE_TTL", 45*time.Second),
		PresenceHeartbeatInterval: getEnvDuration("PRESENCE_HEARTBEAT_INTERVAL", 15*time.Second),

		AutoMigrate: getEnvBool("AUTO_MIGRATE", true),

		ServiceVersion: getEnv("SERVICE_VERSION", version),

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

// Need names a secret a service depends on; see Validate.
type Need int

const (
	// NeedJWT is JWT_SECRET, the key tokens are signed and verified with.
	NeedJWT Need = iota
	// NeedPostgres is the password in POSTGRES_DSN.
	NeedPostgres
	// NeedRedis is REDIS_PASSWORD.
	NeedRedis
)

// minSecretLen is the shortest secret Validate accepts in production. The
// production values come from openssl rand -hex 32 (64 characters).
const minSecretLen = 32

// knownDevSecrets are the development defaults long enough to pass the length
// check: the code's own JWT default and the one in .env.example. They are
// public in this repository, so a token signed with them proves nothing.
var knownDevSecrets = map[string]bool{
	"insecure-dev-secret-do-not-use-in-prod": true,
	"change-me-generate-a-real-secret":       true,
}

// Validate refuses to run in production (APP_ENV=production) with a secret
// the service needs that is unset, shorter than minSecretLen or a known
// development default. Without it a production deployment started silently
// with the JWT default from this public repository, and anyone could mint a
// valid token for any player. A service lists only what it uses, so one that
// never talks to Postgres is not failed for the DSN default. Outside
// production it accepts the defaults. The error names the setting, never its
// value.
func (c *Config) Validate(needs ...Need) error {
	if c.Env != "production" {
		return nil
	}
	var errs []error
	for _, n := range needs {
		switch n {
		case NeedJWT:
			errs = append(errs, checkSecret("JWT_SECRET", c.JWTSecret))
		case NeedPostgres:
			errs = append(errs, checkSecret("the password in POSTGRES_DSN", dsnPassword(c.PostgresDSN)))
		case NeedRedis:
			errs = append(errs, checkSecret("REDIS_PASSWORD", c.RedisPassword))
		default:
			errs = append(errs, fmt.Errorf("unknown config.Need %d", n))
		}
	}
	return errors.Join(errs...)
}

func checkSecret(name, value string) error {
	switch {
	case value == "":
		return fmt.Errorf("%s is not set", name)
	case knownDevSecrets[value]:
		return fmt.Errorf("%s is a known development default", name)
	case len(value) < minSecretLen:
		return fmt.Errorf("%s is shorter than %d characters", name, minSecretLen)
	}
	return nil
}

// dsnPassword returns the password of a Postgres DSN in either form pgx
// accepts: key=value ("... password=x ...", value optionally quoted) or URL
// ("postgres://user:x@host/db").
func dsnPassword(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		p, _ := u.User.Password()
		return p
	}
	for _, field := range strings.Fields(dsn) {
		if v, ok := strings.CutPrefix(field, "password="); ok {
			return strings.Trim(v, "'")
		}
	}
	return ""
}
