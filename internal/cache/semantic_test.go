package cache

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"
)

// unit returns a vector scaled to length 1, so tests can reason about
// cosine values by hand.
func unit(v ...float32) []float32 {
	var mag float64
	for _, x := range v {
		mag += float64(x) * float64(x)
	}
	mag = math.Sqrt(mag)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / mag)
	}
	return out
}

func entry(body string) *Entry {
	return &Entry{Body: []byte(body), Provider: "test", Model: "test-model"}
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 2, 3}, []float32{1, 2, 3}, 1},
		{"identical unit", unit(3, 4), unit(3, 4), 1},
		{"parallel different magnitude", []float32{1, 2, 3}, []float32{2, 4, 6}, 1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"orthogonal in higher dimensions", []float32{0, 1, 0, 0}, []float32{0, 0, 1, 0}, 0},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1},
		{"zero vector on the left", []float32{0, 0}, []float32{1, 1}, 0},
		{"zero vector on the right", []float32{1, 1}, []float32{0, 0}, 0},
		{"both zero", []float32{0, 0}, []float32{0, 0}, 0},
		{"width mismatch", []float32{1, 0}, []float32{1, 0, 0}, 0},
		{"empty", []float32{}, []float32{}, 0},
		{"nan input", []float32{float32(math.NaN()), 1}, []float32{1, 1}, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cosineSimilarity(tc.a, tc.b)
			if math.IsNaN(got) {
				t.Fatalf("cosineSimilarity returned NaN for %v and %v", tc.a, tc.b)
			}
			if got != tc.want {
				t.Fatalf("cosineSimilarity = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCosineSimilarityNeverExceedsOne(t *testing.T) {
	// An identical pair is the case where accumulated rounding can push
	// the ratio past 1, so it is the one worth pinning.
	v := unit(0.31, 0.72, 0.19, 0.55, 0.08)
	if got := cosineSimilarity(v, v); got != 1 {
		t.Fatalf("identical vectors scored %v, want exactly 1", got)
	}
}

func TestNearestExactMatchScoresOne(t *testing.T) {
	s := NewSemantic(SemanticOptions{})
	ctx := context.Background()
	v := []float32{1, 2, 3}
	want := entry("stored")

	s.Add(ctx, "acme", v, want)

	got, score, ok := s.Nearest(ctx, "acme", []float32{1, 2, 3})
	if !ok {
		t.Fatal("an identical vector missed")
	}
	if got != want {
		t.Fatalf("got entry %q, want %q", got.Body, want.Body)
	}
	if score != 1 {
		t.Fatalf("score = %v, want exactly 1", score)
	}
}

func TestNearestOrthogonalMisses(t *testing.T) {
	s := NewSemantic(SemanticOptions{})
	ctx := context.Background()
	s.Add(ctx, "acme", []float32{1, 0, 0}, entry("stored"))

	got, score, ok := s.Nearest(ctx, "acme", []float32{0, 1, 0})
	if ok {
		t.Fatalf("an orthogonal vector hit with score %v", score)
	}
	if got != nil || score != 0 {
		t.Fatalf("miss returned entry %v and score %v, want nil and 0", got, score)
	}
}

func TestNearestNearAboveThresholdHitsAndBelowMisses(t *testing.T) {
	ctx := context.Background()
	stored := unit(1, 0, 0)

	// A near paraphrase: a small tilt off the stored direction, well
	// inside the default threshold.
	near := unit(1, 0.05, 0)
	// A related but different question: close enough to be tempting,
	// nowhere near a paraphrase.
	far := unit(1, 1, 0)

	if got := cosineSimilarity(stored, near); got <= DefaultSimilarityThreshold {
		t.Fatalf("test vector is not above the threshold: %v", got)
	}
	if got := cosineSimilarity(stored, far); got >= DefaultSimilarityThreshold {
		t.Fatalf("test vector is not below the threshold: %v", got)
	}

	s := NewSemantic(SemanticOptions{})
	s.Add(ctx, "acme", stored, entry("stored"))

	if _, _, ok := s.Nearest(ctx, "acme", near); !ok {
		t.Fatal("a vector above the threshold missed")
	}
	if _, _, ok := s.Nearest(ctx, "acme", far); ok {
		t.Fatal("a vector below the threshold hit")
	}
}

func TestNearestThresholdBoundary(t *testing.T) {
	ctx := context.Background()
	stored := unit(1, 0)
	query := unit(4, 3)
	score := cosineSimilarity(stored, query)

	// At the threshold exactly, the entry is a hit. One representable
	// step stricter, the same pair is a miss. Both sides of the boundary
	// are pinned so a later change to the comparison cannot slide it.
	t.Run("at the threshold", func(t *testing.T) {
		s := NewSemantic(SemanticOptions{Threshold: score})
		s.Add(ctx, "acme", stored, entry("stored"))
		got, gotScore, ok := s.Nearest(ctx, "acme", query)
		if !ok {
			t.Fatalf("similarity %v missed at threshold %v", score, score)
		}
		if got == nil || gotScore != score {
			t.Fatalf("score = %v, want %v", gotScore, score)
		}
	})

	t.Run("one step above the threshold", func(t *testing.T) {
		stricter := math.Nextafter(score, 1)
		s := NewSemantic(SemanticOptions{Threshold: stricter})
		s.Add(ctx, "acme", stored, entry("stored"))
		if _, _, ok := s.Nearest(ctx, "acme", query); ok {
			t.Fatalf("similarity %v hit at threshold %v", score, stricter)
		}
	})
}

func TestDefaultThresholdIsConservative(t *testing.T) {
	// The default is a correctness control, not a tuning knob, so its
	// floor is asserted rather than left to review.
	threshold := DefaultSimilarityThreshold
	if threshold < 0.95 {
		t.Fatalf("default threshold %v is looser than 0.95", threshold)
	}

	ctx := context.Background()
	// A threshold left unset, or set to something meaningless, must not
	// produce a store that hits on anything.
	for _, opts := range []SemanticOptions{{}, {Threshold: 0}, {Threshold: -1}} {
		s := NewSemantic(opts)
		s.Add(ctx, "acme", unit(1, 0, 0), entry("stored"))
		if _, _, ok := s.Nearest(ctx, "acme", unit(1, 1, 0)); ok {
			t.Fatalf("options %+v produced a store that hit at similarity 0.707", opts)
		}
	}
}

func TestZeroVectorIsSafe(t *testing.T) {
	ctx := context.Background()
	s := NewSemantic(SemanticOptions{})

	s.Add(ctx, "acme", []float32{0, 0, 0}, entry("zero"))
	s.Add(ctx, "acme", []float32{1, 2, 3}, entry("real"))

	// A zero vector has no direction, so it matches nothing, including
	// the zero vector already stored.
	got, score, ok := s.Nearest(ctx, "acme", []float32{0, 0, 0})
	if ok {
		t.Fatalf("a zero vector hit entry %q with score %v", got.Body, score)
	}
	if math.IsNaN(score) {
		t.Fatal("a zero vector produced a NaN score")
	}

	// The stored zero vector must not poison a lookup that should hit.
	if _, _, ok := s.Nearest(ctx, "acme", []float32{1, 2, 3}); !ok {
		t.Fatal("a stored zero vector broke an otherwise exact match")
	}
}

func TestDimensionMismatchIsRefused(t *testing.T) {
	ctx := context.Background()
	s := NewSemantic(SemanticOptions{})
	s.Add(ctx, "acme", []float32{1, 2, 3}, entry("stored"))

	// A query of another width is refused rather than compared: a
	// truncated comparison would return a confident number about two
	// vectors that do not live in the same space.
	if _, _, ok := s.Nearest(ctx, "acme", []float32{1, 2}); ok {
		t.Fatal("a narrower query vector hit")
	}
	if _, _, ok := s.Nearest(ctx, "acme", []float32{1, 2, 3, 4}); ok {
		t.Fatal("a wider query vector hit")
	}

	// A stored vector of another width is dropped rather than kept where
	// it could be compared later.
	s.Add(ctx, "acme", []float32{9, 9}, entry("wrong width"))
	if _, _, ok := s.Nearest(ctx, "acme", []float32{9, 9}); ok {
		t.Fatal("a vector of the wrong width was stored and retrieved")
	}
	if got, _, ok := s.Nearest(ctx, "acme", []float32{1, 2, 3}); !ok || string(got.Body) != "stored" {
		t.Fatal("the correctly sized entry did not survive a rejected add")
	}
}

func TestTenantIsolation(t *testing.T) {
	ctx := context.Background()
	s := NewSemantic(SemanticOptions{})

	// The two tenants hold vectors that are all but identical, so
	// anything short of structural separation would let one answer for
	// the other.
	acmeVec := unit(1, 0, 0)
	globexVec := unit(1, 0.001, 0)
	if got := cosineSimilarity(acmeVec, globexVec); got <= DefaultSimilarityThreshold {
		t.Fatalf("test vectors are not near identical: %v", got)
	}

	s.Add(ctx, "acme", acmeVec, entry("acme answer"))
	s.Add(ctx, "globex", globexVec, entry("globex answer"))

	// Each tenant retrieves its own answer even though the other tenant
	// holds a vector that would clear the threshold.
	for _, tc := range []struct {
		tenant string
		query  []float32
		want   string
	}{
		{"acme", acmeVec, "acme answer"},
		{"acme", globexVec, "acme answer"},
		{"globex", globexVec, "globex answer"},
		{"globex", acmeVec, "globex answer"},
	} {
		got, _, ok := s.Nearest(ctx, tc.tenant, tc.query)
		if !ok {
			t.Fatalf("tenant %q missed its own entry", tc.tenant)
		}
		if string(got.Body) != tc.want {
			t.Fatalf("tenant %q got %q, want %q", tc.tenant, got.Body, tc.want)
		}
	}

	// A tenant that has stored nothing sees nothing, however close the
	// other tenants' vectors are.
	if got, _, ok := s.Nearest(ctx, "initech", acmeVec); ok {
		t.Fatalf("an empty tenant retrieved %q from another tenant", got.Body)
	}
}

func TestTTLExpiryViaClock(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	s := NewSemantic(SemanticOptions{
		TTL:   time.Minute,
		Clock: func() time.Time { return now },
	})
	v := []float32{1, 2, 3}
	s.Add(ctx, "acme", v, entry("stored"))

	if _, _, ok := s.Nearest(ctx, "acme", v); !ok {
		t.Fatal("a fresh entry missed")
	}

	now = now.Add(59 * time.Second)
	if _, _, ok := s.Nearest(ctx, "acme", v); !ok {
		t.Fatal("an entry inside its TTL missed")
	}

	// The boundary counts as expired.
	now = now.Add(time.Second)
	if _, _, ok := s.Nearest(ctx, "acme", v); ok {
		t.Fatal("an entry at exactly its TTL still hit")
	}

	now = now.Add(time.Hour)
	if _, _, ok := s.Nearest(ctx, "acme", v); ok {
		t.Fatal("an expired entry still hit")
	}

	// A later write reclaims the expired record, and the fresh one it
	// wrote is served.
	s.Add(ctx, "acme", v, entry("fresh"))
	got, _, ok := s.Nearest(ctx, "acme", v)
	if !ok || string(got.Body) != "fresh" {
		t.Fatal("a replacement entry was not served after expiry")
	}
}

func TestEvictionIsBoundedPerTenant(t *testing.T) {
	ctx := context.Background()
	events := map[Event]int{}
	s := NewSemantic(SemanticOptions{
		MaxPerTenant: 2,
		OnEvent:      func(e Event) { events[e]++ },
	})

	// Mutually orthogonal vectors, so each one only ever matches itself.
	first := []float32{1, 0, 0}
	second := []float32{0, 1, 0}
	third := []float32{0, 0, 1}

	s.Add(ctx, "acme", first, entry("first"))
	s.Add(ctx, "acme", second, entry("second"))
	if events[EventEvicted] != 0 {
		t.Fatalf("evicted %d entries while inside the bound", events[EventEvicted])
	}

	s.Add(ctx, "acme", third, entry("third"))
	if events[EventEvicted] != 1 {
		t.Fatalf("evicted %d entries, want 1", events[EventEvicted])
	}

	if _, _, ok := s.Nearest(ctx, "acme", first); ok {
		t.Fatal("the oldest entry survived eviction")
	}
	for _, v := range [][]float32{second, third} {
		if _, _, ok := s.Nearest(ctx, "acme", v); !ok {
			t.Fatal("an entry inside the bound was evicted")
		}
	}

	// The bound is per tenant, so a busy tenant cannot evict a quiet
	// one's entries.
	s.Add(ctx, "globex", first, entry("globex first"))
	s.Add(ctx, "acme", first, entry("acme again"))
	s.Add(ctx, "acme", second, entry("acme again too"))
	if _, _, ok := s.Nearest(ctx, "globex", first); !ok {
		t.Fatal("one tenant's writes evicted another tenant's entry")
	}

	if events[EventSemanticHit] == 0 {
		t.Fatal("no semantic hit was reported")
	}
}

func TestHitEventIsReportedOnlyOnAHit(t *testing.T) {
	ctx := context.Background()
	var hits int
	s := NewSemantic(SemanticOptions{
		OnEvent: func(e Event) {
			if e == EventSemanticHit {
				hits++
			}
		},
	})
	s.Add(ctx, "acme", []float32{1, 0, 0}, entry("stored"))

	if _, _, ok := s.Nearest(ctx, "acme", []float32{0, 1, 0}); ok {
		t.Fatal("an orthogonal query hit")
	}
	if hits != 0 {
		t.Fatalf("a miss reported %d hits", hits)
	}

	if _, _, ok := s.Nearest(ctx, "acme", []float32{1, 0, 0}); !ok {
		t.Fatal("an exact query missed")
	}
	if hits != 1 {
		t.Fatalf("reported %d hits, want 1", hits)
	}
}

func TestAddIgnoresUnusableInput(t *testing.T) {
	ctx := context.Background()
	s := NewSemantic(SemanticOptions{})

	s.Add(ctx, "acme", nil, entry("no vector"))
	s.Add(ctx, "acme", []float32{}, entry("empty vector"))
	s.Add(ctx, "acme", []float32{1, 2, 3}, nil)

	if _, _, ok := s.Nearest(ctx, "acme", []float32{1, 2, 3}); ok {
		t.Fatal("an unusable add was stored")
	}
	if _, _, ok := s.Nearest(ctx, "acme", nil); ok {
		t.Fatal("an empty query hit")
	}
}

func TestStoredVectorIsIndependentOfCallerSlice(t *testing.T) {
	ctx := context.Background()
	s := NewSemantic(SemanticOptions{})

	// A caller reusing its buffer for the next embedding must not
	// rewrite what the store already holds.
	buf := []float32{1, 0, 0}
	s.Add(ctx, "acme", buf, entry("stored"))
	buf[0], buf[1] = 0, 1

	if _, _, ok := s.Nearest(ctx, "acme", []float32{1, 0, 0}); !ok {
		t.Fatal("mutating the caller's slice changed the stored vector")
	}
}

func TestConcurrentUse(t *testing.T) {
	ctx := context.Background()
	s := NewSemantic(SemanticOptions{
		MaxPerTenant: 8,
		TTL:          time.Hour,
		OnEvent:      func(Event) {},
	})
	tenants := []string{"acme", "globex", "initech"}

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tenant := tenants[i%len(tenants)]
			v := unit(float32(i+1), float32(i%3), 1)
			for range 50 {
				s.Add(ctx, tenant, v, entry("stored"))
				s.Nearest(ctx, tenant, v)
				s.Nearest(ctx, tenant, []float32{1, 2})
			}
		}()
	}
	wg.Wait()

	// The store is still coherent, and still bounded, after the storm.
	for _, tenant := range tenants {
		v := unit(1, 0, 1)
		s.Add(ctx, tenant, v, entry("final"))
		if _, _, ok := s.Nearest(ctx, tenant, v); !ok {
			t.Fatalf("tenant %q lost an entry written after concurrent use", tenant)
		}
	}
}
