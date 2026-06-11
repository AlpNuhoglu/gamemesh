package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestLoadDefaults(t *testing.T) {
	cfg := Load("gateway")

	assert.Equal(t, "gateway", cfg.ServiceName)
	assert.Equal(t, "development", cfg.Env)
	assert.Equal(t, "8080", cfg.HTTPPort)
	assert.Equal(t, "localhost:6379", cfg.RedisAddr)
	assert.Equal(t, 24*time.Hour, cfg.JWTExpiry)
	assert.Equal(t, "gamemesh", cfg.JWTIssuer)
	assert.Equal(t, 5*time.Second, cfg.MatchInterval)
	assert.Equal(t, 100, cfg.MatchRankWindow)
	assert.Equal(t, []string{"*"}, cfg.AllowedOrigins)
	assert.True(t, cfg.AutoMigrate)
}

func TestDefaultPortsPerService(t *testing.T) {
	for svc, port := range map[string]string{
		"gateway":     "8080",
		"player":      "8081",
		"matchmaking": "8082",
		"leaderboard": "8083",
		"websocket":   "8084",
		"unknown":     "8080",
	} {
		assert.Equal(t, port, Load(svc).HTTPPort, svc)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("HTTP_PORT", "9999")
	t.Setenv("APP_ENV", "production")
	t.Setenv("JWT_EXPIRY", "1h")
	t.Setenv("RATE_LIMIT_RPS", "12.5")
	t.Setenv("RATE_LIMIT_BURST", "7")
	t.Setenv("AUTO_MIGRATE", "false")
	t.Setenv("ALLOWED_ORIGINS", "https://a.example.com, https://b.example.com")

	cfg := Load("player")
	assert.Equal(t, "9999", cfg.HTTPPort)
	assert.Equal(t, "production", cfg.Env)
	assert.Equal(t, time.Hour, cfg.JWTExpiry)
	assert.Equal(t, 12.5, cfg.RateLimitRPS)
	assert.Equal(t, 7, cfg.RateLimitBurst)
	assert.False(t, cfg.AutoMigrate)
	assert.Equal(t, []string{"https://a.example.com", "https://b.example.com"}, cfg.AllowedOrigins)
}

func TestMalformedEnvFallsBackToDefaults(t *testing.T) {
	t.Setenv("RATE_LIMIT_BURST", "not-a-number")
	t.Setenv("JWT_EXPIRY", "not-a-duration")
	t.Setenv("RATE_LIMIT_RPS", "NaNish")
	t.Setenv("AUTO_MIGRATE", "maybe")

	cfg := Load("gateway")
	assert.Equal(t, 100, cfg.RateLimitBurst)
	assert.Equal(t, 24*time.Hour, cfg.JWTExpiry)
	assert.Equal(t, float64(50), cfg.RateLimitRPS)
	assert.True(t, cfg.AutoMigrate)
}
