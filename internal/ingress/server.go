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

const (
	shutdownGrace = 5 * time.Second

	chatPath   = "/v1/chat/completions"
	modelsPath = "/v1/models"
	healthPath = "/healthz"

	// defaultInflight bounds concurrent requests when the operator sets
	// no limit of their own.
	defaultInflight = 256
)

// Server routes OpenAI-style chat traffic to configured providers.
type Server struct {
	cfg        config.ServerConfig
	routes     map[string]providers.Provider
	log        *slog.Logger
	metrics    MetricsSink
	clientKeys *keySet
	inflight   *inflight
	accounting Accountant
	handler    http.Handler
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

// WithClientKeys requires callers to present one of these keys as a
// bearer token. With no keys the gateway stays open, which is only safe
// on a loopback listener.
func WithClientKeys(keys []string) Option {
	return func(s *Server) { s.clientKeys = newKeySet(keys) }
}

// WithTenantKeys adds keys that carry an identity, so spend under them
// can be attributed and limited. A deployment may use these alone: they
// authenticate exactly like anonymous client keys.
func WithTenantKeys(byTenant map[string][]string) Option {
	return func(s *Server) {
		for tenant, keys := range byTenant {
			for _, key := range keys {
				s.clientKeys.add(key, tenant)
			}
		}
	}
}

// WithInflightLimit caps concurrent in flight requests.
func WithInflightLimit(limit int) Option {
	return func(s *Server) { s.inflight = newInflight(limit) }
}

// WithAccounting enforces per tenant budgets and rate limits. Without
// it the gateway serves every authenticated caller without metering.
func WithAccounting(a Accountant) Option {
	return func(s *Server) {
		if a != nil {
			s.accounting = a
		}
	}
}

// NewServer builds the ingress. routes maps a model id to the provider
// serving it.
func NewServer(cfg config.ServerConfig, routes map[string]providers.Provider, log *slog.Logger, opts ...Option) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{
		cfg:        cfg,
		routes:     routes,
		log:        log,
		metrics:    noopSink{},
		clientKeys: newKeySet(nil),
		inflight:   newInflight(defaultInflight),
	}
	for _, opt := range opts {
		opt(s)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+chatPath, s.handleChatCompletions)
	mux.HandleFunc("GET "+modelsPath, s.handleModels)
	mux.HandleFunc("GET "+healthPath, s.handleHealthz)

	// Order matters: recovery sees every panic, the access log records
	// every outcome, and rejected callers never consume a slot.
	s.handler = s.withRecovery(s.withAccessLog(s.withAuth(s.withLimit(mux))))
	return s
}

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// RequiresAuth reports whether any client key is configured. Callers use
// it to refuse binding an open gateway to a public address.
func (s *Server) RequiresAuth() bool { return !s.clientKeys.empty() }

// ListenAndServe serves on cfg.Listen until ctx is canceled, then drains
// gracefully.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.handler,
		ReadHeaderTimeout: msToDuration(s.cfg.ReadTimeoutMS),
		ReadTimeout:       msToDuration(s.cfg.ReadTimeoutMS),
		// No WriteTimeout: it would sever long lived SSE streams. Stalled
		// clients are bounded by a per write deadline instead.
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
	tenant   string
	stream   bool
}

func logInfoFrom(ctx context.Context) *logInfo {
	if info, ok := ctx.Value(ctxKeyLogInfo{}).(*logInfo); ok {
		return info
	}
	// handler running outside the middleware; give it a discardable sink
	return &logInfo{}
}

// withRecovery keeps one bad request from taking down every other
// tenant's in flight work.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic recovered", "path", normalizePath(r.URL.Path), "panic", rec)
				writeErrorJSON(w, http.StatusInternalServerError, "internal error", errTypeAPI, "internal")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &logInfo{start: start}
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyLogInfo{}, info))
		sw := newStatusWriter(w)

		// Deferred so a panic unwinding through here is still measured.
		defer func() {
			elapsed := time.Since(start)
			s.metrics.ObserveRequest(normalizePath(r.URL.Path), info.provider,
				strconv.Itoa(sw.Status()), elapsed.Seconds(), info.stream)

			// Request and response bodies are deliberately never logged.
			s.log.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"model", info.model,
				"provider", info.provider,
				"tenant", info.tenant,
				"status", sw.Status(),
				"duration_ms", elapsed.Milliseconds(),
				"stream", info.stream,
			)
		}()
		next.ServeHTTP(sw, r)
	})
}

// statusWriter records the response status for access logging. It
// reports whether the wrapped writer can really flush so the SSE path
// never promises streaming it cannot deliver.
type statusWriter struct {
	http.ResponseWriter
	flusher http.Flusher
	status  int
}

func newStatusWriter(w http.ResponseWriter) *statusWriter {
	sw := &statusWriter{ResponseWriter: w}
	if f, ok := w.(http.Flusher); ok {
		sw.flusher = f
	}
	return sw
}

// Unwrap lets http.ResponseController reach the real writer for
// deadline control.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

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
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

// Flusher exposes the underlying flusher, nil when the wrapped writer
// cannot stream.
func (w *statusWriter) Flusher() http.Flusher { return w.flusher }

func (w *statusWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
