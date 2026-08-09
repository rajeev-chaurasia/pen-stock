package budget

// This file is the contract for the per tenant budget enforcer.
//
// The rules come from types.go, which is frozen. Its package doc states
// the overshoot bound as a documented property rather than an aspiration,
// so TestEnforcerOvershootStaysWithinTheDocumentedBound restates the
// formula in code and drives concurrency at a nearly exhausted budget
// until it either holds or does not.
//
// This is the money path. Where a behavior could reasonably go two ways
// the assertion follows what the doc comments promise the caller, not
// what is convenient to implement, and the reason is written down next to
// it. Where two plausible implementations would both be defensible the
// test is deliberately written so that either passes, so a failure here
// always means a real disagreement about money.
//
// Every helper in this file carries a budget prefix. The package has
// several test files and a bare newClock or fakeClock would collide.

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// The enforcer the gateway holds is the Enforcer from types.go. It must
// additionally expose the accounting the operator and these tests read,
// plus the switch that simulates the accounting store going degraded.
var _ interface {
	Enforcer
	Spent(TenantID) (float64, float64)
	Committed(TenantID) float64
	SetStoreHealthy(bool)
} = (*MemEnforcer)(nil)

// --- fixtures --------------------------------------------------------

const (
	// budgetTenant is the tenant under test. budgetOtherTenant proves one
	// tenant's exhaustion is not another's problem. budgetMissingTenant is
	// never placed in the limits map, so it is the unknown case.
	budgetTenant        TenantID = "acme"
	budgetOtherTenant   TenantID = "globex"
	budgetMissingTenant TenantID = "nobody-configured-me"
)

// budgetClockStart sits exactly on a minute boundary and early in a
// month, so a rate window and a calendar window both start clean and a
// test can advance across either boundary without arithmetic surprises.
var budgetClockStart = time.Date(2024, 5, 2, 10, 0, 0, 0, time.UTC)

// budgetEpsilon absorbs float64 representation noise. Every amount in
// this suite is a dyadic fraction (halves, quarters, eighths) so sums are
// exact regardless of the order they are added in; the tolerance exists
// only so an implementation that accumulates differently is not failed
// for arithmetic that is right.
const budgetEpsilon = 1e-9

// --- fake clock ------------------------------------------------------

// budgetFakeClock is the injected time source. Window rollover and rate
// limits are driven by Advance, never by sleeping: a suite that sleeps a
// minute to test a per minute limit is a suite nobody runs.
type budgetFakeClock struct {
	mu  sync.Mutex
	now time.Time
}

var _ Clock = (*budgetFakeClock)(nil)

func newBudgetFakeClock() *budgetFakeClock {
	return &budgetFakeClock{now: budgetClockStart}
}

func (c *budgetFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward. It is safe to call while requests are
// in flight, which is what lets a concurrency test also cross a window.
func (c *budgetFakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// --- helpers ---------------------------------------------------------

// budgetNew is the constructor every test in this file goes through.
//
// It is a variable rather than a direct call to NewEnforcer so that one
// test can replay every assertion here against a second implementation
// without duplicating or editing a single assertion. See durable_test.go.
//
// Nothing in this package calls t.Parallel, which is what makes swapping
// a package level variable safe. Do not add it.
var budgetNew = func(t *testing.T, limits map[TenantID]Limits, clock Clock) *MemEnforcer {
	t.Helper()
	return NewEnforcer(limits, clock)
}

// budgetEnforcer builds an enforcer holding a single tenant, budgetTenant,
// on a fake clock the caller gets back for advancing.
func budgetEnforcer(t *testing.T, lim Limits) (*MemEnforcer, *budgetFakeClock) {
	t.Helper()
	clock := newBudgetFakeClock()
	return budgetNew(t, map[TenantID]Limits{budgetTenant: lim}, clock), clock
}

// budgetEstUSD is an estimate that costs money but consumes no notable
// tokens, for the tests where only a spend cap is configured.
func budgetEstUSD(usd float64) Estimate {
	return Estimate{PromptTokens: 1, CompletionTokens: 1, USD: usd}
}

// budgetEstTokens is an estimate that consumes tokens but no money, for
// the token rate tests.
func budgetEstTokens(prompt, completion int) Estimate {
	return Estimate{PromptTokens: prompt, CompletionTokens: completion}
}

// budgetUsageOf reports usage matching an estimate exactly, so a test
// that varies only the settled dollars is not accidentally also varying
// the tokens.
func budgetUsageOf(est Estimate) providers.Usage {
	return providers.Usage{
		PromptTokens:     est.PromptTokens,
		CompletionTokens: est.CompletionTokens,
		TotalTokens:      est.PromptTokens + est.CompletionTokens,
	}
}

func budgetClose(got, want float64) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d <= budgetEpsilon
}

// budgetMustReserve admits a request or fails the test. Used wherever the
// admission itself is setup rather than the thing under test.
func budgetMustReserve(t *testing.T, e *MemEnforcer, tenant TenantID, est Estimate) *Reservation {
	t.Helper()
	r, err := e.Reserve(context.Background(), tenant, est)
	if err != nil {
		t.Fatalf("Reserve(%s, %+v) = %v, want admitted", tenant, est, err)
	}
	if r == nil {
		t.Fatalf("Reserve(%s, %+v) returned a nil reservation and a nil error", tenant, est)
	}
	return r
}

func budgetMustSettle(t *testing.T, e *MemEnforcer, r *Reservation, usd float64) {
	t.Helper()
	if err := e.Settle(context.Background(), r, budgetUsageOf(r.Estimate), usd); err != nil {
		t.Fatalf("Settle(%+v, %v): %v", r, usd, err)
	}
}

func budgetMustRelease(t *testing.T, e *MemEnforcer, r *Reservation) {
	t.Helper()
	if err := e.Release(context.Background(), r); err != nil {
		t.Fatalf("Release(%+v): %v", r, err)
	}
}

// budgetDenied asserts a refusal and returns the *Denial so the caller can
// look at RetryAfter. It also pins the two things every refusal owes its
// caller: it is an ErrDenied so a handler can errors.Is it without knowing
// this package's types, and it hands back no reservation, because a denied
// request that still holds a claim leaks budget until the claim expires.
func budgetDenied(t *testing.T, r *Reservation, err error, want DenyReason) *Denial {
	t.Helper()
	if err == nil {
		t.Fatalf("Reserve succeeded, want a denial with reason %q", want)
	}
	if r != nil {
		t.Errorf("Reserve returned reservation %+v alongside its error; a denied request must hold nothing", r)
	}
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("errors.Is(%v, ErrDenied) = false, so no caller can recognize this as a denial", err)
	}
	var d *Denial
	if !errors.As(err, &d) {
		t.Fatalf("error %v is not a *Denial, so the caller cannot read the reason or RetryAfter", err)
	}
	if d.Reason != want {
		t.Errorf("denial reason = %q, want %q", d.Reason, want)
	}
	if d.Message == "" {
		t.Errorf("denial for %q carries an empty Message; the client is told nothing actionable", d.Reason)
	}
	return d
}

