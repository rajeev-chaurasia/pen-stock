package budget

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// Window lengths for the two spend caps and the rate limits.
const (
	dailyWindow   = 24 * time.Hour
	monthlyWindow = 30 * 24 * time.Hour
	rateWindow    = time.Minute

	// storeWriteTimeout bounds one durable write, so a pathological disk
	// cannot wedge a settlement forever. Not configurable: it is a single
	// row upsert on a local file.
	storeWriteTimeout = 5 * time.Second
)

// Clock is injectable so window rollover and rate limits are tested
// without sleeping.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// MemEnforcer keeps tenant accounting in memory behind one mutex.
//
// A mutex rather than atomics on purpose: admitting a request means
// reading several counters and updating them together, and that has to
// be one indivisible step. Anything finer grained lets two requests
// both observe room that only one of them can have.
type MemEnforcer struct {
	mu      sync.Mutex
	limits  map[TenantID]Limits
	state   map[TenantID]*tenantState
	clock   Clock
	healthy bool
	nextID  uint64
	// done records reservations that have already settled or been
	// released, so a repeat of either is ignored rather than counted
	// twice.
	//
	// It is not persisted, and does not need to be. A *Reservation never
	// leaves the process, so after a restart none exists to be settled a
	// second time. That stops being true the day anything accepts a
	// client supplied idempotency key or resumes a request across
	// processes, and this is the comment that should stop it.
	done map[string]bool

	// store mirrors the settled totals so they outlive the process. Nil
	// means memory only, which is the default and what a local run wants.
	store Store
	// storeMu serializes durable writes. Deliberately separate from mu:
	// Reserve never touches it, so admission never waits on a disk.
	storeMu sync.Mutex
	// seq and written order the writes. seq is bumped under mu when a
	// snapshot is taken, written records the newest seq that reached the
	// store, and persist drops anything older.
	seq              map[TenantID]uint64
	written          map[TenantID]uint64
	onDurabilityLost func(error)
}

// tenantState is one tenant's counters. Windows carry their own start
// so a stale window can be recognized and cleared on read.
type tenantState struct {
	dailySpent   float64
	dailyStart   time.Time
	monthlySpent float64
	monthlyStart time.Time
	committed    float64

	requests   int
	requestsAt time.Time
	tokens     int
	tokensAt   time.Time
}

// NewEnforcer builds an enforcer over a fixed set of tenants. A nil
// clock means real time.
func NewEnforcer(limits map[TenantID]Limits, clock Clock) *MemEnforcer {
	if clock == nil {
		clock = systemClock{}
	}
	copied := make(map[TenantID]Limits, len(limits))
	for id, l := range limits {
		copied[id] = l
	}
	return &MemEnforcer{
		limits:  copied,
		state:   make(map[TenantID]*tenantState, len(limits)),
		clock:   clock,
		healthy: true,
		done:    make(map[string]bool),
		seq:     make(map[TenantID]uint64, len(limits)),
		written: make(map[TenantID]uint64, len(limits)),
	}
}

// DurableOptions configures an enforcer whose settled totals outlive the
// process.
type DurableOptions struct {
	Limits map[TenantID]Limits
	Clock  Clock
	Store  Store
	// OnDurabilityLost reports a settlement that could not be written.
	// The figures in memory are still right; only their survival is at
	// risk, so this is a report rather than a decision.
	OnDurabilityLost func(error)
}

// NewDurableEnforcer builds an enforcer backed by a store, restoring the
// settled totals it already holds.
//
// A restore failure is fatal rather than degraded. Starting with zeros
// would forgive every tenant's spend while looking exactly like a cap
// that is working, which is the failure this whole feature exists to
// prevent.
func NewDurableEnforcer(opts DurableOptions) (*MemEnforcer, error) {
	e := NewEnforcer(opts.Limits, opts.Clock)
	if opts.Store == nil {
		return e, nil
	}
	e.store = opts.Store
	e.onDurabilityLost = opts.OnDurabilityLost

	rows, err := opts.Store.Load(context.Background())
	if err != nil {
		return nil, fmt.Errorf("restore budget store: %w", err)
	}

	// The injected clock, never time.Now: every window boundary in this
	// package is driven by it, and restore is not an exception.
	now := e.clock.Now()
	for _, row := range rows {
		if _, configured := e.limits[row.Tenant]; !configured {
			// A tenant that is no longer in the config keeps its row. An
			// operator who removes one and puts it back must not be handed
			// a fresh daily budget by doing so.
			continue
		}
		e.state[row.Tenant] = &tenantState{
			dailySpent:   row.DailyUSD,
			dailyStart:   row.DailyStart,
			monthlySpent: row.MonthlyUSD,
			monthlyStart: row.MonthlyStart,
			// committed stays zero: see SpendRow. The rate windows start
			// fresh for the same documented reason.
			requestsAt: now,
			tokensAt:   now,
		}
	}
	// Windows are rolled by stateFor on first touch, which every one of
	// Reserve, Spent and Committed goes through. Down for five seconds
	// and down across a month boundary take the same path.
	return e, nil
}

