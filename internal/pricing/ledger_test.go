package pricing

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func sampleEntry() Entry {
	return Entry{
		Timestamp:        time.Date(2026, time.August, 5, 12, 34, 56, 123456789, time.UTC),
		Tenant:           "acme",
		ProviderKind:     "groq",
		Model:            "llama-3.3-70b-versatile",
		PromptTokens:     12345,
		CompletionTokens: 6789,
		USD:              0.01264686,
		PriceVersion:     7,
		CacheHit:         true,
		RequestID:        "req-abc123",
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path) // #nosec G304
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan ledger: %v", err)
	}
	return lines
}

func TestFileLedgerRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.jsonl")
	l, err := OpenFileLedger(path)
	if err != nil {
		t.Fatalf("OpenFileLedger: %v", err)
	}
	want := sampleEntry()
	if err := l.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := readLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("len(lines) = %d, want 1", len(lines))
	}

	var got Entry
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}
	if !got.Timestamp.Equal(want.Timestamp) {
		t.Errorf("Timestamp = %v, want %v", got.Timestamp, want.Timestamp)
	}
	got.Timestamp = want.Timestamp // compared above, times differ by location
	if got != want {
		t.Errorf("entry = %+v, want %+v", got, want)
	}

	// Field names and the timestamp encoding are the ledger's contract
	// with whatever reads it later, so pin them.
	var raw map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if got, want := raw["timestamp"], want.Timestamp.Format(time.RFC3339Nano); got != want {
		t.Errorf("timestamp = %v, want RFC3339Nano %q", got, want)
	}
	for _, field := range []string{
		"timestamp", "tenant", "provider_kind", "model", "prompt_tokens",
		"completion_tokens", "usd", "price_version", "cache_hit", "request_id",
	} {
		if _, ok := raw[field]; !ok {
			t.Errorf("entry is missing field %q", field)
		}
	}
}

func TestFileLedgerConcurrentWrites(t *testing.T) {
	const (
		writers     = 16
		perWriter   = 64
		wantEntries = writers * perWriter
	)

	path := filepath.Join(t.TempDir(), "cost.jsonl")
	l, err := OpenFileLedger(path)
	if err != nil {
		t.Fatalf("OpenFileLedger: %v", err)
	}

	errCh := make(chan error, wantEntries)
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				e := sampleEntry()
				e.RequestID = fmt.Sprintf("req-%d-%d", w, i)
				if err := l.Write(e); err != nil {
					errCh <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("Write: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := readLines(t, path)
	if len(lines) != wantEntries {
		t.Fatalf("len(lines) = %d, want %d", len(lines), wantEntries)
	}

	seen := make(map[string]bool, wantEntries)
	for i, line := range lines {
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d is not valid JSON (%v): %q", i, err, line)
		}
		if e.PromptTokens != sampleEntry().PromptTokens {
			t.Errorf("line %d PromptTokens = %d, want %d", i, e.PromptTokens, sampleEntry().PromptTokens)
		}
		if seen[e.RequestID] {
			t.Errorf("line %d repeats request id %q", i, e.RequestID)
		}
		seen[e.RequestID] = true
	}
	for w := range writers {
		for i := range perWriter {
			if id := fmt.Sprintf("req-%d-%d", w, i); !seen[id] {
				t.Errorf("request id %q never reached the ledger", id)
			}
		}
	}
}

// Reopening must not truncate: the ledger is append-only so an audit can
// replay it from the first entry.
func TestFileLedgerAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.jsonl")

	for i := range 3 {
		l, err := OpenFileLedger(path)
		if err != nil {
			t.Fatalf("OpenFileLedger #%d: %v", i, err)
		}
		e := sampleEntry()
		e.RequestID = fmt.Sprintf("req-%d", i)
		if err := l.Write(e); err != nil {
			t.Fatalf("Write #%d: %v", i, err)
		}
		if err := l.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}
	}

	if lines := readLines(t, path); len(lines) != 3 {
		t.Fatalf("len(lines) = %d, want 3", len(lines))
	}
}

func TestFileLedgerSync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.jsonl")
	l, err := OpenFileLedger(path)
	if err != nil {
		t.Fatalf("OpenFileLedger: %v", err)
	}
	defer l.Close()

	if err := l.Write(sampleEntry()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := l.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if lines := readLines(t, path); len(lines) != 1 {
		t.Fatalf("len(lines) = %d, want 1", len(lines))
	}
}

func TestFileLedgerWriteErrorIsWrapped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.jsonl")
	l, err := OpenFileLedger(path)
	if err != nil {
		t.Fatalf("OpenFileLedger: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err = l.Write(sampleEntry())
	if err == nil {
		t.Fatal("Write to a closed ledger = nil, want error")
	}
	if !strings.Contains(err.Error(), "write cost ledger entry") {
		t.Errorf("error %q is not wrapped with context", err)
	}
	if !errors.Is(err, os.ErrClosed) {
		t.Errorf("error %v does not unwrap to os.ErrClosed", err)
	}
}

func TestOpenFileLedgerErrorIsWrapped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "cost.jsonl")

	_, err := OpenFileLedger(path)
	if err == nil {
		t.Fatal("OpenFileLedger in a missing directory = nil, want error")
	}
	if !strings.Contains(err.Error(), "open cost ledger") {
		t.Errorf("error %q is not wrapped with context", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error %v does not unwrap to os.ErrNotExist", err)
	}
}

func TestNopLedger(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var l Ledger = NopLedger{}
	for range 3 {
		if err := l.Write(sampleEntry()); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("NopLedger created %d files, want none", len(entries))
	}
}