// budgetBalance asserts the whole visible accounting for a tenant at once.
// Checking committed alongside spend is what catches the classic bug where
// a settle credits the spend but forgets to release the claim, so a tenant
// slowly strangles itself while under budget.
func budgetBalance(t *testing.T, e *MemEnforcer, tenant TenantID, wantDaily, wantMonthly, wantCommitted float64) {
	t.Helper()
	daily, monthly := e.Spent(tenant)
	if !budgetClose(daily, wantDaily) {
		t.Errorf("Spent(%s) daily = %v, want %v", tenant, daily, wantDaily)
	}
	if !budgetClose(monthly, wantMonthly) {
		t.Errorf("Spent(%s) monthly = %v, want %v", tenant, monthly, wantMonthly)
	}
	if got := e.Committed(tenant); !budgetClose(got, wantCommitted) {
		t.Errorf("Committed(%s) = %v, want %v", tenant, got, wantCommitted)
	}
}

// --- tenancy ---------------------------------------------------------

func TestEnforcerDeniesUnknownTenant(t *testing.T) {
	// A tenant nobody configured has no cap to check against, so admitting
	// it would mean spending real money on an unbounded account. Denying is
	// the only safe reading of a missing entry.
	e, _ := budgetEnforcer(t, Limits{DailyUSD: 100})

	r, err := e.Reserve(context.Background(), budgetMissingTenant, budgetEstUSD(1))
	budgetDenied(t, r, err, DenyUnknownTenant)

	// A refusal must leave no trace. If the unknown tenant were created on
	// first sight, a typo in a client's key would quietly mint a new
	// unlimited account.
	budgetBalance(t, e, budgetMissingTenant, 0, 0, 0)
	budgetBalance(t, e, budgetTenant, 0, 0, 0)
}

func TestEnforcerZeroLimitsMeansUnlimitedNotUnknown(t *testing.T) {
	// A zero field is documented as unlimited, which keeps a partially
	// configured tenant usable. Together with the unknown tenant test this
	// forces a present check on the map rather than a zero value lookup:
	// a tenant with all zero Limits and a tenant that is absent have the
	// same Limits value and must behave in opposite ways.
	e, _ := budgetEnforcer(t, Limits{})

	const requests = 50
	const huge = 1_000_000.0
	est := Estimate{PromptTokens: 1_000_000, CompletionTokens: 1_000_000, USD: huge}

	for i := 0; i < requests; i++ {
		// Every one of these lands on the same clock instant, so a zero
		// RequestsPerMinute or TokensPerMinute that was treated as "cap of
		// zero" instead of "no cap" would deny at the first request.
		r := budgetMustReserve(t, e, budgetTenant, est)
		budgetMustSettle(t, e, r, huge)
	}
	budgetBalance(t, e, budgetTenant, requests*huge, requests*huge, 0)
}

func TestEnforcerIsolatesTenants(t *testing.T) {
	// Budgets are per tenant. One noisy tenant burning its cap must not
	// take the gateway down for everyone else, and its spend must not show
	// up in anyone else's accounting.
	clock := newBudgetFakeClock()
	e := budgetNew(t, map[TenantID]Limits{
		budgetTenant:      {DailyUSD: 1},
		budgetOtherTenant: {DailyUSD: 1},
	}, clock)

	r := budgetMustReserve(t, e, budgetTenant, budgetEstUSD(1))
	budgetMustSettle(t, e, r, 1)

	got, err := e.Reserve(context.Background(), budgetTenant, budgetEstUSD(0.25))
	budgetDenied(t, got, err, DenyDailyBudget)

	other := budgetMustReserve(t, e, budgetOtherTenant, budgetEstUSD(1))
	budgetMustSettle(t, e, other, 1)

	budgetBalance(t, e, budgetTenant, 1, 1, 0)
	budgetBalance(t, e, budgetOtherTenant, 1, 1, 0)
}

// --- reservation shape -----------------------------------------------

func TestReserveStampsTheReservationFromTheInjectedClock(t *testing.T) {
	// IssuedAt is what an expiry sweep for abandoned reservations will key
	// off. If it comes from time.Now instead of the injected clock, that
	// sweep can never be tested and will drift against the rest of the
	// gateway.
	e, clock := budgetEnforcer(t, Limits{DailyUSD: 100})
	est := Estimate{PromptTokens: 7, CompletionTokens: 11, USD: 0.5}

	r := budgetMustReserve(t, e, budgetTenant, est)
	if r.Tenant != budgetTenant {
		t.Errorf("reservation Tenant = %q, want %q", r.Tenant, budgetTenant)
	}
	if r.Estimate != est {
		t.Errorf("reservation Estimate = %+v, want %+v echoed back unchanged", r.Estimate, est)
	}
	if r.ID == "" {
		t.Error("reservation ID is empty, so concurrent claims by one tenant cannot be told apart")
	}
	if !r.IssuedAt.Equal(budgetClockStart) {
		t.Errorf("reservation IssuedAt = %v, want the injected %v", r.IssuedAt, budgetClockStart)
	}

	clock.Advance(5 * time.Minute)
	later := budgetMustReserve(t, e, budgetTenant, est)
	if want := budgetClockStart.Add(5 * time.Minute); !later.IssuedAt.Equal(want) {
		t.Errorf("second reservation IssuedAt = %v, want %v; the clock is read once at construction, not per request", later.IssuedAt, want)
	}
}

