// Package obs is the gateway's logging, metrics, and tracing.
//
// Instruments live on a private registry rather than the Prometheus
// default one, so two instances in a process cannot collide on a
// duplicate registration and each test can build its own set. Tracing
// stays a no-op until an OTLP endpoint is configured, so a default
// deployment opens no connection it was not asked for.
//
// The dependency points one way. obs imports the Prometheus and
// OpenTelemetry libraries and satisfies ingress.MetricsSink
// structurally, so the request path never imports either.
package obs

import (
	"fmt"
	"log/slog"
	"os"
)

// Log level names accepted by NewLogger.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// NewLogger returns a JSON logger writing to stderr at the given level.
func NewLogger(level string) (*slog.Logger, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler), nil
}

func parseLevel(level string) (slog.Level, error) {
	switch level {
	case LevelDebug:
		return slog.LevelDebug, nil
	case LevelInfo:
		return slog.LevelInfo, nil
	case LevelWarn:
		return slog.LevelWarn, nil
	case LevelError:
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q", level)
	}
}
