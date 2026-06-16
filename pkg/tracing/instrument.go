package tracing

import (
	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"github.com/uptrace/opentelemetry-go-extra/otelgorm"
	"gorm.io/gorm"
)

// InstrumentRedis enables tracing on a go-redis client. Every command (queue
// ZADD/ZRANGE, leaderboard ZINCRBY/ZREVRANK, session SET/EXISTS/DEL, pub/sub
// PUBLISH) then emits a child span with db.system=redis, the command name and
// any error. Safe to call when tracing is disabled — spans are non-recording.
//
// It is a no-op-friendly hook: redisotel reads the global tracer provider, so
// the same call works in both enabled and disabled modes.
func InstrumentRedis(rdb *redis.Client) error {
	return redisotel.InstrumentTracing(rdb)
}

// InstrumentGorm installs the uptrace GORM OTel tracing plugin. Each query
// gets a span with the SQL statement, table and error. We use this over the
// first-party gorm.io plugin because the latter transitively pulls the
// ClickHouse/MySQL/SQLServer/quic-go drivers into this Postgres-only binary;
// otelgorm is driver-agnostic with identical span coverage. WithoutQueryVariables
// keeps bound parameters (which can be high-cardinality / PII) out of spans.
func InstrumentGorm(db *gorm.DB) error {
	return db.Use(otelgorm.NewPlugin(otelgorm.WithoutQueryVariables()))
}