func TestNewEnforcerAcceptsANilClock(t *testing.T) {
	// A nil clock means real time, so production wiring does not have to
	// pass a clock it does not care about. Panicking here would turn an
	// omitted option into a crash on the first request.
	e := budgetNew(t, map[TenantID]Limits{budgetTenant: {DailyUSD: 100}}, nil)

	before := time.Now()
	r := budgetMustReserve(t, e, budgetTenant, budgetEstUSD(1))
	after := time.Now()

	if r.IssuedAt.Before(before) || r.IssuedAt.After(after) {
		t.Errorf("reservation IssuedAt = %v, want a real time within [%v, %v]", r.IssuedAt, before, after)
	}
}

func TestReserveIDsAreDistinctUnderConcurrency(t *testing.T) {
	// Reservation IDs are how a settle finds the claim it is closing. Two
	// concurrent requests sharing an ID would let one request's settle
	// close the other's claim, which is a silent double count.
	e, _ := budgetEnforcer(t, Limits{DailyUSD: 1_000_000})

	const goroutines = 200
	var (
		mu       sync.Mutex
		ids      = make(map[string]int, goroutines)
		problems []error
	)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			r, err := e.Reserve(context.Background(), budgetTenant, budgetEstUSD(1))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				problems = append(problems, err)
				return
			}
			if r == nil {
				problems = append(problems, errors.New("nil reservation with a nil error"))
				return
			}
			ids[r.ID]++
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range problems {
		t.Errorf("unexpected error from a worker: %v", err)
	}
	if _, dup := ids[""]; dup {
		t.Error("at least one reservation had an empty ID")
	}
	if len(ids) != goroutines {
		t.Errorf("distinct reservation IDs = %d, want %d", len(ids), goroutines)
	}
	for id, n := range ids {
		if n > 1 {
			t.Errorf("reservation ID %q was issued %d times", id, n)
		}
	}
}

// --- budget admission ------------------------------------------------

func TestEnforcerBudgetAdmitsUntilTheEstimateNoLongerFits(t *testing.T) {
	// The cap holds four quarter dollar requests exactly, so the fourth
	// must be admitted (an estimate that fits exactly still fits) and the
	// fifth must not. Quarters are exact in binary, so nothing here turns
	// on rounding.
	//
	// The settle_between axis is the point of the table. With no settles
	// the only thing standing between the fifth request and the money is
	// the committed total, so an implementation that checks settled spend
	// alone passes the settled case and fails this one.
	tests := []struct {
		name          string
		limits        Limits
		wantReason    DenyReason
		settleBetween bool
	}{
		{"daily_unsettled", Limits{DailyUSD: 1}, DenyDailyBudget, false},
		{"daily_settled", Limits{DailyUSD: 1}, DenyDailyBudget, true},
		{"monthly_unsettled", Limits{MonthlyUSD: 1}, DenyMonthlyBudget, false},
		{"monthly_settled", Limits{MonthlyUSD: 1}, DenyMonthlyBudget, true},
		// When both are configured, the one with less room left is the one
		// that binds, and its reason is what the client is told. Reporting
		// the wrong one sends the client away to wait for a window that was
		// never the problem.
		{"daily_binds_first", Limits{DailyUSD: 1, MonthlyUSD: 1000}, DenyDailyBudget, true},
		{"monthly_binds_first", Limits{DailyUSD: 1000, MonthlyUSD: 1}, DenyMonthlyBudget, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := budgetEnforcer(t, tc.limits)
			est := budgetEstUSD(0.25)

			for i := 0; i < 4; i++ {
				r := budgetMustReserve(t, e, budgetTenant, est)
				if tc.settleBetween {
					budgetMustSettle(t, e, r, 0.25)
				}
			}

			got, err := e.Reserve(context.Background(), budgetTenant, est)
			d := budgetDenied(t, got, err, tc.wantReason)

			// A budget is not a bucket that refills. Handing the client a
			// RetryAfter here would send it back on a schedule that cannot
			// help, turning one denial into a retry storm.
			if d.RetryAfter != 0 {
				t.Errorf("budget denial RetryAfter = %v, want 0: waiting does not refill a budget", d.RetryAfter)
			}

			// Four quarters were consumed either way. Whether they sit in
			// spend or in committed depends only on whether they settled.
			wantSpent, wantCommitted := 0.0, 1.0
			if tc.settleBetween {
				wantSpent, wantCommitted = 1.0, 0.0
			}
			budgetBalance(t, e, budgetTenant, wantSpent, wantSpent, wantCommitted)
		})
	}
}

func TestEnforcerBudgetDenialIsPerRequestNotALatch(t *testing.T) {
	// Denying one request must not blacklist the tenant. There is still
	// room in the cap and a smaller request genuinely fits, so refusing it
	// would throw away budget the tenant paid for.
	e, _ := budgetEnforcer(t, Limits{DailyUSD: 1})

	first := budgetMustReserve(t, e, budgetTenant, budgetEstUSD(0.75))
	budgetMustSettle(t, e, first, 0.75)

	got, err := e.Reserve(context.Background(), budgetTenant, budgetEstUSD(0.75))
	budgetDenied(t, got, err, DenyDailyBudget)

	fits := budgetMustReserve(t, e, budgetTenant, budgetEstUSD(0.25))
	budgetMustSettle(t, e, fits, 0.25)
	budgetBalance(t, e, budgetTenant, 1, 1, 0)
}

func TestEnforcerBudgetDenialTieIsOneOfTheTwoBudgets(t *testing.T) {
	// Both caps are exhausted at the same instant. Which one is named is
	// genuinely arbitrary, so this pins only that the reason is one of the
	// two budgets and never something unrelated like a rate limit.
	e, _ := budgetEnforcer(t, Limits{DailyUSD: 1, MonthlyUSD: 1})
	r := budgetMustReserve(t, e, budgetTenant, budgetEstUSD(1))
	budgetMustSettle(t, e, r, 1)

	got, err := e.Reserve(context.Background(), budgetTenant, budgetEstUSD(0.25))
	if err == nil {
		if got != nil {
			t.Fatal("Reserve succeeded with both budgets exhausted")
		}
		t.Fatal("Reserve returned nothing and no error")
	}
	var d *Denial
	if !errors.As(err, &d) {
		t.Fatalf("error %v is not a *Denial", err)
	}
	if d.Reason != DenyDailyBudget && d.Reason != DenyMonthlyBudget {
		t.Errorf("denial reason = %q, want one of %q or %q", d.Reason, DenyDailyBudget, DenyMonthlyBudget)
	}
}

