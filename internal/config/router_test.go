package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestValidateRouter(t *testing.T) {
	tests := []struct {
		name         string
		router       RouterConfig
		wantContains []string
	}{
		{
			name: "unset takes the defaults and passes",
		},
		{
			name:   "ordinary tuning passes",
			router: RouterConfig{MaxAttempts: 4, RetryBaseDelayMS: 250, MaxRetryDelayMS: 4000, BreakerThreshold: 7, BreakerCooldownSeconds: 90},
		},
		{
			name:         "negative attempts",
			router:       RouterConfig{MaxAttempts: -1},
			wantContains: []string{"router: max_attempts is -1, must not be negative"},
		},
		{
			name:         "negative cooldown",
			router:       RouterConfig{BreakerCooldownSeconds: -30},
			wantContains: []string{"router: breaker_cooldown_seconds is -30, must not be negative"},
		},
		{
			// Each attempt is a real upstream call, so an unbounded budget
			// is a way to turn one client request into a storm against a
			// provider that is already struggling.
			name:         "attempt budget above the ceiling",
			router:       RouterConfig{MaxAttempts: 50},
			wantContains: []string{"router: max_attempts is 50, must be at most 10", "storm"},
		},
		{
			name:         "cooldown longer than an hour",
			router:       RouterConfig{BreakerCooldownSeconds: 7200},
			wantContains: []string{"router: breaker_cooldown_seconds is 7200, must be at most 3600"},
		},
		{
			// Together these two say something neither says alone: the cap
			// would be the only value that ever applied.
			name:         "first backoff step longer than the cap",
			router:       RouterConfig{RetryBaseDelayMS: 5000, MaxRetryDelayMS: 1000},
			wantContains: []string{"retry_base_delay_ms is 5000 but max_retry_delay_ms is 1000"},
		},
		{
			name:   "base delay alone is not compared against an unset cap",
			router: RouterConfig{RetryBaseDelayMS: 5000},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var errs []error
			add := func(format string, args ...any) {
				errs = append(errs, fmt.Errorf(format, args...))
			}
			c := &Config{Router: tc.router}
			c.validateRouter(add)

			joined := errors.Join(errs...)
			if len(tc.wantContains) == 0 {
				if joined != nil {
					t.Fatalf("valid router config was rejected: %v", joined)
				}
				return
			}
			if joined == nil {
				t.Fatalf("router config %+v was accepted, want it rejected", tc.router)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(joined.Error(), want) {
					t.Errorf("error %q does not mention %q", joined, want)
				}
			}
		})
	}
}
