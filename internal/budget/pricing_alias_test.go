package budget

import (
	"context"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/pricing"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// A route may present one name to callers and ask each provider for a
// different one. Pricing the name the caller used finds nothing in a
// table keyed by vendor and real model, an unpriced request costs zero,
// and a zero cost never trips a USD budget. The failure is silent: the
// gateway keeps serving, the ledger fills with rows worth nothing, and
// the cap the operator configured does not exist.
//
// This was found by running a route called "auto" in front of Groq and
// noticing the ledger said every request was free.
func TestAliasedRouteIsPricedByProviderAndUpstreamModel(t *testing.T) {
	prices, err := pricing.DefaultTable()
	if err != nil {
		t.Fatalf("DefaultTable: %v", err)
	}

	// The operator named the provider "fast", not "groq", so a lookup
	// keyed on either the route name or the provider name would miss.
	g := NewGuard(GuardOptions{
		Prices: prices,
		KindOf: func(provider string) string {
			if provider == "fast" {
				return "groq"
			}
			return ""
		},
	})

	usage := providers.Usage{PromptTokens: 1000, CompletionTokens: 1000}

	priced := g.Price("fast", "llama-3.3-70b-versatile", usage)
	if priced <= 0 {
		t.Fatalf("priced = %v, want a real cost for a model the table knows", priced)
	}

	// The alias must not price. If it did, the table would be answering
	// for a key it should never have, which is worse than a miss.
	if alias := g.Price("fast", "auto", usage); alias != 0 {
		t.Errorf("the routed alias priced at %v, want 0: only the upstream model is in the table", alias)
	}

	// And an unknown provider yields no kind, so nothing prices.
	if unknown := g.Price("nobody", "llama-3.3-70b-versatile", usage); unknown != 0 {
		t.Errorf("unknown provider priced at %v, want 0", unknown)
	}
}

// The same trap at reserve time. An estimate that prices nothing
// reserves nothing, so a USD cap admits every request no matter how
// little budget is left.
func TestEstimateResolvesTheAliasBeforePricing(t *testing.T) {
	prices, err := pricing.DefaultTable()
	if err != nil {
		t.Fatalf("DefaultTable: %v", err)
	}
	resolve := func(model string) (string, string) {
		if model == "auto" {
			return "groq", "llama-3.3-70b-versatile"
		}
		return "", ""
	}
	est := NewEstimator(prices, resolve, EstimatorOptions{DefaultCompletionTokens: 256})

	got := est.Estimate("auto", []byte(`{"model":"auto","messages":[{"role":"user","content":"hello there"}]}`))
	if got.USD <= 0 {
		t.Errorf("estimate USD = %v, want a real reservation for an aliased route", got.USD)
	}
	if got.PromptTokens <= 0 || got.CompletionTokens <= 0 {
		t.Errorf("estimate = %+v, want token counts as well", got)
	}

	// A route the resolver does not know still yields tokens, so token
	// limits keep working even with no price for it.
	unknown := est.Estimate("mystery", []byte(`{"model":"mystery","messages":[{"role":"user","content":"hi"}]}`))
	if unknown.USD != 0 {
		t.Errorf("unknown route priced at %v, want 0 rather than a guess", unknown.USD)
	}
	if unknown.CompletionTokens <= 0 {
		t.Errorf("unknown route estimate = %+v, want tokens still counted", unknown)
	}
}

func TestGuardWithoutPricesStillSettles(t *testing.T) {
	g := NewGuard(GuardOptions{
		Estimator: NewEstimator(nil, nil, EstimatorOptions{}),
		Enforcer:  NewEnforcer(map[TenantID]Limits{"acme": {}}, nil),
	})
	res, err := g.Begin(context.Background(), "acme", "auto", []byte(`{}`))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if usd := g.Settle(context.Background(), res, providers.Usage{PromptTokens: 5}, "auto", "fast"); usd != 0 {
		t.Errorf("usd = %v, want 0 with no price table configured", usd)
	}
}