// --- settle and release ----------------------------------------------

func TestSettleRecordsActualNotTheEstimate(t *testing.T) {
	// An estimate is a hold, not a charge. If a generous estimate were kept
	// as the charge, every tenant would be billed for the completion length
	// the gateway guessed rather than the one it got, and a cautious
	// estimator would quietly halve everyone's usable budget.
	tests := []struct {
		name     string
		estimate float64
		actual   float64
	}{
		// The overestimate is the important direction: the difference has to
		// come back, and the tenant must be able to spend it again.
		{"actual_below_estimate", 0.75, 0.25},
		{"actual_equals_estimate", 0.5, 0.5},
		// Settling above the estimate is the only way a tenant ends up over
		// budget. It must overshoot by exactly the difference and no more,
		// which is the per request term of the documented bound.
		{"actual_above_estimate", 0.25, 0.75},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := budgetEnforcer(t, Limits{DailyUSD: 4, MonthlyUSD: 8})

			r := budgetMustReserve(t, e, budgetTenant, budgetEstUSD(tc.estimate))
			// While the request is in flight the estimate is held, not spent:
			// nothing has been charged yet but the room is gone.
			budgetBalance(t, e, budgetTenant, 0, 0, tc.estimate)

			budgetMustSettle(t, e, r, tc.actual)
			budgetBalance(t, e, budgetTenant, tc.actual, tc.actual, 0)

			// The freed room must be usable, not merely reported. Reserving
			// the entire remainder proves the claim was really given back.
			rest := budgetMustReserve(t, e, budgetTenant, budgetEstUSD(4-tc.actual))
			budgetMustSettle(t, e, rest, 4-tc.actual)
			budgetBalance(t, e, budgetTenant, 4, 4, 0)
		})
	}
}

func TestReleaseReturnsTheWholeReservation(t *testing.T) {
	// A call that never reached the upstream cost nothing, so it must
	// consume nothing. Otherwise every provider outage would also burn the
	// tenant's budget for the day.
	e, _ := budgetEnforcer(t, Limits{DailyUSD: 1, MonthlyUSD: 1})

	r := budgetMustReserve(t, e, budgetTenant, budgetEstUSD(1))
	budgetBalance(t, e, budgetTenant, 0, 0, 1)

	budgetMustRelease(t, e, r)
	budgetBalance(t, e, budgetTenant, 0, 0, 0)

	// The whole budget must be reservable again, which a partial refund
	// would not allow.
	again := budgetMustReserve(t, e, budgetTenant, budgetEstUSD(1))
	budgetMustSettle(t, e, again, 1)
	budgetBalance(t, e, budgetTenant, 1, 1, 0)
}

func TestSettleIsIdempotentPerReservation(t *testing.T) {
	// A retry in the caller, a duplicated defer, or a settle on both the
	// success and the cleanup path all produce a second settle for one
	// reservation. Charging twice for one completion is the most expensive
	// bug this package can have.
	//
	// The second settle deliberately carries a different, much larger
	// amount, so an implementation that simply overwrites the recorded cost
	// fails here rather than passing by accident.
	e, _ := budgetEnforcer(t, Limits{DailyUSD: 100, MonthlyUSD: 100})

	r := budgetMustReserve(t, e, budgetTenant, budgetEstUSD(0.5))
	budgetMustSettle(t, e, r, 0.25)
	budgetBalance(t, e, budgetTenant, 0.25, 0.25, 0)

	// Whether the duplicate reports an error is the implementation's
	// choice. What is not negotiable is the balance afterwards.
	_ = e.Settle(context.Background(), r, budgetUsageOf(r.Estimate), 5)
	budgetBalance(t, e, budgetTenant, 0.25, 0.25, 0)
}

func TestSettleAndReleaseAreTerminalFirstOneWins(t *testing.T) {
	// Reserve, Settle and Release are the two ways a claim ends, and both
	// paths can fire for one request when a cleanup path races a success
	// path. The first terminal call decides; later ones are no ops.
	//
	// The failure mode being hunted here is a second decrement of the
	// committed total. That drives Committed negative, and a negative
	// committed total silently hands the tenant free budget: spent plus
	// committed reads lower than the truth, so the next reserve is admitted
	// against money that is already gone.
	t.Run("release_then_settle", func(t *testing.T) {
		e, _ := budgetEnforcer(t, Limits{DailyUSD: 1, MonthlyUSD: 1})
		r := budgetMustReserve(t, e, budgetTenant, budgetEstUSD(1))
		budgetMustRelease(t, e, r)

		// Release already declared this request never reached the upstream.
		// Honoring a later settle would charge for a call that by that
		// declaration did not happen.
		_ = e.Settle(context.Background(), r, budgetUsageOf(r.Estimate), 1)
		budgetBalance(t, e, budgetTenant, 0, 0, 0)

		// And the budget is genuinely whole, not merely reported as whole.
		again := budgetMustReserve(t, e, budgetTenant, budgetEstUSD(1))
		budgetMustSettle(t, e, again, 1)
		budgetBalance(t, e, budgetTenant, 1, 1, 0)
	})

	t.Run("settle_then_release", func(t *testing.T) {
		e, _ := budgetEnforcer(t, Limits{DailyUSD: 1, MonthlyUSD: 1})
		r := budgetMustReserve(t, e, budgetTenant, budgetEstUSD(0.5))
		budgetMustSettle(t, e, r, 0.5)

		// Money already spent cannot be clawed back by a late release.
		_ = e.Release(context.Background(), r)
		budgetBalance(t, e, budgetTenant, 0.5, 0.5, 0)
	})

	t.Run("double_release", func(t *testing.T) {
		e, _ := budgetEnforcer(t, Limits{DailyUSD: 1, MonthlyUSD: 1})
		r := budgetMustReserve(t, e, budgetTenant, budgetEstUSD(0.5))
		budgetMustRelease(t, e, r)
		_ = e.Release(context.Background(), r)
		budgetBalance(t, e, budgetTenant, 0, 0, 0)
	})
}

