package llmsim

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	objectCompletion = "chat.completion"
	objectChunk      = "chat.completion.chunk"
	roleAssistant    = "assistant"
	finishStop       = "stop"
	doneEvent        = "[DONE]"
	idPrefix         = "sim-"
	ownerName        = "llmsim"

	// promptCharsPerToken is the crude chars-to-tokens ratio for usage counts.
	promptCharsPerToken = 4

	retryAfterSeconds = "2"
	hangTimeout       = 5 * time.Minute
)

// wordlist feeds generated completion text; realism of the prose is not a goal.
var wordlist = [...]string{
	"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel",
	"india", "juliet", "kilo", "lima", "mike", "november", "oscar", "papa",
	"quebec", "romeo", "sierra", "tango", "uniform", "victor", "whiskey",
	"xray", "yankee", "zulu",
}

// Options configures a simulator Server.
type Options struct {
	Seed    int64
	Profile Profile
	// TimeScale multiplies every simulated sleep; 0 means 1.0.
	TimeScale float64
	// Fail429, FailHang and FailCut are per-request probabilities of the
	// corresponding failure injection, evaluated in that order.
	Fail429  float64
	FailHang float64
	FailCut  float64
}

// Server is a deterministic OpenAI-compatible mock provider.
type Server struct {
	opts Options
	mux  *http.ServeMux
	next atomic.Int64
}

