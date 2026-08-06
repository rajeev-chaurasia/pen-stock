package router

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// Router serves one model name from a chain of providers.
type Router struct {
	name   string
	names  []string
	byName map[string]providers.Provider
	health Health
	sel    Selector
	opts   Options
	clock  Clock

	// mu guards attempts only. Attempts describes the most recent call
	// and exists for tracing and tests, so under concurrent traffic it
	// reflects whichever call finished last rather than all of them.
	mu       sync.Mutex
	attempts []Attempt
}

// New builds a routed provider over chain, in configured priority order.
func New(name string, chain []providers.Provider, h Health, sel Selector, opts Options, clock Clock) (providers.Provider, error) {
	if len(chain) == 0 {
		return nil, fmt.Errorf("router %q: needs at least one provider", name)
	}
	if h == nil {
		return nil, fmt.Errorf("router %q: needs a health tracker", name)
	}
	if sel == nil {
		return nil, fmt.Errorf("router %q: needs a selector", name)
	}
	if clock == nil {
		clock = realClock{}
	}

	r := &Router{
		name:   name,
		names:  make([]string, 0, len(chain)),
		byName: make(map[string]providers.Provider, len(chain)),
		health: h,
		sel:    sel,
		opts:   opts.withDefaults(),
		clock:  clock,
	}
	for _, p := range chain {
		if _, dup := r.byName[p.Name()]; dup {
			return nil, fmt.Errorf("router %q: duplicate provider %q in chain", name, p.Name())
		}
		r.names = append(r.names, p.Name())
		r.byName[p.Name()] = p
	}
	return r, nil
}

// Name returns the routed model label, not the provider that answers it.
func (r *Router) Name() string { return r.name }

// Attempts returns the upstream calls made for the most recent request,
// in order.
func (r *Router) Attempts() []Attempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Attempt(nil), r.attempts...)
}

func (r *Router) resetAttempts() {
	r.mu.Lock()
	r.attempts = nil
	r.mu.Unlock()
}

func (r *Router) recordAttempt(a Attempt) {
	r.mu.Lock()
	r.attempts = append(r.attempts, a)
	r.mu.Unlock()
}

func (r *Router) Chat(ctx context.Context, req *providers.ChatRequest) (*providers.ChatResponse, error) {
	return runChain(r, ctx, func(ctx context.Context, p providers.Provider) (*providers.ChatResponse, error) {
		resp, err := p.Chat(ctx, req)
		if err != nil {
			return nil, err
		}
		// Success is known the moment the call returns, so latency here
		// is the whole round trip.
		r.health.RecordSuccess(p.Name(), 0, r.clock.Now())
		return resp, nil
	})
}

func (r *Router) ChatStream(ctx context.Context, req *providers.ChatRequest) (providers.StreamReader, error) {
	return runChain(r, ctx, func(ctx context.Context, p providers.Provider) (providers.StreamReader, error) {
		reader, err := p.ChatStream(ctx, req)
		if err != nil {
			return nil, err
		}
		// Health is recorded at the first chunk rather than here: what
		// matters for latency aware routing is time to first token, and
		// a provider that accepts a connection then says nothing has not
		// actually served anything yet.
		return &timedReader{
			inner:    reader,
			provider: p.Name(),
			health:   r.health,
			clock:    r.clock,
			start:    time.Now(),
		}, nil
	})
}