func TestSettleAndReleaseRejectANilReservation(t *testing.T) {
	// A caller that lost its reservation on an error path can reach here
	// with nil. Crashing takes the gateway down; returning nil would tell
	// the caller its cost was recorded when nothing was. An error is the
	// only honest answer.
	e, _ := budgetEnforcer(t, Limits{DailyUSD: 1})

	if err := e.Settle(context.Background(), nil, providers.Usage{}, 1); err == nil {
		t.Error("Settle(nil) returned no error, so a lost reservation looks like a recorded cost")
	}
	if err := e.Release(context.Background(), nil); err == nil {
		t.Error("Release(nil) returned no error")
	}
	budgetBalance(t, e, budgetTenant, 0, 0, 0)
}

// --- the overshoot bound ---------------------------------------------

func TestEnforcerOvershootStaysWithinTheDocumentedBound(t *testing.T) {
	// The flagship. The package doc promises:
	//
	//	overshoot <= sum over in flight requests of (actual - estimate)
	//
	// Two hundred requests arrive at a budget with room for three at
	// estimate prices, and every admitted one settles fifty cents above its
	// estimate. All amounts are halves and whole dollars, so every sum here
	// is exact in float64.
	//
	// (a) Admission must be atomic. A read then write race lets all two
	// hundred read the same remaining balance, agree there is room, and
	// spend it, so an admitted count above the ceiling is the signature of
	// that bug and not a rounding quibble.
	//
	// (b) Final spend must sit inside budget + admitted * underestimate.
	// That is the doc's bound summed over every admitted request rather
	// than only the in flight ones, which is the weaker and therefore safe
	// form of the same statement. With these numbers the assertion is
	// exactly tight, so a bound that is off by even one request's worth of
	// underestimate fails.
	//
	// (c) Nothing goes negative and nothing panics.
	//
	// This is the test that must stay meaningful under -race in CI: every
	// goroutine is released at once from a single channel close and they
	// all touch one tenant's accounting.
	const (
		dailyCap      = 3.0
		estimateUSD   = 1.0
		actualUSD     = 1.5
		underestimate = actualUSD - estimateUSD
		goroutines    = 200
	)

	e, _ := budgetEnforcer(t, Limits{DailyUSD: dailyCap})

	var (
		mu       sync.Mutex
		admitted int
		denials  []*Denial
		problems []error
	)
	record := func(err error) {
		mu.Lock()
		problems = append(problems, err)
		mu.Unlock()
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start

			r, err := e.Reserve(context.Background(), budgetTenant, budgetEstUSD(estimateUSD))
			if err != nil {
				var d *Denial
				if !errors.Is(err, ErrDenied) || !errors.As(err, &d) {
					record(err)
					return
				}
				mu.Lock()
				denials = append(denials, d)
				mu.Unlock()
				return
			}
			if r == nil {
				record(errors.New("Reserve returned a nil reservation and a nil error"))
				return
			}

			mu.Lock()
			admitted++
			mu.Unlock()

			if err := e.Settle(context.Background(), r, budgetUsageOf(r.Estimate), actualUSD); err != nil {
				record(err)
			}
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range problems {
		t.Errorf("unexpected error from a worker: %v", err)
	}
	if got := admitted + len(denials) + len(problems); got != goroutines {
		t.Errorf("accounted outcomes = %d, want %d; some request neither succeeded nor was denied", got, goroutines)
	}

	// (a) Atomicity. Settles only ever push the total up here, since every
	// actual exceeds its estimate, so no admission can ever free room for
	// another. The ceiling is therefore what the cap holds at estimate
	// prices.
	const maxAdmissions = int(dailyCap / estimateUSD)
	if admitted > maxAdmissions {
		t.Fatalf("admitted = %d, want at most %d: reservation is not atomic, %d concurrent requests read the same remaining balance", admitted, maxAdmissions, goroutines)
	}
	// The floor comes from the opposite extreme, where every settle lands
	// before the next reserve is even attempted: one dollar spent as $1.50
	// still leaves $1.50 of a $3 cap, which fits another dollar estimate.
	// Admitting fewer than two means the enforcer is refusing requests that
	// genuinely fit, which costs availability for no safety.
	if admitted < 2 {
		t.Fatalf("admitted = %d, want at least 2: a $%v cap has room for a $%v estimate even after one $%v settlement", admitted, dailyCap, estimateUSD, actualUSD)
	}

	daily, monthly := e.Spent(budgetTenant)
	committed := e.Committed(budgetTenant)

	// (b) The bound itself.
	bound := dailyCap + float64(admitted)*underestimate
	if daily > bound+budgetEpsilon {
		t.Errorf("daily spend = %v, want at most the documented bound %v (cap %v + %d admitted * %v underestimate)", daily, bound, dailyCap, admitted, underestimate)
	}
	// Every admitted request settled, and each settle must have landed
	// whole. This is what separates "within the bound" from "within the
	// bound because updates were lost".
	if want := float64(admitted) * actualUSD; !budgetClose(daily, want) {
		t.Errorf("daily spend = %v, want exactly %d admitted * %v = %v", daily, admitted, actualUSD, want)
	}
	if !budgetClose(monthly, daily) {
		t.Errorf("monthly spend = %v, want %v: both windows are open here so they must agree", monthly, daily)
	}

	// (c) No negatives, and every claim closed.
	if daily < 0 || monthly < 0 || committed < 0 {
		t.Errorf("negative accounting: daily = %v, monthly = %v, committed = %v", daily, monthly, committed)
	}
	if !budgetClose(committed, 0) {
		t.Errorf("Committed = %v after every request settled, want 0", committed)
	}

	// Everyone who was turned away must have been turned away for the
	// budget, with no retry advice attached.
	for i, d := range denials {
		if d.Reason != DenyDailyBudget {
			t.Errorf("denial %d reason = %q, want %q", i, d.Reason, DenyDailyBudget)
		}
		if d.RetryAfter != 0 {
			t.Errorf("denial %d RetryAfter = %v, want 0", i, d.RetryAfter)
		}
	}

	// The budget really is gone afterwards. Spend is at least the cap by
	// now, so nothing more can fit.
	got, err := e.Reserve(context.Background(), budgetTenant, budgetEstUSD(estimateUSD))
	budgetDenied(t, got, err, DenyDailyBudget)
}

func TestConcurrentReserveAndSettleLoseNoUpdates(t *testing.T) {
	// Same shape as the bound test but with a cap nothing can reach, so
	// every request is admitted and the only question is whether the
	// arithmetic survives contention. A lost update here is a request the
	// tenant used and was never charged for, which is money the operator
	// eats.
	//
	// Amounts are eighths of a dollar, so the expected total is exact in
	// float64 no matter what order the additions happen in.
	tests := []struct {
		name string
		// releaseEvery n means every nth worker releases instead of
		// settling, mixing both terminal paths into the same contention.
		releaseEvery int
	}{
		{"settle_only", 0},
		{"mixed_settle_and_release", 4},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const (
				goroutines  = 200
				estimateUSD = 1.0
				actualUSD   = 0.125
			)
			e, _ := budgetEnforcer(t, Limits{DailyUSD: 1_000_000, MonthlyUSD: 1_000_000})

			var (
				mu       sync.Mutex
				settled  int
				problems []error
			)
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(goroutines)
			for i := 0; i < goroutines; i++ {
				go func(i int) {
					defer wg.Done()
					<-start

					r, err := e.Reserve(context.Background(), budgetTenant, budgetEstUSD(estimateUSD))
					if err != nil {
						mu.Lock()
						problems = append(problems, err)
						mu.Unlock()
						return
					}
					if r == nil {
						mu.Lock()
						problems = append(problems, errors.New("nil reservation with a nil error"))
						mu.Unlock()
						return
					}
					if tc.releaseEvery > 0 && i%tc.releaseEvery == 0 {
						if err := e.Release(context.Background(), r); err != nil {
							mu.Lock()
							problems = append(problems, err)
							mu.Unlock()
						}
						return
					}
					if err := e.Settle(context.Background(), r, budgetUsageOf(r.Estimate), actualUSD); err != nil {
						mu.Lock()
						problems = append(problems, err)
						mu.Unlock()
						return
					}
					mu.Lock()
					settled++
					mu.Unlock()
				}(i)
			}
			close(start)
			wg.Wait()

			for _, err := range problems {
				t.Errorf("unexpected error from a worker: %v", err)
			}

			wantSettled := goroutines
			if tc.releaseEvery > 0 {
				wantSettled = goroutines - (goroutines+tc.releaseEvery-1)/tc.releaseEvery
			}
			if settled != wantSettled {
				t.Fatalf("settled = %d, want %d", settled, wantSettled)
			}

			want := float64(settled) * actualUSD
			budgetBalance(t, e, budgetTenant, want, want, 0)
		})
	}
}

