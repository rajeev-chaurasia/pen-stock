package cache

import (
	"context"
	"errors"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

type stubEmbedder struct {
	vec  []float32
	err  error
	call int
}

func (s *stubEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	s.call++
	if s.err != nil {
		return nil, s.err
	}
	return [][]float32{s.vec}, nil
}

func (s *stubEmbedder) Dimensions() int { return len(s.vec) }

func newLookup(t *testing.T, emb Embedder, events *[]Event) *Lookup {
	t.Helper()
	record := func(e Event) { *events = append(*events, e) }
	var sem Semantic
	if emb != nil {
		sem = NewSemantic(SemanticOptions{OnEvent: record})
	}
	return NewLookup(LookupOptions{
		Exact:    NewExact(ExactOptions{OnEvent: record}),
		Semantic: sem,
		Embedder: emb,
		OnEvent:  record,
	})
}

const cacheableBody = `{"model":"m","temperature":0,"messages":[{"role":"user","content":"what is a penstock"}]}`

func TestLookupExactHitAfterStore(t *testing.T) {
	var events []Event
	l := newLookup(t, nil, &events)
	ctx := context.Background()

	first := l.Get(ctx, "acme", "m", []byte(cacheableBody))
	if first.Entry != nil {
		t.Fatal("first lookup hit an empty cache")
	}
	if !first.Eligible {
		t.Fatal("a zero temperature request should be eligible")
	}

	l.Put(ctx, first, &Entry{Body: []byte(`{"ok":true}`), Usage: providers.Usage{TotalTokens: 9}}, []byte(cacheableBody))

	second := l.Get(ctx, "acme", "m", []byte(cacheableBody))
	if second.Entry == nil {
		t.Fatal("second lookup missed a stored answer")
	}
	if second.Semantic {
		t.Error("an identical request should hit the exact tier, not the semantic one")
	}
	if got := string(second.Entry.Body); got != `{"ok":true}` {
		t.Errorf("body = %s, want the stored answer", got)
	}
}

// The two are not the same thing: a miss means the cache has not seen
// this yet, a refusal means it never will. An operator reading a low hit
// rate needs to tell them apart.
func TestLookupDistinguishesRefusalFromMiss(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Event
	}{
		{"eligible request misses", cacheableBody, EventMiss},
		{
			name: "sampling asked to vary is refused",
			body: `{"model":"m","temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`,
			want: EventIneligible,
		},
		{
			name: "a tool call is refused, since replaying it repeats a side effect",
			body: `{"model":"m","temperature":0,"tools":[{"type":"function"}],"messages":[{"role":"user","content":"hi"}]}`,
			want: EventIneligible,
		},
		{
			name: "an unreadable body is refused rather than guessed at",
			body: `{"model":`,
			want: EventIneligible,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var events []Event
			l := newLookup(t, nil, &events)

			res := l.Get(context.Background(), "acme", "m", []byte(tc.body))
			if res.Entry != nil {
				t.Error("nothing was stored, so nothing should have been found")
			}
			if len(events) == 0 || events[len(events)-1] != tc.want {
				t.Errorf("events = %v, want the last one to be %q", events, tc.want)
			}
			if tc.want == EventIneligible && res.Eligible {
				t.Error("a refused request must not be reported as eligible")
			}
		})
	}
}

func TestLookupDoesNotStoreARefusedRequest(t *testing.T) {
	var events []Event
	l := newLookup(t, nil, &events)
	ctx := context.Background()
	varying := `{"model":"m","temperature":0.9,"messages":[{"role":"user","content":"hi"}]}`

	res := l.Get(ctx, "acme", "m", []byte(varying))
	l.Put(ctx, res, &Entry{Body: []byte(`{"a":1}`)}, []byte(varying))

	if got := l.exact.Len(); got != 0 {
		t.Errorf("cache holds %d entries, want 0 for a refused request", got)
	}
}

