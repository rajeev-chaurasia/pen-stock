package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/budget"
	"github.com/rajeev-chaurasia/pen-stock/internal/cache"
	"github.com/rajeev-chaurasia/pen-stock/internal/httperr"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

const (
	// maxBodyBytes caps client request bodies. A chat payload with a
	// long conversation still fits well inside this.
	maxBodyBytes int64 = 1 << 20

	ssePrefix     = "data: "
	sseTerminator = "\n\n"
	sseDoneFrame  = "data: [DONE]\n\n"
	// sseKeepaliveFrame is an SSE comment: it keeps intermediaries from
	// timing out a connection during a long time to first token, and
	// clients ignore it.
	sseKeepaliveFrame = ": keep-alive\n\n"

	// The JSON media type is part of the error envelope contract, so it
	// comes from the same place that contract does.
	contentTypeJSON = httperr.ContentTypeJSON
	contentTypeSSE  = "text/event-stream"

	headerNoSniff = "nosniff"

	// terminalWriteBudget bounds the final frame of a stream, which must
	// outlive whatever deadline the preceding chunk left behind.
	terminalWriteBudget = 2 * time.Second
)

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	info := logInfoFrom(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErrorJSON(w, http.StatusRequestEntityTooLarge,
				"request body exceeds the 10MB limit", errTypeInvalidRequest, "request_too_large")
			return
		}
		writeErrorJSON(w, http.StatusBadRequest,
			"failed to read request body", errTypeInvalidRequest, "invalid_body")
		return
	}

	// Only the routing envelope is parsed here; the raw body is forwarded
	// untouched so fields the gateway does not model survive the trip.
	var envelope struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		writeErrorJSON(w, http.StatusBadRequest,
			"request body is not valid JSON", errTypeInvalidRequest, "invalid_json")
		return
	}
	info.model = envelope.Model
	info.stream = envelope.Stream

	if envelope.Model == "" {
		writeErrorJSON(w, http.StatusBadRequest,
			"model is required", errTypeInvalidRequest, "missing_model")
		return
	}
	prov, ok := s.routes[envelope.Model]
	if !ok {
		writeErrorJSON(w, http.StatusNotFound,
			fmt.Sprintf("model %q is not served by this gateway", envelope.Model),
			errTypeInvalidRequest, codeModelNotFound)
		return
	}
	info.provider = prov.Name()

	// The cache is consulted before accounting, because a hit calls no
	// provider and so has no cost to reserve against. A tenant at its
	// spend cap can still be served an answer the gateway already has,
	// which is the behavior an operator wants: the cap exists to bound
	// what gets spent, not to withhold what was already paid for.
	// Concurrency is still bounded by the in flight limit above.
	cacheRes := s.cache.Get(r.Context(), info.tenant, envelope.Model, body)
	if cacheRes.Entry != nil && s.serveCached(w, r, cacheRes) {
		return
	}

	// Budget is claimed before the upstream is called, because a limit
	// checked after the money is spent is not a limit.
	var reservation *budget.Reservation
	if s.accounting != nil {
		var err error
		reservation, err = s.accounting.Begin(r.Context(),
			budget.TenantID(info.tenant), envelope.Model, body)
		if err != nil {
			s.writeDenial(w, info.tenant, err)
			return
		}
	}

	req := &providers.ChatRequest{Model: envelope.Model, Stream: envelope.Stream, Raw: body}
	if envelope.Stream {
		s.serveStream(w, r, prov, req, reservation, cacheRes)
		return
	}
	s.serveChat(w, r, prov, req, reservation, cacheRes)
}

// settleOrAbort closes out a reservation exactly once: with the real
// usage when the upstream answered, and by returning the claim when it
// produced nothing. Leaving it open would strand the estimate against
// the tenant until it expired.
func (s *Server) settleOrAbort(ctx context.Context, res *budget.Reservation, usage *providers.Usage, model, provider string) {
	if s.accounting == nil || res == nil {
		return
	}
	if usage == nil {
		s.accounting.Abort(ctx, res)
		return
	}
	usd := s.accounting.Settle(ctx, res, *usage, model, provider)
	s.metrics.AddCost(string(res.Tenant), provider, model, usd)
}

