package router

// This file is the contract for the router orchestrator.
//
// The rules come from policy.go, which is frozen: classDisposition and
// countsAgainstHealth are the specification, and the tables below carry
// their own copy of both so drift between policy and behavior shows up
// as a failure rather than as a quiet agreement. Where a behavior could
// reasonably go two ways, the assertion follows what the doc comments
// promise the caller, not what is convenient to implement.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// The orchestrator must be a Provider the ingress can hold onto, and
// must additionally report the attempts it made so traces and these
// tests can see what actually happened upstream.
var _ interface {
	providers.Provider
	Attempts() []Attempt
} = (*Router)(nil)

// routedName is the label of the routed model under test. It is
// deliberately different from every provider name so the two cannot be
// confused in an assertion.
const routedName = "routed-model"

// clockStart is the only instant the suite ever uses. A fixed clock
// means every timestamp the router hands to Health or Selector can be
// compared exactly.
var clockStart = time.Date(2024, 5, 17, 9, 30, 0, 0, time.UTC)

// fastOptions keeps every backoff far below a millisecond so the suite
// never waits on a real timer, whatever the implementation sleeps on.
func fastOptions(maxAttempts int) Options {
	return Options{
		MaxAttempts:    maxAttempts,
		RetryBaseDelay: time.Microsecond,
		MaxRetryDelay:  time.Microsecond,
	}
}

// perr builds the error shape a real adapter returns. The router routes
// on ErrorClass and the ingress classifies the same value, so tests must
// exercise that shape rather than a bare string error.
func perr(provider string, class providers.ErrorClass) *providers.ProviderError {
	return &providers.ProviderError{
		Provider: provider,
		Class:    class,
		Message:  "scripted " + string(class),
	}
}