// --- rate limits -----------------------------------------------------

func TestEnforcerRequestRateLimit(t *testing.T) {
	// Unlike a budget, a rate limit does refill, so the denial has to say
	// so. A zero RetryAfter here would tell a well behaved client to give
	// up on a limit it only had to wait out.
	const perMinute = 3
	e, clock := budgetEnforcer(t, Limits{RequestsPerMinute: perMinute})
	est := budgetEstUSD(0)

	for i := 0; i < perMinute; i++ {
		budgetMustReserve(t, e, budgetTenant, est)
	}

	got, err := e.Reserve(context.Background(), budgetTenant, est)
	d := budgetDenied(t, got, err, DenyRequestRate)
	if d.RetryAfter <= 0 {
		t.Errorf("request rate denial RetryAfter = %v, want a positive wait", d.RetryAfter)
	}
	// A retry advice longer than the window itself is a lie that costs the
	// client throughput it is entitled to.
	if d.RetryAfter > time.Minute {
		t.Errorf("request rate denial RetryAfter = %v, want at most the one minute window", d.RetryAfter)
	}

	// Half a minute in, the window has not turned over under any reading of
	// "per minute", so the answer must not change.
	clock.Advance(30 * time.Second)
	got, err = e.Reserve(context.Background(), budgetTenant, est)
	budgetDenied(t, got, err, DenyRequestRate)

	// Ninety seconds later no earlier attempt lies within a minute of now,
	// so the allowance is whole again whether the window is fixed or
	// sliding.
	clock.Advance(90 * time.Second)
	for i := 0; i < perMinute; i++ {
		budgetMustReserve(t, e, budgetTenant, est)
	}
	got, err = e.Reserve(context.Background(), budgetTenant, est)
	budgetDenied(t, got, err, DenyRequestRate)
}

func TestEnforcerTokenRateLimit(t *testing.T) {
	// TokensPerMinute counts prompt plus completion, which is what the
	// Limits doc says and what an upstream actually charges for. The
	// numbers below are chosen so an implementation counting prompt tokens
	// alone would admit three requests instead of two.
	const perMinute = 300
	e, clock := budgetEnforcer(t, Limits{TokensPerMinute: perMinute})
	est := budgetEstTokens(100, 50)

	budgetMustReserve(t, e, budgetTenant, est)
	budgetMustReserve(t, e, budgetTenant, est)

	got, err := e.Reserve(context.Background(), budgetTenant, est)
	d := budgetDenied(t, got, err, DenyTokenRate)
	if d.RetryAfter <= 0 {
		t.Errorf("token rate denial RetryAfter = %v, want a positive wait", d.RetryAfter)
	}
	if d.RetryAfter > time.Minute {
		t.Errorf("token rate denial RetryAfter = %v, want at most the one minute window", d.RetryAfter)
	}

	clock.Advance(30 * time.Second)
	got, err = e.Reserve(context.Background(), budgetTenant, est)
	budgetDenied(t, got, err, DenyTokenRate)

	clock.Advance(90 * time.Second)
	budgetMustReserve(t, e, budgetTenant, est)

	t.Run("estimate_larger_than_the_whole_allowance", func(t *testing.T) {
		// A single request bigger than the per minute allowance can never
		// fit. Admitting it because the counter happens to be empty would
		// let one caller blow through the cap by sending one huge request.
		fresh, _ := budgetEnforcer(t, Limits{TokensPerMinute: perMinute})
		big := budgetEstTokens(perMinute, 1)
		r, err := fresh.Reserve(context.Background(), budgetTenant, big)
		budgetDenied(t, r, err, DenyTokenRate)
	})
}

// --- window rollover -------------------------------------------------

