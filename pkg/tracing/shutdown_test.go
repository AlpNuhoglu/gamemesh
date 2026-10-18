package tracing

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestShutdownIsBoundedWithoutCollector reproduces a pod stopping while no
// collector is reachable: flushing the batched span must give up within
// shutdownTimeout instead of holding the process until the kubelet's SIGKILL.
func TestShutdownIsBoundedWithoutCollector(t *testing.T) {
	prev := shutdownTimeout
	shutdownTimeout = 300 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = prev })

	shutdown, err := Init(context.Background(), Config{
		Enabled:     true,
		ServiceName: "test",
		Endpoint:    "127.0.0.1:1", // nothing listens there
	}, zap.NewNop())
	require.NoError(t, err)

	_, span := Tracer().Start(context.Background(), "pending")
	span.End()

	start := time.Now()
	_ = shutdown(context.Background())
	took := time.Since(start)
	t.Logf("shutdown took %s", took.Round(10*time.Millisecond))
	require.Less(t, took, shutdownTimeout+time.Second)
}
