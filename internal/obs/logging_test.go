package obs

import (
	"context"
	"log/slog"
	"testing"
)

func TestNewLogger(t *testing.T) {
	tests := []struct {
		name       string
		level      string
		wantErr    bool
		enabled    slog.Level
		suppressed slog.Level
	}{
		{name: "debug", level: "debug", enabled: slog.LevelDebug, suppressed: slog.LevelDebug - 1},
		{name: "info", level: "info", enabled: slog.LevelInfo, suppressed: slog.LevelDebug},
		{name: "warn", level: "warn", enabled: slog.LevelWarn, suppressed: slog.LevelInfo},
		{name: "error", level: "error", enabled: slog.LevelError, suppressed: slog.LevelWarn},
		{name: "unknown level", level: "verbose", wantErr: true},
		{name: "empty level", level: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, err := NewLogger(tt.level)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NewLogger(%q) expected error, got nil", tt.level)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewLogger(%q) unexpected error: %v", tt.level, err)
			}
			ctx := context.Background()
			if !logger.Enabled(ctx, tt.enabled) {
				t.Errorf("level %v should be enabled", tt.enabled)
			}
			if logger.Enabled(ctx, tt.suppressed) {
				t.Errorf("level %v should be suppressed", tt.suppressed)
			}
		})
	}
}
