// Package ingress exposes Penstock's client-facing HTTP API: an
// OpenAI-style chat surface routed to configured providers.
package ingress

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

const shutdownGrace = 5 * time.Second

// Server routes OpenAI-style chat traffic to configured providers.
type Server struct {
	cfg     config.ServerConfig
	routes  map[string]providers.Provider
	log     *slog.Logger
	metrics MetricsSink
	handler http.Handler
}

// Option customizes a Server beyond the required arguments.
type Option func(*Server)

// WithMetrics attaches a sink for request, TTFT, and token measurements.
func WithMetrics(m MetricsSink) Option {
	return func(s *Server) {
		if m != nil {
			s.metrics = m
		}
	}
}

// NewServer builds the ingress. routes maps a model id to the provider
// serving it.
func NewServer(cfg config.ServerConfig, routes map[string]providers.Provider, log *slog.Logger, opts ...Option) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{cfg: cfg, routes: routes, log: log, metrics: noopSink{}}
	for _, opt := range opts {
		opt(s)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.handler = s.withAccessLog(mux)
	return s
}

// Handler returns the fully wired HTTP handler, access logging included.
func (s *Server) Handler() http.Handler { return s.handler }

// ListenAndServe serves on cfg.Listen until ctx is canceled, then drains
// gracefully. Intended as the one-liner main needs.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:        s.cfg.Listen,
		Handler:     s.handler,
		ReadTimeout: msToDuration(s.cfg.ReadTimeoutMS),
		// No WriteTimeout: it would sever long-lived SSE streams. Stream
		// liveness is enforced per chunk via StreamIdleTimeoutMS instead.
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func msToDuration(ms int) time.Duration { return time.Duration(ms) * time.Millisecond }

type ctxKeyLogInfo struct{}

// logInfo is filled in by handlers so the access log line can carry
// routing details the middleware cannot know on its own.
type logInfo struct {
	start    time.Time
	model    string
	provider string
	stream   bool
}

func logInfoFrom(ctx context.Context) *logInfo {
	if info, ok := ctx.Value(ctxKeyLogInfo{}).(*logInfo); ok {
		return info
	}
	// handler running outside the middleware; give it a discardable sink
	return &logInfo{}
}

func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &logInfo{start: start}
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyLogInfo{}, info))
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)

		elapsed := time.Since(start)
		s.metrics.ObserveRequest(normalizePath(r.URL.Path), info.provider,
			strconv.Itoa(sw.Status()), elapsed.Seconds(), info.stream)

		// Request and response bodies are deliberately never logged.
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"model", info.model,
			"provider", info.provider,
			"status", sw.Status(),
			"duration_ms", elapsed.Milliseconds(),
			"stream", info.stream,
		)
	})
}

// statusWriter records the response status for access logging while
// keeping Flush available for SSE.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
