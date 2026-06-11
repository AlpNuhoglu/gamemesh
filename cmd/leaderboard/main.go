// The leaderboard service maintains global rankings on a Redis sorted set
// and publishes LeaderboardUpdated events on every score change.
package main

import (
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/leaderboard"
	"github.com/alpnuhoglu/gamemesh/pkg/config"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/logger"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/server"
)

func main() {
	cfg := config.Load("leaderboard")
	log := logger.Must(cfg.ServiceName, cfg.Env)
	defer func() { _ = log.Sync() }()

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})

	m := metrics.New(cfg.ServiceName)
	bus := events.NewRedisBus(rdb, log)
	svc := leaderboard.NewService(leaderboard.NewStore(rdb), bus, m, log)
	handler := leaderboard.NewHandler(svc)

	engine := server.NewEngine(cfg, log, m)
	handler.RegisterRoutes(engine)

	if err := server.Run(engine, cfg.HTTPPort, log); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
