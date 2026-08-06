package ingress_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/budget"
	"github.com/rajeev-chaurasia/pen-stock/internal/ingress"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// recordingAccountant scripts admission and records how each request was
// closed out, which is the part that silently goes wrong: a reservation
// that is never settled or released strands budget until it expires.
type recordingAccountant struct {
	mu       sync.Mutex
	denyWith error
	begins   []string
	settles  []providers.Usage
	aborts   int
	nextID   int
}

func (a *recordingAccountant) Begin(_ context.Context, tenant budget.TenantID, model string, _ []byte) (*budget.Reservation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.denyWith != nil {
		return nil, a.denyWith
	}
	a.begins = append(a.begins, string(tenant)+"|"+model)
	a.nextID++
	return &budget.Reservation{Tenant: tenant, ID: string(rune('a' + a.nextID))}, nil
}

func (a *recordingAccountant) Settle(_ context.Context, _ *budget.Reservation, usage providers.Usage, _, _ string) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.settles = append(a.settles, usage)
	return 0
}

func (a *recordingAccountant) Abort(_ context.Context, _ *budget.Reservation) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.aborts++
}

func (a *recordingAccountant) snapshot() (begins []string, settles []providers.Usage, aborts int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.begins...), append([]providers.Usage(nil), a.settles...), a.aborts
}

// countingProvider records whether the upstream was reached at all,
// which is the assertion that matters for a denial: the budget must be
// refused before any money is spent, not after.
type countingProvider struct {
	name  string
	mu    sync.Mutex
	calls int
}

func (c *countingProvider) Name() string { return c.name }

func (c *countingProvider) Chat(_ context.Context, _ *providers.ChatRequest) (*providers.ChatResponse, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return &providers.ChatResponse{Body: []byte(`{}`)}, nil
}

func (c *countingProvider) ChatStream(_ context.Context, _ *providers.ChatRequest) (providers.StreamReader, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return &scriptReader{}, nil
}

func (c *countingProvider) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func newAccountedServer(t *testing.T, routes map[string]providers.Provider, acct ingress.Accountant) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := ingress.NewServer(defaultCfg(), routes, log, ingress.WithAccounting(acct))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestBudgetDenialsMapToHonestStatuses(t *testing.T) {
	// A rate limit and a spent budget are different answers. Telling a
	// client to retry a budget it cannot refill sends it into a loop
	// that can never succeed.
	cases := []struct {
		name           string
		denial         *budget.Denial
		wantStatus     int
		wantRetryAfter bool
	}{
		{
			name:           "request rate is a wait",
			denial:         &budget.Denial{Reason: budget.DenyRequestRate, RetryAfter: 30, Message: "too many requests"},
			wantStatus:     http.StatusTooManyRequests,
			wantRetryAfter: true,
		},
		{
			name:       "daily budget is not a wait",
			denial:     &budget.Denial{Reason: budget.DenyDailyBudget, Message: "daily budget exhausted"},
			wantStatus: http.StatusPaymentRequired,
		},
		{
			name:       "monthly budget is not a wait",
			denial:     &budget.Denial{Reason: budget.DenyMonthlyBudget, Message: "monthly budget exhausted"},
			wantStatus: http.StatusPaymentRequired,
		},
		{
			name:       "a blind accounting store is unavailable",
			denial:     &budget.Denial{Reason: budget.DenyStoreDegraded, Message: "accounting unavailable"},
			wantStatus: http.StatusServiceUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &countingProvider{name: "p"}
			acct := &recordingAccountant{denyWith: tc.denial}
			ts := newAccountedServer(t, map[string]providers.Provider{"m": prov}, acct)

			resp := postChat(t, ts, `{"model":"m"}`)
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if got := resp.Header.Get("Retry-After"); (got != "") != tc.wantRetryAfter {
				t.Errorf("Retry-After = %q, wanted presence %v", got, tc.wantRetryAfter)
			}
			// A denied request must never reach the upstream, or the
			// budget was spent by the very request that broke it.
			if got := prov.count(); got != 0 {
				t.Errorf("upstream calls = %d, want 0 for a denied request", got)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), `"error"`) {
				t.Errorf("body = %s, want an error envelope", body)
			}
		})
	}
}

