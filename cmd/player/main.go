// The player service owns identity: registration, login, profiles. Backed by
// PostgreSQL (GORM) with sessions cached in Redis.
package main

import (
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/redis/go-redis/v9"

	"github.com/alpnuhoglu/gamemesh/internal/player"
	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/config"
	"github.com/alpnuhoglu/gamemesh/pkg/logger"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/server"
)

func main() {
	cfg := config.Load("player")
	log := logger.Must(cfg.ServiceName, cfg.Env)
	defer func() { _ = log.Sync() }()

	db, err := gorm.Open(postgres.Open(cfg.PostgresDSN), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		log.Fatal("failed to connect to postgres", zap.Error(err))
	}
	// SQL migrations under /migrations are the source of truth; AutoMigrate
	// is a dev convenience that keeps `docker compose up` zero-step.
	if cfg.AutoMigrate {
		if err := db.AutoMigrate(&player.Player{}, &player.Stats{}); err != nil {
			log.Fatal("auto-migration failed", zap.Error(err))
		}
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})

	tokens := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTExpiry, cfg.JWTIssuer)
	repo := player.NewRepository(db)
	sessions := player.NewSessionStore(rdb)
	svc := player.NewService(repo, sessions, tokens, log)
	handler := player.NewHandler(svc)

	m := metrics.New(cfg.ServiceName)
	engine := server.NewEngine(cfg, log, m)
	handler.RegisterRoutes(engine)

	if err := server.Run(engine, cfg.HTTPPort, log); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
