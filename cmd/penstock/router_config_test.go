package main

import (
	"context"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
	"github.com/rajeev-chaurasia/pen-stock/internal/router"
)

// alwaysFails stands in for a provider that is down, so a chain runs to
// whatever attempt budget it was given.
type alwaysFails struct{ name string }

func (p alwaysFails) Name() string { return p.name }

func (p alwaysFails) Chat(context.Context, *providers.ChatRequest) (*providers.ChatResponse, error) {
	return nil, &providers.ProviderError{Provider: p.name, Class: providers.ErrClassUpstream, Message: "down"}
}

func (p alwaysFails) ChatStream(context.Context, *providers.ChatRequest) (providers.StreamReader, error) {
	return nil, &providers.ProviderError{Provider: p.name, Class: providers.ErrClassUpstream, Message: "down"}
}

func threeBrokenProviders() map[string]providers.Provider {
	return map[string]providers.Provider{
		"a": alwaysFails{name: "a"},
		"b": alwaysFails{name: "b"},
		"c": alwaysFails{name: "c"},
	}
}

func chainOfThree(r config.RouterConfig) *config.Config {
	return &config.Config{
		Router: r,
		Routes: []config.RouteConfig{{Model: "auto", Providers: []string{"a", "b", "c"}}},
	}
}

// The gap this closes: the tuning existed on router.Options but nothing
// carried a configured value into it, so every deployment ran on package
// defaults no matter what the file said. Parsing the field is not the
// test worth having; reaching the router is.
func TestConfiguredAttemptBudgetReachesTheRouter(t *testing.T) {
	cfg := chainOfThree(config.RouterConfig{MaxAttempts: 2, RetryBaseDelayMS: 1, MaxRetryDelayMS: 2})

	routes, err := buildRoutes(cfg, threeBrokenProviders())
	if err != nil {
		t.Fatalf("buildRoutes: %v", err)
	}
	routed, ok := routes["auto"].(*router.Router)
	if !ok {
		t.Fatalf("route is %T, want *router.Router", routes["auto"])
	}

	if _, err = routed.Chat(context.Background(), &providers.ChatRequest{Model: "auto"}); err == nil {
		t.Fatal("Chat succeeded against three broken providers")
	}

	// Three providers were available and all fail. Without the budget
	// arriving, the chain would try all three.
	if got := len(routed.Attempts()); got != 2 {
		t.Errorf("attempts = %d, want 2: the configured budget did not reach the router", got)
	}
}

// Zero must keep meaning "take the default" rather than "try nothing",
// so a config that sets none of this behaves as it did before the block
// existed.
func TestUnsetRouterConfigKeepsTheDefaults(t *testing.T) {
	cfg := chainOfThree(config.RouterConfig{})

	routes, err := buildRoutes(cfg, threeBrokenProviders())
	if err != nil {
		t.Fatalf("buildRoutes: %v", err)
	}
	routed := routes["auto"].(*router.Router)

	if _, err = routed.Chat(context.Background(), &providers.ChatRequest{Model: "auto"}); err == nil {
		t.Fatal("Chat succeeded against three broken providers")
	}
	if got := len(routed.Attempts()); got != router.DefaultMaxAttempts {
		t.Errorf("attempts = %d, want the package default %d", got, router.DefaultMaxAttempts)
	}
}

func TestRouterOptionsLeavesZerosForTheRouterToDefault(t *testing.T) {
	// Filling defaults here as well as in the router would give two
	// places an opinion, and they would drift.
	if got := routerOptions(&config.Config{}); got != (router.Options{}) {
		t.Errorf("routerOptions on an empty config = %+v, want the zero value", got)
	}

	got := routerOptions(&config.Config{Router: config.RouterConfig{
		MaxAttempts:            4,
		RetryBaseDelayMS:       250,
		MaxRetryDelayMS:        4000,
		BreakerThreshold:       7,
		BreakerCooldownSeconds: 90,
	}})
	if got.MaxAttempts != 4 || got.BreakerThreshold != 7 {
		t.Errorf("counts did not carry: %+v", got)
	}
	if got.RetryBaseDelay.Milliseconds() != 250 || got.MaxRetryDelay.Milliseconds() != 4000 {
		t.Errorf("millisecond fields did not convert: %+v", got)
	}
	if got.BreakerCooldown.Seconds() != 90 {
		t.Errorf("breaker cooldown did not convert: %v", got.BreakerCooldown)
	}
}
