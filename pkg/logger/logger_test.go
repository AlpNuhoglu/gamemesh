package logger

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestNewDevelopmentAndProduction(t *testing.T) {
	dev, err := New("test", "development", SampleConfig{})
	require.NoError(t, err)
	assert.NotNil(t, dev)

	prod, err := New("test", "production", SampleConfig{Initial: 100, Thereafter: 100})
	require.NoError(t, err)
	assert.NotNil(t, prod)
}

func TestMust(t *testing.T) {
	assert.NotPanics(t, func() { Must("test", "development", SampleConfig{}) })
}

// TestSampledCoreThinsInfoButNeverDropsWarnAndAbove is the critical guarantee:
// the sampler must thin high-volume Info logs while letting EVERY Warn/Error
// through, so errors and slow requests are never lost.
func TestSampledCoreThinsInfoButNeverDropsWarnAndAbove(t *testing.T) {
	obs, logs := observer.New(zapcore.DebugLevel)
	// first=2, thereafter=0 → after the first 2 identical entries per tick, the
	// rest are dropped (thereafter==0 drops all beyond first).
	core := newSampledCore(obs, time.Minute, 2, 0)
	log := zap.New(core)

	// 10 identical Info messages: only the first 2 should survive sampling.
	for i := 0; i < 10; i++ {
		log.Info("same info message")
	}
	// 10 identical Error messages: ALL must survive (sampler bypassed).
	for i := 0; i < 10; i++ {
		log.Error("same error message")
	}
	// 10 identical Warn messages: ALL must survive too.
	for i := 0; i < 10; i++ {
		log.Warn("same warn message")
	}

	assert.Equal(t, 2, logs.FilterMessage("same info message").Len(), "Info must be sampled")
	assert.Equal(t, 10, logs.FilterMessage("same error message").Len(), "Error must never be sampled")
	assert.Equal(t, 10, logs.FilterMessage("same warn message").Len(), "Warn must never be sampled")
}

// TestSampleDisabledLogsEverything verifies that Initial<=0 leaves all logs
// untouched (the development default).
func TestSampleDisabledLogsEverything(t *testing.T) {
	obs, logs := observer.New(zapcore.DebugLevel)
	log := zap.New(obs) // no sampling wrapper
	for i := 0; i < 10; i++ {
		log.Info("noisy")
	}
	assert.Equal(t, 10, logs.Len())
}
