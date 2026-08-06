package cache

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// exactClock is a hand wound clock, so expiry is driven rather than
// waited on and a TTL test costs no wall time.
type exactClock struct {
	mu sync.Mutex
	at time.Time
}

func newExactClock() *exactClock {
	return &exactClock{at: time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)}
}

func (c *exactClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *exactClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// exactEvents collects what the cache reported, which is the only way
// the ingress learns of a hit, a miss, or an eviction.
type exactEvents struct {
	mu   sync.Mutex
	seen []Event
}

func (e *exactEvents) record(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, ev)
}

func (e *exactEvents) list() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Event(nil), e.seen...)
}

func (e *exactEvents) count(want Event) int {
	n := 0
	for _, ev := range e.list() {
		if ev == want {
			n++
		}
	}
	return n
}

func newExactForTest(t *testing.T, opts ExactOptions) *Exact {
	t.Helper()
	c, ok := NewExact(opts).(*Exact)
	if !ok {
		t.Fatalf("NewExact returned something other than an *Exact")
	}
	return c
}

func exactTestKey(tenant, hash string) Key {
	return Key{Tenant: tenant, Model: testModel, Hash: hash}
}

func requireBody(t *testing.T, e *Entry, want string) {
	t.Helper()
	if e == nil {
		t.Fatalf("no entry, want body %q", want)
	}
	if string(e.Body) != want {
		t.Errorf("body is %q, want %q", e.Body, want)
	}
}

func TestExactStoresAndServesAnAnswer(t *testing.T) {
	events := &exactEvents{}
	c := newExactForTest(t, ExactOptions{OnEvent: events.record})
	ctx := context.Background()
	k := exactTestKey(testTenant, "hash-1")

	if _, ok := c.Get(ctx, k); ok {
		t.Fatal("an empty cache answered a Get")
	}
	c.Put(ctx, k, &Entry{
		Body:     []byte(`{"choices":[]}`),
		Usage:    testUsage(),
		USD:      0.004,
		Provider: "openai",
		Model:    testModel,
	})

	got, ok := c.Get(ctx, k)
	if !ok {
		t.Fatal("a stored answer was not served back")
	}
	requireBody(t, got, `{"choices":[]}`)
	if got.USD != 0.004 || got.Usage.TotalTokens != 30 || got.Provider != "openai" {
		t.Errorf("the saving a hit reports was not preserved: %+v", got)
	}
	if c.Len() != 1 {
		t.Errorf("Len is %d, want 1", c.Len())
	}

	want := []Event{EventMiss, EventStored, EventExactHit}
	if got := events.list(); !slices.Equal(got, want) {
		t.Errorf("events were %v, want %v", got, want)
	}
}

// TestExactIsolatesTenantsThatShareAHash builds the situation a hash
// collision would create, on purpose, and asserts the cache cannot cross
// it. Isolation here is structural: the tenant selects the map, so there
// is no lookup that could reach another tenant's entry to begin with.
func TestExactIsolatesTenantsThatShareAHash(t *testing.T) {
	c := newExactForTest(t, ExactOptions{})
	ctx := context.Background()

	const shared = "0000000000000000000000000000000000000000000000000000000000000000"
	acme := exactTestKey("acme", shared)
	globex := exactTestKey("globex", shared)

	c.Put(ctx, acme, &Entry{Body: []byte("acme confidential")})
	c.Put(ctx, globex, &Entry{Body: []byte("globex confidential")})

	got, ok := c.Get(ctx, acme)
	if !ok {
		t.Fatal("acme lost its own entry")
	}
	requireBody(t, got, "acme confidential")

	got, ok = c.Get(ctx, globex)
	if !ok {
		t.Fatal("globex lost its own entry")
	}
	requireBody(t, got, "globex confidential")

	if _, ok := c.Get(ctx, exactTestKey("initech", shared)); ok {
		t.Error("a tenant that stored nothing was served another tenant's answer")
	}
	if c.Len() != 2 {
		t.Errorf("Len is %d, want 2: one identical hash overwrote the other tenant", c.Len())
	}

	// The separation is in the shape of the store, not in a filter that
	// a future call site could forget to apply.
	if len(c.byTenant) != 2 {
		t.Fatalf("entries live in %d tenant maps, want 2", len(c.byTenant))
	}
	for tenant, byKey := range c.byTenant {
		if len(byKey) != 1 {
			t.Errorf("tenant %q holds %d entries, want 1", tenant, len(byKey))
		}
	}
}

