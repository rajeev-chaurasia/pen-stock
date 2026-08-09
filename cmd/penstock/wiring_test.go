package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/rajeev-chaurasia/pen-stock/internal/budget"
	"github.com/rajeev-chaurasia/pen-stock/internal/cache"
	"github.com/rajeev-chaurasia/pen-stock/internal/config"
	"github.com/rajeev-chaurasia/pen-stock/internal/ingress"
	"github.com/rajeev-chaurasia/pen-stock/internal/obs"
	"github.com/rajeev-chaurasia/pen-stock/internal/pricing"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// tenantConfig is one billable tenant in front of one aliased route,
// which is the shape every accounting test here needs: the route is
// called "auto" and the provider is called "primary", so neither name
// appears in the price table and only the resolved pair does.
func tenantConfig() *config.Config {
	return &config.Config{
		Auth: config.AuthConfig{Tenants: []config.TenantConfig{{
			Name: "acme",
			Keys: []string{"acme-key-0123456789abcdef"},
			Limits: config.TenantLimits{
				RequestsPerMinute: 60,
				DailyUSD:          5,
				MonthlyUSD:        50,
			},
		}}},
		Providers: []config.ProviderConfig{{
			Name:    "primary",
			Kind:    config.KindOpenAI,
			BaseURL: "http://127.0.0.1:1/v1",
			APIKey:  "unused-in-these-tests",
		}},
		Routes: []config.RouteConfig{{
			Model:          "auto",
			Provider:       "primary",
			ProviderModels: map[string]string{"primary": "gpt-4o-mini"},
		}},
	}
}

// A gateway without budgeting still has to serve. Returning an error
// here would make every deployment that declares no tenant refuse to
// start, and returning a non-nil accounting would put an enforcer with
// no limits on the request path.
func TestNoTenantsMeansNoAccountingAndTheNilIsSafe(t *testing.T) {
	cfg := tenantConfig()
	cfg.Auth.Tenants = nil

	acct, err := buildAccounting(cfg, discardLogger())
	if err != nil {
		t.Fatalf("buildAccounting: %v", err)
	}
	if acct != nil {
		t.Fatalf("accounting = %+v, want nil when no tenant declares a limit", acct)
	}

	// main calls all three of these on whatever buildAccounting returned,
	// so the nil has to answer them rather than panic on the path every
	// budget-less deployment takes.
	acct.shutdown()
	if got := acct.requestPath(); got != nil {
		t.Errorf("requestPath on nil accounting = %v, want nil so the ingress meters nothing", got)
	}
	if got := acct.adminHandler(); got != nil {
		t.Errorf("adminHandler on nil accounting = %v, want nil so nothing is mounted", got)
	}
}

// openAccounting builds accounting and registers the close first, so the
// SQLite file is shut before t.TempDir removes the directory. On Windows
// that removal fails while the file is open, and the error names the
// directory rather than the test that leaked it.
func openAccounting(t *testing.T, cfg *config.Config) *accounting {
	t.Helper()
	acct, err := buildAccounting(cfg, discardLogger())
	if err != nil {
		t.Fatalf("buildAccounting: %v", err)
	}
	t.Cleanup(acct.shutdown)
	return acct
}