// SetStoreHealthy marks the accounting store as able or unable to
// answer. Tests use it to drive the degraded path directly; in a running
// gateway a failed durable write flips it.
func (e *MemEnforcer) SetStoreHealthy(healthy bool) {
	e.setStoreHealthy(healthy)
}

func (e *MemEnforcer) setStoreHealthy(healthy bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.healthy = healthy
}

// Spent reports settled spend in the tenant's current windows, as of
// now rather than as of the last request: a caller reading a dashboard
// must see yesterday's cap released even if nothing has arrived since.
func (e *MemEnforcer) Spent(t TenantID) (daily, monthly float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.stateFor(t, e.clock.Now())
	return st.dailySpent, st.monthlySpent
}

// Committed reports reserved but unsettled spend.
func (e *MemEnforcer) Committed(t TenantID) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stateFor(t, e.clock.Now()).committed
}

func (e *MemEnforcer) Reserve(_ context.Context, tenant TenantID, est Estimate) (*Reservation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	limits, known := e.limits[tenant]
	if !known {
		return nil, &Denial{
			Reason:  DenyUnknownTenant,
			Message: fmt.Sprintf("tenant %q is not configured", tenant),
		}
	}

	if !e.healthy {
		// Blind. A hard cap on real money refuses rather than guesses;
		// a soft limit would rather serve the request and be told later.
		if limits.FailClosed {
			return nil, &Denial{
				Reason:  DenyStoreDegraded,
				Message: "budget accounting is unavailable and this tenant is configured to fail closed",
			}
		}
	}

	now := e.clock.Now()
	st := e.stateFor(tenant, now)

	if d := checkRates(st, limits, est, now); d != nil {
		return nil, d
	}
	if d := checkBudgets(st, limits, est); d != nil {
		return nil, d
	}

	// Claim the estimate before returning. Everything above ran under
	// the same lock, so no other request could have seen this room.
	st.committed += est.USD
	st.requests++
	st.tokens += est.PromptTokens + est.CompletionTokens

	e.nextID++
	return &Reservation{
		Tenant:   tenant,
		Estimate: est,
		IssuedAt: now,
		ID:       strconv.FormatUint(e.nextID, 10),
	}, nil
}

func (e *MemEnforcer) Settle(ctx context.Context, r *Reservation, actual providers.Usage, usd float64) error {
	if r == nil {
		return fmt.Errorf("settle: nil reservation")
	}
	// Not deferred. The durable write below must happen outside this
	// lock, because holding it across a syscall would serialize every
	// concurrent Reserve behind a disk write.
	e.mu.Lock()
	if e.done[r.ID] {
		// Already terminal. Counting it again would bill twice.
		e.mu.Unlock()
		return nil
	}
	e.done[r.ID] = true

	st := e.stateFor(r.Tenant, e.clock.Now())
	// The claim goes back and the real cost takes its place, so an
	// estimate that was too generous is refunded and one that was too
	// small is made good.
	st.committed -= r.Estimate.USD
	if st.committed < 0 {
		st.committed = 0
	}
	st.dailySpent += usd
	st.monthlySpent += usd

	// Reconcile the token window against what was really used. Leaving
	// it on the estimate would let a tenant whose answers routinely run
	// longer than predicted sit above its tokens per minute cap.
	if delta := actual.TotalTokens - (r.Estimate.PromptTokens + r.Estimate.CompletionTokens); delta != 0 {
		st.tokens += delta
		if st.tokens < 0 {
			st.tokens = 0
		}
	}

	if e.store == nil {
		e.mu.Unlock()
		return nil
	}
	// A sequence number taken under the same lock that produced the
	// snapshot. Two settlements for one tenant can reach persist in
	// either order once the lock is released, and without this the older,
	// smaller total can land last and quietly erase money. The race
	// detector will never see it: it is a reordering, not a data race.
	e.seq[r.Tenant]++
	row := SpendRow{
		Tenant:       r.Tenant,
		DailyUSD:     st.dailySpent,
		MonthlyUSD:   st.monthlySpent,
		DailyStart:   st.dailyStart,
		MonthlyStart: st.monthlyStart,
	}
	seq := e.seq[r.Tenant]
	e.mu.Unlock()

	return e.persist(ctx, row, seq)
}