func chatReq(stream bool) *providers.ChatRequest {
	raw := `{"model":"routed-model","messages":[{"role":"user","content":"hi"}]}`
	if stream {
		raw = `{"model":"routed-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	}
	return &providers.ChatRequest{Model: routedName, Stream: stream, Raw: json.RawMessage(raw)}
}

// --- fake provider ---------------------------------------------------

// outcome scripts one upstream call. A non-nil err fails the call;
// otherwise Chat returns a completion and ChatStream returns reader.
type outcome struct {
	err    error
	reader *fakeReader
	// before runs inside the upstream call, just before it returns. Tests
	// use it to cancel a context from the middle of the chain.
	before func()
}

func succeed() outcome { return outcome{} }

func failWith(provider string, class providers.ErrorClass) outcome {
	return outcome{err: perr(provider, class)}
}

func failWithErr(err error) outcome { return outcome{err: err} }

func streamOf(r *fakeReader) outcome { return outcome{reader: r} }

// fakeProvider replays a script of outcomes and counts what it was asked
// to do. The final scripted outcome repeats, so "always fails" is a one
// entry script.
type fakeProvider struct {
	name string

	mu          sync.Mutex
	script      []outcome
	used        int
	chatCalls   int
	streamCalls int
	lastRaw     json.RawMessage
}

func newProvider(name string, script ...outcome) *fakeProvider {
	return &fakeProvider{name: name, script: script}
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) next(req *providers.ChatRequest) outcome {
	p.mu.Lock()
	defer p.mu.Unlock()
	if req != nil {
		p.lastRaw = req.Raw
	}
	if len(p.script) == 0 {
		return outcome{}
	}
	i := p.used
	if i >= len(p.script) {
		i = len(p.script) - 1
	}
	p.used++
	return p.script[i]
}

func (p *fakeProvider) Chat(ctx context.Context, req *providers.ChatRequest) (*providers.ChatResponse, error) {
	_ = ctx
	p.mu.Lock()
	p.chatCalls++
	p.mu.Unlock()

	o := p.next(req)
	if o.before != nil {
		o.before()
	}
	if o.err != nil {
		return nil, o.err
	}
	return &providers.ChatResponse{
		Model:    req.Model,
		Provider: p.name,
		Body:     json.RawMessage(`{"object":"chat.completion","choices":[]}`),
		Usage:    providers.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
	}, nil
}

func (p *fakeProvider) ChatStream(ctx context.Context, req *providers.ChatRequest) (providers.StreamReader, error) {
	_ = ctx
	p.mu.Lock()
	p.streamCalls++
	p.mu.Unlock()

	o := p.next(req)
	if o.before != nil {
		o.before()
	}
	if o.err != nil {
		return nil, o.err
	}
	if o.reader == nil {
		return newReader(dataChunk("from "+p.name), recvErr(io.EOF)), nil
	}
	return o.reader, nil
}

func (p *fakeProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.chatCalls + p.streamCalls
}

func (p *fakeProvider) raw() json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastRaw
}

// --- fake stream reader ----------------------------------------------

// recvResult is one scripted Recv: a chunk, or the terminal error.
type recvResult struct {
	chunk providers.StreamChunk
	err   error
}

func dataChunk(s string) recvResult {
	return recvResult{chunk: providers.StreamChunk{Data: []byte(s)}}
}

func recvErr(err error) recvResult { return recvResult{err: err} }

// fakeReader replays scripted Recv results and counts Close calls, which
// is how the suite checks that a wrapped reader still releases the
// upstream connection.
type fakeReader struct {
	mu      sync.Mutex
	results []recvResult
	i       int
	closes  int
}

func newReader(results ...recvResult) *fakeReader {
	return &fakeReader{results: results}
}

func (r *fakeReader) Recv() (providers.StreamChunk, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.i >= len(r.results) {
		return providers.StreamChunk{}, io.EOF
	}
	res := r.results[r.i]
	r.i++
	return res.chunk, res.err
}

func (r *fakeReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closes++
	return nil
}

func (r *fakeReader) closeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closes
}

// drainReader reads to the terminal error, collecting the payloads so a
// test can prove chunks reached the caller unaltered.
func drainReader(r providers.StreamReader) ([]string, error) {
	var got []string
	for {
		chunk, err := r.Recv()
		if err != nil {
			return got, err
		}
		got = append(got, string(chunk.Data))
	}
}

// --- fake health -----------------------------------------------------

// healthCall records one Health notification so a test can assert who
// was blamed or credited, and with what.
type healthCall struct {
	provider   string
	class      providers.ErrorClass
	ttft       time.Duration
	retryAfter time.Duration
	now        time.Time
}

type fakeHealth struct {
	mu          sync.Mutex
	unavailable map[string]bool
	successes   []healthCall
	failures    []healthCall
	nows        []time.Time
}

func newHealth() *fakeHealth {
	return &fakeHealth{unavailable: make(map[string]bool)}
}

func (h *fakeHealth) setUnavailable(names ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, n := range names {
		h.unavailable[n] = true
	}
}

func (h *fakeHealth) Available(provider string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nows = append(h.nows, now)
	return !h.unavailable[provider]
}

func (h *fakeHealth) RecordSuccess(provider string, ttft time.Duration, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nows = append(h.nows, now)
	h.successes = append(h.successes, healthCall{provider: provider, ttft: ttft, now: now})
}

func (h *fakeHealth) RecordFailure(provider string, class providers.ErrorClass, retryAfter time.Duration, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nows = append(h.nows, now)
	h.failures = append(h.failures, healthCall{provider: provider, class: class, retryAfter: retryAfter, now: now})
}

func (h *fakeHealth) Latency(provider string) (time.Duration, bool) {
	_ = provider
	return 0, false
}

func (h *fakeHealth) successesFor(provider string) []healthCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []healthCall
	for _, c := range h.successes {
		if c.provider == provider {
			out = append(out, c)
		}
	}
	return out
}

func (h *fakeHealth) failuresFor(provider string) []healthCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []healthCall
	for _, c := range h.failures {
		if c.provider == provider {
			out = append(out, c)
		}
	}
	return out
}

func (h *fakeHealth) failureCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.failures)
}

func (h *fakeHealth) allNows() []time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Time(nil), h.nows...)
}

// --- fake selector ---------------------------------------------------

// fakeSelector stands in for the real selectors, which are being written
// separately. With no fixed order it behaves like priority selection:
// configured order, minus whatever health says is unavailable.
type fakeSelector struct {
	mu     sync.Mutex
	order  []string
	empty  bool
	chains [][]string
	nows   []time.Time
	calls  int
}

func (s *fakeSelector) Order(chain []string, h Health, now time.Time) []string {
	s.mu.Lock()
	s.calls++
	s.chains = append(s.chains, append([]string(nil), chain...))
	s.nows = append(s.nows, now)
	fixed, empty := s.order, s.empty
	s.mu.Unlock()

	if empty {
		return nil
	}
	if fixed != nil {
		return append([]string(nil), fixed...)
	}
	out := make([]string, 0, len(chain))
	for _, name := range chain {
		if h == nil || h.Available(name, now) {
			out = append(out, name)
		}
	}
	return out
}

func (s *fakeSelector) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *fakeSelector) firstChain() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.chains) == 0 {
		return nil
	}
	return s.chains[0]
}

func (s *fakeSelector) seenNows() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.nows...)
}

// --- fake clock ------------------------------------------------------

// fakeClock hands out a fixed instant, so nothing in the suite depends
// on the wall clock and every timestamp the router passes on can be
// matched exactly.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	calls int
}

func newClock() *fakeClock { return &fakeClock{now: clockStart} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.now
}

func (c *fakeClock) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// --- harness ---------------------------------------------------------

type harness struct {
	t         *testing.T
	providers []*fakeProvider
	health    *fakeHealth
	selector  *fakeSelector
	clock     *fakeClock
	router    providers.Provider
}

func newHarness(t *testing.T, opts Options, ps ...*fakeProvider) *harness {
	t.Helper()
	chain := make([]providers.Provider, len(ps))
	for i, p := range ps {
		chain[i] = p
	}
	h := &harness{
		t:         t,
		providers: ps,
		health:    newHealth(),
		selector:  &fakeSelector{},
		clock:     newClock(),
	}
	r, err := New(routedName, chain, h.health, h.selector, opts, h.clock)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.router = r
	return h
}

func (h *harness) attempts() []Attempt {
	h.t.Helper()
	r, ok := h.router.(interface{ Attempts() []Attempt })
	if !ok {
		h.t.Fatalf("router of type %T does not expose Attempts() []Attempt", h.router)
	}
	return r.Attempts()
}

func (h *harness) attemptNames() []string {
	h.t.Helper()
	atts := h.attempts()
	names := make([]string, len(atts))
	for i, a := range atts {
		names[i] = a.Provider
	}
	return names
}

// totalCalls is the number of upstream calls the whole chain saw, which
// is what MaxAttempts is a budget for.
func (h *harness) totalCalls() int {
	total := 0
	for _, p := range h.providers {
		total += p.calls()
	}
	return total
}

func sameNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// --- construction ----------------------------------------------------

func TestNewRejectsEmptyChain(t *testing.T) {
	// A routed model with no providers can never answer anything. Failing
	// at construction turns a config mistake into a startup error instead
	// of a 500 for every caller at request time.
	if _, err := New(routedName, nil, newHealth(), &fakeSelector{}, fastOptions(3), newClock()); err == nil {
		t.Fatal("New with an empty chain returned no error")
	}
}

func TestRouterNameIsTheRoutedModel(t *testing.T) {
	// Callers ask for the routed name, so that is what the router answers
	// to. Reporting a provider name here would leak the chain's shape into
	// logs and metrics keyed by model.
	h := newHarness(t, fastOptions(3), newProvider("a", succeed()))
	if got := h.router.Name(); got != routedName {
		t.Errorf("Name() = %q, want %q", got, routedName)
	}
}

// --- happy path ------------------------------------------------------

func TestRouterFirstProviderSuccessMakesOneCall(t *testing.T) {
	a := newProvider("a", succeed())
	b := newProvider("b", succeed())
	h := newHarness(t, fastOptions(3), a, b)

	req := chatReq(false)
	resp, err := h.router.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := a.calls(); got != 1 {
		t.Errorf("provider a calls = %d, want 1", got)
	}
	// The chain exists for failures, not for redundancy. A second call on
	// a healthy answer would double the cost of every request.
	if got := b.calls(); got != 0 {
		t.Errorf("provider b calls = %d, want 0", got)
	}
	if resp == nil {
		t.Fatal("Chat returned a nil response and a nil error")
	}
	// Attribution has to survive routing: billing and metrics key off
	// which upstream actually answered, not off the routed label.
	if resp.Provider != "a" {
		t.Errorf("response Provider = %q, want %q", resp.Provider, "a")
	}
	// Raw is the client's own bytes, forwarded untouched so the gateway
	// never drops fields it does not model.
	if got, want := string(a.raw()), string(req.Raw); got != want {
		t.Errorf("provider saw Raw %s, want %s", got, want)
	}
	if got := h.attemptNames(); !sameNames(got, []string{"a"}) {
		t.Errorf("attempts = %v, want [a]", got)
	}
	if got := len(h.health.successesFor("a")); got != 1 {
		t.Errorf("RecordSuccess calls for a = %d, want 1", got)
	}
	if got := h.health.failureCount(); got != 0 {
		t.Errorf("RecordFailure calls = %d on a clean success, want 0", got)
	}
	// The selector decides what gets tried, so it must actually be asked.
	if h.selector.callCount() == 0 {
		t.Error("Selector.Order was never called: the router bypassed selection")
	}
	if got := h.selector.firstChain(); !sameNames(got, []string{"a", "b"}) {
		t.Errorf("Selector.Order saw chain %v, want the configured order [a b]", got)
	}
}

// --- the policy table ------------------------------------------------

// TestRouterEnforcesClassPolicy is the core contract: for every error
// class, what the loop does next and whether the failure is held against
// the provider. Both expectations are restated here and checked against
// policy.go, so a change to either side shows up as a failure.
func TestRouterEnforcesClassPolicy(t *testing.T) {
	tests := []struct {
		name                string
		class               providers.ErrorClass
		disposition         disposition
		countsAgainstHealth bool
	}{
		// The caller's own payload. Every provider rejects it identically,
		// so failing over multiplies one mistake into N upstream calls,
		// and blaming the provider would open a breaker for everyone.
		{"invalid_request", providers.ErrClassInvalidRequest, dispositionFail, false},
		// The client hung up. Nobody is waiting for a second opinion.
		{"canceled", providers.ErrClassCanceled, dispositionFail, false},
		// Transient: the same provider deserves another try.
		{"upstream_unavailable", providers.ErrClassUpstream, dispositionRetry, true},
		{"timeout", providers.ErrClassTimeout, dispositionRetry, true},
		// A different bucket may have room right now, so move on at once
		// instead of waiting out this provider's limit.
		{"rate_limited", providers.ErrClassRateLimited, dispositionFailover, true},
		// Configuration faults on this provider. Retrying cannot help, a
		// peer with working credentials can.
		{"auth", providers.ErrClassAuth, dispositionFailover, true},
		{"payment_required", providers.ErrClassPaymentRequired, dispositionFailover, true},
		// Failing over but NOT counted against health: the model missing
		// here says nothing about whether this provider is well.
		{"model_not_found", providers.ErrClassModelNotFound, dispositionFailover, false},
		// Anything unrecognized is treated as this provider's problem.
		{"internal", providers.ErrClassInternal, dispositionFailover, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Guard the table against policy.go rather than trusting that
			// the two were written from the same understanding.
			if got := classDisposition(tc.class); got != tc.disposition {
				t.Fatalf("classDisposition(%s) = %v, table says %v", tc.class, got, tc.disposition)
			}
			if got := countsAgainstHealth(tc.class); got != tc.countsAgainstHealth {
				t.Fatalf("countsAgainstHealth(%s) = %v, table says %v", tc.class, got, tc.countsAgainstHealth)
			}

			a := newProvider("a", failWith("a", tc.class))
			b := newProvider("b", succeed())
			h := newHarness(t, fastOptions(5), a, b)

			resp, err := h.router.Chat(context.Background(), chatReq(false))
			names := h.attemptNames()

			switch tc.disposition {
			case dispositionFail:
				if err == nil {
					t.Fatalf("Chat succeeded on a %s failure, want the error returned as is", tc.class)
				}
				if got := b.calls(); got != 0 {
					t.Errorf("provider b calls = %d, want 0: %s must not fail over", got, tc.class)
				}
				if got := a.calls(); got != 1 {
					t.Errorf("provider a calls = %d, want 1: %s must not be retried", got, tc.class)
				}
				if !sameNames(names, []string{"a"}) {
					t.Errorf("attempts = %v, want exactly [a]", names)
				}
				var pe *providers.ProviderError
				if !errors.As(err, &pe) {
					t.Fatalf("error %v is not a *providers.ProviderError", err)
				}
				if pe.Class != tc.class {
					t.Errorf("returned class = %q, want %q unchanged", pe.Class, tc.class)
				}

			case dispositionFailover:
				if err != nil {
					t.Fatalf("Chat: %v, want the peer to answer after a %s failure", err, tc.class)
				}
				// Retrying a provider that cannot serve this request wastes
				// the caller's latency budget on a known answer.
				if got := a.calls(); got != 1 {
					t.Errorf("provider a calls = %d, want 1: %s must fail over without retrying", got, tc.class)
				}
				if got := b.calls(); got != 1 {
					t.Errorf("provider b calls = %d, want 1", got)
				}
				if !sameNames(names, []string{"a", "b"}) {
					t.Errorf("attempts = %v, want [a b]", names)
				}
				if resp == nil || resp.Provider != "b" {
					t.Errorf("response did not come from b: %+v", resp)
				}

			case dispositionRetry:
				if err != nil {
					t.Fatalf("Chat: %v, want the chain to recover from a %s failure", err, tc.class)
				}
				if len(names) < 2 {
					t.Fatalf("attempts = %v, want the same provider tried again first", names)
				}
				if names[0] != "a" || names[1] != "a" {
					t.Errorf("attempts = %v, want a retried on itself before moving on", names)
				}
				// The retry budget must not swallow the whole chain budget:
				// a chain with room left has to get its turn.
				if got := b.calls(); got != 1 {
					t.Errorf("provider b calls = %d, want 1: retries consumed the whole MaxAttempts budget", got)
				}
				if names[len(names)-1] != "b" {
					t.Errorf("attempts = %v, want the last attempt on b", names)
				}
				if len(names) > 5 {
					t.Errorf("attempts = %v, want at most MaxAttempts (5) entries", names)
				}
			}

			// Health accounting is independent of disposition: model_not_found
			// fails over yet must never be held against the provider.
			gotFailures := h.health.failuresFor("a")
			wantFailures := 0
			if tc.countsAgainstHealth {
				wantFailures = a.calls()
			}
			if len(gotFailures) != wantFailures {
				t.Errorf("RecordFailure calls for a = %d, want %d for class %s", len(gotFailures), wantFailures, tc.class)
			}
			for _, f := range gotFailures {
				if f.class != tc.class {
					t.Errorf("RecordFailure carried class %q, want %q", f.class, tc.class)
				}
			}
			if got := len(h.health.successesFor("a")); got != 0 {
				t.Errorf("RecordSuccess calls for a = %d, want 0 on a failing provider", got)
			}
			if b.calls() > 0 {
				if got := len(h.health.successesFor("b")); got != 1 {
					t.Errorf("RecordSuccess calls for b = %d, want 1", got)
				}
			}
		})
	}
}

// --- attempt budget --------------------------------------------------

func TestRouterRetryStopsAtMaxAttempts(t *testing.T) {
	// A retryable class is the one that can loop forever if nothing bounds
	// it, so the ceiling has to hold even with a single provider.
	tests := []struct {
		name        string
		maxAttempts int
	}{
		{"one_attempt_means_no_retry", 1},
		{"two_attempts_means_one_retry", 2},
		{"four_attempts_means_three_retries", 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newProvider("a", failWith("a", providers.ErrClassUpstream))
			h := newHarness(t, fastOptions(tc.maxAttempts), a)

			if _, err := h.router.Chat(context.Background(), chatReq(false)); err == nil {
				t.Fatal("Chat succeeded against an always failing provider")
			}
			if got := a.calls(); got != tc.maxAttempts {
				t.Errorf("upstream calls = %d, want exactly MaxAttempts (%d)", got, tc.maxAttempts)
			}
			if got := len(h.attempts()); got != tc.maxAttempts {
				t.Errorf("Attempts() length = %d, want %d", got, tc.maxAttempts)
			}
		})
	}
}

func TestRouterMaxAttemptsBoundsTheWholeChain(t *testing.T) {
	// MaxAttempts is a budget for one client request across every
	// provider, not an allowance per provider. Without that reading a long
	// chain turns a single request into a storm.
	names := []string{"a", "b", "c", "d", "e"}
	ps := make([]*fakeProvider, 0, len(names))
	for _, n := range names {
		ps = append(ps, newProvider(n, failWith(n, providers.ErrClassRateLimited)))
	}
	h := newHarness(t, fastOptions(3), ps...)

	if _, err := h.router.Chat(context.Background(), chatReq(false)); err == nil {
		t.Fatal("Chat succeeded against a chain where everything fails")
	}
	if got := h.totalCalls(); got != 3 {
		t.Errorf("upstream calls across the chain = %d, want 3", got)
	}
	if got := len(h.attempts()); got != 3 {
		t.Errorf("Attempts() length = %d, want 3", got)
	}
	for _, p := range ps[3:] {
		if got := p.calls(); got != 0 {
			t.Errorf("provider %s calls = %d, want 0: the budget was already spent", p.Name(), got)
		}
	}
}

func TestRouterAppliesDefaultMaxAttempts(t *testing.T) {
	// A zero Options must mean the documented defaults, not "unlimited",
	// which is what a literal zero ceiling would otherwise imply.
	names := []string{"a", "b", "c", "d", "e"}
	ps := make([]*fakeProvider, 0, len(names))
	for _, n := range names {
		ps = append(ps, newProvider(n, failWith(n, providers.ErrClassRateLimited)))
	}
	h := newHarness(t, Options{}, ps...)

	if _, err := h.router.Chat(context.Background(), chatReq(false)); err == nil {
		t.Fatal("Chat succeeded against a chain where everything fails")
	}
	if got := h.totalCalls(); got != DefaultMaxAttempts {
		t.Errorf("upstream calls = %d, want DefaultMaxAttempts (%d)", got, DefaultMaxAttempts)
	}
}

// --- error surfaced to the caller ------------------------------------

func TestRouterAllFailingReturnsLastProviderError(t *testing.T) {
	// The ingress renders whichever *ProviderError it finds first, so the
	// router must hand back the last attempt's error and nothing else. An
	// aggregate, or the first error, would give the caller a status that
	// has nothing to do with how the chain actually ended.
	aErr := perr("a", providers.ErrClassAuth)
	bErr := perr("b", providers.ErrClassRateLimited)
	cErr := perr("c", providers.ErrClassPaymentRequired)

	a := newProvider("a", failWithErr(aErr))
	b := newProvider("b", failWithErr(bErr))
	c := newProvider("c", failWithErr(cErr))
	h := newHarness(t, fastOptions(3), a, b, c)

	_, err := h.router.Chat(context.Background(), chatReq(false))
	if err == nil {
		t.Fatal("Chat succeeded against a chain where everything fails")
	}
	if !errors.Is(err, cErr) {
		t.Errorf("returned error %v does not carry the last attempt's error", err)
	}
	if errors.Is(err, aErr) {
		t.Errorf("returned error %v carries the first attempt's error too, so the ingress would classify an arbitrary one", err)
	}
	var pe *providers.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not a *providers.ProviderError, so the ingress cannot classify it", err)
	}
	if pe.Class != providers.ErrClassPaymentRequired {
		t.Errorf("class = %q, want %q from the last attempt", pe.Class, providers.ErrClassPaymentRequired)
	}
	if got := h.attemptNames(); !sameNames(got, []string{"a", "b", "c"}) {
		t.Errorf("attempts = %v, want [a b c]", got)
	}
}

func TestRouterUnavailableChainFailsWithoutUpstreamCalls(t *testing.T) {
	// When health has already taken every provider out of rotation, the
	// router answers from the gateway. Probing a chain it knows is sick
	// spends the caller's latency to learn nothing.
	//
	// The class must be upstream_unavailable: the ingress turns that into
	// a 502, which is honest. A caller facing class would blame the client
	// for the gateway's own state.
	t.Run("chat", func(t *testing.T) {
		a := newProvider("a", succeed())
		b := newProvider("b", succeed())
		h := newHarness(t, fastOptions(3), a, b)
		h.health.setUnavailable("a", "b")

		_, err := h.router.Chat(context.Background(), chatReq(false))
		if err == nil {
			t.Fatal("Chat succeeded with no provider available")
		}
		if got := h.totalCalls(); got != 0 {
			t.Errorf("upstream calls = %d, want 0", got)
		}
		if got := len(h.attempts()); got != 0 {
			t.Errorf("Attempts() length = %d, want 0", got)
		}
		var pe *providers.ProviderError
		if !errors.As(err, &pe) {
			t.Fatalf("error %v is not a *providers.ProviderError", err)
		}
		if pe.Class != providers.ErrClassUpstream {
			t.Errorf("class = %q, want %q", pe.Class, providers.ErrClassUpstream)
		}
	})

	t.Run("stream", func(t *testing.T) {
		a := newProvider("a", succeed())
		h := newHarness(t, fastOptions(3), a)
		h.health.setUnavailable("a")

		r, err := h.router.ChatStream(context.Background(), chatReq(true))
		if err == nil {
			if r != nil {
				_ = r.Close()
			}
			t.Fatal("ChatStream succeeded with no provider available")
		}
		if got := h.totalCalls(); got != 0 {
			t.Errorf("upstream calls = %d, want 0", got)
		}
		var pe *providers.ProviderError
		if !errors.As(err, &pe) {
			t.Fatalf("error %v is not a *providers.ProviderError", err)
		}
		if pe.Class != providers.ErrClassUpstream {
			t.Errorf("class = %q, want %q", pe.Class, providers.ErrClassUpstream)
		}
	})
}

// --- selection -------------------------------------------------------

func TestRouterFollowsSelectorOrder(t *testing.T) {
	// Configured order is an input to selection, not the plan. Least
	// latency and round robin only mean something if the router tries the
	// order it was handed back.
	a := newProvider("a", succeed())
	b := newProvider("b", succeed())
	c := newProvider("c", failWith("c", providers.ErrClassRateLimited))
	h := newHarness(t, fastOptions(3), a, b, c)
	h.selector.order = []string{"c", "a"}

	resp, err := h.router.Chat(context.Background(), chatReq(false))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := h.attemptNames(); !sameNames(got, []string{"c", "a"}) {
		t.Errorf("attempts = %v, want [c a] in the order the selector gave", got)
	}
	// A provider the selector left out is not a fallback of last resort.
	// It was excluded because health says it is unfit right now.
	if got := b.calls(); got != 0 {
		t.Errorf("provider b calls = %d, want 0: the selector excluded it", got)
	}
	if resp == nil || resp.Provider != "a" {
		t.Errorf("response did not come from a: %+v", resp)
	}
	if got := h.selector.firstChain(); !sameNames(got, []string{"a", "b", "c"}) {
		t.Errorf("Selector.Order saw chain %v, want the configured order [a b c]", got)
	}
}

func TestRouterSkipsProvidersHealthMarkedUnavailable(t *testing.T) {
	// The first provider in the chain is skipped without a call when
	// health has it in a breaker window. That is the whole point of
	// tracking health: stop paying for calls that are going to fail.
	a := newProvider("a", succeed())
	b := newProvider("b", succeed())
	h := newHarness(t, fastOptions(3), a, b)
	h.health.setUnavailable("a")

	resp, err := h.router.Chat(context.Background(), chatReq(false))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := a.calls(); got != 0 {
		t.Errorf("provider a calls = %d, want 0 while it is unavailable", got)
	}
	if resp == nil || resp.Provider != "b" {
		t.Errorf("response did not come from b: %+v", resp)
	}
	if got := h.attemptNames(); !sameNames(got, []string{"b"}) {
		t.Errorf("attempts = %v, want [b]", got)
	}
}

// --- clock injection -------------------------------------------------

func TestRouterUsesInjectedClock(t *testing.T) {
	// Every timestamp the router hands out has to come from the injected
	// clock. If it reaches for time.Now instead, breaker windows and
	// cooldowns cannot be driven from a test and will drift against the
	// rest of the gateway.
	a := newProvider("a", failWith("a", providers.ErrClassRateLimited))
	b := newProvider("b", succeed())
	h := newHarness(t, fastOptions(3), a, b)

	if _, err := h.router.Chat(context.Background(), chatReq(false)); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if h.clock.callCount() == 0 {
		t.Fatal("the injected Clock was never read")
	}
	for i, now := range h.health.allNows() {
		if !now.Equal(clockStart) {
			t.Errorf("Health call %d saw now = %v, want the injected %v", i, now, clockStart)
		}
	}
	for i, now := range h.selector.seenNows() {
		if !now.Equal(clockStart) {
			t.Errorf("Selector.Order call %d saw now = %v, want the injected %v", i, now, clockStart)
		}
	}
}

func TestRouterBackoffIsCappedByMaxRetryDelay(t *testing.T) {
	// MaxRetryDelay caps a single backoff so a retry loop cannot outlive
	// the caller's patience. A huge base with a tiny cap must still finish
	// immediately: if this test times out, the router is sleeping on the
	// base delay and ignoring the cap.
	opts := Options{
		MaxAttempts:    4,
		RetryBaseDelay: 30 * time.Second,
		MaxRetryDelay:  2 * time.Millisecond,
	}
	a := newProvider("a", failWith("a", providers.ErrClassUpstream))
	h := newHarness(t, opts, a)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.router.Chat(context.Background(), chatReq(false))
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Chat did not return within 2s: backoff is not bounded by MaxRetryDelay")
	}
	if got := a.calls(); got != 4 {
		t.Errorf("upstream calls = %d, want 4", got)
	}
}

// --- streaming -------------------------------------------------------

func TestRouterStreamConnectFailureFollowsPolicy(t *testing.T) {
	// Failover during a stream is legal only while connecting, because
	// nothing has been written to the client yet. The same class policy
	// applies as for a non streaming call.
	tests := []struct {
		name         string
		class        providers.ErrorClass
		wantFailover bool
	}{
		{"rate_limited_fails_over", providers.ErrClassRateLimited, true},
		{"invalid_request_does_not", providers.ErrClassInvalidRequest, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newProvider("a", failWith("a", tc.class))
			b := newProvider("b", streamOf(newReader(dataChunk("hello"), recvErr(io.EOF))))
			h := newHarness(t, fastOptions(3), a, b)

			r, err := h.router.ChatStream(context.Background(), chatReq(true))
			if !tc.wantFailover {
				if err == nil {
					if r != nil {
						_ = r.Close()
					}
					t.Fatalf("ChatStream succeeded after a %s failure, want the error returned as is", tc.class)
				}
				if got := b.calls(); got != 0 {
					t.Errorf("provider b calls = %d, want 0: %s must not fail over", got, tc.class)
				}
				if got := h.attemptNames(); !sameNames(got, []string{"a"}) {
					t.Errorf("attempts = %v, want [a]", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("ChatStream: %v, want the peer to connect", err)
			}
			defer func() { _ = r.Close() }()

			got, err := drainReader(r)
			if !errors.Is(err, io.EOF) {
				t.Fatalf("stream ended with %v, want io.EOF", err)
			}
			// Chunks belong to the caller untouched: the router relays, it
			// does not rewrite the completion.
			if !sameNames(got, []string{"hello"}) {
				t.Errorf("chunks = %v, want [hello]", got)
			}
			if got := h.attemptNames(); !sameNames(got, []string{"a", "b"}) {
				t.Errorf("attempts = %v, want [a b]", got)
			}
			// A completed stream is a success for the provider that served
			// it, or latency aware selection goes blind on streaming
			// traffic, which is most of it.
			if got := len(h.health.successesFor("b")); got != 1 {
				t.Errorf("RecordSuccess calls for b = %d, want 1 after a whole stream", got)
			}
			if got := len(h.health.failuresFor("a")); got != 1 {
				t.Errorf("RecordFailure calls for a = %d, want 1", got)
			}
		})
	}
}

func TestRouterMidStreamFailureNeverFailsOver(t *testing.T) {
	// Once a reader has been handed back, bytes may already be on the
	// wire. Starting a second provider now would splice two different
	// completions into one answer, so the failure is surfaced instead.
	// upstream_unavailable is a retry class for connects: once the stream
	// is open it must stop being one.
	tests := []struct {
		name string
		err  error
	}{
		{"truncated", providers.ErrStreamTruncated},
		{"upstream_unavailable", perr("a", providers.ErrClassUpstream)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader := newReader(dataChunk("first"), recvErr(tc.err))
			a := newProvider("a", streamOf(reader))
			b := newProvider("b", streamOf(newReader(dataChunk("second"), recvErr(io.EOF))))
			h := newHarness(t, fastOptions(5), a, b)

			r, err := h.router.ChatStream(context.Background(), chatReq(true))
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}

			got, streamErr := drainReader(r)
			if !errors.Is(streamErr, tc.err) {
				t.Fatalf("stream ended with %v, want %v surfaced to the caller", streamErr, tc.err)
			}
			if !sameNames(got, []string{"first"}) {
				t.Errorf("chunks = %v, want [first] from the provider that opened the stream", got)
			}
			if got := b.calls(); got != 0 {
				t.Errorf("provider b calls = %d, want 0: a mid stream failure must never start a second completion", got)
			}
			if got := h.attemptNames(); !sameNames(got, []string{"a"}) {
				t.Errorf("attempts = %v, want [a]", got)
			}

			// Closing the reader the caller holds must release the upstream
			// one, wrapper or not, or every truncated stream leaks a
			// connection.
			if err := r.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
			if got := reader.closeCount(); got == 0 {
				t.Error("closing the returned reader did not close the upstream reader")
			}
		})
	}
}

// --- cancellation ----------------------------------------------------

func TestRouterStopsWhenContextIsCanceled(t *testing.T) {
	t.Run("canceled_mid_chain", func(t *testing.T) {
		// The caller left while the first provider was answering. Walking
		// the rest of the chain spends real upstream quota on a response
		// nobody will ever read.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		a := newProvider("a", outcome{err: perr("a", providers.ErrClassRateLimited), before: cancel})
		b := newProvider("b", succeed())
		c := newProvider("c", succeed())
		h := newHarness(t, fastOptions(5), a, b, c)

		_, err := h.router.Chat(ctx, chatReq(false))
		if err == nil {
			t.Fatal("Chat succeeded after its context was canceled")
		}
		if got := b.calls() + c.calls(); got != 0 {
			t.Errorf("calls after cancellation = %d, want 0", got)
		}
		if got := len(h.attempts()); got != 1 {
			t.Errorf("Attempts() length = %d, want 1", got)
		}
		// The reason the chain stopped was the cancellation, and that is
		// what the caller should be told. The ingress maps both a bare
		// context.Canceled and a canceled class to 499; relaying the
		// provider's rate limit instead would report a 429 for a request
		// that was never really rate limited.
		var pe *providers.ProviderError
		signalsCancel := errors.Is(err, context.Canceled) ||
			(errors.As(err, &pe) && pe.Class == providers.ErrClassCanceled)
		if !signalsCancel {
			t.Errorf("error = %v, want context.Canceled or a canceled *ProviderError", err)
		}
	})

	t.Run("canceled_before_the_call", func(t *testing.T) {
		// A dead context must not be multiplied across the chain either.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		a := newProvider("a", succeed())
		b := newProvider("b", succeed())
		c := newProvider("c", succeed())
		h := newHarness(t, fastOptions(5), a, b, c)

		_, err := h.router.Chat(ctx, chatReq(false))
		if err == nil {
			t.Fatal("Chat succeeded on an already canceled context")
		}
		if got := a.calls(); got > 1 {
			t.Errorf("provider a calls = %d, want at most 1", got)
		}
		if got := b.calls() + c.calls(); got != 0 {
			t.Errorf("calls to the rest of the chain = %d, want 0", got)
		}
	})
}

// --- attempts ---------------------------------------------------------

func TestRouterAttemptsMirrorEveryUpstreamCall(t *testing.T) {
	// Attempts is what a trace and an on call engineer read to explain a
	// slow answer, so it must be one honest entry per upstream call, in
	// the order the calls happened.
	a := newProvider("a", failWith("a", providers.ErrClassRateLimited))
	b := newProvider("b", failWith("b", providers.ErrClassAuth))
	c := newProvider("c", succeed())
	h := newHarness(t, fastOptions(3), a, b, c)

	if _, err := h.router.Chat(context.Background(), chatReq(false)); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	atts := h.attempts()
	if len(atts) != h.totalCalls() {
		t.Fatalf("Attempts() length = %d, want one per upstream call (%d)", len(atts), h.totalCalls())
	}
	want := []struct {
		provider string
		class    providers.ErrorClass
		failed   bool
	}{
		{"a", providers.ErrClassRateLimited, true},
		{"b", providers.ErrClassAuth, true},
		// A success has no error class, so the zero value is the honest
		// record: anything else invents a failure that did not happen.
		{"c", "", false},
	}
	if len(atts) != len(want) {
		t.Fatalf("attempts = %v, want %d entries", h.attemptNames(), len(want))
	}
	for i, w := range want {
		got := atts[i]
		if got.Provider != w.provider {
			t.Errorf("attempt %d provider = %q, want %q", i, got.Provider, w.provider)
		}
		if got.Class != w.class {
			t.Errorf("attempt %d class = %q, want %q", i, got.Class, w.class)
		}
		if w.failed && got.Err == nil {
			t.Errorf("attempt %d has no Err, want the failure it recorded", i)
		}
		if !w.failed && got.Err != nil {
			t.Errorf("attempt %d Err = %v, want nil on a success", i, got.Err)
		}
		if got.Duration < 0 {
			t.Errorf("attempt %d duration = %v, want a non negative value", i, got.Duration)
		}
	}
}

func TestRouterAttemptsCoverOnlyTheMostRecentCall(t *testing.T) {
	// Attempts describes the latest request. If entries accumulated across
	// requests the slice would grow without bound and every trace after
	// the first would be a lie.
	a := newProvider("a", failWith("a", providers.ErrClassRateLimited), succeed())
	b := newProvider("b", succeed())
	h := newHarness(t, fastOptions(3), a, b)

	if _, err := h.router.Chat(context.Background(), chatReq(false)); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if got := h.attemptNames(); !sameNames(got, []string{"a", "b"}) {
		t.Fatalf("attempts after the first call = %v, want [a b]", got)
	}

	if _, err := h.router.Chat(context.Background(), chatReq(false)); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if got := h.attemptNames(); !sameNames(got, []string{"a"}) {
		t.Errorf("attempts after the second call = %v, want just [a]", got)
	}
}