// runChain walks the selected providers applying the failure policy. It
// is generic so the streaming and non streaming paths share one loop
// and cannot drift apart.
func runChain[T any](r *Router, ctx context.Context, call func(context.Context, providers.Provider) (T, error)) (T, error) {
	var zero T
	r.resetAttempts()

	now := r.clock.Now()
	order := r.sel.Order(r.names, r.health, now)
	if len(order) == 0 {
		// Every provider is in cooldown or broken. Saying so beats
		// hammering an upstream we already know will refuse.
		return zero, &providers.ProviderError{
			Provider: r.name,
			Class:    providers.ErrClassUpstream,
			Message:  "no healthy provider is available for this model",
		}
	}

	var lastErr error
	used := 0
	for i, name := range order {
		p, ok := r.byName[name]
		if !ok {
			continue
		}
		// Reserve one attempt for each provider still untried, so a
		// retry loop on a sick provider cannot eat the whole budget and
		// starve a healthy peer further down the chain.
		reserved := len(order) - (i + 1)

		for {
			if used >= r.opts.MaxAttempts {
				return zero, r.exhausted(lastErr)
			}
			if err := ctx.Err(); err != nil {
				return zero, canceledError(r.name, err)
			}

			started := time.Now()
			result, err := call(ctx, p)
			used++
			if err == nil {
				r.recordAttempt(Attempt{Provider: name, Duration: time.Since(started)})
				return result, nil
			}

			class := classOf(err)
			r.recordAttempt(Attempt{
				Provider: name,
				Class:    class,
				Err:      err,
				Duration: time.Since(started),
			})
			if countsAgainstHealth(class) {
				r.health.RecordFailure(name, class, 0, r.clock.Now())
			}
			lastErr = err

			if ctx.Err() != nil && class == providers.ErrClassCanceled {
				return zero, err
			}

			switch classDisposition(class) {
			case dispositionFail:
				return zero, err
			case dispositionRetry:
				if used+reserved >= r.opts.MaxAttempts {
					// No room to retry without stranding a peer.
					break
				}
				r.backoff(ctx, used)
				continue
			case dispositionFailover:
			}
			break
		}
	}
	return zero, r.exhausted(lastErr)
}

// backoff waits an exponentially growing, fully jittered interval so a
// chain of gateways retrying together does not resynchronize into a
// thundering herd.
func (r *Router) backoff(ctx context.Context, attempt int) {
	delay := r.opts.RetryBaseDelay << min(attempt-1, 16)
	if delay > r.opts.MaxRetryDelay || delay <= 0 {
		delay = r.opts.MaxRetryDelay
	}
	// Jitter spreads retries; it decides nothing secret.
	// #nosec G404
	jittered := time.Duration(rand.Int64N(int64(delay) + 1))

	timer := time.NewTimer(jittered)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// exhausted reports the failure the caller should see once the chain is
// spent: the last upstream answer, since that is the freshest evidence.
func (r *Router) exhausted(last error) error {
	if last != nil {
		return last
	}
	return &providers.ProviderError{
		Provider: r.name,
		Class:    providers.ErrClassUpstream,
		Message:  "no provider attempt was made for this model",
	}
}

func canceledError(name string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &providers.ProviderError{
			Provider: name,
			Class:    providers.ErrClassTimeout,
			Message:  "deadline exceeded before another provider could be tried",
			Err:      err,
		}
	}
	return err
}

// classOf extracts the routing class from any error crossing a provider
// boundary. An error without a class is treated as the provider's fault
// rather than the caller's, which keeps an unknown failure eligible for
// failover.
func classOf(err error) providers.ErrorClass {
	var pe *providers.ProviderError
	if errors.As(err, &pe) {
		return pe.Class
	}
	switch {
	case errors.Is(err, context.Canceled):
		return providers.ErrClassCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return providers.ErrClassTimeout
	default:
		return providers.ErrClassInternal
	}
}

// timedReader reports health once the upstream actually produces a
// token, and otherwise stays out of the way of the stream.
type timedReader struct {
	inner    providers.StreamReader
	provider string
	health   Health
	clock    Clock
	start    time.Time
	recorded bool
}

func (t *timedReader) Recv() (providers.StreamChunk, error) {
	chunk, err := t.inner.Recv()
	if err == nil && !t.recorded && !chunk.Keepalive {
		t.recorded = true
		t.health.RecordSuccess(t.provider, time.Since(t.start), t.clock.Now())
	}
	return chunk, err
}

func (t *timedReader) Close() error { return t.inner.Close() }

// AnsweringProvider names the upstream serving this stream, so cost and
// latency land on it rather than on the routed model's label.
func (t *timedReader) AnsweringProvider() string { return t.provider }
