package cache

import (
	"context"
	"math"
	"slices"
	"sync"
	"time"
)

const (
	// DefaultSimilarityThreshold is how close a stored question has to be
	// before its answer is reused. It is deliberately strict.
	//
	// Similarity is not equivalence. Two prompts can sit at 0.90 while
	// asking opposite things, because an embedding of "how do I enable
	// this" and one of "how do I disable this" differ in a single token
	// that the model spends most of its capacity ignoring. At 0.95 a
	// candidate has to be close to a paraphrase before it answers for
	// another prompt.
	//
	// The asymmetry settles the number: a false hit is a confidently
	// wrong answer that nobody traces back to the cache, while a miss
	// costs one ordinary API call. Trading hit rate for that is cheap.
	DefaultSimilarityThreshold = 0.95

	// DefaultMaxPerTenant bounds how many vectors one tenant may hold.
	// The bound is what keeps an exhaustive scan honest: 1024 vectors of
	// 768 floats is under a million multiply-adds, which is noise next to
	// the provider call a hit avoids.
	DefaultMaxPerTenant = 1024
)

// SemanticOptions configures a Semantic store. The zero value is usable:
// an unset threshold, bound, or clock takes the documented default,
// because a semantic cache configured by accident should still be a
// conservative one.
type SemanticOptions struct {
	// Threshold is the minimum cosine similarity for a hit. Values at or
	// below zero take DefaultSimilarityThreshold; a zero threshold would
	// make an unrelated vector a hit, which is never what an operator
	// leaving the field empty meant.
	Threshold float64
	// MaxPerTenant bounds entries per tenant. Zero or less takes
	// DefaultMaxPerTenant.
	MaxPerTenant int
	// TTL expires an entry by age. Zero or less means entries live until
	// they are evicted for space.
	TTL time.Duration
	// Clock supplies the current time, so expiry is testable without
	// sleeping. Nil takes time.Now.
	Clock func() time.Time
	// OnEvent reports outcomes for metrics. It is called without the
	// store's lock held, so an implementation may call back in.
	OnEvent func(Event)
}

// semanticRecord is one stored vector and the answer it stands for.
type semanticRecord struct {
	vector []float32
	entry  *Entry
	// storedAt comes from the injected clock rather than Entry.StoredAt,
	// which describes when the upstream answered rather than when this
	// tier took custody of it.
	storedAt time.Time
}

// semanticStore is an in-memory Semantic implementation.
//
// Vectors are held in a map keyed by tenant, one independent set each.
// That is the whole isolation mechanism: a lookup reaches exactly one
// set, and there is no collection of all vectors that a lookup could
// forget to filter. Tenancy is not configurable here for the reason
// given in the package doc.
type semanticStore struct {
	threshold    float64
	maxPerTenant int
	ttl          time.Duration
	clock        func() time.Time
	onEvent      func(Event)

	mu sync.RWMutex
	// dims is the vector width this store has adopted, learned from the
	// first vector it accepts. One gateway runs one embedding model, and
	// the store dies with the process, so there is no width to migrate:
	// a vector of another width is a misconfiguration and is refused
	// rather than compared.
	dims    int
	tenants map[string][]semanticRecord
}

