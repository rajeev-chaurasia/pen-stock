// Package budget enforces what a tenant may spend before the gateway
// spends it.
//
// The hard part is that a request's true cost is unknown until the
// upstream answers, so enforcement is two phase: reserve an estimate
// before the call, settle the real usage after it. Reservation is what
// bounds concurrency. Without it, a hundred requests arriving together
// would each read the same remaining balance, each conclude there is
// room, and each spend it.
//
// # The overshoot bound
//
// Reserving is atomic, so nothing is admitted once the balance is gone.
// A tenant can still finish slightly over budget, and the amount is
// bounded rather than arbitrary:
//
//	overshoot <= sum over in flight requests of (actual - estimate)
//
// That is, a tenant can exceed its budget only by how badly the
// estimates for currently running requests undershot. Capping the
// estimate's completion allowance keeps each term small, and the
// in flight ceiling keeps the sum finite. A request whose estimate was
// too generous returns the difference at settle time.
//
// This bound is a documented property, not an aspiration: a test drives
// concurrent requests at a nearly exhausted budget and asserts it.
package budget

import (
	"context"
	"errors"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// TenantID identifies who is spending. It never contains the client's
// key, only a label derived from configuration, so it is safe to log.
type TenantID string

// Limits is what one tenant may consume. A zero field means unlimited,
// which keeps a partially configured tenant usable rather than locked
// out by accident.
type Limits struct {
	// RequestsPerMinute caps request rate.
	RequestsPerMinute int
	// TokensPerMinute caps token throughput, counting prompt plus
	// completion.
	TokensPerMinute int
	// DailyUSD and MonthlyUSD cap spend over a rolling window.
	DailyUSD   float64
	MonthlyUSD float64
	// FailClosed decides what happens when the accounting store cannot
	// answer. True denies the request, which is right for a hard cap on
	// real money. False allows it and raises an alert, which suits a
	// soft advisory limit.
	FailClosed bool
}

// Estimate is what a request is expected to consume, computed before
// the upstream is called.
type Estimate struct {
	PromptTokens     int
	CompletionTokens int
	USD              float64
}

// Reservation is the claim held between reserve and settle. Settle must
// be called exactly once for each successful Reserve, including when
// the upstream fails, or the reserved amount stays claimed until it
// expires.
type Reservation struct {
	Tenant   TenantID
	Estimate Estimate
	IssuedAt time.Time
	// ID distinguishes concurrent reservations by the same tenant.
	ID string
}

// DenyReason says which limit refused the request, so the caller can
// map it to a status and tell the client something actionable.
type DenyReason string

const (
	DenyNone          DenyReason = ""
	DenyRequestRate   DenyReason = "request_rate"
	DenyTokenRate     DenyReason = "token_rate"
	DenyDailyBudget   DenyReason = "daily_budget"
	DenyMonthlyBudget DenyReason = "monthly_budget"
	DenyStoreDegraded DenyReason = "accounting_unavailable"
	DenyUnknownTenant DenyReason = "unknown_tenant"
)

// ErrDenied is returned by Reserve when a limit refuses the request.
var ErrDenied = errors.New("request denied by a tenant limit")

// Denial carries why a request was refused and when retrying could
// work. RetryAfter is zero when waiting will not help, which is the
// case for an exhausted budget as opposed to a rate limit.
type Denial struct {
	Reason     DenyReason
	RetryAfter time.Duration
	Message    string
}

func (d *Denial) Error() string { return string(d.Reason) + ": " + d.Message }

func (d *Denial) Unwrap() error { return ErrDenied }

// Enforcer admits or refuses a request and records what it actually
// cost.
type Enforcer interface {
	// Reserve claims est against the tenant's limits. It returns a
	// *Denial wrapping ErrDenied when a limit refuses.
	Reserve(ctx context.Context, tenant TenantID, est Estimate) (*Reservation, error)
	// Settle replaces the reservation with the real usage and cost.
	// Calling it twice for one reservation must not double count.
	Settle(ctx context.Context, r *Reservation, actual providers.Usage, usd float64) error
	// Release returns a reservation whose request never reached the
	// upstream, so a failed call does not consume budget.
	Release(ctx context.Context, r *Reservation) error
}

// Estimator predicts a request's consumption before it is sent.
type Estimator interface {
	Estimate(model string, raw []byte) Estimate
}
