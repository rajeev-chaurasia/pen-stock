package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

const (
	// maxBodyBytes caps client request bodies at 10MB.
	maxBodyBytes int64 = 10 << 20

	ssePrefix     = "data: "
	sseTerminator = "\n\n"
	sseDoneFrame  = "data: [DONE]\n\n"

	contentTypeJSON = "application/json"
	contentTypeSSE  = "text/event-stream"
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

	req := &providers.ChatRequest{Model: envelope.Model, Stream: envelope.Stream, Raw: body}
	if envelope.Stream {
		s.serveStream(w, r, prov, req)
		return
	}
	s.serveChat(w, r, prov, req)
}

func (s *Server) serveChat(w http.ResponseWriter, r *http.Request, prov providers.Provider, req *providers.ChatRequest) {
	ctx := r.Context()
	if timeout := msToDuration(s.cfg.UpstreamTimeoutMS); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	resp, err := prov.Chat(ctx, req)
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	s.metrics.AddTokens(prov.Name(), resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp.Body)
}

type recvResult struct {
	chunk providers.StreamChunk
	err   error
}

func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, prov providers.Provider, req *providers.ChatRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErrorJSON(w, http.StatusInternalServerError,
			"streaming is not supported by this server", errTypeAPI, "streaming_unsupported")
		return
	}

	// Child of the request context: client disconnect cancels upstream,
	// and so does the idle-timeout abort below.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	reader, err := prov.ChatStream(ctx, req)
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	defer reader.Close()

	h := w.Header()
	h.Set("Content-Type", contentTypeSSE)
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
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
	firstChunk := true

	for {
		select {
		case res := <-results:
			if res.err != nil {
				// io.EOF is the clean end; anything else aborts mid-stream
				// and the client sees truncation without [DONE].
				if errors.Is(res.err, io.EOF) {
					_, _ = io.WriteString(w, sseDoneFrame)
					flusher.Flush()
				}
				return
			}
			if firstChunk {
				firstChunk = false
				if !info.start.IsZero() {
					s.metrics.ObserveTTFT(prov.Name(), time.Since(info.start).Seconds())
				}
			}
			if res.chunk.Usage != nil {
				s.metrics.AddTokens(prov.Name(), res.chunk.Usage.PromptTokens, res.chunk.Usage.CompletionTokens)
			}
			if err := writeSSEFrame(w, res.chunk.Data); err != nil {
				return
			}
			flusher.Flush()
			if idleTimer != nil {
				idleTimer.Reset(idle)
			}
		case <-idleC:
			// Upstream went quiet past its budget; kill the call.
			cancel()
			_ = reader.Close()
			return
		case <-ctx.Done():
			// Client went away; release the upstream promptly.
			_ = reader.Close()
			return
		}
	}
}

// pumpStream forwards Recv results one at a time until a terminal error
// or until nobody is listening. Only a single chunk is ever in flight,
// so the response is never buffered whole.
func pumpStream(ctx context.Context, reader providers.StreamReader, out chan<- recvResult) {
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
