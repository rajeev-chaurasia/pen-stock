package main

import (
	"context"
	"fmt"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/cache"
)

const (
	// embedAttempts is how many times one batch is tried before the run
	// gives up. A rate limited batch two thirds of the way through a
	// sweep would otherwise throw away every vector already paid for.
	embedAttempts int = 4

	// embedBackoff is the pause added per failed attempt. It is long
	// because the failure being waited out is a per minute quota, and a
	// retry that arrives inside the same window only spends more of it.
	embedBackoff time.Duration = 30 * time.Second

	// embedWindow is the period the item budget is measured over.
	embedWindow time.Duration = time.Minute
)

// embedPacer holds the run under a per minute item budget.
//
// The quota is counted in embedded texts, not in HTTP calls: a batch of
// fifty spends fifty. Without pacing a fast run trips the limit a
// quarter of the way in, and every retry then spends more of the same
// window it is waiting on.
type embedPacer struct {
	budget      int
	sent        int
	windowStart time.Time
}

// wait blocks until n more items fit inside the budget.
func (p *embedPacer) wait(ctx context.Context, n int) error {
	if p.budget <= 0 {
		return nil
	}
	now := time.Now()
	if p.windowStart.IsZero() || now.Sub(p.windowStart) >= embedWindow {
		p.windowStart, p.sent = now, 0
	}
	if p.sent+n <= p.budget {
		p.sent += n
		return nil
	}

	pause := embedWindow - now.Sub(p.windowStart)
	select {
	case <-ctx.Done():
		return fmt.Errorf("waiting for the embedding rate window: %w", ctx.Err())
	case <-time.After(pause):
	}
	p.windowStart, p.sent = time.Now(), n
	return nil
}

// embedAll returns one vector per distinct text.
//
// Everything is embedded once, here, and the map is reused for every
// threshold in the sweep. Re-embedding per threshold would multiply the
// cost by the number of sweep points and, worse, would let a change in
// the upstream model mid run show up as a threshold effect.
func embedAll(ctx context.Context, embedder cache.Embedder, texts []string, batchSize, perMinute int, report func(done, total int)) (map[string][]float32, error) {
	if batchSize <= 0 {
		return nil, fmt.Errorf("embed batch size must be positive, got %d", batchSize)
	}
	vectors := make(map[string][]float32, len(texts))
	pacer := &embedPacer{budget: perMinute}

	for start := 0; start < len(texts); start += batchSize {
		end := min(start+batchSize, len(texts))
		batch := texts[start:end]

		if err := pacer.wait(ctx, len(batch)); err != nil {
			return nil, err
		}
		got, err := embedBatch(ctx, embedder, batch)
		if err != nil {
			return nil, fmt.Errorf("embed texts %d to %d: %w", start, end-1, err)
		}
		for i, text := range batch {
			vectors[text] = got[i]
		}
		if report != nil {
			report(end, len(texts))
		}
	}
	return vectors, nil
}

// embedBatch retries a batch a few times. A transient rate limit is the
// expected failure and it is worth waiting out; anything else fails the
// same way after the last attempt, wrapped so the cause survives.
func embedBatch(ctx context.Context, embedder cache.Embedder, batch []string) ([][]float32, error) {
	var lastErr error
	for attempt := 1; attempt <= embedAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("waiting to retry: %w", ctx.Err())
			case <-time.After(embedBackoff):
			}
		}
		got, err := embedder.Embed(ctx, batch)
		if err == nil {
			if len(got) != len(batch) {
				return nil, fmt.Errorf("embedder returned %d vectors for %d texts", len(got), len(batch))
			}
			return got, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("after %d attempts: %w", embedAttempts, lastErr)
}
