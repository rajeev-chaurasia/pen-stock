// Command llmsim runs the deterministic mock LLM provider used for load
// testing and integration tests.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/llmsim"
)

const (
	shutdownGrace = 5 * time.Second

	// Slow-loris guards. WriteTimeout stays unset on purpose: it would
	// sever long-lived SSE streams mid-flight.
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	idleTimeout       = 2 * time.Minute
)

func main() {
	// Every exit path funnels through run so deferred cleanup always
	// gets to happen before the process leaves.
	if err := run(); err != nil {
		log.Printf("llmsim: %v", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", ":8089", "address to listen on")
	seed := flag.Int64("seed", 1, "determinism seed")
	profilePath := flag.String("profile", "", "path to a profile JSON file; built-in default when empty")
	timeScale := flag.Float64("time-scale", 1.0, "multiplier applied to every simulated sleep")
	fail429 := flag.Float64("fail-429", 0, "probability of responding 429")
	failHang := flag.Float64("fail-hang", 0, "probability of hanging the connection")
	failCut := flag.Float64("fail-cut", 0, "probability of cutting a stream mid-way")
	flag.Parse()

	profile := llmsim.DefaultProfile
	if *profilePath != "" {
		p, err := llmsim.LoadProfile(*profilePath)
		if err != nil {
			return err
		}
		profile = p
	}

	sim := llmsim.New(llmsim.Options{
		Seed:      *seed,
		Profile:   profile,
		TimeScale: *timeScale,
		Fail429:   *fail429,
		FailHang:  *failHang,
		FailCut:   *failCut,
	})
	srv := &http.Server{
		Addr:              *listen,
		Handler:           sim,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	log.Printf("llmsim listening on %s profile=%s seed=%d time-scale=%g fail-429=%g fail-hang=%g fail-cut=%g",
		*listen, profile.Name, *seed, *timeScale, *fail429, *failHang, *failCut)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
	}
	return nil
}