func TestEnforcerBudgetWindowsRollOver(t *testing.T) {
	// Yesterday's spend must stop counting against today's daily cap, or a
	// tenant that hit its limit once is offline forever. It must keep
	// counting against the month, or the monthly cap is decorative.
	//
	// Every step is driven by the injected clock. The advances are 25 hours
	// and 40 days, both far enough past their boundary that a calendar
	// window and a trailing window agree, so this test pins the behavior
	// without pinning an implementation choice types.go leaves open.
	e, clock := budgetEnforcer(t, Limits{DailyUSD: 1, MonthlyUSD: 2})

	// Day one: spend the whole daily cap.
	r := budgetMustReserve(t, e, budgetTenant, budgetEstUSD(1))
	budgetMustSettle(t, e, r, 1)
	budgetBalance(t, e, budgetTenant, 1, 1, 0)

	got, err := e.Reserve(context.Background(), budgetTenant, budgetEstUSD(0.25))
	budgetDenied(t, got, err, DenyDailyBudget)

	// Day two. The daily window has turned over on its own: Spent must
	// report the window as of the clock right now, without needing a
	// request to arrive first and trigger a lazy sweep.
	clock.Advance(25 * time.Hour)
	budgetBalance(t, e, budgetTenant, 0, 1, 0)

	r = budgetMustReserve(t, e, budgetTenant, budgetEstUSD(1))
	budgetMustSettle(t, e, r, 1)
	budgetBalance(t, e, budgetTenant, 1, 2, 0)

	// Day three. The daily allowance is fresh again, but the month is now
	// exhausted by two days of spending, so the monthly cap is what refuses
	// the request. This is the assertion the whole test exists for: a
	// naive implementation that resets one counter resets both, and this
	// request would be wrongly admitted with money already gone.
	clock.Advance(25 * time.Hour)
	budgetBalance(t, e, budgetTenant, 0, 2, 0)

	got, err = e.Reserve(context.Background(), budgetTenant, budgetEstUSD(0.25))
	budgetDenied(t, got, err, DenyMonthlyBudget)

	// Next month, both windows are clean.
	clock.Advance(40 * 24 * time.Hour)
	budgetBalance(t, e, budgetTenant, 0, 0, 0)

	r = budgetMustReserve(t, e, budgetTenant, budgetEstUSD(1))
	budgetMustSettle(t, e, r, 1)
	budgetBalance(t, e, budgetTenant, 1, 1, 0)
}

func TestEnforcerBudgetWindowHoldsWithinTheSameDay(t *testing.T) {
	// The mirror of the rollover test. Time passing inside a window must
	// not forgive anything, or the daily cap becomes an hourly one and the
	// tenant spends a multiple of what the operator configured.
	e, clock := budgetEnforcer(t, Limits{DailyUSD: 1, MonthlyUSD: 10})

	r := budgetMustReserve(t, e, budgetTenant, budgetEstUSD(1))
	budgetMustSettle(t, e, r, 1)

	clock.Advance(time.Hour)
	budgetBalance(t, e, budgetTenant, 1, 1, 0)

	got, err := e.Reserve(context.Background(), budgetTenant, budgetEstUSD(0.25))
	budgetDenied(t, got, err, DenyDailyBudget)
}

// --- store health ----------------------------------------------------

func TestEnforcerStoreDegradedHonorsFailClosed(t *testing.T) {
	// When the accounting store cannot answer, the enforcer is blind. What
	// it does then is the difference between a hard cap on real money and a
	// soft advisory limit, and types.go says the tenant chooses.
	const (
		strict TenantID = "strict-tenant"
		loose  TenantID = "loose-tenant"
	)
	clock := newBudgetFakeClock()
	e := budgetNew(t, map[TenantID]Limits{
		// Both have room to spare, so nothing but the store's health can be
		// what decides.
		strict: {DailyUSD: 100, FailClosed: true},
		loose:  {DailyUSD: 100, FailClosed: false},
	}, clock)

	// While the store is healthy the flag changes nothing.
	budgetMustReserve(t, e, strict, budgetEstUSD(1))
	budgetMustReserve(t, e, loose, budgetEstUSD(1))

	e.SetStoreHealthy(false)

	// Fail closed: refuse rather than spend money that cannot be counted.
	got, err := e.Reserve(context.Background(), strict, budgetEstUSD(1))
	budgetDenied(t, got, err, DenyStoreDegraded)

	// Fail open: the limit is advisory, so an accounting outage must not
	// become an availability outage.
	if r, err := e.Reserve(context.Background(), loose, budgetEstUSD(1)); err != nil {
		t.Errorf("Reserve for the fail open tenant = %v, want admitted while the store is degraded", err)
	} else if r == nil {
		t.Error("Reserve for the fail open tenant returned a nil reservation and a nil error")
	}

	// Degradation is a condition, not a latch: once the store answers
	// again the strict tenant must be served without a restart.
	e.SetStoreHealthy(true)
	budgetMustReserve(t, e, strict, budgetEstUSD(1))
}

// --- denial shape ----------------------------------------------------

