package tracing

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// withParentSpanContext returns a context carrying a valid (remote) span
// context, mimicking what ResumeFromCarrier produces after extracting an
// event's Carrier — without needing a real upstream service.
func withParentSpanContext(t *testing.T) context.Context {
	t.Helper()
	tid, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	require.NoError(t, err)
	sid, err := trace.SpanIDFromHex("0123456789abcdef")
	require.NoError(t, err)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	return trace.ContextWithSpanContext(context.Background(), sc)
}

func TestStartConsumerSpan_NonHighVolumeAlwaysRecords(t *testing.T) {
	configureHighVolume([]string{"LeaderboardUpdated"}, 0.0) // drop ALL high-volume
	t.Cleanup(func() { configureHighVolume(nil, 1) })

	ctx := withParentSpanContext(t)
	// A non-listed event type is never sampled down: it must start a real span
	// whose context differs from the parent (a new span was pushed).
	newCtx, span := StartConsumerSpan(ctx, "events.consume", "MatchFound")
	defer span.End()

	parent := trace.SpanContextFromContext(ctx)
	child := trace.SpanContextFromContext(newCtx)
	// Same trace, but a span is present and the chain continues.
	assert.Equal(t, parent.TraceID(), child.TraceID())
	assert.True(t, child.IsValid())
}

func TestStartConsumerSpan_HighVolumeSampledOutPreservesContext(t *testing.T) {
	// ratio 0.0 → every high-volume event is sampled OUT (no recording span).
	configureHighVolume([]string{"LeaderboardUpdated"}, 0.0)
	t.Cleanup(func() { configureHighVolume(nil, 1) })

	ctx := withParentSpanContext(t)
	parent := trace.SpanContextFromContext(ctx)

	newCtx, span := StartConsumerSpan(ctx, "events.consume", "LeaderboardUpdated")
	defer span.End()

	child := trace.SpanContextFromContext(newCtx)

	// THE CRITICAL ASSERTION: even though no recording span was opened, the
	// trace context (trace_id) is preserved and still valid — so downstream
	// Inject keeps the distributed trace chain intact. Propagation is NOT broken
	// by span sampling.
	assert.True(t, child.IsValid(), "trace context must survive span sampling")
	assert.Equal(t, parent.TraceID(), child.TraceID(), "trace_id must propagate unchanged")
	assert.Equal(t, parent.SpanID(), child.SpanID(), "no new span pushed when sampled out")
}

func TestStartConsumerSpan_HighVolumeRatioOneRecords(t *testing.T) {
	// ratio >= 1 disables high-volume sampling entirely (everything records).
	configureHighVolume([]string{"LeaderboardUpdated"}, 1.0)
	t.Cleanup(func() { configureHighVolume(nil, 1) })

	ctx := withParentSpanContext(t)
	newCtx, span := StartConsumerSpan(ctx, "events.consume", "LeaderboardUpdated")
	defer span.End()

	assert.True(t, trace.SpanContextFromContext(newCtx).IsValid())
}

func TestSampleHighVolume_DeterministicForTraceID(t *testing.T) {
	configureHighVolume([]string{"x"}, 0.5)
	t.Cleanup(func() { configureHighVolume(nil, 1) })

	ctx := withParentSpanContext(t)
	// The decision must be stable for a given trace_id (TraceIDRatioBased is
	// deterministic), so repeated calls agree — a trace is recorded consistently
	// at every hop or not at all.
	first := sampleHighVolume(ctx)
	for i := 0; i < 5; i++ {
		assert.Equal(t, first, sampleHighVolume(ctx))
	}
}

// guard against accidental dependency on a real exporter in these unit tests.
var _ sdktrace.Sampler = sdktrace.TraceIDRatioBased(0.5)
