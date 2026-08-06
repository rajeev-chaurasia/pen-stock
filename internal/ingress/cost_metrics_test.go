package ingress_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/budget"
	"github.com/rajeev-chaurasia/pen-stock/internal/ingress"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// pricingAccountant settles at a fixed price so the metric can be
// asserted exactly.
type pricingAccountant struct {
	usd    float64
	denial error
}

func (p *pricingAccountant) Begin(_ context.Context, tenant budget.TenantID, _ string, _ []byte) (*budget.Reservation, error) {
	if p.denial != nil {
		return nil, p.denial
	}
	return &budget.Reservation{Tenant: tenant, ID: "r1"}, nil
}

func (p *pricingAccountant) Settle(context.Context, *budget.Reservation, providers.Usage, string, string) float64 {
	return p.usd
}

func (p *pricingAccountant) Abort(context.Context, *budget.Reservation) {}

// newCostServer wires a server with both a metrics sink and accounting,
// which is the combination the cost series depends on.
func newCostServer(t *testing.T, sink ingress.MetricsSink, acct ingress.Accountant, routes map[string]providers.Provider) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := ingress.NewServer(defaultCfg(), routes, log,
		ingress.WithMetrics(sink), ingress.WithAccounting(acct))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// Token counters say how busy the gateway is. Only a cost series says
// what that traffic is costing, which is the number the whole project
// exists to control.
func TestSettledCostReachesTheMetrics(t *testing.T) {
	sink := &recordingSink{}
	prov := &fakeProvider{
		name: "groq",
		chatFn: func(context.Context, *providers.ChatRequest) (*providers.ChatResponse, error) {
			return &providers.ChatResponse{
				Body:     []byte(`{}`),
				Provider: "groq",
				Usage:    providers.Usage{PromptTokens: 100, CompletionTokens: 50},
			}, nil
		},
	}
	acct := &pricingAccountant{usd: 0.25}

	ts := newCostServer(t, sink, acct, map[string]providers.Provider{"m": prov})

	resp := postChat(t, ts, `{"model":"m"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.ReadAll(resp.Body)

	costs, denials := sink.costSnapshot()
	if len(costs) != 1 {
		t.Fatalf("cost samples = %v, want one", costs)
	}
	// Labelled by the provider that answered and the model asked for, so
	// spend can be sliced by either without guessing.
	if costs[0] != "|groq|m|0.25" {
		t.Errorf("cost sample = %q, want an anonymous tenant billed 0.25 to groq for m", costs[0])
	}
	if len(denials) != 0 {
		t.Errorf("denials = %v, want none on a served request", denials)
	}
}

func TestDenialsReachTheMetrics(t *testing.T) {
	// An operator needs to tell a tenant that hit its ceiling apart from
	// one being rate limited, which is what the reason label carries.
	sink := &recordingSink{}
	acct := &pricingAccountant{denial: &budget.Denial{
		Reason:  budget.DenyDailyBudget,
		Message: "daily budget exhausted",
	}}

	ts := newCostServer(t, sink, acct, map[string]providers.Provider{"m": okProvider("groq")})

	if got := postChat(t, ts, `{"model":"m"}`).StatusCode; got != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", got)
	}

	costs, denials := sink.costSnapshot()
	if len(denials) != 1 || denials[0] != "|daily_budget" {
		t.Errorf("denials = %v, want one daily_budget denial", denials)
	}
	if len(costs) != 0 {
		t.Errorf("costs = %v, want nothing billed for a refused request", costs)
	}
}

func TestUnpricedModelStillRecordsAZero(t *testing.T) {
	// A model with no known price must still produce a sample. A missing
	// series looks like no traffic, which is the opposite of the truth.
	sink := &recordingSink{}
	prov := &fakeProvider{
		name: "custom",
		chatFn: func(context.Context, *providers.ChatRequest) (*providers.ChatResponse, error) {
			return &providers.ChatResponse{Body: []byte(`{}`), Provider: "custom"}, nil
		},
	}
	acct := &pricingAccountant{usd: 0}

	ts := newCostServer(t, sink, acct, map[string]providers.Provider{"m": prov})

	postChat(t, ts, `{"model":"m"}`)

	costs, _ := sink.costSnapshot()
	if len(costs) != 1 {
		t.Fatalf("cost samples = %v, want a zero sample for an unpriced model", costs)
	}
}
