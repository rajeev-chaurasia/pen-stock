// Command penstock runs the gateway: config in, providers wired, ingress
// served with metrics and tracing attached.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/rajeev-chaurasia/pen-stock/internal/admin"
	"github.com/rajeev-chaurasia/pen-stock/internal/budget"
	"github.com/rajeev-chaurasia/pen-stock/internal/cache"
	"github.com/rajeev-chaurasia/pen-stock/internal/config"
	"github.com/rajeev-chaurasia/pen-stock/internal/ingress"
	"github.com/rajeev-chaurasia/pen-stock/internal/obs"
	"github.com/rajeev-chaurasia/pen-stock/internal/pricing"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
	"github.com/rajeev-chaurasia/pen-stock/internal/router"

	// Blank imports register each adapter's kinds with the factory. A
	// missing one here means the kind is rejected at startup.
	_ "github.com/rajeev-chaurasia/pen-stock/internal/providers/anthropic"
	_ "github.com/rajeev-chaurasia/pen-stock/internal/providers/gemini"
	_ "github.com/rajeev-chaurasia/pen-stock/internal/providers/openaiwire"
)

const (
	shutdownGrace = 5 * time.Second
	// idleConnTimeout reaps keep-alive connections that never send
	// another request, which is what a slow loris looks like.
	idleConnTimeout = 2 * time.Minute
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "penstock:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.yaml", "path to the YAML config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	log, err := obs.NewLogger(cfg.Telemetry.LogLevel)
	if err != nil {
		return err
	}
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := obs.SetupTracing(ctx, cfg.Telemetry.ServiceName, cfg.Telemetry.OTLPEndpoint)
	if err != nil {
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := shutdownTracing(flushCtx); err != nil {
			log.Warn("tracing shutdown", "error", err)
		}
	}()

	provs, err := providers.BuildAll(cfg.Providers)
	if err != nil {
		return err
	}
	routes, err := buildRoutes(cfg, provs)
	if err != nil {
		return err
	}

	accounting, err := buildAccounting(cfg, modelKinds(cfg), log)
	if err != nil {
		return err
	}
	defer accounting.shutdown()

	metrics := obs.NewMetrics()
	gateway := ingress.NewServer(cfg.Server, routes, log,
		ingress.WithCache(buildCache(cfg, metrics, log)),
		ingress.WithAccounting(accounting.requestPath()),
		ingress.WithMetrics(metrics),
		ingress.WithClientKeys(cfg.Auth.ClientKeys),
		ingress.WithTenantKeys(tenantKeys(cfg)),
		ingress.WithInflightLimit(cfg.Server.MaxInflight),
	)

	readTimeout := time.Duration(cfg.Server.ReadTimeoutMS) * time.Millisecond
	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           withSpan(gateway.Handler()),
		ReadHeaderTimeout: readTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleConnTimeout,
		// No WriteTimeout: it would sever long lived SSE streams. Stream
		// liveness is enforced per write via StreamIdleTimeoutMS instead.
	}

	// Metrics carry token spend and latency profiles, which is operator
	// data rather than caller data, so they get their own listener.
	adminMux := http.NewServeMux()
	adminMux.Handle("GET /metrics", metrics.Handler())
	// The metrics pattern is more specific and keeps winning; everything
	// else falls through to the tenant API, which answers a JSON 404 for
	// paths it does not know rather than an HTML default.
	if h := accounting.adminHandler(); h != nil {
		adminMux.Handle("/", h)
	}
	adminSrv := &http.Server{
		Addr:              cfg.Server.AdminListen,
		Handler:           adminMux,
		ReadHeaderTimeout: readTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleConnTimeout,
	}

	log.Info("penstock starting",
		"listen", cfg.Server.Listen,
		"admin_listen", cfg.Server.AdminListen,
		"providers", len(provs),
		"routes", len(routes),
		"auth_required", gateway.RequiresAuth(),
		"max_inflight", cfg.Server.MaxInflight,
		"otlp_endpoint", cfg.Telemetry.OTLPEndpoint,
	)
	if !gateway.RequiresAuth() {
		log.Warn("no client_keys configured, every caller reaching this listener spends the configured provider keys")
	}

	errCh := make(chan error, 2)
	go func() { errCh <- srv.ListenAndServe() }()
	go func() { errCh <- adminSrv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		log.Info("penstock draining")
		drainCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = adminSrv.Shutdown(drainCtx)
		return srv.Shutdown(drainCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// buildRoutes turns each configured route into a provider, wrapping the
// chain in a router so a single-provider route and a fallback chain
// behave identically from the ingress side.
func buildRoutes(cfg *config.Config, provs map[string]providers.Provider) (map[string]providers.Provider, error) {
	// Health is shared across routes on purpose: a provider that is rate
	// limited or broken is equally unusable for every model it serves,
	// and learning that once is the point.
	health := router.NewHealth(router.Options{}, nil)

	routes := make(map[string]providers.Provider, len(cfg.Routes))
	for _, route := range cfg.Routes {
		chain := make([]providers.Provider, 0, len(route.Chain()))
		for _, name := range route.Chain() {
			p, ok := provs[name]
			if !ok {
				return nil, fmt.Errorf("route %q: provider %q was not built", route.Model, name)
			}
			// Providers in one chain rarely share a model vocabulary, so
			// each is asked for the name it actually knows.
			if upstream := route.UpstreamModel(name); upstream != route.Model {
				p = providers.WithModel(p, upstream)
			}
			chain = append(chain, p)
		}

		selector, err := router.NewSelector(router.Strategy(strategyOrDefault(route.Strategy)))
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", route.Model, err)
		}
		routed, err := router.New(route.Model, chain, health, selector, router.Options{}, nil)
		if err != nil {
			return nil, err
		}
		routes[route.Model] = routed
	}
	return routes, nil
}

// buildAccounting turns tenant limits into an enforcer, or returns nil
// when no tenant declares a limit worth enforcing. A gateway without
// budgeting must still serve, so nil here means metering is off rather
// than being an error.
func buildAccounting(cfg *config.Config, kindOf func(string) string, log *slog.Logger) (*accounting, error) {
	limits := make(map[budget.TenantID]budget.Limits, len(cfg.Auth.Tenants))
	for _, t := range cfg.Auth.Tenants {
		limits[budget.TenantID(t.Name)] = budget.Limits{
			RequestsPerMinute: t.Limits.RequestsPerMinute,
			TokensPerMinute:   t.Limits.TokensPerMinute,
			DailyUSD:          t.Limits.DailyUSD,
			MonthlyUSD:        t.Limits.MonthlyUSD,
			FailClosed:        t.Limits.FailClosed,
		}
	}
	if len(limits) == 0 {
		return nil, nil
	}

	prices, err := pricing.DefaultTable()
	if err != nil {
		return nil, fmt.Errorf("load price table: %w", err)
	}
	ledger, closeLedger, err := openLedger(cfg.Accounting.LedgerPath)
	if err != nil {
		return nil, err
	}

	enforcer := budget.NewEnforcer(limits, nil)
	return &accounting{
		guard: budget.NewGuard(budget.GuardOptions{
			Estimator: budget.NewEstimator(prices, kindOf, budget.EstimatorOptions{}),
			Enforcer:  enforcer,
			Prices:    prices,
			KindOf:    kindOf,
			Ledger:    ledger,
			// A ledger that cannot be written is a silent hole in the
			// audit trail, so it is reported rather than assumed empty.
			OnLedgerError: func(err error) {
				log.Error("cost ledger write failed", "error", err)
			},
		}),
		enforcer: enforcer,
		limits:   limits,
		close:    closeLedger,
	}, nil
}

// openLedger returns the cost ledger and a closer. An empty path means
// no ledger, which keeps a local run from littering the working
// directory with audit files nobody asked for.
func openLedger(path string) (pricing.Ledger, func(), error) {
	if path == "" {
		return pricing.NopLedger{}, func() {}, nil
	}
	f, err := pricing.OpenFileLedger(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open cost ledger: %w", err)
	}
	return f, func() { _ = f.Close() }, nil
}

// accounting keeps the enforcer beside the guard that wraps it, because
// the admin API reads the same counters the request path writes. Handing
// the API its own enforcer would let it report balances nobody is
// actually enforcing.
type accounting struct {
	guard    ingress.Accountant
	enforcer *budget.MemEnforcer
	limits   map[budget.TenantID]budget.Limits
	close    func()
}

// shutdown flushes the cost ledger. Skipping it would lose whatever the
// last writes left sitting in the OS page cache.
func (a *accounting) shutdown() {
	if a == nil || a.close == nil {
		return
	}
	a.close()
}

// requestPath reports the accountant the ingress should use, or nil when
// no tenant is configured.
func (a *accounting) requestPath() ingress.Accountant {
	if a == nil {
		return nil
	}
	return a.guard
}

// adminHandler serves tenant balances, or nil when there is nothing to
// report.
func (a *accounting) adminHandler() http.Handler {
	if a == nil {
		return nil
	}
	return admin.New(a.enforcer, a.limits).Handler()
}

// modelKinds maps each routed model to the kind of the first provider
// serving it, which is what the price table is keyed by. A fallback
// chain can span vendors, so this is the price the route is expected to
// be billed at rather than a guarantee.
func modelKinds(cfg *config.Config) func(string) string {
	kindOf := make(map[string]string, len(cfg.Routes))
	byName := make(map[string]config.ProviderKind, len(cfg.Providers))
	for _, p := range cfg.Providers {
		byName[p.Name] = p.Kind
	}
	for _, route := range cfg.Routes {
		chain := route.Chain()
		if len(chain) == 0 {
			continue
		}
		kindOf[route.Model] = string(byName[chain[0]])
	}
	return func(model string) string { return kindOf[model] }
}

// buildCache assembles the cache tiers from config, or returns nil when
// caching is off. The semantic tier is built only when it has an
// embedder to work with, since a similarity search with no vectors is
// just a slower miss.
func buildCache(cfg *config.Config, metrics *obs.Metrics, log *slog.Logger) *cache.Lookup {
	if !cfg.Cache.Enabled {
		return nil
	}
	onEvent := func(e cache.Event) { metrics.AddCacheEvent(string(e)) }

	opts := cache.LookupOptions{
		Exact: cache.NewExact(cache.ExactOptions{
			MaxEntries: cfg.Cache.MaxEntries,
			TTL:        time.Duration(cfg.Cache.TTLSeconds) * time.Second,
			OnEvent:    onEvent,
		}),
		MaxTemperature: cfg.Cache.MaxTemperature,
		OnEvent:        onEvent,
	}

	if sem := cfg.Cache.Semantic; sem.Enabled {
		opts.Embedder = cache.NewGeminiEmbedder(sem.EmbedURL, sem.EmbedAPIKey, sem.EmbedModel, nil)
		opts.Semantic = cache.NewSemantic(cache.SemanticOptions{
			Threshold:    sem.Threshold,
			MaxPerTenant: sem.MaxPerTenant,
			TTL:          time.Duration(cfg.Cache.TTLSeconds) * time.Second,
			OnEvent:      onEvent,
		})
		// Loud on purpose. Similarity does not separate a paraphrase
		// from its negation, so this tier can answer the opposite of
		// what was asked. An operator turning it on should have decided
		// that a wrong answer is cheaper than an API call for their
		// workload, not discovered it later.
		log.Warn("semantic cache enabled: similar is not the same question, and this tier can serve the answer to an opposite one",
			"threshold", sem.Threshold,
			"embed_model", sem.EmbedModel,
		)
	}
	return cache.NewLookup(opts)
}

// tenantKeys indexes every tenant's keys by tenant name. Config permits
// a deployment whose only credentials are tenant keys, so leaving these
// out would open such a gateway to anyone.
func tenantKeys(cfg *config.Config) map[string][]string {
	if len(cfg.Auth.Tenants) == 0 {
		return nil
	}
	byTenant := make(map[string][]string, len(cfg.Auth.Tenants))
	for _, t := range cfg.Auth.Tenants {
		byTenant[t.Name] = t.Keys
	}
	return byTenant
}

func strategyOrDefault(s string) string {
	if s == "" {
		return config.StrategyPriority
	}
	return s
}

// withSpan opens one span per request. Provider-level child spans arrive
// with the router phase.
func withSpan(next http.Handler) http.Handler {
	tracer := obs.Tracer()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The span name uses the normalized path so a URL scanner cannot
		// mint unbounded span names; the raw path rides as an attribute.
		ctx, span := tracer.Start(r.Context(), r.Method+" "+ingress.NormalizePath(r.URL.Path),
			trace.WithAttributes(attribute.String("http.request.path", r.URL.Path)))
		defer span.End()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