// New builds a Server; a zero Profile falls back to DefaultProfile.
func New(cfg Options) *Server {
	if cfg.TimeScale <= 0 {
		cfg.TimeScale = 1.0
	}
	if cfg.Profile == (Profile{}) {
		cfg.Profile = DefaultProfile
	}
	s := &Server{opts: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux = mux
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

type failureMode int

const (
	failNone failureMode = iota
	fail429
	failHang
	failCut
)

// plan holds everything a request will do, drawn up front from its own RNG so
// the wire behavior depends only on Seed and the request index, never on
// scheduling of concurrent requests.
type plan struct {
	n     int64
	fail  failureMode
	ttft  time.Duration
	words []string
	itl   []time.Duration
}

// newPlan seeds the request RNG with Seed XOR n and draws the failure mode,
// token count, latencies and words in a fixed order.
func (s *Server) newPlan(n int64) plan {
	seed := uint64(s.opts.Seed ^ n)
	rng := rand.New(rand.NewPCG(seed, seed))

	p := plan{n: n, fail: failNone}
	u := rng.Float64()
	switch {
	case u < s.opts.Fail429:
		p.fail = fail429
	case u < s.opts.Fail429+s.opts.FailHang:
		p.fail = failHang
	case u < s.opts.Fail429+s.opts.FailHang+s.opts.FailCut:
		p.fail = failCut
	}

	k := int(math.Round(s.opts.Profile.OutputTokens.sample(rng)))
	if k < 1 {
		k = 1
	}
	p.ttft = s.scale(s.opts.Profile.TTFT.sample(rng))
	p.words = make([]string, k)
	p.itl = make([]time.Duration, k)
	for i := range k {
		p.words[i] = wordlist[rng.IntN(len(wordlist))]
		p.itl[i] = s.scale(s.opts.Profile.ITL.sample(rng))
	}
	return p
}

func (p plan) id() string {
	return idPrefix + strconv.FormatInt(p.n, 10)
}

func (s *Server) scale(ms float64) time.Duration {
	return time.Duration(ms * s.opts.TimeScale * float64(time.Millisecond))
}

// sleepCtx sleeps for d unless ctx ends first; false means the client is gone.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Wire types, kept to the subset of the OpenAI schema the gateway consumes.

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type chatDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type chatChoice struct {
	Index        int          `json:"index"`
	Message      *chatMessage `json:"message,omitempty"`
	Delta        *chatDelta   `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

type errorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error(), "invalid_request_error", "invalid_request_error")
		return
	}

	n := s.next.Add(1) - 1
	p := s.newPlan(n)

	switch p.fail {
	case fail429:
		w.Header().Set("Retry-After", retryAfterSeconds)
		writeError(w, http.StatusTooManyRequests, "simulated rate limit exceeded", "rate_limit_exceeded", "rate_limit_exceeded")
		return
	case failHang:
		s.hang(w, r)
		return
	}

	promptChars := 0
	for _, m := range req.Messages {
		promptChars += len(m.Content)
	}
	u := chatUsage{
		PromptTokens:     promptChars / promptCharsPerToken,
		CompletionTokens: len(p.words),
	}
	u.TotalTokens = u.PromptTokens + u.CompletionTokens

	if req.Stream {
		s.streamChat(w, r, req.Model, p, u)
		return
	}
	s.completeChat(w, r, req.Model, p, u)
}

func (s *Server) completeChat(w http.ResponseWriter, r *http.Request, model string, p plan, u chatUsage) {
	if !sleepCtx(r.Context(), p.ttft) {
		return
	}
	if p.fail == failCut {
		abort(w)
		return
	}
	stop := finishStop
	resp := chatResponse{
		ID:      p.id(),
		Object:  objectCompletion,
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []chatChoice{{
			Message:      &chatMessage{Role: roleAssistant, Content: strings.Join(p.words, " ")},
			FinishReason: &stop,
		}},
		Usage: &u,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, model string, p plan, u chatUsage) {
	ctx := r.Context()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)

	if !sleepCtx(ctx, p.ttft) {
		return
	}

	emit := len(p.words)
	if p.fail == failCut {
		emit = len(p.words) / 2
	}
	created := time.Now().Unix()
	for i := 0; i < emit; i++ {
		if i > 0 && !sleepCtx(ctx, p.itl[i]) {
			return
		}
		delta := chatDelta{Content: p.words[i]}
		if i == 0 {
			delta.Role = roleAssistant
		} else {
			delta.Content = " " + delta.Content
		}
		chunk := chatResponse{
			ID:      p.id(),
			Object:  objectChunk,
			Created: created,
			Model:   model,
			Choices: []chatChoice{{Delta: &delta}},
		}
		if !writeEvent(w, rc, chunk) {
			return
		}
	}

	if p.fail == failCut {
		// Drop the connection abruptly: no usage chunk, no [DONE].
		abort(w)
		return
	}

	stop := finishStop
	final := chatResponse{
		ID:      p.id(),
		Object:  objectChunk,
		Created: created,
		Model:   model,
		Choices: []chatChoice{{Delta: &chatDelta{}, FinishReason: &stop}},
		Usage:   &u,
	}
	if !writeEvent(w, rc, final) {
		return
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", doneEvent); err != nil {
		return
	}
	_ = rc.Flush()
}

func writeEvent(w http.ResponseWriter, rc *http.ResponseController, chunk chatResponse) bool {
	b, err := json.Marshal(chunk)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return false
	}
	return rc.Flush() == nil
}

// hang holds the connection open with nothing written until the client goes
// away or the (scaled) timeout elapses, then drops it.
func (s *Server) hang(w http.ResponseWriter, r *http.Request) {
	limit := time.Duration(float64(hangTimeout) * s.opts.TimeScale)
	if sleepCtx(r.Context(), limit) {
		abort(w)
	}
}

// abort closes the underlying connection without a graceful HTTP finish so
// clients observe a truncated response. If the connection cannot be hijacked
// the plain handler return still ends the response without [DONE].
func abort(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	_ = conn.Close()
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	resp := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{
		Object: "list",
		Data: []model{{
			ID:      s.opts.Profile.Name,
			Object:  "model",
			Created: time.Now().Unix(),
			OwnedBy: ownerName,
		}},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func writeError(w http.ResponseWriter, status int, msg, typ, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorDetail{Message: msg, Type: typ, Code: code}})
}