func TestDenialWrapsErrDeniedAndExplainsItself(t *testing.T) {
	// The ingress maps a denial to a status and a client facing message
	// without knowing this package's concrete types. It needs three things
	// from every refusal, whatever refused it: errors.Is against ErrDenied,
	// a machine readable Reason, and a human readable Message.
	tests := []struct {
		name   string
		limits map[TenantID]Limits
		tenant TenantID
		// setup drives the enforcer to the edge of the limit under test.
		setup func(t *testing.T, e *MemEnforcer)
		est   Estimate
		want  DenyReason
	}{
		{
			name:   "unknown_tenant",
			limits: map[TenantID]Limits{budgetTenant: {DailyUSD: 100}},
			tenant: budgetMissingTenant,
			est:    budgetEstUSD(1),
			want:   DenyUnknownTenant,
		},
		{
			name:   "daily_budget",
			limits: map[TenantID]Limits{budgetTenant: {DailyUSD: 1}},
			tenant: budgetTenant,
			setup: func(t *testing.T, e *MemEnforcer) {
				budgetMustSettle(t, e, budgetMustReserve(t, e, budgetTenant, budgetEstUSD(1)), 1)
			},
			est:  budgetEstUSD(0.5),
			want: DenyDailyBudget,
		},
		{
			name:   "monthly_budget",
			limits: map[TenantID]Limits{budgetTenant: {MonthlyUSD: 1}},
			tenant: budgetTenant,
			setup: func(t *testing.T, e *MemEnforcer) {
				budgetMustSettle(t, e, budgetMustReserve(t, e, budgetTenant, budgetEstUSD(1)), 1)
			},
			est:  budgetEstUSD(0.5),
			want: DenyMonthlyBudget,
		},
		{
			name:   "request_rate",
			limits: map[TenantID]Limits{budgetTenant: {RequestsPerMinute: 1}},
			tenant: budgetTenant,
			setup: func(t *testing.T, e *MemEnforcer) {
				budgetMustReserve(t, e, budgetTenant, budgetEstUSD(0))
			},
			est:  budgetEstUSD(0),
			want: DenyRequestRate,
		},
		{
			name:   "token_rate",
			limits: map[TenantID]Limits{budgetTenant: {TokensPerMinute: 100}},
			tenant: budgetTenant,
			setup: func(t *testing.T, e *MemEnforcer) {
				budgetMustReserve(t, e, budgetTenant, budgetEstTokens(60, 40))
			},
			est:  budgetEstTokens(60, 40),
			want: DenyTokenRate,
		},
		{
			name:   "store_degraded",
			limits: map[TenantID]Limits{budgetTenant: {DailyUSD: 100, FailClosed: true}},
			tenant: budgetTenant,
			setup: func(_ *testing.T, e *MemEnforcer) {
				e.SetStoreHealthy(false)
			},
			est:  budgetEstUSD(1),
			want: DenyStoreDegraded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := budgetNew(t, tc.limits, newBudgetFakeClock())
			if tc.setup != nil {
				tc.setup(t, e)
			}

			beforeDaily, beforeMonthly := e.Spent(tc.tenant)
			beforeCommitted := e.Committed(tc.tenant)

			r, err := e.Reserve(context.Background(), tc.tenant, tc.est)
			d := budgetDenied(t, r, err, tc.want)

			// Whichever limit refused, the refusal itself must move no
			// money and hold no claim.
			budgetBalance(t, e, tc.tenant, beforeDaily, beforeMonthly, beforeCommitted)

			// Error() is what lands in a log line, so both halves have to be
			// in it or an operator reading the log learns only half of why
			// the request was refused.
			msg := d.Error()
			if !strings.Contains(msg, string(tc.want)) {
				t.Errorf("Error() = %q, want it to contain the reason %q", msg, tc.want)
			}
			if !strings.Contains(msg, d.Message) {
				t.Errorf("Error() = %q, want it to contain the message %q", msg, d.Message)
			}
		})
	}
}

// budgetContractTests is every test in this file, by value, so a second
// implementation can be held to the same contract by replaying them
// rather than by restating any of them. A test added above and not
// listed here is checked against the in memory enforcer only, which
// TestContractListIsComplete catches.
var budgetContractTests = []struct {
	name string
	fn   func(*testing.T)
}{
	{"EnforcerDeniesUnknownTenant", TestEnforcerDeniesUnknownTenant},
	{"EnforcerZeroLimitsMeansUnlimitedNotUnknown", TestEnforcerZeroLimitsMeansUnlimitedNotUnknown},
	{"EnforcerIsolatesTenants", TestEnforcerIsolatesTenants},
	{"ReserveStampsTheReservationFromTheInjectedClock", TestReserveStampsTheReservationFromTheInjectedClock},
	{"NewEnforcerAcceptsANilClock", TestNewEnforcerAcceptsANilClock},
	{"ReserveIDsAreDistinctUnderConcurrency", TestReserveIDsAreDistinctUnderConcurrency},
	{"EnforcerBudgetAdmitsUntilTheEstimateNoLongerFits", TestEnforcerBudgetAdmitsUntilTheEstimateNoLongerFits},
	{"EnforcerBudgetDenialIsPerRequestNotALatch", TestEnforcerBudgetDenialIsPerRequestNotALatch},
	{"EnforcerBudgetDenialTieIsOneOfTheTwoBudgets", TestEnforcerBudgetDenialTieIsOneOfTheTwoBudgets},
	{"SettleRecordsActualNotTheEstimate", TestSettleRecordsActualNotTheEstimate},
	{"ReleaseReturnsTheWholeReservation", TestReleaseReturnsTheWholeReservation},
	{"SettleIsIdempotentPerReservation", TestSettleIsIdempotentPerReservation},
	{"SettleAndReleaseAreTerminalFirstOneWins", TestSettleAndReleaseAreTerminalFirstOneWins},
	{"SettleAndReleaseRejectANilReservation", TestSettleAndReleaseRejectANilReservation},
	{"EnforcerOvershootStaysWithinTheDocumentedBound", TestEnforcerOvershootStaysWithinTheDocumentedBound},
	{"ConcurrentReserveAndSettleLoseNoUpdates", TestConcurrentReserveAndSettleLoseNoUpdates},
	{"EnforcerRequestRateLimit", TestEnforcerRequestRateLimit},
	{"EnforcerTokenRateLimit", TestEnforcerTokenRateLimit},
	{"EnforcerBudgetWindowsRollOver", TestEnforcerBudgetWindowsRollOver},
	{"EnforcerBudgetWindowHoldsWithinTheSameDay", TestEnforcerBudgetWindowHoldsWithinTheSameDay},
	{"EnforcerStoreDegradedHonorsFailClosed", TestEnforcerStoreDegradedHonorsFailClosed},
	{"DenialWrapsErrDeniedAndExplainsItself", TestDenialWrapsErrDeniedAndExplainsItself},
}

// A list maintained by hand rots. This reads the file back and fails if
// a test was added without being listed, which is the only way the
// replay silently stops covering something.
func TestContractListIsComplete(t *testing.T) {
	src, err := os.ReadFile("enforcer_contract_test.go")
	if err != nil {
		t.Fatalf("read own source: %v", err)
	}
	declared := regexp.MustCompile(`(?m)^func (Test\w+)\(`).FindAllStringSubmatch(string(src), -1)

	listed := make(map[string]bool, len(budgetContractTests))
	for _, tc := range budgetContractTests {
		listed["Test"+tc.name] = true
	}
	// This test is itself excluded: replaying it against a second
	// implementation would only re-read this file.
	listed["TestContractListIsComplete"] = true

	for _, m := range declared {
		if !listed[m[1]] {
			t.Errorf("%s is not in budgetContractTests, so it never runs against the durable enforcer", m[1])
		}
	}
}
