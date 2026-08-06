package budget

import (
	"context"

	"github.com/rajeev-chaurasia/pen-stock/internal/pricing"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// Guard is the single call the request path makes into budgeting. It
// joins the three pieces that have to agree with each other: what a
// request is predicted to cost, what a tenant is allowed to spend, and
// what the answer actually cost once it arrived.
//
// Keeping them together here means the ingress never has to know that
// an estimate is priced differently from a settlement, which is the
// kind of detail that drifts apart when it is spread across layers.
type Guard struct {
	estimator Estimator
	enforcer  Enforcer
	prices    *pricing.Table
	kindOf    func(model string) string
	ledger    pricing.Ledger
}

// GuardOptions carries the collaborators a Guard needs. A nil prices
// table or kindOf means requests are enforced on tokens alone, since a
// price that is not known must never be invented.
type GuardOptions struct {
	Estimator Estimator
	Enforcer  Enforcer
	Prices    *pricing.Table
	KindOf    func(model string) string
	Ledger    pricing.Ledger
}

func NewGuard(opts GuardOptions) *Guard {
	return &Guard{
		estimator: opts.Estimator,
		enforcer:  opts.Enforcer,
		prices:    opts.Prices,
		kindOf:    opts.KindOf,
		ledger:    opts.Ledger,
	}
}

// Begin admits or refuses a request. A nil reservation with a nil error
// means budgeting is not configured, and the caller should proceed
// without settling anything.
func (g *Guard) Begin(ctx context.Context, tenant TenantID, model string, raw []byte) (*Reservation, error) {
	if g == nil || g.enforcer == nil || g.estimator == nil {
		return nil, nil
	}
	est := g.estimator.Estimate(model, raw)
	return g.enforcer.Reserve(ctx, tenant, est)
}

// Settle records what the request really consumed and returns the cost
// it was billed at, so the caller can attribute it.
func (g *Guard) Settle(ctx context.Context, r *Reservation, usage providers.Usage, model, provider string) float64 {
	if g == nil || r == nil || g.enforcer == nil {
		return 0
	}
	usd := g.Price(model, usage)
	_ = g.enforcer.Settle(ctx, r, usage, usd)
	return usd
}

// Abort returns a reservation whose request never produced an answer,
// so a failed upstream call costs the tenant nothing.
func (g *Guard) Abort(ctx context.Context, r *Reservation) {
	if g == nil || r == nil || g.enforcer == nil {
		return
	}
	_ = g.enforcer.Release(ctx, r)
}

// Price reports what usage costs for a model, or zero when the model
// carries no known price. Zero here means unpriced, never free.
func (g *Guard) Price(model string, usage providers.Usage) float64 {
	if g == nil || g.prices == nil || g.kindOf == nil {
		return 0
	}
	cost, ok := g.prices.Cost(g.kindOf(model), model, usage)
	if !ok {
		return 0
	}
	return cost.USD
}

// PriceVersion reports which price list is in force, so a ledger entry
// can record the numbers it was billed against.
func (g *Guard) PriceVersion() int {
	if g == nil || g.prices == nil {
		return 0
	}
	return g.prices.Version
}
