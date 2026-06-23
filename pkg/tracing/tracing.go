// Package tracing centralizes OpenTelemetry setup for all five services, the
// same way pkg/server, pkg/logger and pkg/metrics centralize the other
// cross-cutting concerns. A single Init builds the SDK, installs the global
// tracer provider and the W3C propagator, and returns a shutdown func; when
// tracing is disabled it installs a no-op provider so the otel.Tracer(...)
// calls scattered through the code stay free and behaviourally inert.
package tracing

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// tracerName is the instrumentation scope reported on every span we create by
// hand. Library instrumentations (otelgin, redisotel, gorm) use their own.
const tracerName = "github.com/alpnuhoglu/gamemesh"

// Config drives Init. All values come from pkg/config (env-driven).
type Config struct {
	Enabled     bool
	ServiceName string
	Endpoint    string // OTLP/gRPC endpoint, e.g. "otel-collector:4317"
	Env         string // deployment.environment
	Version     string // service.version
	// Sampler selects the head sampling strategy. Supported values:
	// "always_on", "always_off", "traceidratio", "parentbased_always_on"
	// (default), "parentbased_traceidratio". Mirrors the OTEL_TRACES_SAMPLER
	// spec so production can be tuned purely via env.
	Sampler      string
	SamplerRatio float64 // used by the ratio-based samplers

	// HighVolumeEvents lists event types (e.g. "LeaderboardUpdated") whose
	// consumer spans are sampled down to HighVolumeRatio. The trace context is
	// ALWAYS propagated regardless — only the recording span is sampled. See
	// StartConsumerSpan.
	HighVolumeEvents []string
	HighVolumeRatio  float64
}

// Init configures global tracing and returns a shutdown function that flushes
// the exporter. It is safe to call the returned shutdown even when tracing is
// disabled. The W3C propagator is installed in BOTH modes so that trace
// headers are still threaded through (cheap, and lets a disabled hop sit
// transparently between two enabled ones).
func Init(ctx context.Context, cfg Config, log *zap.Logger) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	configureHighVolume(cfg.HighVolumeEvents, cfg.HighVolumeRatio)

	if !cfg.Enabled || cfg.Endpoint == "" {
		// No-op provider: otel.Tracer(...).Start returns non-recording spans.
		log.Info("tracing disabled", zap.Bool("otel_enabled", cfg.Enabled), zap.String("endpoint", cfg.Endpoint))
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithInsecure(), // local/in-cluster plaintext; TLS via env in prod
	)
	if err != nil {
		return nil, fmt.Errorf("create otlp exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.Version),
			semconv.DeploymentEnvironment(cfg.Env),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler(cfg.Sampler, cfg.SamplerRatio)),
	)
	otel.SetTracerProvider(tp)

	log.Info("tracing enabled",
		zap.String("otel_service_name", cfg.ServiceName),
		zap.String("endpoint", cfg.Endpoint),
		zap.String("sampler", cfg.Sampler),
		zap.Float64("sampler_ratio", cfg.SamplerRatio))

	return tp.Shutdown, nil
}

// sampler maps the OTEL_TRACES_SAMPLER value to an SDK sampler. Unknown values
// fall back to the safe production default (parent-based, always-on root).
func sampler(name string, ratio float64) sdktrace.Sampler {
	switch name {
	case "always_on":
		return sdktrace.AlwaysSample()
	case "always_off":
		return sdktrace.NeverSample()
	case "traceidratio":
		return sdktrace.TraceIDRatioBased(ratio)
	case "parentbased_traceidratio":
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
	case "parentbased_always_on", "":
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	default:
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	}
}

// MustInit calls Init and fatals on error. Returns the shutdown func. This is
// the one-liner every cmd/*/main.go uses right after building the logger.
func MustInit(ctx context.Context, cfg Config, log *zap.Logger) func(context.Context) error {
	shutdown, err := Init(ctx, cfg, log)
	if err != nil {
		log.Fatal("tracing init failed", zap.Error(err))
	}
	return shutdown
}