// A cache whose embedder is down must cost a hit, never a request.
func TestLookupSurvivesAFailingEmbedder(t *testing.T) {
	var events []Event
	emb := &stubEmbedder{vec: []float32{1, 0, 0}, err: errors.New("embedder unreachable")}
	l := newLookup(t, emb, &events)
	ctx := context.Background()

	res := l.Get(ctx, "acme", "m", []byte(cacheableBody))
	if !res.Eligible {
		t.Fatal("an embedder failure must not make a request ineligible")
	}
	if res.Entry != nil {
		t.Fatal("nothing is stored yet")
	}
	l.Put(ctx, res, &Entry{Body: []byte(`{"ok":true}`)}, []byte(cacheableBody))

	// The exact tier still works, which is the whole point of degrading
	// rather than failing.
	again := l.Get(ctx, "acme", "m", []byte(cacheableBody))
	if again.Entry == nil {
		t.Error("the exact tier stopped working when the embedder failed")
	}
	if !containsEvent(events, EventEmbedFailed) {
		t.Error("an embedder failure should be visible in the events")
	}
}

func TestLookupSemanticHitOnADifferentWording(t *testing.T) {
	// Both requests embed to the same vector here, which is what a
	// paraphrase looks like to the store. The point under test is the
	// wiring: a different body, so no exact hit, still answered.
	var events []Event
	emb := &stubEmbedder{vec: []float32{0, 1, 0}}
	l := newLookup(t, emb, &events)
	ctx := context.Background()

	stored := `{"model":"m","temperature":0,"messages":[{"role":"user","content":"what is a penstock"}]}`
	res := l.Get(ctx, "acme", "m", []byte(stored))
	l.Put(ctx, res, &Entry{Body: []byte(`{"answer":"a pipe"}`)}, []byte(stored))

	paraphrase := `{"model":"m","temperature":0,"messages":[{"role":"user","content":"describe a penstock please"}]}`
	hit := l.Get(ctx, "acme", "m", []byte(paraphrase))
	if hit.Entry == nil {
		t.Fatal("a paraphrase did not reach the semantic tier")
	}
	if !hit.Semantic {
		t.Error("the hit should be reported as semantic, not exact")
	}
	if hit.Similarity < DefaultSimilarityThreshold {
		t.Errorf("similarity = %v, want at least the threshold", hit.Similarity)
	}
}

// Tenancy is the one property that is not a performance question.
func TestLookupNeverCrossesTenants(t *testing.T) {
	var events []Event
	emb := &stubEmbedder{vec: []float32{1, 0, 0}}
	l := newLookup(t, emb, &events)
	ctx := context.Background()

	res := l.Get(ctx, "acme", "m", []byte(cacheableBody))
	l.Put(ctx, res, &Entry{Body: []byte(`{"secret":"acme only"}`)}, []byte(cacheableBody))

	// Same question, same embedding, different tenant.
	other := l.Get(ctx, "globex", "m", []byte(cacheableBody))
	if other.Entry != nil {
		t.Fatalf("tenant globex received acme's answer: %s", other.Entry.Body)
	}
}

func TestDisabledLookupIsInert(t *testing.T) {
	var l *Lookup
	if l.Enabled() {
		t.Error("a nil lookup reports itself enabled")
	}
	res := l.Get(context.Background(), "acme", "m", []byte(cacheableBody))
	if res.Entry != nil || res.Eligible {
		t.Error("a nil lookup should find nothing and claim nothing")
	}
	l.Put(context.Background(), res, &Entry{}, []byte(cacheableBody))
}

func TestPromptTextIncludesRoles(t *testing.T) {
	// The same words from a system turn and a user turn are not the same
	// request, so the role has to be part of what is compared.
	system := `{"messages":[{"role":"system","content":"be terse"}]}`
	user := `{"messages":[{"role":"user","content":"be terse"}]}`
	if PromptText([]byte(system)) == PromptText([]byte(user)) {
		t.Error("role is not part of the compared text")
	}

	// An unreadable body must not reduce to the empty string, or every
	// unreadable request would look identical to every other one.
	if got := PromptText([]byte(`{"messages":`)); got != "" {
		t.Errorf("PromptText on a broken body = %q", got)
	}
}

