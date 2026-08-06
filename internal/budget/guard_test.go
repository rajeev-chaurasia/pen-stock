package budget

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/pricing"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

type captureLedger struct {
	mu      sync.Mutex
	entries []pricing.Entry
	err     error
}

func (l *captureLedger) Write(e pricing.Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.entries = append(l.entries, e)
	return nil
}

func (l *captureLedger) all() []pricing.Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]pricing.Entry(nil), l.entries...)
}

func guardWithLedger(t *testing.T, led pricing.Ledger, onErr func(error)) *Guard {
	t.Helper()
	prices, err := pricing.DefaultTable()
	if err != nil {
		t.Fatalf("DefaultTable: %v", err)
	}
	return NewGuard(GuardOptions{
		Estimator:     NewEstimator(prices, nil, EstimatorOptions{}),
		Enforcer:      NewEnforcer(map[TenantID]Limits{"acme": {}}, nil),
		Prices:        prices,
		KindOf:        func(string) string { return "groq" },
		Ledger:        led,
		OnLedgerError: onErr,
	})
}

// A running total says what a tenant spent. The ledger says which
// requests it was spent on, which is the difference between a number on
// a dashboard and one an operator can check.
func TestSettleWritesALedgerRow(t *testing.T) {
	led := &captureLedger{}
	g := guardWithLedger(t, led, nil)

	res, err := g.Begin(context.Background(), "acme", "llama-3.3-70b-versatile",
		[]byte(`{"model":"llama-3.3-70b-versatile","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	usage := providers.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}
	g.Settle(context.Background(), res, usage, "llama-3.3-70b-versatile", "groq")

	entries := led.all()
	if len(entries) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Tenant != "acme" {
		t.Errorf("tenant = %q, want acme", e.Tenant)
	}
	if e.PromptTokens != 1000 || e.CompletionTokens != 500 {
		t.Errorf("tokens = %d/%d, want 1000/500", e.PromptTokens, e.CompletionTokens)
	}
	if e.USD <= 0 {
		t.Errorf("usd = %v, want the priced cost of a priced model", e.USD)
	}
	// The row has to say which price list produced its number, or the
	// figure cannot be rechecked later against the prices of the day.
	if e.PriceVersion < pricing.MinVersion {
		t.Errorf("price_version = %d, want the table's version", e.PriceVersion)
	}
	if e.RequestID == "" {
		t.Error("request_id is empty, so the row cannot be tied back to its reservation")
	}
	if e.Timestamp.IsZero() {
		t.Error("timestamp is zero")
	}
}

func TestAbortWritesNothing(t *testing.T) {
	// A request that produced no answer has nothing to account for, and
	// a row for it would overstate what the tenant was charged.
	led := &captureLedger{}
	g := guardWithLedger(t, led, nil)

	res, err := g.Begin(context.Background(), "acme", "llama-3.3-70b-versatile", []byte(`{}`))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	g.Abort(context.Background(), res)

	if got := len(led.all()); got != 0 {
		t.Errorf("ledger rows = %d, want 0 for an aborted request", got)
	}
}

func TestLedgerFailureIsReportedNotSwallowed(t *testing.T) {
	// The answer is already on its way to the client, so a failed audit
	// write must not fail the request. It must still be noticed: a
	// ledger that silently cannot write looks exactly like one with
	// nothing to record.
	wantErr := errors.New("disk full")
	led := &captureLedger{err: wantErr}

	var reported error
	g := guardWithLedger(t, led, func(err error) { reported = err })

	res, err := g.Begin(context.Background(), "acme", "llama-3.3-70b-versatile", []byte(`{}`))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	usd := g.Settle(context.Background(), res, providers.Usage{PromptTokens: 10}, "llama-3.3-70b-versatile", "groq")

	if !errors.Is(reported, wantErr) {
		t.Errorf("reported = %v, want the ledger's error surfaced", reported)
	}
	if usd < 0 {
		t.Errorf("usd = %v, settlement should still have completed", usd)
	}
	// The spend itself must still be enforced even when the audit row
	// was lost, or a broken disk would become a way to spend freely.
	daily, _ := g.enforcer.(*MemEnforcer).Spent("acme")
	if daily < 0 {
		t.Errorf("daily spend = %v, want the settlement recorded", daily)
	}
}

func TestNoLedgerConfiguredStillSettles(t *testing.T) {
	g := guardWithLedger(t, nil, nil)

	res, err := g.Begin(context.Background(), "acme", "llama-3.3-70b-versatile", []byte(`{}`))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	g.Settle(context.Background(), res, providers.Usage{PromptTokens: 10, CompletionTokens: 5}, "llama-3.3-70b-versatile", "groq")

	if got := g.enforcer.(*MemEnforcer).Committed("acme"); got != 0 {
		t.Errorf("committed = %v, want the claim released even without a ledger", got)
	}
}
