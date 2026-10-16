package database

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestOpenRetriesThenGivesUp: with nothing listening, Open keeps retrying
// with backoff until maxWait and then reports how many attempts it made.
func TestOpenRetriesThenGivesUp(t *testing.T) {
	obs, logs := observer.New(zapcore.WarnLevel)
	start := time.Now()
	_, err := Open("host=127.0.0.1 port=1 user=x password=x dbname=x sslmode=disable connect_timeout=1",
		2*time.Second, zap.New(obs))
	elapsed := time.Since(start)

	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "postgres unreachable after 3 attempts")
	}
	assert.Equal(t, 2, logs.FilterMessage("postgres not reachable yet, retrying").Len())
	assert.Less(t, elapsed, 3*time.Second, "gives up around maxWait")
}