// NewSemantic returns an in-memory Semantic store, safe for concurrent
// use.
func NewSemantic(opts SemanticOptions) Semantic {
	if opts.Threshold <= 0 {
		opts.Threshold = DefaultSimilarityThreshold
	}
	if opts.MaxPerTenant <= 0 {
		opts.MaxPerTenant = DefaultMaxPerTenant
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return &semanticStore{
		threshold:    opts.Threshold,
		maxPerTenant: opts.MaxPerTenant,
		ttl:          opts.TTL,
		clock:        opts.Clock,
		onEvent:      opts.OnEvent,
		tenants:      make(map[string][]semanticRecord),
	}
}

// Nearest returns the closest entry this tenant holds, if it clears the
// threshold. A miss reports a zero score: the caller's next move is the
// same either way, and a score attached to a miss invites treating it as
// a weak hit.
func (s *semanticStore) Nearest(_ context.Context, tenant string, embedding []float32) (*Entry, float64, bool) {
	if len(embedding) == 0 {
		return nil, 0, false
	}
	now := s.clock()

	s.mu.RLock()
	// The lock is held across the scan rather than around a slice header
	// copy, because a concurrent Add rewrites the backing array in place.
	var (
		entry *Entry
		score float64
	)
	if len(embedding) == s.dims {
		entry, score = scanTenant(s.tenants[tenant], embedding, s.threshold, s.ttl, now)
	}
	s.mu.RUnlock()

	if entry == nil {
		return nil, 0, false
	}
	s.emit(EventSemanticHit)
	return entry, score, true
}

// Add stores an entry against its embedding, evicting to stay within the
// per-tenant bound. A vector of the wrong width, or an absent entry, is
// dropped: a store that accepted them would only be able to report their
// nonsense later.
func (s *semanticStore) Add(_ context.Context, tenant string, embedding []float32, e *Entry) {
	if e == nil || len(embedding) == 0 {
		return
	}
	now := s.clock()

	s.mu.Lock()
	if s.dims == 0 {
		s.dims = len(embedding)
	}
	if len(embedding) != s.dims {
		s.mu.Unlock()
		return
	}

	records, removed := s.dropExpired(s.tenants[tenant], now)
	records = append(records, semanticRecord{
		// The vector is copied because the caller owns the slice it
		// passed and may reuse its array for the next embedding, which
		// would silently rewrite what this store thinks it stored.
		vector:   slices.Clone(embedding),
		entry:    e,
		storedAt: now,
	})
	if excess := len(records) - s.maxPerTenant; excess > 0 {
		records = slices.Delete(records, 0, excess)
		removed += excess
	}
	s.tenants[tenant] = records
	s.mu.Unlock()

	for range removed {
		s.emit(EventEvicted)
	}
}

// dropExpired removes records past their TTL and reports how many went.
// Expiry is reclaimed on write so that a read needs only a read lock;
// reads skip expired records instead of removing them, so a stale record
// is never served in the meantime.
func (s *semanticStore) dropExpired(records []semanticRecord, now time.Time) ([]semanticRecord, int) {
	if s.ttl <= 0 || len(records) == 0 {
		return records, 0
	}
	// Records are appended in clock order, so the expired ones are a
	// prefix. A clock that runs backwards costs a delayed cleanup here
	// and nothing else, since reads check each record's own age.
	cut := 0
	for cut < len(records) && expired(records[cut].storedAt, s.ttl, now) {
		cut++
	}
	if cut == 0 {
		return records, 0
	}
	return slices.Delete(records, 0, cut), cut
}

func (s *semanticStore) emit(e Event) {
	if s.onEvent != nil {
		s.onEvent(e)
	}
}

// scanTenant returns the best record in one tenant's set, or nil when
// nothing clears the threshold. The set is its only input: no other
// tenant's vectors are reachable from this function, so a cross-tenant
// answer is not a thing it could return even if asked wrongly.
//
// The scan is exhaustive on purpose. Against a bounded per-tenant set
// this is a sub-millisecond loop over contiguous floats, so an
// approximate nearest neighbour index would trade an exact answer, a
// vector database, and its operational surface for latency the caller
// cannot measure.
func scanTenant(records []semanticRecord, query []float32, threshold float64, ttl time.Duration, now time.Time) (*Entry, float64) {
	var best *Entry
	var bestScore float64
	for i := range records {
		r := &records[i]
		if expired(r.storedAt, ttl, now) {
			continue
		}
		score := cosineSimilarity(query, r.vector)
		if score < threshold {
			continue
		}
		if best == nil || score > bestScore {
			best, bestScore = r.entry, score
		}
	}
	return best, bestScore
}

// expired reports whether a record has reached its TTL. The boundary is
// inclusive, which resolves the ambiguous instant towards the miss.
func expired(storedAt time.Time, ttl time.Duration, now time.Time) bool {
	return ttl > 0 && now.Sub(storedAt) >= ttl
}

// cosineSimilarity is the cosine of the angle between two vectors, in
// [-1, 1].
//
// It reports 0 rather than NaN for the degenerate cases: mismatched
// widths, and a vector with no magnitude. An all-zero vector has no
// direction, so it has no similarity to anything, and the division that
// would express that is 0/0. Returning 0 keeps every comparison ordered
// and below any sane threshold, where a NaN would poison comparisons
// silently instead.
//
// Sums accumulate in float64: 768 float32 products lose real precision,
// and the loss lands exactly where the threshold decision is made.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, magA, magB float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		magA += x * x
		magB += y * y
	}
	if magA == 0 || magB == 0 {
		return 0
	}
	sim := dot / math.Sqrt(magA*magB)
	// Rounding can push an identical pair a hair past 1, which would let
	// a threshold of exactly 1 admit vectors that are merely very close.
	// A NaN can only arrive from a NaN in the input, and is answered the
	// same way as any other absence of a real angle.
	switch {
	case math.IsNaN(sim):
		return 0
	case sim > 1:
		return 1
	case sim < -1:
		return -1
	default:
		return sim
	}
}
