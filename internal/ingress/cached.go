package ingress

import (
	"io"
	"net/http"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/cache"
)

// maxCachedStreamBytes bounds what a streamed answer may occupy before
// the gateway gives up on remembering it. Accumulating frames to store
// them is the one place the streaming path holds a whole response, so
// it needs a ceiling; past it the answer is still served, just not
// stored.
const maxCachedStreamBytes = 1 << 20

// serveCached answers from a stored entry. No provider is called, so
// nothing is billed and no reservation is taken: charging a tenant for
// a call that never happened would be the same bug as not charging for
// one that did, pointed the other way.
func (s *Server) serveCached(w http.ResponseWriter, r *http.Request, res cache.Result) bool {
	entry := res.Entry
	if entry == nil {
		return false
	}

	info := logInfoFrom(r.Context())
	info.provider = entry.Provider
	info.cached = true

	// The saving is reported, not the spend. An operator wants to see
	// what the cache avoided; the tenant's balance must not move.
	s.metrics.AddTokens(entry.Provider, entry.Usage.PromptTokens, entry.Usage.CompletionTokens)

	if info.stream {
		return s.replayStream(w, entry)
	}
	w.Header().Set("Content-Type", contentTypeJSON)
	w.Header().Set("X-Content-Type-Options", headerNoSniff)
	w.Header().Set(headerCacheStatus, cacheStatusOf(res))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(entry.Body)
	return true
}

// Header names a client can use to tell a cached answer from a fresh
// one, which matters when debugging why a model appears to be repeating
// itself.
const headerCacheStatus = "X-Penstock-Cache"

const (
	cacheStatusExact    = "hit-exact"
	cacheStatusSemantic = "hit-semantic"
)

func cacheStatusOf(res cache.Result) string {
	if res.Semantic {
		return cacheStatusSemantic
	}
	return cacheStatusExact
}

// replayStream serves a stored answer to a caller that asked for a
// stream. The frames are replayed as they arrived, then terminated the
// same way a live stream is, so a client cannot tell the difference
// beyond the header and the speed.
func (s *Server) replayStream(w http.ResponseWriter, entry *cache.Entry) bool {
	flusher := flusherFor(w)
	if flusher == nil {
		return false
	}
	if !entry.Streamed() {
		// The stored answer was never a stream, so there are no frames
		// to replay. Serving it as one would require re-chunking a whole
		// completion, which invents a shape the provider never sent.
		return false
	}

	h := w.Header()
	h.Set("Content-Type", contentTypeSSE)
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Content-Type-Options", headerNoSniff)
	h.Set(headerCacheStatus, cacheStatusExact)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for _, frame := range entry.Frames {
		if err := writeSSEFrame(w, frame); err != nil {
			return true
		}
		flusher.Flush()
	}
	_, _ = io.WriteString(w, sseDoneFrame)
	flusher.Flush()
	return true
}

// streamRecorder collects frames so a completed stream can be stored.
// It stops collecting past the size ceiling rather than growing without
// bound, and reports that it gave up so nothing partial is stored.
type streamRecorder struct {
	frames  [][]byte
	bytes   int
	dropped bool
}

func (rec *streamRecorder) add(frame []byte) {
	if rec == nil || rec.dropped {
		return
	}
	if rec.bytes+len(frame) > maxCachedStreamBytes {
		// A half remembered answer is worse than none: replaying it
		// would truncate a completion that actually finished.
		rec.dropped = true
		rec.frames = nil
		return
	}
	stored := make([]byte, len(frame))
	copy(stored, frame)
	rec.frames = append(rec.frames, stored)
	rec.bytes += len(frame)
}

// entry builds the storable answer, or nil when there is nothing worth
// keeping.
func (rec *streamRecorder) entry(provider, model string, e *cache.Entry) *cache.Entry {
	if rec == nil || rec.dropped || len(rec.frames) == 0 {
		return nil
	}
	e.Frames = rec.frames
	e.Provider = provider
	e.Model = model
	e.StoredAt = time.Now()
	return e
}
