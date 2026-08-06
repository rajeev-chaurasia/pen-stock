package cache

import (
	"context"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// Entry is a stored answer plus what it cost to produce, so a hit can
// report the saving it represents rather than merely being fast.
type Entry struct {
	// Body is the complete non streaming response as the provider sent
	// it, already in the OpenAI shape the gateway serves.
	Body []byte
	// Frames are the SSE payloads of a streamed answer, in order and
	// without the terminating sentinel, so a hit can be replayed as a
	// stream to a caller that asked for one.
	Frames [][]byte
	// Usage and USD describe the original upstream call. A hit reuses
	// them to report what was avoided, and must never add them to a
	// tenant's spend, since no provider was paid this time.
	Usage    providers.Usage
	USD      float64
	Provider string
	Model    string
	StoredAt time.Time
}

// Streamed reports whether this entry came from a streaming call.
func (e *Entry) Streamed() bool { return len(e.Frames) > 0 }

// Key identifies a cacheable request. Tenant is part of the key rather
// than a filter applied afterwards, because a filter is something that
// can be forgotten at one call site.
type Key struct {
	Tenant string
	Model  string
	// Hash covers the request body's semantic content: the messages and
	// every parameter that changes the answer.
	Hash string
}

// Cache stores and retrieves answers. Implementations must be safe for
// concurrent use.
type Cache interface {
	// Get returns a stored answer for an exactly matching request.
	Get(ctx context.Context, k Key) (*Entry, bool)
	// Put stores an answer. A failure to store is not an error worth
	// failing a request over, so it is reported through metrics rather
	// than returned.
	Put(ctx context.Context, k Key, e *Entry)
	// Len reports how many entries are held, for tests and operator
	// visibility.
	Len() int
}

// Semantic finds an answer to a question close enough to one already
// asked. It is separate from Cache because the exact tier is always
// safe and always available, while this one depends on an embedder that
// may be unconfigured or unreachable.
type Semantic interface {
	// Nearest returns the best entry above the similarity threshold,
	// searching only within the given tenant.
	Nearest(ctx context.Context, tenant string, embedding []float32) (*Entry, float64, bool)
	// Add stores an entry against its embedding.
	Add(ctx context.Context, tenant string, embedding []float32, e *Entry)
}

// Embedder turns a request's text into a vector. Implementations reach
// a model, so they can fail and must be given a context.
type Embedder interface {
	// Embed returns one vector per input, in the same order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Dimensions reports the vector width, so a store can reject a
	// mismatched vector rather than silently comparing nonsense.
	Dimensions() int
}

// Event names a cache outcome for metrics. A miss and a refusal are
// different things: one means the cache has not seen this yet, the
// other means policy will never store it, and an operator reading a low
// hit rate needs to know which.
type Event string

const (
	EventExactHit    Event = "exact_hit"
	EventSemanticHit Event = "semantic_hit"
	EventMiss        Event = "miss"
	EventIneligible  Event = "ineligible"
	EventStored      Event = "stored"
	EventEvicted     Event = "evicted"
	EventEmbedFailed Event = "embed_failed"
)
