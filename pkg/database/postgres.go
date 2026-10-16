// Package database opens the Postgres connection shared by the services that
// own state there (player, outbox-relay).
package database

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Open connects to Postgres through GORM, retrying with a capped exponential
// backoff for up to maxWait. A service started alongside a database that is
// still booting or restarting (Kubernetes has no start order) then waits a
// little instead of crash-looping. Each attempt is bounded by the DSN's
// connect_timeout, so an unreachable host cannot hang an attempt.
func Open(dsn string, maxWait time.Duration, log *zap.Logger) (*gorm.DB, error) {
	deadline := time.Now().Add(maxWait)
	backoff := 500 * time.Millisecond
	for attempt := 1; ; attempt++ {
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
			Logger: gormlogger.Default.LogMode(gormlogger.Silent),
		})
		if err == nil {
			return db, nil
		}
		if time.Now().Add(backoff).After(deadline) {
			return nil, fmt.Errorf("postgres unreachable after %d attempts: %w", attempt, err)
		}
		log.Warn("postgres not reachable yet, retrying",
			zap.Int("attempt", attempt), zap.Duration("backoff", backoff), zap.Error(err))
		time.Sleep(backoff)
		backoff = min(2*backoff, 5*time.Second)
	}
}