// Tracer returns the shared application tracer. Returns a no-op tracer when
// tracing is disabled, so callers never need to nil-check.
func Tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

// highVolume holds the event-type span-sampling policy. It is written once by
// Init (before any consumer runs) and only read afterwards, so no lock is
// needed. nil ratioSampler means "no high-volume sampling configured".
var (
	highVolumeEvents map[string]struct{}
	highVolumeRatio  sdktrace.Sampler
)

// configureHighVolume records which event types get their consumer spans
// sampled down and at what ratio. A ratio >= 1 or an empty list disables it.
func configureHighVolume(events []string, ratio float64) {
	if len(events) == 0 || ratio >= 1.0 {
		highVolumeEvents = nil
		highVolumeRatio = nil
		return
	}
	set := make(map[string]struct{}, len(events))
	for _, e := range events {
		set[e] = struct{}{}
	}
	highVolumeEvents = set
	highVolumeRatio = sdktrace.TraceIDRatioBased(ratio)
}

// StartConsumerSpan starts a consumer span for a freshly-extracted context,
// applying high-volume sampling by event type.
//
// CRITICAL — propagation vs. sampling: ctx is assumed to already carry the
// upstream trace context (the caller extracts the Carrier first). For a
// high-volume eventType whose trace_id falls outside HighVolumeRatio, this does
// NOT open a new recording span — it returns the context unchanged and the
// span already in context (a non-recording span still carries the SpanContext,
// so any downstream Inject keeps the trace chain intact). Otherwise it starts a
// normal recording span. Either way the returned span is non-nil and safe to
// End()/RecordError().
func StartConsumerSpan(ctx context.Context, name, eventType string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if highVolumeEvents != nil {
		if _, hot := highVolumeEvents[eventType]; hot && !sampleHighVolume(ctx) {
			// Drop the recording span; keep the (propagating) context + span.
			return ctx, trace.SpanFromContext(ctx)
		}
	}
	return Tracer().Start(ctx, name, opts...)
}

// sampleHighVolume returns true when this trace should keep a recording span for
// a high-volume event, using the same deterministic trace_id ratio the SDK uses
// — so a sampled-in trace is recorded consistently at every hop.
func sampleHighVolume(ctx context.Context) bool {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		// No upstream trace context → fall back to recording (rare; lets a
		// locally-rooted trace still be observed).
		return true
	}
	res := highVolumeRatio.ShouldSample(sdktrace.SamplingParameters{
		ParentContext: ctx,
		TraceID:       sc.TraceID(),
		Name:          "events.consume",
	})
	return res.Decision == sdktrace.RecordAndSample
}

// LogFields returns zap fields carrying the active trace/span IDs, or nil when
// no recording span is in context. Used by the request logger and key service
// logs to correlate logs with traces in Jaeger.
func LogFields(ctx context.Context) []zap.Field {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []zap.Field{
		zap.String("trace_id", sc.TraceID().String()),
		zap.String("span_id", sc.SpanID().String()),
	}
}

// RecordError marks the span as errored and records the error. No-op on a nil
// or non-recording span.
func RecordError(span trace.Span, err error) {
	if err == nil || span == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// InjectCarrier writes the active W3C trace context from ctx into carrier (a
// string map suitable for the event's Carrier field), allocating it if nil, and
// returns it. This is the producer half of async trace propagation: every
// transport (Redis, NATS) and the outbox publisher use it so the inject logic
// lives in exactly one place. See docs/outbox.md §8 and pkg/events/events.go.
func InjectCarrier(ctx context.Context, carrier map[string]string) map[string]string {
	if carrier == nil {
		carrier = make(map[string]string)
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(carrier))
	return carrier
}

// ResumeFromCarrier extracts the W3C trace context stored in carrier and returns
// a ctx whose parent is the originating request's span. This is the consumer
// half: the relay (after a DB round-trip) and the bus subscribers use it so a
// span survives the async boundary. A nil/empty carrier yields ctx unchanged.
func ResumeFromCarrier(ctx context.Context, carrier map[string]string) context.Context {
	if len(carrier) == 0 {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(carrier))
}
