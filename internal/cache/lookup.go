package cache

import (
	"context"
	"strings"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// Lookup is the single call the request path makes into caching. It
// hides the fact that there are two tiers, because the ingress does not
// care which one answered, only whether it may skip the provider.
type Lookup struct {
	exact    Cache
	semantic Semantic
	embedder Embedder
	maxTemp  float64
	onEvent  func(Event)
}

// LookupOptions carries the collaborators. A nil exact tier disables
// caching entirely, and a nil embedder or semantic tier leaves the
// exact tier working on its own.
type LookupOptions struct {
	Exact          Cache
	Semantic       Semantic
	Embedder       Embedder
	MaxTemperature float64
	OnEvent        func(Event)
}

func NewLookup(opts LookupOptions) *Lookup {
	return &Lookup{
		exact:    opts.Exact,
		semantic: opts.Semantic,
		embedder: opts.Embedder,
		maxTemp:  opts.MaxTemperature,
		onEvent:  opts.OnEvent,
	}
}

// Enabled reports whether anything is cached at all.
func (l *Lookup) Enabled() bool { return l != nil && l.exact != nil }

// Result describes what a lookup found, and how.
type Result struct {
	Entry *Entry
	// Semantic is true when the answer came from a similar question
	// rather than the same one, which an operator wants to distinguish
	// when judging whether the threshold is set right.
	Semantic   bool
	Similarity float64
	// Key is the key the answer should be stored under on a miss, so
	// the caller does not build it twice.
	Key Key
	// Eligible is false when policy refused. The caller must not store
	// the response either.
	Eligible bool
}

// Get looks for a usable answer. A miss and a refusal both return
// Result.Entry nil; Eligible tells them apart, and only an eligible
// miss is worth storing later.
func (l *Lookup) Get(ctx context.Context, tenant, model string, raw []byte) Result {
	if !l.Enabled() {
		return Result{}
	}

	if e := Eligible(raw, l.maxTemp); !e.Cacheable {
		l.emit(EventIneligible)
		return Result{Eligible: false}
	}

	key, err := BuildKey(tenant, model, raw)
	if err != nil {
		// A body the key builder cannot read is one whose cache safety
		// nobody can judge.
		l.emit(EventIneligible)
		return Result{Eligible: false}
	}
	res := Result{Key: key, Eligible: true}

	if entry, ok := l.exact.Get(ctx, key); ok {
		l.emit(EventExactHit)
		res.Entry = entry
		return res
	}

	if entry, sim, ok := l.nearest(ctx, tenant, raw); ok {
		l.emit(EventSemanticHit)
		res.Entry = entry
		res.Semantic = true
		res.Similarity = sim
		return res
	}

	l.emit(EventMiss)
	return res
}

// nearest consults the semantic tier, and treats every failure as a
// miss. An embedder that is down or slow must cost a cache hit, never a
// request.
func (l *Lookup) nearest(ctx context.Context, tenant string, raw []byte) (*Entry, float64, bool) {
	if l.semantic == nil || l.embedder == nil {
		return nil, 0, false
	}
	text := PromptText(raw)
	if text == "" {
		return nil, 0, false
	}
	vectors, err := l.embedder.Embed(ctx, []string{text})
	if err != nil || len(vectors) == 0 {
		l.emit(EventEmbedFailed)
		return nil, 0, false
	}
	return l.semantic.Nearest(ctx, tenant, vectors[0])
}

// Put stores an answer under a key an earlier Get returned. Storing is
// best effort by design: failing a request because its answer could not
// be remembered would trade a served response for nothing.
func (l *Lookup) Put(ctx context.Context, res Result, entry *Entry, raw []byte) {
	if !l.Enabled() || !res.Eligible || entry == nil {
		return
	}
	l.exact.Put(ctx, res.Key, entry)
	l.emit(EventStored)

	if l.semantic == nil || l.embedder == nil {
		return
	}
	text := PromptText(raw)
	if text == "" {
		return
	}
	vectors, err := l.embedder.Embed(ctx, []string{text})
	if err != nil || len(vectors) == 0 {
		l.emit(EventEmbedFailed)
		return
	}
	l.semantic.Add(ctx, res.Key.Tenant, vectors[0], entry)
}

func (l *Lookup) emit(e Event) {
	if l.onEvent != nil {
		l.onEvent(e)
	}
}

// ReplayUsage reports the usage a hit avoided. It is deliberately not
// the tenant's spend: no provider was paid this time, and adding it
// would bill a caller twice for one answer.
func ReplayUsage(e *Entry) providers.Usage { return e.Usage }

// PromptText extracts the conversation text a semantic lookup compares
// on. Roles are included because the same words from a system and a
// user turn are not the same request.
func PromptText(raw []byte) string {
	msgs, ok := decodeMessages(raw)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, m := range msgs {
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(m.role)
		sb.WriteByte(':')
		sb.WriteString(m.text)
	}
	return sb.String()
}