// persist writes one snapshot, dropping it if a fresher one already
// landed for that tenant.
//
// A failure here does not mean the numbers are wrong. It means they will
// not survive a restart, which is exactly the condition DenyStoreDegraded
// and the per tenant fail_closed flag were built for, so the store is
// marked unhealthy and Reserve starts refusing the tenants that asked to
// be stopped rather than guessed at.
func (e *MemEnforcer) persist(ctx context.Context, row SpendRow, seq uint64) error {
	e.storeMu.Lock()
	defer e.storeMu.Unlock()

	if seq <= e.written[row.Tenant] {
		// A newer snapshot for this tenant already reached the store, and
		// it is a superset of this one. Writing this now would move the
		// totals backwards.
		return nil
	}

	// The caller's context belongs to an HTTP request that may already be
	// cancelled, because a client that hangs up mid answer cancels it.
	// Settlement outlives the client: the money was spent either way.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeWriteTimeout)
	defer cancel()

	if err := e.store.Save(writeCtx, row); err != nil {
		e.setStoreHealthy(false)
		if e.onDurabilityLost != nil {
			e.onDurabilityLost(err)
		}
		return fmt.Errorf("persist tenant %q: %w", row.Tenant, err)
	}
	e.written[row.Tenant] = seq
	e.setStoreHealthy(true)
	return nil
}

func (e *MemEnforcer) Release(_ context.Context, r *Reservation) error {
	if r == nil {
		return fmt.Errorf("release: nil reservation")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done[r.ID] {
		return nil
	}
	e.done[r.ID] = true

	st := e.stateFor(r.Tenant, e.clock.Now())
	st.committed -= r.Estimate.USD
	if st.committed < 0 {
		st.committed = 0
	}
	return nil
}

// stateFor returns the tenant's counters with any expired window
// already cleared, so every caller sees the same view of now.
func (e *MemEnforcer) stateFor(t TenantID, now time.Time) *tenantState {
	st, ok := e.state[t]
	if !ok {
		st = &tenantState{
			dailyStart:   now,
			monthlyStart: now,
			requestsAt:   now,
			tokensAt:     now,
		}
		e.state[t] = st
		return st
	}

	// Each window turns over independently. Resetting them together is
	// the classic bug: a fresh day would also forgive the month.
	if now.Sub(st.dailyStart) >= dailyWindow {
		st.dailySpent = 0
		st.dailyStart = now
	}
	if now.Sub(st.monthlyStart) >= monthlyWindow {
		st.monthlySpent = 0
		st.monthlyStart = now
	}
	if now.Sub(st.requestsAt) >= rateWindow {
		st.requests = 0
		st.requestsAt = now
	}
	if now.Sub(st.tokensAt) >= rateWindow {
		st.tokens = 0
		st.tokensAt = now
	}
	return st
}

// checkRates refuses a request that would exceed a per minute cap. Rate
// denials carry a RetryAfter because waiting genuinely helps.
func checkRates(st *tenantState, limits Limits, est Estimate, now time.Time) *Denial {
	if limits.RequestsPerMinute > 0 && st.requests >= limits.RequestsPerMinute {
		return &Denial{
			Reason:     DenyRequestRate,
			RetryAfter: remainingIn(st.requestsAt, now),
			Message: fmt.Sprintf("tenant is limited to %d requests per minute",
				limits.RequestsPerMinute),
		}
	}
	if limits.TokensPerMinute > 0 {
		wanted := est.PromptTokens + est.CompletionTokens
		if st.tokens+wanted > limits.TokensPerMinute {
			return &Denial{
				Reason:     DenyTokenRate,
				RetryAfter: remainingIn(st.tokensAt, now),
				Message: fmt.Sprintf("tenant is limited to %d tokens per minute",
					limits.TokensPerMinute),
			}
		}
	}
	return nil
}

// checkBudgets refuses a request whose estimate does not fit what is
// left. Budget denials carry no RetryAfter: unlike a rate limit, a
// spent budget does not refill by waiting a moment.
func checkBudgets(st *tenantState, limits Limits, est Estimate) *Denial {
	if limits.DailyUSD > 0 && st.dailySpent+st.committed+est.USD > limits.DailyUSD {
		return &Denial{
			Reason: DenyDailyBudget,
			Message: fmt.Sprintf("this request would exceed the daily budget of %s USD",
				formatUSD(limits.DailyUSD)),
		}
	}
	if limits.MonthlyUSD > 0 && st.monthlySpent+st.committed+est.USD > limits.MonthlyUSD {
		return &Denial{
			Reason: DenyMonthlyBudget,
			Message: fmt.Sprintf("this request would exceed the monthly budget of %s USD",
				formatUSD(limits.MonthlyUSD)),
		}
	}
	return nil
}

// formatUSD renders an amount readably at both ends of the range. Cents
// are the usual case, but a sub cent budget shown as 0.00 tells an
// operator their limit is zero when it is merely small.
func formatUSD(v float64) string {
	if v > 0 && v < 0.01 {
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// remainingIn reports how long the current window has left, floored at
// a moment so a caller is never told to retry immediately.
func remainingIn(start, now time.Time) time.Duration {
	left := rateWindow - now.Sub(start)
	if left <= 0 {
		return time.Second
	}
	return left
}
