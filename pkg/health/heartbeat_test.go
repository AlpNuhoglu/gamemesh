package health

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestHeartbeatCheck(t *testing.T) {
	h := NewHeartbeat()
	check := h.Check(30 * time.Second)
	assert.NoError(t, check(context.Background()), "a new heartbeat counts as fresh")

	h.last.Store(time.Now().Add(-time.Minute).UnixNano())
	err := check(context.Background())
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "limit 30s")
	}

	h.Beat()
	assert.NoError(t, check(context.Background()), "a beat makes it fresh again")
}

func TestNilHeartbeatBeatIsNoop(t *testing.T) {
	var h *Heartbeat
	assert.NotPanics(t, h.Beat)
}

func TestStallAfter(t *testing.T) {
	assert.Equal(t, 30*time.Second, StallAfter(time.Second), "floor for fast loops")
	assert.Equal(t, 30*time.Second, StallAfter(5*time.Second))
	assert.Equal(t, 3*time.Minute, StallAfter(time.Minute), "three missed iterations for slow loops")
}
