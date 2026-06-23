// Package logger provides structured zap logging with the service name
// attached to every entry, so logs from all services can be aggregated and
// filtered in one place.
package logger

import (
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// SampleConfig tunes the access-log sampler. initial entries (per identical
// message, per second) pass in full; then one in every thereafter passes. It
// only ever thins Info-and-below — Warn/Error/etc. always bypass it (see
// New), so errors and slow requests are never dropped. A zero/negative
// initial disables sampling entirely (every entry passes), which is the
// development default.
type SampleConfig struct {
	Initial    int
	Thereafter int
	Tick       time.Duration
}

// New builds a zap logger. Production gets JSON output for log aggregation;
// development gets a human-readable console encoder. When sample.Initial > 0 the
// returned logger samples high-volume Info logs (see sampledCore) so a flood of
// successful 2xx access logs does not dominate CPU/IO, while still emitting
// every Warn+ entry.
func New(serviceName, env string, sample SampleConfig) (*zap.Logger, error) {
	var cfg zap.Config
	if env == "production" {
		cfg = zap.NewProductionConfig()
	} else {
		cfg = zap.NewDevelopmentConfig()
	}
	cfg.EncoderConfig.TimeKey = "ts"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	opts := []zap.Option{}
	if sample.Initial > 0 {
		tick := sample.Tick
		if tick <= 0 {
			tick = time.Second
		}
		// Wrap the core so only Info-and-below is sampled; Warn and above pass
		// through untouched. zap's own sampler samples ALL levels up to Error, so
		// it cannot be used directly without dropping error logs.
		opts = append(opts, zap.WrapCore(func(core zapcore.Core) zapcore.Core {
			return newSampledCore(core, tick, sample.Initial, sample.Thereafter)
		}))
	}

	return cfg.Build(append(opts, zap.Fields(zap.String("service", serviceName)))...)
}

// Must is a convenience wrapper that panics on logger construction failure —
// a service that cannot log should not start.
func Must(serviceName, env string, sample SampleConfig) *zap.Logger {
	l, err := New(serviceName, env, sample)
	if err != nil {
		panic(err)
	}
	return l
}

// sampledCore routes Info-and-below through a zap sampler and Warn-and-above
// straight to the underlying core. This is the "bypass" the architecture review
// asked for: high-volume successful access logs (Info) are thinned, while the
// rare, valuable Warn/Error lines (errors, slow requests) are never sampled.
type sampledCore struct {
	zapcore.Core              // underlying core: handles Warn and above directly
	sampled      zapcore.Core // sampler-wrapped core: handles Info and below
}

func newSampledCore(core zapcore.Core, tick time.Duration, first, thereafter int) zapcore.Core {
	return &sampledCore{
		Core:    core,
		sampled: zapcore.NewSamplerWithOptions(core, tick, first, thereafter),
	}
}

func (c *sampledCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if ent.Level >= zapcore.WarnLevel {
		return c.Core.Check(ent, ce) // bypass the sampler entirely
	}
	return c.sampled.Check(ent, ce)
}

// With must clone BOTH wrapped cores so contextual fields (e.g. the service
// name) are present whichever path an entry takes.
func (c *sampledCore) With(fields []zapcore.Field) zapcore.Core {
	return &sampledCore{
		Core:    c.Core.With(fields),
		sampled: c.sampled.With(fields),
	}
}
