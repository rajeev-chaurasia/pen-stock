package router

import (
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
)

// The config package cannot import this one without a cycle, so the
// strategy names exist in both places. This is what keeps them honest:
// a rename here that misses config would otherwise pass every test and
// fail only when an operator writes a config file.
func TestStrategyNamesMatchConfig(t *testing.T) {
	pairs := []struct {
		router Strategy
		config string
	}{
		{StrategyPriority, config.StrategyPriority},
		{StrategyLeastLatency, config.StrategyLeastLatency},
		{StrategyRoundRobin, config.StrategyRoundRobin},
	}
	for _, p := range pairs {
		if string(p.router) != p.config {
			t.Errorf("router strategy %q does not match config %q", p.router, p.config)
		}
	}

	if len(config.AllStrategies) != len(pairs) {
		t.Errorf("config.AllStrategies has %d entries, this test pins %d", len(config.AllStrategies), len(pairs))
	}
	// Every name config accepts must build a selector, or validation
	// would pass a config the gateway then refuses to start on.
	for _, name := range config.AllStrategies {
		if _, err := NewSelector(Strategy(name)); err != nil {
			t.Errorf("config accepts strategy %q but NewSelector rejects it: %v", name, err)
		}
	}
}