func (s *Server) serveChat(w http.ResponseWriter, r *http.Request, prov providers.Provider, req *providers.ChatRequest, res *budget.Reservation, cacheRes cache.Result) {
	ctx := r.Context()
	if timeout := msToDuration(s.cfg.UpstreamTimeoutMS); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	resp, err := prov.Chat(ctx, req)
	if err != nil {
		// Nothing was produced, so nothing is owed.
		s.settleOrAbort(r.Context(), res, nil, req.Model, prov.Name())
		s.writeUpstreamError(w, err)
		return
	}
	// A routed model can be served by any provider in its chain, so cost
	// and latency are attributed to whoever actually answered rather
	// than to the route's label.
	answered := answeringProvider(prov.Name(), resp.Provider)
	logInfoFrom(r.Context()).provider = answered
	s.metrics.AddTokens(answered, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	s.settleOrAbort(r.Context(), res, &resp.Usage, req.Model, answered)

	// Stored after settling, so a cached answer carries the cost of the
	// call that produced it and a later hit can report what it avoided.
	s.cache.Put(r.Context(), cacheRes, &cache.Entry{
		Body:     resp.Body,
		Usage:    resp.Usage,
		Provider: answered,
		Model:    req.Model,
		StoredAt: time.Now(),
	}, req.Raw)

	w.Header().Set("Content-Type", contentTypeJSON)
	w.Header().Set("X-Content-Type-Options", headerNoSniff)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp.Body)
}

type recvResult struct {
	chunk providers.StreamChunk
	err   error
}

func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, prov providers.Provider, req *providers.ChatRequest, res *budget.Reservation, cacheRes cache.Result) {
	flusher := flusherFor(w)
	if flusher == nil {
		s.settleOrAbort(r.Context(), res, nil, req.Model, prov.Name())
		writeErrorJSON(w, http.StatusInternalServerError,
			"streaming is not supported by this server", errTypeAPI, "streaming_unsupported")
		return
	}

	// Child of the request context: client disconnect cancels upstream,
	// and so do the timeouts below.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// The wait for response headers needs its own bound. The idle timer
	// cannot cover it because it only starts once headers arrive, so an
	// upstream that accepts a connection and says nothing would hang the
	// request forever.
	var headerTimedOut atomic.Bool
	if upstream := msToDuration(s.cfg.UpstreamTimeoutMS); upstream > 0 {
		headerTimer := time.AfterFunc(upstream, func() {
			headerTimedOut.Store(true)
			cancel()
		})
		defer headerTimer.Stop()
	}

	reader, err := prov.ChatStream(ctx, req)
	if err != nil {
		s.settleOrAbort(r.Context(), res, nil, req.Model, prov.Name())
		if headerTimedOut.Load() {
			err = &providers.ProviderError{
				Provider: prov.Name(),
				Class:    providers.ErrClassTimeout,
				Message:  "upstream did not send response headers in time",
				Err:      err,
			}
		}
		s.writeUpstreamError(w, err)
		return
	}
	defer func() { _ = reader.Close() }()

	h := w.Header()
	h.Set("Content-Type", contentTypeSSE)
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Content-Type-Options", headerNoSniff)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	results := make(chan recvResult)
	go pumpStream(ctx, reader, results)

	idle := msToDuration(s.cfg.StreamIdleTimeoutMS)
	var idleC <-chan time.Time
	var idleTimer *time.Timer
	if idle > 0 {
		idleTimer = time.NewTimer(idle)
		defer idleTimer.Stop()
		idleC = idleTimer.C
	}

	info := logInfoFrom(r.Context())
	answered := answeringProvider(prov.Name(), providerOfStream(reader))
	info.provider = answered
	rc := http.NewResponseController(w)
	firstData := true

	// Providers differ on how often they report usage: some send it once
	// at the end, some repeat cumulative totals on every chunk. Keeping
	// only the last report and recording it at stream end is correct for
	// both, where summing would multiply the count.
	// Frames are collected so a completed stream can be replayed later.
	// Only a stream that finished cleanly is stored: see the [DONE]
	// branch below.
	var recorder streamRecorder
	var completed bool

	var lastUsage *providers.Usage
	defer func() {
		if lastUsage != nil {
			s.metrics.AddTokens(answered, lastUsage.PromptTokens, lastUsage.CompletionTokens)
		}
		if completed {
			usage := providers.Usage{}
			if lastUsage != nil {
				usage = *lastUsage
			}
			if entry := recorder.entry(answered, req.Model, &cache.Entry{Usage: usage}); entry != nil {
				s.cache.Put(context.WithoutCancel(r.Context()), cacheRes, entry, req.Raw)
			}
		}
		// A stream that ended without ever reporting usage produced no
		// billable evidence, so the claim goes back rather than being
		// settled at a number nobody measured. Settling on the estimate
		// instead would bill a guess.
		s.settleOrAbort(context.WithoutCancel(r.Context()), res, lastUsage, req.Model, answered)
	}()

	for {
		select {
		case res := <-results:
			if res.err != nil {
				// Only a stream the upstream actually finished may be
				// remembered. Storing a truncated one would replay a
				// partial answer forever as though it were whole.
				completed = errors.Is(res.err, io.EOF)
				s.finishStream(w, flusher, answered, res.err)
				return
			}
			if res.chunk.Usage != nil {
				lastUsage = res.chunk.Usage
			}

			// A stalled client must not pin this goroutine and its
			// upstream connection indefinitely, so every write carries
			// the same budget the upstream gets.
			if idle > 0 {
				_ = rc.SetWriteDeadline(time.Now().Add(idle))
			}

			if res.chunk.Keepalive {
				// Upstream is alive but still working. Reset the idle
				// budget and keep the client connection warm.
				if _, err := io.WriteString(w, sseKeepaliveFrame); err != nil {
					return
				}
			} else {
				if firstData {
					firstData = false
					if !info.start.IsZero() {
						s.metrics.ObserveTTFT(answered, time.Since(info.start).Seconds())
					}
				}
				if err := writeSSEFrame(w, res.chunk.Data); err != nil {
					return
				}
				recorder.add(res.chunk.Data)
			}
			flusher.Flush()
			if idleTimer != nil {
				idleTimer.Reset(idle)
			}
		case <-idleC:
			// Upstream went quiet past its budget; kill the call.
			cancel()
			_ = reader.Close()
			s.finishStream(w, flusher, answered, providers.ErrStreamTruncated)
			return
		case <-ctx.Done():
			// Client went away; release the upstream promptly.
			_ = reader.Close()
			return
		}
	}
}

