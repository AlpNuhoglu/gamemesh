// Package logger provides structured zap logging with the service name
// attached to every entry, so logs from all services can be aggregated and
// filtered in one place.
package logger

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New builds a zap logger. Production gets JSON output for log aggregation;
// development gets a human-readable console encoder.
func New(serviceName, env string) (*zap.Logger, error) {
	var cfg zap.Config
	if env == "production" {
		cfg = zap.NewProductionConfig()
	} else {
		cfg = zap.NewDevelopmentConfig()
	}
	cfg.EncoderConfig.TimeKey = "ts"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	return cfg.Build(zap.Fields(zap.String("service", serviceName)))
}

// Must is a convenience wrapper that panics on logger construction failure —
// a service that cannot log should not start.
func Must(serviceName, env string) *zap.Logger {
	l, err := New(serviceName, env)
	if err != nil {
		panic(err)
	}
	return l
}