func containsEvent(events []Event, want Event) bool {
	for _, e := range events {
		if e == want {
			return true
		}
	}
	return false
}

func countEvents(events []Event) map[Event]int {
	counts := make(map[Event]int, len(events))
	for _, e := range events {
		counts[e]++
	}
	return counts
}

// assertCounts checks the whole event set, not just the events named, so
// a stray extra event fails rather than passing unnoticed. That is the
// difference that matters here: every assertion in this file used to ask
// whether an event was present, which a doubled count answers yes to.
func assertCounts(t *testing.T, events []Event, want map[Event]int) {
	t.Helper()
	got := countEvents(events)
	for e, n := range want {
		if got[e] != n {
			t.Errorf("event %q count = %d, want %d (all events: %v)", e, got[e], n, events)
		}
	}
	for e, n := range got {
		if _, expected := want[e]; !expected {
			t.Errorf("unexpected event %q emitted %d times (all events: %v)", e, n, events)
		}
	}
}

// One callback is wired to the exact tier, the semantic tier and the
// Lookup that owns them. Each tier reports the outcomes it alone can
// see, so nothing may be reported twice.
//
// This shipped doubled: Lookup emitted exact_hit, semantic_hit, miss and
// stored on top of the tier that had already emitted them, which made
// penstock_cache_events_total wrong and the published hit ratio wrong
// with it. It survived because every assertion asked whether an event
// was present rather than how often.
func TestEachCacheEventIsCountedOnce(t *testing.T) {
	ctx := context.Background()
	const paraphrase = `{"model":"m","temperature":0,"messages":[{"role":"user","content":"describe a penstock please"}]}`

	t.Run("miss then store", func(t *testing.T) {
		var events []Event
		l := newLookup(t, nil, &events)

		res := l.Get(ctx, "acme", "m", []byte(cacheableBody))
		l.Put(ctx, res, &Entry{Body: []byte(`{"ok":true}`)}, []byte(cacheableBody))

		assertCounts(t, events, map[Event]int{EventMiss: 1, EventStored: 1})
	})

	t.Run("exact hit", func(t *testing.T) {
		var events []Event
		l := newLookup(t, nil, &events)

		res := l.Get(ctx, "acme", "m", []byte(cacheableBody))
		l.Put(ctx, res, &Entry{Body: []byte(`{"ok":true}`)}, []byte(cacheableBody))
		events = nil

		if hit := l.Get(ctx, "acme", "m", []byte(cacheableBody)); hit.Entry == nil {
			t.Fatal("the repeat did not hit")
		}
		assertCounts(t, events, map[Event]int{EventExactHit: 1})
	})

	t.Run("semantic hit is preceded by one miss", func(t *testing.T) {
		var events []Event
		emb := &stubEmbedder{vec: []float32{0, 1, 0}}
		l := newLookup(t, emb, &events)

		res := l.Get(ctx, "acme", "m", []byte(cacheableBody))
		l.Put(ctx, res, &Entry{Body: []byte(`{"answer":"a pipe"}`)}, []byte(cacheableBody))
		events = nil

		hit := l.Get(ctx, "acme", "m", []byte(paraphrase))
		if hit.Entry == nil || !hit.Semantic {
			t.Fatal("the paraphrase did not reach the semantic tier")
		}
		// The miss is real: the exact tier genuinely did not have it.
		assertCounts(t, events, map[Event]int{EventMiss: 1, EventSemanticHit: 1})
	})

	t.Run("ineligible refuses without counting a miss", func(t *testing.T) {
		var events []Event
		l := newLookup(t, nil, &events)

		// A temperature above the ceiling asked for variety, so it is
		// refused rather than looked up. Counting a miss here would make
		// the hit ratio look worse than the cache is behaving.
		hot := `{"model":"m","temperature":1,"messages":[{"role":"user","content":"hi"}]}`
		if res := l.Get(ctx, "acme", "m", []byte(hot)); res.Eligible {
			t.Fatal("a hot request was treated as cacheable")
		}
		assertCounts(t, events, map[Event]int{EventIneligible: 1})
	})
}