// TestExactSeparatesModelsWithinATenant covers the other half of the
// key: one tenant asking one question of two models gets two answers.
func TestExactSeparatesModelsWithinATenant(t *testing.T) {
	c := newExactForTest(t, ExactOptions{})
	ctx := context.Background()

	mini := Key{Tenant: testTenant, Model: "gpt-4o-mini", Hash: "shared-hash"}
	full := Key{Tenant: testTenant, Model: "gpt-4o", Hash: "shared-hash"}

	c.Put(ctx, mini, &Entry{Body: []byte("small model answer")})
	c.Put(ctx, full, &Entry{Body: []byte("large model answer")})

	got, ok := c.Get(ctx, mini)
	if !ok {
		t.Fatal("the small model entry vanished")
	}
	requireBody(t, got, "small model answer")

	got, ok = c.Get(ctx, full)
	if !ok {
		t.Fatal("the large model entry vanished")
	}
	requireBody(t, got, "large model answer")
}

func TestExactExpiredEntryIsAMissAndIsRemoved(t *testing.T) {
	clock := newExactClock()
	events := &exactEvents{}
	c := newExactForTest(t, ExactOptions{TTL: time.Minute, Clock: clock.now, OnEvent: events.record})
	ctx := context.Background()
	k := exactTestKey(testTenant, "hash-1")

	c.Put(ctx, k, &Entry{Body: []byte("answer")})

	clock.advance(59 * time.Second)
	if _, ok := c.Get(ctx, k); !ok {
		t.Fatal("an entry inside its lifetime was treated as expired")
	}

	// A hit orders the entry more recently but does not extend its life,
	// so the deadline set at store time still holds.
	clock.advance(time.Second)
	if _, ok := c.Get(ctx, k); ok {
		t.Error("an expired entry was served")
	}
	if c.Len() != 0 {
		t.Errorf("Len is %d after expiry, want 0: the entry was not removed", c.Len())
	}
	if events.count(EventEvicted) != 0 {
		t.Error("expiry was reported as an eviction, which would misread capacity pressure")
	}
	if events.count(EventMiss) != 1 {
		t.Errorf("expiry reported %d misses, want 1", events.count(EventMiss))
	}
}

func TestExactUsesADefaultLifetimeWhenNoneIsConfigured(t *testing.T) {
	clock := newExactClock()
	c := newExactForTest(t, ExactOptions{TTL: 0, Clock: clock.now})
	ctx := context.Background()
	k := exactTestKey(testTenant, "hash-1")

	c.Put(ctx, k, &Entry{Body: []byte("answer")})

	clock.advance(DefaultExactTTL - time.Second)
	if _, ok := c.Get(ctx, k); !ok {
		t.Fatal("the default lifetime expired early")
	}
	clock.advance(time.Second)
	if _, ok := c.Get(ctx, k); ok {
		t.Error("an unset TTL meant no expiry at all")
	}
}

func TestExactEvictsTheLeastRecentlyUsed(t *testing.T) {
	events := &exactEvents{}
	c := newExactForTest(t, ExactOptions{MaxEntries: 2, OnEvent: events.record})
	ctx := context.Background()
	first := exactTestKey(testTenant, "hash-1")
	second := exactTestKey(testTenant, "hash-2")
	third := exactTestKey(testTenant, "hash-3")

	c.Put(ctx, first, &Entry{Body: []byte("first")})
	c.Put(ctx, second, &Entry{Body: []byte("second")})

	// Reading the first entry makes the second the least recently used.
	if _, ok := c.Get(ctx, first); !ok {
		t.Fatal("the first entry was already gone")
	}
	c.Put(ctx, third, &Entry{Body: []byte("third")})

	if c.Len() != 2 {
		t.Errorf("Len is %d, want 2: the cache grew past its limit", c.Len())
	}
	if _, ok := c.Get(ctx, second); ok {
		t.Error("the least recently used entry survived while a newer one was stored")
	}
	if got, ok := c.Get(ctx, first); !ok {
		t.Error("the recently read entry was evicted instead")
	} else {
		requireBody(t, got, "first")
	}
	if got, ok := c.Get(ctx, third); !ok {
		t.Error("the newest entry was not stored")
	} else {
		requireBody(t, got, "third")
	}
	if n := events.count(EventEvicted); n != 1 {
		t.Errorf("%d evictions were reported, want 1", n)
	}
}

func TestExactBoundsItselfWhenNoLimitIsConfigured(t *testing.T) {
	c := newExactForTest(t, ExactOptions{MaxEntries: 0})
	ctx := context.Background()

	for i := range DefaultExactMaxEntries + 16 {
		k := exactTestKey(testTenant, "hash-"+strconv.Itoa(i))
		c.Put(ctx, k, &Entry{Body: []byte("answer")})
	}

	if got := c.Len(); got != DefaultExactMaxEntries {
		t.Errorf("Len is %d, want %d: an unset limit meant unlimited", got, DefaultExactMaxEntries)
	}
}