// settleOnce puts a known amount of real spend through the guard, which
// is the only path that touches the estimator, the price table and the
// enforcer together.
func settleOnce(t *testing.T, acct *accounting) float64 {
	t.Helper()
	ctx := context.Background()
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"hello there"}]}`)

	res, err := acct.requestPath().Begin(ctx, "acme", "auto", body)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if res == nil {
		t.Fatal("Begin returned no reservation, so budgeting is not wired")
	}
	usd := acct.requestPath().Settle(ctx, res,
		providers.Usage{PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000},
		"gpt-4o-mini", "primary")
	if usd <= 0 {
		t.Fatalf("settled at %v USD, want a real cost: the price table was not reached", usd)
	}
	return usd
}

// The failure this prevents: a store that looks configured but is not.
// Spend counters that live only in memory reset to zero on every
// restart, so a daily cap is forgiven as often as the process is
// bounced, and nothing about that is visible from outside.
func TestDurableStoreRestoresSpendAcrossARebuild(t *testing.T) {
	dir := t.TempDir()
	cfg := tenantConfig()
	cfg.Accounting.StorePath = filepath.Join(dir, "budget.db")

	first := openAccounting(t, cfg)
	if _, err := os.Stat(cfg.Accounting.StorePath); err != nil {
		t.Fatalf("the configured store file was not created: %v", err)
	}

	usd := settleOnce(t, first)
	if daily, _ := first.enforcer.Spent("acme"); daily != usd {
		t.Fatalf("daily spend = %v immediately after settling %v", daily, usd)
	}
	first.shutdown()

	// A restart, as far as the store is concerned.
	second := openAccounting(t, cfg)
	daily, monthly := second.enforcer.Spent("acme")
	// Exact equality on purpose. The store round trips IEEE-754 bits, so
	// an epsilon comparison here would hide a path that rounds money.
	if daily != usd || monthly != usd {
		t.Errorf("after a rebuild daily = %v monthly = %v, want exactly %v carried over the restart", daily, monthly, usd)
	}
}

// Without a store the counters are supposed to be forgotten, so the
// memory-only path is pinned too: otherwise the test above could pass
// against a build that persisted regardless of configuration and nobody
// would know which behaviour was being exercised.
func TestWithoutAStoreSpendDoesNotSurviveARebuild(t *testing.T) {
	cfg := tenantConfig()

	first := openAccounting(t, cfg)
	settleOnce(t, first)
	first.shutdown()

	second := openAccounting(t, cfg)
	if daily, _ := second.enforcer.Spent("acme"); daily != 0 {
		t.Errorf("daily spend = %v with no store_path configured, want 0", daily)
	}
}

// A store that cannot be opened must stop the gateway. Starting anyway
// would begin the day with every tenant's spend at zero, which looks
// exactly like a cap that is working right up until the invoice arrives.
func TestABadStorePathFailsTheBuild(t *testing.T) {
	dir := t.TempDir()
	cfg := tenantConfig()
	cfg.Accounting.LedgerPath = filepath.Join(dir, "ledger.jsonl")
	cfg.Accounting.StorePath = filepath.Join(dir, "no-such-directory", "budget.db")

	acct, err := buildAccounting(cfg, discardLogger())
	if err == nil {
		acct.shutdown()
		t.Fatal("buildAccounting accepted a store path it cannot open")
	}
	if acct != nil {
		t.Errorf("accounting = %+v alongside an error, want nil", acct)
	}
	if !strings.Contains(err.Error(), "budget store") {
		t.Errorf("error = %v, want it to name the budget store so an operator knows what to fix", err)
	}

	// The ledger opened before the store did, and the error path has to
	// hand that file back.
	//
	// Worth knowing before trusting this line: it only bites on Windows,
	// where an open file cannot be unlinked. On Linux, and so on CI, the
	// remove succeeds whether or not the handle leaked, and this passes
	// vacuously. It is kept because it costs nothing and does catch the
	// leak on the machine this is developed on, but it is not a gate.
	if err := os.Remove(cfg.Accounting.LedgerPath); err != nil {
		t.Errorf("removing the ledger failed, so it was left open on the error path: %v", err)
	}
}

// A ledger path that cannot be opened is likewise a startup failure: an
// audit trail nobody can write is a hole in the record of what the
// gateway spent.
func TestABadLedgerPathFailsTheBuild(t *testing.T) {
	cfg := tenantConfig()
	cfg.Accounting.LedgerPath = filepath.Join(t.TempDir(), "no-such-directory", "ledger.jsonl")

	acct, err := buildAccounting(cfg, discardLogger())
	if err == nil {
		acct.shutdown()
		t.Fatal("buildAccounting accepted a ledger path it cannot open")
	}
	if !strings.Contains(err.Error(), "ledger") {
		t.Errorf("error = %v, want it to name the ledger", err)
	}
}

// The admin API and the request path must read and write one set of
// counters. Two enforcers would report balances nobody is enforcing.
func TestTheAdminHandlerReadsTheEnforcerTheRequestPathWrites(t *testing.T) {
	acct := openAccounting(t, tenantConfig())
	if acct.adminHandler() == nil {
		t.Fatal("adminHandler = nil for a config that declares a tenant")
	}

	usd := settleOnce(t, acct)
	daily, _ := acct.enforcer.Spent("acme")
	if daily != usd {
		t.Errorf("the enforcer behind the admin API reports %v, want the %v the request path settled", daily, usd)
	}
}

// The nil this asserts is load bearing. The ingress calls Get and Put on
// whatever buildCache returns, without a nil check at either call site,
// so a disabled cache that returned anything other than a usable nil
// would panic on the first request of every deployment that leaves
// caching off, which is the default.
func TestCacheOffReturnsANilLookupTheRequestPathCanStillCall(t *testing.T) {
	cfg := tenantConfig()
	got := buildCache(cfg, obs.NewMetrics(), discardLogger())
	if got != nil {
		t.Fatalf("buildCache with caching off = %v, want nil", got)
	}

	ctx := context.Background()
	raw := []byte(`{"model":"auto","temperature":0,"messages":[{"role":"user","content":"hi"}]}`)
	res := got.Get(ctx, "acme", "auto", raw)
	if res.Entry != nil || res.Eligible {
		t.Errorf("a disabled cache answered %+v, want an empty result", res)
	}
	got.Put(ctx, res, &cache.Entry{Body: []byte("stored")}, raw)
	if got.Enabled() {
		t.Error("a disabled cache reports itself enabled")
	}
}

func TestCacheOnBuildsTheExactTierAndReportsItsEventsToMetrics(t *testing.T) {
	cfg := tenantConfig()
	cfg.Cache = config.CacheConfig{Enabled: true, MaxEntries: 16, TTLSeconds: 60}

	metrics := obs.NewMetrics()
	lookup := buildCache(cfg, metrics, discardLogger())
	if !lookup.Enabled() {
		t.Fatal("buildCache with caching on returned a lookup that is not enabled")
	}

	ctx := context.Background()
	raw := []byte(`{"model":"auto","temperature":0,"messages":[{"role":"user","content":"hi"}]}`)

	miss := lookup.Get(ctx, "acme", "auto", raw)
	if miss.Entry != nil {
		t.Fatal("a fresh cache returned an entry")
	}
	if !miss.Eligible {
		t.Fatal("a temperature 0 request was refused, so nothing downstream would ever be stored")
	}
	lookup.Put(ctx, miss, &cache.Entry{Body: []byte(`{"stored":true}`), Model: "auto", Provider: "primary"}, raw)

	hit := lookup.Get(ctx, "acme", "auto", raw)
	if hit.Entry == nil {
		t.Fatal("the same question missed after being stored")
	}

	// The event callback is the only wiring between the cache and the
	// dashboard. Without it an operator judging whether caching is worth
	// its risk reads a flat zero line while the cache is working.
	if got := testutil.ToFloat64(metrics.CacheEventsTotal.WithLabelValues(string(cache.EventExactHit))); got != 1 {
		t.Errorf("exact_hit events = %v, want 1", got)
	}
}

// Turning the semantic tier on has to be loud. Measured against labelled
// pairs it answers a differently meaning question a large fraction of
// the time, and an operator should meet that number at startup rather
// than discover it from a support ticket.
func TestSemanticCacheWarnsWithTheMeasuredNumber(t *testing.T) {
	cfg := tenantConfig()
	cfg.Cache = config.CacheConfig{
		Enabled:    true,
		MaxEntries: 16,
		TTLSeconds: 60,
		Semantic: config.SemanticCacheConfig{
			Enabled:     true,
			Threshold:   0.95,
			EmbedModel:  "text-embedding-004",
			EmbedAPIKey: "unused-in-this-test",
			// Deliberately unroutable. Building the tier must not call it,
			// and nothing in this test asks it a question.
			EmbedURL:     "http://127.0.0.1:1",
			MaxPerTenant: 8,
		},
	}

	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if lookup := buildCache(cfg, obs.NewMetrics(), log); !lookup.Enabled() {
		t.Fatal("buildCache with the semantic tier on returned a lookup that is not enabled")
	}

	out := logged.String()
	if !strings.Contains(out, "semantic cache enabled") {
		t.Fatalf("no warning was logged when the semantic tier was switched on:\n%s", out)
	}
	// The figure is in the message on purpose. "Similar is not the same"
	// reads as a caveat; the measured percentage reads as what it is.
	if !strings.Contains(out, "43.9 percent") {
		t.Errorf("the warning does not carry the measured figure:\n%s", out)
	}
}

// The failure this pins was found in production: a route named "auto" in
// front of a provider named "fast" priced against neither name, every
// ledger row read $0.00, and the USD caps admitted every request because
// nothing ever appeared to cost anything.
func TestRoutePricingResolvesAnAliasToTheVendorAndUpstreamModel(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "fast", Kind: config.KindGroq},
			{Name: "backup", Kind: config.KindOpenAI},
		},
		Routes: []config.RouteConfig{{
			Model:     "auto",
			Providers: []string{"fast", "backup"},
			ProviderModels: map[string]string{
				"fast":   "llama-3.3-70b-versatile",
				"backup": "gpt-4o-mini",
			},
		}},
	}

	kind, upstream := routePricing(cfg)("auto")
	// The first provider in the chain, because that is the one an
	// estimate is most likely to be spent against.
	if kind != "groq" || upstream != "llama-3.3-70b-versatile" {
		t.Fatalf("routePricing(auto) = %q, %q, want groq, llama-3.3-70b-versatile", kind, upstream)
	}

	// The resolved pair has to be a key the shipped table actually holds.
	// Resolving correctly to a model nobody priced is the same $0.00 row
	// by another route.
	prices, err := pricing.DefaultTable()
	if err != nil {
		t.Fatalf("DefaultTable: %v", err)
	}
	cost, ok := prices.Cost(kind, upstream, providers.Usage{PromptTokens: 1000, CompletionTokens: 1000})
	if !ok || cost.USD <= 0 {
		t.Errorf("the resolved pair %q/%q is not priced by the shipped table", kind, upstream)
	}

	// A model this gateway does not route prices at nothing rather than
	// at a guess.
	if kind, upstream := routePricing(cfg)("something-else"); kind != "" || upstream != "" {
		t.Errorf("an unrouted model resolved to %q, %q, want empty", kind, upstream)
	}
}

// Settlement prices through the provider name, because by then the
// gateway knows which provider actually answered, which on a fallback
// chain is not the one the estimate assumed.
func TestProviderKindsMapsEveryConfiguredName(t *testing.T) {
	cfg := &config.Config{Providers: []config.ProviderConfig{
		{Name: "fast", Kind: config.KindGroq},
		{Name: "backup", Kind: config.KindOpenAI},
	}}
	kindOf := providerKinds(cfg)

	if got := kindOf("fast"); got != "groq" {
		t.Errorf("kindOf(fast) = %q, want groq", got)
	}
	if got := kindOf("backup"); got != "openai" {
		t.Errorf("kindOf(backup) = %q, want openai", got)
	}
	// An unknown provider yields no kind, so the price table is asked
	// nothing rather than asked for a key made of an empty vendor.
	if got := kindOf("nobody"); got != "" {
		t.Errorf("kindOf(nobody) = %q, want empty", got)
	}
}

// The singular spelling is the one most configs use, and a route whose
// providers were left out must not take a slot in the pricing map that
// then answers for it with an empty vendor.
func TestRoutePricingHandlesBothSpellingsAndSkipsAnEmptyChain(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{Name: "solo", Kind: config.KindAnthropic}},
		Routes: []config.RouteConfig{
			{Model: "single", Provider: "solo"},
			{Model: "orphan"},
		},
	}
	priced := routePricing(cfg)

	// No provider_models entry, so the route's own name is what the
	// provider is asked for and what the estimate prices.
	if kind, upstream := priced("single"); kind != "anthropic" || upstream != "single" {
		t.Errorf("routePricing(single) = %q, %q, want anthropic, single", kind, upstream)
	}
	if kind, upstream := priced("orphan"); kind != "" || upstream != "" {
		t.Errorf("a route with no providers resolved to %q, %q, want empty", kind, upstream)
	}
}

// Config permits a deployment whose only credentials are tenant keys.
// Dropping them on the way to the ingress leaves that gateway open to
// anyone who can reach the listener, while the config file still reads
// as though it were locked.
func TestTenantKeysAreTheOnlyCredentialsSomeDeploymentsHave(t *testing.T) {
	cfg := tenantConfig()
	cfg.Auth.Tenants = append(cfg.Auth.Tenants, config.TenantConfig{
		Name: "beta",
		Keys: []string{"beta-key-0123456789abcdef", "beta-key-fedcba9876543210"},
	})

	byTenant := tenantKeys(cfg)
	if len(byTenant) != 2 {
		t.Fatalf("tenantKeys = %v, want both configured tenants", byTenant)
	}
	if got := byTenant["acme"]; len(got) != 1 || got[0] != "acme-key-0123456789abcdef" {
		t.Errorf("acme keys = %v, want the one configured key", got)
	}
	if got := byTenant["beta"]; len(got) != 2 {
		t.Errorf("beta keys = %v, want both configured keys", got)
	}

	// The consequence, rather than the map shape: with no client_keys at
	// all, these are what close the gateway.
	gateway := ingress.NewServer(cfg.Server, nil, discardLogger(),
		ingress.WithClientKeys(cfg.Auth.ClientKeys),
		ingress.WithTenantKeys(byTenant),
	)
	if !gateway.RequiresAuth() {
		t.Error("a tenants-only gateway reports no auth required, so it would serve anyone who reaches it")
	}
}

func TestNoTenantsMeansNoTenantKeys(t *testing.T) {
	cfg := tenantConfig()
	cfg.Auth.Tenants = nil
	if got := tenantKeys(cfg); got != nil {
		t.Errorf("tenantKeys with no tenants = %v, want nil", got)
	}
}

// budget.TenantID is what the enforcer is keyed by, and the conversion
// happens in buildAccounting. A limit that landed under a different key
// than the one requests arrive with would never bind.
func TestConfiguredLimitsReachTheEnforcerUnderTheTenantName(t *testing.T) {
	cfg := tenantConfig()
	acct := openAccounting(t, cfg)

	want := budget.Limits{RequestsPerMinute: 60, DailyUSD: 5, MonthlyUSD: 50}
	if got := acct.limits[budget.TenantID("acme")]; got != want {
		t.Errorf("limits for acme = %+v, want %+v", got, want)
	}

	// An unknown tenant is refused rather than served without a budget.
	if _, err := acct.requestPath().Begin(context.Background(), "stranger", "auto", []byte(`{}`)); err == nil {
		t.Error("a tenant with no configured limits was admitted")
	}
}