func TestSuccessfulRequestSettlesActualUsage(t *testing.T) {
	prov := &fakeProvider{
		name: "p",
		chatFn: func(_ context.Context, _ *providers.ChatRequest) (*providers.ChatResponse, error) {
			return &providers.ChatResponse{
				Body:  []byte(`{}`),
				Usage: providers.Usage{PromptTokens: 12, CompletionTokens: 30, TotalTokens: 42},
			}, nil
		},
	}
	acct := &recordingAccountant{}
	ts := newAccountedServer(t, map[string]providers.Provider{"m": prov}, acct)

	if got := postChat(t, ts, `{"model":"m"}`).StatusCode; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	begins, settles, aborts := acct.snapshot()
	if len(begins) != 1 {
		t.Errorf("begins = %v, want one reservation", begins)
	}
	if len(settles) != 1 || settles[0].CompletionTokens != 30 {
		t.Errorf("settles = %+v, want the upstream's real usage", settles)
	}
	if aborts != 0 {
		t.Errorf("aborts = %d, want 0 on a successful request", aborts)
	}
}

func TestFailedUpstreamReturnsTheReservation(t *testing.T) {
	// A request that produced no answer must cost the tenant nothing,
	// and must not leave its claim outstanding either.
	prov := &fakeProvider{
		name: "p",
		chatFn: func(_ context.Context, _ *providers.ChatRequest) (*providers.ChatResponse, error) {
			return nil, &providers.ProviderError{Provider: "p", Class: providers.ErrClassUpstream, Message: "boom"}
		},
	}
	acct := &recordingAccountant{}
	ts := newAccountedServer(t, map[string]providers.Provider{"m": prov}, acct)

	postChat(t, ts, `{"model":"m"}`)

	_, settles, aborts := acct.snapshot()
	if aborts != 1 {
		t.Errorf("aborts = %d, want 1 after a failed upstream", aborts)
	}
	if len(settles) != 0 {
		t.Errorf("settles = %+v, want nothing billed for an answer that never arrived", settles)
	}
}

func TestStreamSettlesReportedUsage(t *testing.T) {
	prov := &fakeProvider{
		name: "p",
		streamFn: func(_ context.Context, _ *providers.ChatRequest) (providers.StreamReader, error) {
			return &usageReader{
				frames: [][]byte{[]byte(`{"a":1}`)},
				usage:  providers.Usage{PromptTokens: 7, CompletionTokens: 11, TotalTokens: 18},
			}, nil
		},
	}
	acct := &recordingAccountant{}
	ts := newAccountedServer(t, map[string]providers.Provider{"m": prov}, acct)

	resp := postChat(t, ts, `{"model":"m","stream":true}`)
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("drain: %v", err)
	}
	_, settles, aborts := acct.snapshot()
	if len(settles) != 1 || settles[0].CompletionTokens != 11 {
		t.Errorf("settles = %+v, want the usage the stream reported", settles)
	}
	if aborts != 0 {
		t.Errorf("aborts = %d, want 0 for a stream that reported usage", aborts)
	}
}

func TestStreamWithoutUsageReturnsTheReservation(t *testing.T) {
	// No usage reported means no measured cost. Settling on the estimate
	// instead would bill the tenant for a guess.
	prov := &fakeProvider{
		name: "p",
		streamFn: func(_ context.Context, _ *providers.ChatRequest) (providers.StreamReader, error) {
			return &scriptReader{frames: [][]byte{[]byte(`{"a":1}`)}}, nil
		},
	}
	acct := &recordingAccountant{}
	ts := newAccountedServer(t, map[string]providers.Provider{"m": prov}, acct)

	resp := postChat(t, ts, `{"model":"m","stream":true}`)
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("drain: %v", err)
	}
	_, settles, aborts := acct.snapshot()
	if len(settles) != 0 {
		t.Errorf("settles = %+v, want nothing billed without reported usage", settles)
	}
	if aborts != 1 {
		t.Errorf("aborts = %d, want the reservation returned", aborts)
	}
}

func TestNoAccountingConfiguredStillServes(t *testing.T) {
	// Budgeting is optional. A gateway without it must not start
	// refusing traffic.
	prov := okProvider("p")
	ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m": prov})
	if got := postChat(t, ts, `{"model":"m"}`).StatusCode; got != http.StatusOK {
		t.Errorf("status = %d, want 200 without accounting configured", got)
	}
}

func TestDenialBodyNamesTheLimit(t *testing.T) {
	acct := &recordingAccountant{denyWith: &budget.Denial{
		Reason:  budget.DenyDailyBudget,
		Message: "this request would exceed the daily budget of 1.00 USD",
	}}
	ts := newAccountedServer(t, map[string]providers.Provider{"m": okProvider("p")}, acct)

	resp := postChat(t, ts, `{"model":"m"}`)
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(envelope.Error.Message, "daily budget") {
		t.Errorf("message = %q, want it to name the limit that refused", envelope.Error.Message)
	}
	if envelope.Error.Code != "daily_budget_exhausted" {
		t.Errorf("code = %q, want a machine readable reason", envelope.Error.Code)
	}
}