// TestExactEvictionReleasesTheTenantMap keeps a gateway that serves many
// short lived tenants from retaining a map per tenant it has ever seen.
func TestExactEvictionReleasesTheTenantMap(t *testing.T) {
	c := newExactForTest(t, ExactOptions{MaxEntries: 1})
	ctx := context.Background()

	c.Put(ctx, exactTestKey("acme", "hash-1"), &Entry{Body: []byte("acme")})
	c.Put(ctx, exactTestKey("globex", "hash-1"), &Entry{Body: []byte("globex")})

	if len(c.byTenant) != 1 {
		t.Errorf("%d tenant maps are held after the only acme entry was evicted, want 1", len(c.byTenant))
	}
	if _, ok := c.byTenant["acme"]; ok {
		t.Error("an emptied tenant map was retained")
	}
}

// TestExactCopiesTheEntryOnPut covers a caller that reuses its response
// buffer. Without a copy the stored answer would change underneath the
// cache, silently, long after the Put returned.
func TestExactCopiesTheEntryOnPut(t *testing.T) {
	c := newExactForTest(t, ExactOptions{})
	ctx := context.Background()
	k := exactTestKey(testTenant, "hash-1")

	body := []byte("original body")
	frames := [][]byte{[]byte("frame one"), []byte("frame two")}
	entry := &Entry{Body: body, Frames: frames}

	c.Put(ctx, k, entry)

	// The caller reuses everything it handed over.
	copy(body, "overwritten!!")
	copy(frames[0], "clobbered")
	frames[1] = []byte("replaced")
	entry.Body = []byte("a different body entirely")
	entry.USD = 99

	got, ok := c.Get(ctx, k)
	if !ok {
		t.Fatal("the entry disappeared")
	}
	requireBody(t, got, "original body")
	requireFrames(t, got.Frames, "frame one", "frame two")
	if got.USD != 0 {
		t.Errorf("USD is %v, want 0: the stored entry aliased the caller's struct", got.USD)
	}
}

// TestExactCopiesTheEntryOnGet covers the other direction: a gateway
// that rewrites an id or a timestamp in the body it is about to serve
// must not rewrite the entry behind it.
func TestExactCopiesTheEntryOnGet(t *testing.T) {
	c := newExactForTest(t, ExactOptions{})
	ctx := context.Background()
	k := exactTestKey(testTenant, "hash-1")

	c.Put(ctx, k, &Entry{Body: []byte("original body"), Frames: [][]byte{[]byte("frame one")}})

	first, ok := c.Get(ctx, k)
	if !ok {
		t.Fatal("the entry disappeared")
	}
	copy(first.Body, "overwritten!!")
	copy(first.Frames[0], "clobbered")

	second, ok := c.Get(ctx, k)
	if !ok {
		t.Fatal("the entry disappeared after being read")
	}
	requireBody(t, second, "original body")
	requireFrames(t, second.Frames, "frame one")
}

func TestExactReplacesAnEntryUnderTheSameKey(t *testing.T) {
	clock := newExactClock()
	c := newExactForTest(t, ExactOptions{TTL: time.Minute, Clock: clock.now})
	ctx := context.Background()
	k := exactTestKey(testTenant, "hash-1")

	c.Put(ctx, k, &Entry{Body: []byte("stale answer")})
	clock.advance(30 * time.Second)
	c.Put(ctx, k, &Entry{Body: []byte("fresh answer")})

	if c.Len() != 1 {
		t.Errorf("Len is %d, want 1: a replacement was stored as a second entry", c.Len())
	}

	// The replacement carries its own lifetime, so the original deadline
	// no longer applies.
	clock.advance(45 * time.Second)
	got, ok := c.Get(ctx, k)
	if !ok {
		t.Fatal("a replaced entry expired on the deadline of the entry it replaced")
	}
	requireBody(t, got, "fresh answer")
}

func TestExactStampsStoredAtWhenTheCallerDidNot(t *testing.T) {
	clock := newExactClock()
	c := newExactForTest(t, ExactOptions{Clock: clock.now})
	ctx := context.Background()

	stamped := exactTestKey(testTenant, "hash-1")
	provided := exactTestKey(testTenant, "hash-2")
	upstreamAt := clock.now().Add(-time.Hour)

	c.Put(ctx, stamped, &Entry{Body: []byte("answer")})
	c.Put(ctx, provided, &Entry{Body: []byte("answer"), StoredAt: upstreamAt})

	got, ok := c.Get(ctx, stamped)
	if !ok {
		t.Fatal("the entry disappeared")
	}
	if !got.StoredAt.Equal(clock.now()) {
		t.Errorf("StoredAt is %v, want %v", got.StoredAt, clock.now())
	}

	got, ok = c.Get(ctx, provided)
	if !ok {
		t.Fatal("the entry disappeared")
	}
	if !got.StoredAt.Equal(upstreamAt) {
		t.Errorf("StoredAt is %v, want the caller's %v", got.StoredAt, upstreamAt)
	}
}

