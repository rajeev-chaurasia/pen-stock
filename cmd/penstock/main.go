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

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
	"github.com/rajeev-chaurasia/pen-stock/internal/ingress"
	"github.com/rajeev-chaurasia/pen-stock/internal/obs"
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

	metrics := obs.NewMetrics()
	gateway := ingress.NewServer(cfg.Server, routes, log,
		ingress.WithMetrics(metrics),
		ingress.WithClientKeys(cfg.Auth.ClientKeys),
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
	admin := &http.Server{
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
	go func() { errCh <- admin.ListenAndServe() }()

	select {
	case <-ctx.Done():
		log.Info("penstock draining")
		drainCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = admin.Shutdown(drainCtx)
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
