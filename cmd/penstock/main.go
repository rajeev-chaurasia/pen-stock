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

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
	"github.com/rajeev-chaurasia/pen-stock/internal/ingress"
	"github.com/rajeev-chaurasia/pen-stock/internal/obs"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"

	// Blank import registers the openaiwire provider kinds with the factory.
	_ "github.com/rajeev-chaurasia/pen-stock/internal/providers/openaiwire"
)

const shutdownGrace = 5 * time.Second

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
	routes := make(map[string]providers.Provider, len(cfg.Routes))
	for _, route := range cfg.Routes {
		routes[route.Model] = provs[route.Provider]
	}

	metrics := obs.NewMetrics()
	gateway := ingress.NewServer(cfg.Server, routes, log, ingress.WithMetrics(metrics))

	mux := http.NewServeMux()
	mux.Handle("/", withSpan(gateway.Handler()))
	mux.Handle("GET /metrics", metrics.Handler())

	srv := &http.Server{
		Addr:        cfg.Server.Listen,
		Handler:     mux,
		ReadTimeout: time.Duration(cfg.Server.ReadTimeoutMS) * time.Millisecond,
		// No WriteTimeout: it would sever long-lived SSE streams. Stream
		// liveness is enforced per chunk via StreamIdleTimeoutMS instead.
	}

	log.Info("penstock starting",
		"listen", cfg.Server.Listen,
		"providers", len(provs),
		"routes", len(routes),
		"otlp_endpoint", cfg.Telemetry.OTLPEndpoint,
	)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		log.Info("penstock draining")
		drainCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		return srv.Shutdown(drainCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// withSpan opens one span per request. Provider-level child spans arrive
// with the router phase.
func withSpan(next http.Handler) http.Handler {
	tracer := obs.Tracer()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), r.Method+" "+r.URL.Path)
		defer span.End()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