func TestExactIgnoresANilEntry(t *testing.T) {
	events := &exactEvents{}
	c := newExactForTest(t, ExactOptions{OnEvent: events.record})
	ctx := context.Background()
	k := exactTestKey(testTenant, "hash-1")

	c.Put(ctx, k, nil)

	if c.Len() != 0 {
		t.Errorf("Len is %d after storing nothing, want 0", c.Len())
	}
	if events.count(EventStored) != 0 {
		t.Error("a nil entry was reported as stored")
	}
	if _, ok := c.Get(ctx, k); ok {
		t.Error("a nil entry was served back")
	}
}

// TestExactEmitsEventsWithoutHoldingItsLock pins the ordering that lets
// a metrics sink read the cache it is being told about. If events were
// emitted under the lock this test would not fail, it would hang, so the
// failure mode is a timeout rather than an assertion.
func TestExactEmitsEventsWithoutHoldingItsLock(t *testing.T) {
	var (
		mu       sync.Mutex
		nested   bool
		observed int
	)
	var c *Exact
	c = newExactForTest(t, ExactOptions{OnEvent: func(Event) {
		_ = c.Len()

		mu.Lock()
		first := !nested
		nested = true
		observed++
		mu.Unlock()

		if first {
			// A sink that looks something up while recording must not
			// deadlock against the call that reported to it.
			c.Get(context.Background(), exactTestKey(testTenant, "hash-nested"))
		}
	}})

	c.Put(context.Background(), exactTestKey(testTenant, "hash-1"), &Entry{Body: []byte("answer")})

	mu.Lock()
	defer mu.Unlock()
	if observed < 2 {
		t.Errorf("observed %d events, want the outer store and the nested lookup", observed)
	}
}

// TestExactSurvivesConcurrentUse hammers every entry point at once. It
// asserts the invariants that a data race would break, so it is written
// to be meaningful under the race detector in CI.
func TestExactSurvivesConcurrentUse(t *testing.T) {
	const (
		goroutines = 16
		iterations = 200
		distinct   = 24
		maxEntries = 8
	)

	events := &exactEvents{}
	c := newExactForTest(t, ExactOptions{MaxEntries: maxEntries, OnEvent: events.record})
	tenants := []string{"acme", "globex", "initech"}
	ctx := context.Background()

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range iterations {
				tenant := tenants[(g+i)%len(tenants)]
				n := (g*7 + i) % distinct
				k := exactTestKey(tenant, "hash-"+strconv.Itoa(n))
				want := fmt.Sprintf("answer-%s-%d", tenant, n)

				body := []byte(want)
				frame := []byte(want)
				c.Put(ctx, k, &Entry{Body: body, Frames: [][]byte{frame}})

				// Reusing the buffers immediately: the stored copy must
				// not follow them.
				body[0] = 'X'
				frame[0] = 'X'

				// An entry may have been evicted by another goroutine,
				// but whatever comes back must be whole and must belong
				// to this tenant.
				if got, ok := c.Get(ctx, k); ok {
					if string(got.Body) != want {
						t.Errorf("Get returned body %q, want %q", got.Body, want)
					}
					if len(got.Frames) != 1 || string(got.Frames[0]) != want {
						t.Errorf("Get returned frames %q, want one frame %q", got.Frames, want)
					}
				}
				if n := c.Len(); n > maxEntries {
					t.Errorf("Len is %d, above the limit of %d", n, maxEntries)
				}
			}
		}()
	}
	wg.Wait()

	if got := c.Len(); got > maxEntries {
		t.Errorf("Len settled at %d, above the limit of %d", got, maxEntries)
	}
	if got := events.count(EventStored); got != goroutines*iterations {
		t.Errorf("%d stores were reported, want %d", got, goroutines*iterations)
	}
	if events.count(EventEvicted) == 0 {
		t.Error("no eviction was reported, so the bounded path was never exercised")
	}
}

func testUsage() providers.Usage {
	return providers.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30}
}

func requireFrames(t *testing.T, got [][]byte, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d frames, want %d", len(got), len(want))
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Errorf("frame %d is %q, want %q", i, got[i], want[i])
		}
	}
}
