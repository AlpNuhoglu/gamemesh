package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	// No proxy is trusted unless configured: the peer address is the client.
	assert.Empty(t, cfg.TrustedProxies)
	assert.True(t, cfg.AutoMigrate)
	// 0 means "let go-redis size the pool" (10*GOMAXPROCS) — the default must
	// not silently change the client's own behaviour.
	assert.Equal(t, 0, cfg.RedisPoolSize)
}

// TestRedisPoolSizeOptIn pins that the pool is only overridden when explicitly
// configured: unset or malformed leaves go-redis's own sizing in place.
func TestRedisPoolSizeOptIn(t *testing.T) {
	assert.Equal(t, 0, Load("matchmaking").RedisPoolSize)

	t.Setenv("REDIS_POOL_SIZE", "512")
	assert.Equal(t, 512, Load("matchmaking").RedisPoolSize)
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
	t.Setenv("TRUSTED_PROXIES", "10.42.0.0/16, 192.0.2.10")

	cfg := Load("player")
	assert.Equal(t, "9999", cfg.HTTPPort)
	assert.Equal(t, "production", cfg.Env)
	assert.Equal(t, time.Hour, cfg.JWTExpiry)
	assert.Equal(t, 12.5, cfg.RateLimitRPS)
	assert.Equal(t, 7, cfg.RateLimitBurst)
	assert.False(t, cfg.AutoMigrate)
	assert.Equal(t, []string{"https://a.example.com", "https://b.example.com"}, cfg.AllowedOrigins)
	assert.Equal(t, []string{"10.42.0.0/16", "192.0.2.10"}, cfg.TrustedProxies)
}

func TestMalformedEnvFallsBackToDefaults(t *testing.T) {
	t.Setenv("RATE_LIMIT_BURST", "not-a-number")
	t.Setenv("JWT_EXPIRY", "not-a-duration")
	t.Setenv("RATE_LIMIT_RPS", "NaNish")
	t.Setenv("AUTO_MIGRATE", "maybe")
	t.Setenv("REDIS_POOL_SIZE", "lots")

	cfg := Load("gateway")
	assert.Equal(t, 100, cfg.RateLimitBurst)
	assert.Equal(t, 24*time.Hour, cfg.JWTExpiry)
	assert.Equal(t, float64(50), cfg.RateLimitRPS)
	assert.True(t, cfg.AutoMigrate)
	assert.Equal(t, 0, cfg.RedisPoolSize)
}

// TestServiceVersionFromBuild: the version baked in at link time is the
// default, and SERVICE_VERSION still overrides it.
func TestServiceVersionFromBuild(t *testing.T) {
	prev := version
	version = "abc1234"
	t.Cleanup(func() { version = prev })

	assert.Equal(t, "abc1234", Load("gateway").ServiceVersion)

	t.Setenv("SERVICE_VERSION", "override")
	assert.Equal(t, "override", Load("gateway").ServiceVersion)
}

// strongSecret is long enough and not a known default. Built at run time: as
// a literal assigned to a name with "secret" in it, it matched gitleaks'
// generic-api-key rule (see .gitleaksignore for the commit that had it).
var strongSecret = strings.Repeat("0123456789abcdef", 4)

func TestValidateAcceptsDefaultsOutsideProduction(t *testing.T) {
	cfg := Load("player")
	assert.NoError(t, cfg.Validate(NeedJWT, NeedPostgres, NeedRedis))
}

// TestValidateRefusesWeakSecretsInProduction: each weak shape is refused with
// the setting's name, and the message never contains the value. An empty
// JWT_SECRET falls back to the code's default, so "not set" shows on
// REDIS_PASSWORD, whose default is empty.
func TestValidateRefusesWeakSecretsInProduction(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	for name, tc := range map[string]struct {
		env  map[string]string
		need Need
		want string
	}{
		"JWT default":    {map[string]string{}, NeedJWT, "JWT_SECRET is a known development default"},
		"JWT empty":      {map[string]string{"JWT_SECRET": ""}, NeedJWT, "JWT_SECRET is a known development default"},
		"JWT too short":  {map[string]string{"JWT_SECRET": "short-secret"}, NeedJWT, "JWT_SECRET is shorter than 32 characters"},
		"Redis not set":  {map[string]string{}, NeedRedis, "REDIS_PASSWORD is not set"},
		"Postgres short": {map[string]string{"POSTGRES_DSN": "host=db password=short-password"}, NeedPostgres, "the password in POSTGRES_DSN is shorter than 32 characters"},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg := Load("gateway")
			err := cfg.Validate(tc.need)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			for _, secret := range []string{cfg.JWTSecret, cfg.RedisPassword, dsnPassword(cfg.PostgresDSN)} {
				if secret != "" {
					assert.NotContains(t, err.Error(), secret)
				}
			}
		})
	}
}

func TestValidateChecksOnlyWhatTheServiceNeeds(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("REDIS_PASSWORD", strongSecret)
	// leaderboard never talks to Postgres or signs tokens: the DSN and JWT
	// defaults do not concern it.
	assert.NoError(t, Load("leaderboard").Validate(NeedRedis))

	err := Load("player").Validate(NeedJWT, NeedPostgres, NeedRedis)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JWT_SECRET")
	assert.Contains(t, err.Error(), "the password in POSTGRES_DSN")
	assert.NotContains(t, err.Error(), "REDIS_PASSWORD")
}

func TestValidateAcceptsStrongSecretsInProduction(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("JWT_SECRET", strongSecret)
	t.Setenv("REDIS_PASSWORD", strongSecret)
	t.Setenv("POSTGRES_DSN", "host=postgres user=gamemesh password="+strongSecret+" dbname=gamemesh sslmode=disable connect_timeout=5")
	assert.NoError(t, Load("player").Validate(NeedJWT, NeedPostgres, NeedRedis))
}

func TestDSNPassword(t *testing.T) {
	assert.Equal(t, "s3cret", dsnPassword("host=db user=u password=s3cret dbname=x"))
	assert.Equal(t, "s3cret", dsnPassword("host=db password='s3cret' dbname=x"))
	assert.Equal(t, "s3cret", dsnPassword("postgres://u:s3cret@db:5432/x?sslmode=disable"))
	assert.Equal(t, "", dsnPassword("host=db user=u dbname=x"))
}
