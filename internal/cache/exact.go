package cache

import (
	"bytes"
	"context"
	"sync"
	"time"
)

const (
	// DefaultExactMaxEntries bounds a cache configured without a limit.
	// A cache with no ceiling is a memory leak with a friendly name, so
	// an unset limit means this rather than none. It is sized to hold a
	// working set of a few tens of megabytes of stored answers.
	DefaultExactMaxEntries = 4096

	// DefaultExactTTL bounds how long an answer may be reused when no
	// lifetime is configured. An entry that never expires outlives the
	// model version that produced it, so an unset lifetime means this
	// rather than forever.
	DefaultExactTTL = 5 * time.Minute
)

// ExactOptions configures an exact match cache. The zero value is
// usable: it takes the documented default bound and lifetime, the real
// clock, and reports no events.
type ExactOptions struct {
	// MaxEntries is the ceiling across all tenants. Zero or less means
	// DefaultExactMaxEntries.
	MaxEntries int
	// TTL is how long a stored answer may be served. Zero or less means
	// DefaultExactTTL.
	TTL time.Duration
	// Clock supplies the current time. Leave it nil for time.Now; tests
	// set it so expiry can be driven rather than waited on.
	Clock func() time.Time
	// OnEvent reports each outcome. It exists so the ingress can drive
	// metrics without this package importing a metrics library, and it
	// is called with no lock held so an implementation may call back
	// into the cache.
	OnEvent func(Event)
}

// exactKey is the part of a Key that identifies a request within one
// tenant. Tenant is deliberately absent: it selects the map this key is
// looked up in, so a lookup has no way to name another tenant's entry.
type exactKey struct {
	Model string
	Hash  string
}

// exactNode is a stored answer and its place in the recency order. The
// list is intrusive rather than a container/list so that following the
// order never needs a type assertion to get back to the entry.
type exactNode struct {
	tenant    string
	key       exactKey
	entry     *Entry
	expiresAt time.Time
	prev      *exactNode
	next      *exactNode
}

// Exact is an in memory Cache holding whole answers under the exact
// request that produced them.
//
// Tenants are separated structurally: entries live in a map per tenant,
// and a Get reaches that map before it looks at a hash. Two tenants
// whose requests hashed identically still get their own answers, because
// no lookup ever consults a map it was not handed by its own tenant.
type Exact struct {
	mu         sync.Mutex
	byTenant   map[string]map[exactKey]*exactNode
	head       *exactNode
	tail       *exactNode
	size       int
	maxEntries int
	ttl        time.Duration
	clock      func() time.Time
	onEvent    func(Event)
}

// NewExact returns a Cache safe for concurrent use.
func NewExact(opts ExactOptions) Cache {
	maxEntries := opts.MaxEntries
	if maxEntries <= 0 {
		maxEntries = DefaultExactMaxEntries
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultExactTTL
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Exact{
		byTenant:   make(map[string]map[exactKey]*exactNode),
		maxEntries: maxEntries,
		ttl:        ttl,
		clock:      clock,
		onEvent:    opts.OnEvent,
	}
}

// Get returns a stored answer for an exactly matching request.
//
// An expired entry is a miss and is dropped on the way out, so a stale
// answer is never served and never lingers once it has been asked for.
func (c *Exact) Get(_ context.Context, k Key) (*Entry, bool) {
	entry, ok := c.lookup(k)
	if !ok {
		c.emit(EventMiss)
		return nil, false
	}
	c.emit(EventExactHit)
	return entry, true
}

// Put stores an answer, replacing any entry already held under the same
// key and refreshing its lifetime.
func (c *Exact) Put(_ context.Context, k Key, e *Entry) {
	if e == nil {
		return
	}
	evicted := c.insert(k, c.snapshot(e))
	for range evicted {
		c.emit(EventEvicted)
	}
	c.emit(EventStored)
}

// Len reports how many entries are held. Expiry is lazy, so an entry
// that has aged out but has not been asked for since is still counted.
func (c *Exact) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size
}

func (c *Exact) lookup(k Key) (*Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	byKey, ok := c.byTenant[k.Tenant]
	if !ok {
		return nil, false
	}
	node, ok := byKey[exactKey{Model: k.Model, Hash: k.Hash}]
	if !ok {
		return nil, false
	}
	if !c.clock().Before(node.expiresAt) {
		c.drop(node)
		return nil, false
	}
	c.unlink(node)
	c.pushFront(node)
	return c.snapshot(node.entry), true
}

// insert stores entry and returns how many entries were evicted to make
// room for it.
func (c *Exact) insert(k Key, entry *Entry) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.clock()
	if entry.StoredAt.IsZero() {
		entry.StoredAt = now
	}
	expiresAt := now.Add(c.ttl)
	bodyKey := exactKey{Model: k.Model, Hash: k.Hash}

	byKey, ok := c.byTenant[k.Tenant]
	if !ok {
		byKey = make(map[exactKey]*exactNode)
		c.byTenant[k.Tenant] = byKey
	}
	if node, ok := byKey[bodyKey]; ok {
		node.entry = entry
		node.expiresAt = expiresAt
		c.unlink(node)
		c.pushFront(node)
		return 0
	}

	node := &exactNode{tenant: k.Tenant, key: bodyKey, entry: entry, expiresAt: expiresAt}
	byKey[bodyKey] = node
	c.pushFront(node)
	c.size++

	evicted := 0
	for c.size > c.maxEntries && c.tail != nil {
		c.drop(c.tail)
		evicted++
	}
	return evicted
}

// snapshot copies an Entry's payloads. Storing the caller's slices would
// let a caller reusing its buffer rewrite an answer already stored, and
// returning them would let a caller editing a served body corrupt the
// entry behind it. Both are silent, so both are copied.
func (*Exact) snapshot(e *Entry) *Entry {
	dup := *e
	dup.Body = bytes.Clone(e.Body)
	if e.Frames != nil {
		dup.Frames = make([][]byte, len(e.Frames))
		for i, frame := range e.Frames {
			dup.Frames[i] = bytes.Clone(frame)
		}
	}
	return &dup
}

// emit reports an outcome. It is called without the lock held: an
// OnEvent that reads the cache while updating a metric would otherwise
// deadlock against the call that reported to it.
func (c *Exact) emit(ev Event) {
	if c.onEvent != nil {
		c.onEvent(ev)
	}
}

// drop removes a node from the order and from its tenant's map. The
// caller must hold the lock.
func (c *Exact) drop(node *exactNode) {
	c.unlink(node)
	if byKey, ok := c.byTenant[node.tenant]; ok {
		delete(byKey, node.key)
		// An emptied tenant map is deleted too, so a gateway that sees
		// many short lived tenants does not keep a map per tenant it has
		// ever served.
		if len(byKey) == 0 {
			delete(c.byTenant, node.tenant)
		}
	}
	c.size--
}

// pushFront makes node the most recently used. The caller must hold the
// lock.
func (c *Exact) pushFront(node *exactNode) {
	node.prev = nil
	node.next = c.head
	if c.head != nil {
		c.head.prev = node
	}
	c.head = node
	if c.tail == nil {
		c.tail = node
	}
}

// unlink takes node out of the recency order. The caller must hold the
// lock.
func (c *Exact) unlink(node *exactNode) {
	if node.prev != nil {
		node.prev.next = node.next
	} else if c.head == node {
		c.head = node.next
	}
	if node.next != nil {
		node.next.prev = node.prev
	} else if c.tail == node {
		c.tail = node.prev
	}
	node.prev = nil
	node.next = nil
}