// finishStream terminates the SSE response. [DONE] is written only when
// the upstream actually completed: on the OpenAI wire that sentinel is
// the sole completeness signal, so emitting it for a severed stream
// would present a partial answer as a whole one.
func (s *Server) finishStream(w http.ResponseWriter, flusher http.Flusher, provider string, cause error) {
	// The last chunk's write deadline may already be spent, which would
	// sink the terminal frame and leave the client guessing.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(terminalWriteBudget))

	if errors.Is(cause, io.EOF) {
		_, _ = io.WriteString(w, sseDoneFrame)
		flusher.Flush()
		return
	}

	s.log.Error("stream ended early", "provider", provider, "error", cause)
	frame, err := json.Marshal(errorEnvelope{Error: errorBody{
		Message: "upstream stream ended before completion",
		Type:    errTypeAPI,
		Code:    "stream_truncated",
	}})
	if err != nil {
		return
	}
	_ = writeSSEFrame(w, frame)
	flusher.Flush()
}

// flusherFor reports the writer's real flushing ability. The access log
// wrapper always defines Flush, so a plain type assertion would claim
// streaming works even when the underlying writer cannot flush.
func flusherFor(w http.ResponseWriter) http.Flusher {
	if fw, ok := w.(interface{ Flusher() http.Flusher }); ok {
		return fw.Flusher()
	}
	if f, ok := w.(http.Flusher); ok {
		return f
	}
	return nil
}

// pumpStream forwards Recv results one at a time until a terminal error
// or until nobody is listening. Only a single chunk is ever in flight,
// so the response is never buffered whole.
func pumpStream(ctx context.Context, reader providers.StreamReader, out chan<- recvResult) {
	// This goroutine is not owned by net/http, so an unrecovered panic
	// here would take the whole process down with it.
	defer func() {
		if rec := recover(); rec != nil {
			select {
			case out <- recvResult{err: fmt.Errorf("stream reader panicked: %v", rec)}:
			case <-ctx.Done():
			}
		}
	}()
	for {
		chunk, err := reader.Recv()
		select {
		case out <- recvResult{chunk: chunk, err: err}:
			if err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func writeSSEFrame(w io.Writer, data []byte) error {
	if _, err := io.WriteString(w, ssePrefix); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err := io.WriteString(w, sseTerminator)
	return err
}

type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type modelList struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	entries := make([]modelEntry, 0, len(s.routes))
	for model, prov := range s.routes {
		entries = append(entries, modelEntry{ID: model, Object: "model", OwnedBy: prov.Name()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	writeJSON(w, http.StatusOK, modelList{Object: "list", Data: entries})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
