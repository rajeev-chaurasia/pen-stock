package pricing

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// ledgerFileMode keeps the ledger readable only by the account running the
// gateway. It holds per-tenant spend, which is nobody else's business.
const ledgerFileMode os.FileMode = 0o600

// Entry is one priced request as it lands in the ledger.
//
// Timestamp is supplied by the caller rather than read from the clock in
// the write path: that keeps writes deterministic under test, and a
// replayed or buffered entry keeps the time it actually happened. It
// serializes as RFC3339Nano, which is what encoding/json gives a
// time.Time.
type Entry struct {
	Timestamp        time.Time `json:"timestamp"`
	Tenant           string    `json:"tenant"`
	ProviderKind     string    `json:"provider_kind"`
	Model            string    `json:"model"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	USD              float64   `json:"usd"`
	PriceVersion     int       `json:"price_version"`
	CacheHit         bool      `json:"cache_hit"`
	RequestID        string    `json:"request_id"`
}

// Ledger records priced requests. Implementations must be safe for
// concurrent use: the gateway writes one entry per in-flight request.
type Ledger interface {
	Write(Entry) error
}

// NopLedger drops every entry, for deployments running with cost
// recording turned off.
type NopLedger struct{}

// Write discards the entry.
func (NopLedger) Write(Entry) error { return nil }

// FileLedger appends entries to a JSONL file, one JSON object per line.
type FileLedger struct {
	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
}

var (
	_ Ledger    = (*FileLedger)(nil)
	_ Ledger    = NopLedger{}
	_ io.Closer = (*FileLedger)(nil)
)

// OpenFileLedger opens the ledger at path in append mode, creating it if
// it does not exist. Existing content is never rewritten: the ledger is
// append-only so an audit can replay it from the beginning.
//
// Durability: Write does NOT fsync. An fsync costs single-digit
// milliseconds on most disks, which on a fast completion is a large slice
// of the whole request, and paying it on every request would make the
// ledger the gateway's latency ceiling. Entries land in the operating
// system page cache and reach the disk on the kernel's schedule. The
// tradeoff that accepts: a crash of the gateway process loses nothing,
// because the kernel still owns those buffers, but a kernel panic or a
// power cut can lose the last few seconds of entries. Cost accounting can
// live with that; a caller that cannot calls Sync on a timer, and Close
// syncs before it closes.
func OpenFileLedger(path string) (*FileLedger, error) {
	// The path comes from the operator's own configuration, so there is
	// no untrusted input to traverse with.
	// #nosec G304
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, ledgerFileMode)
	if err != nil {
		return nil, fmt.Errorf("open cost ledger %s: %w", path, err)
	}
	enc := json.NewEncoder(f)
	// Model ids and request ids are not HTML, and escaping them only
	// makes the ledger harder to read by eye.
	enc.SetEscapeHTML(false)
	return &FileLedger{f: f, enc: enc}, nil
}

// Write appends one entry as a single line. The mutex serializes
// concurrent requests: json.Encoder emits a value and its newline in one
// write, so a whole line either lands or does not.
func (l *FileLedger) Write(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.enc.Encode(e); err != nil {
		return fmt.Errorf("write cost ledger entry: %w", err)
	}
	return nil
}

// Sync flushes written entries through to the disk. See OpenFileLedger
// for why this is not on the write path.
func (l *FileLedger) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("sync cost ledger: %w", err)
	}
	return nil
}

// Close syncs and closes the ledger file. FileLedger implements io.Closer
// so a caller holding a Ledger can type assert to shut it down.
func (l *FileLedger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.f.Sync()
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("close cost ledger: %w", err)
	}
	return nil
}
