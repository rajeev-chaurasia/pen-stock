package budget

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// memStore is a Store with no durability at all, which is exactly what
// the seam needs: it proves the contract still holds when settlements
// route through a store, without a driver in the way. If the replay
// below breaks against this, the problem is the enforcer, not SQLite.
type memStore struct {
	mu      sync.Mutex
	rows    map[TenantID]SpendRow
	saves   int
	failing error
}

func newMemStore() *memStore { return &memStore{rows: make(map[TenantID]SpendRow)} }

func (m *memStore) Load(context.Context) ([]SpendRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failing != nil {
		return nil, m.failing
	}
	out := make([]SpendRow, 0, len(m.rows))
	for _, r := range m.rows {
		out = append(out, r)
	}
	return out, nil
}

func (m *memStore) Save(_ context.Context, row SpendRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failing != nil {
		return m.failing
	}
	m.saves++
	m.rows[row.Tenant] = row
	return nil
}

func (m *memStore) Close() error { return nil }

func (m *memStore) fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failing = err
}

func (m *memStore) row(t TenantID) (SpendRow, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[t]
	return r, ok
}

// The durable enforcer makes every decision with the same code the in
// memory one does, because the store is a mirror rather than the source
// of truth. So the contract is not restated here, it is replayed: the
// same assertion functions, run a second time against an enforcer that
// persists on every settlement.
func TestDurableEnforcerRunsTheWholeContract(t *testing.T) {
	original := budgetNew
	budgetNew = func(t *testing.T, limits map[TenantID]Limits, clock Clock) *MemEnforcer {
		t.Helper()
		e, err := NewDurableEnforcer(DurableOptions{
			Limits: limits,
			Clock:  clock,
			Store:  newMemStore(),
		})
		if err != nil {
			t.Fatalf("NewDurableEnforcer: %v", err)
		}
		return e
	}
	t.Cleanup(func() { budgetNew = original })

	for _, tc := range budgetContractTests {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t) })
	}
}

// What the in memory suite structurally cannot express: a process
// boundary. It can advance a clock, but it cannot forget.
func TestSpendSurvivesARestart(t *testing.T) {
	store := newMemStore()
	clock := newBudgetFakeClock()
	limits := map[TenantID]Limits{budgetTenant: {DailyUSD: 100}}

	first, err := NewDurableEnforcer(DurableOptions{Limits: limits, Clock: clock, Store: store})
	if err != nil {
		t.Fatalf("NewDurableEnforcer: %v", err)
	}

	// 0.125 is deliberate: it is 12.5 cents, so an implementation storing
	// integer cents cannot represent it at all.
	const each = 0.125
	const times = 150
	for i := 0; i < times; i++ {
		r, err := first.Reserve(context.Background(), budgetTenant, Estimate{USD: each})
		if err != nil {
			t.Fatalf("Reserve %d: %v", i, err)
		}
		if err := first.Settle(context.Background(), r, providers.Usage{}, each); err != nil {
			t.Fatalf("Settle %d: %v", i, err)
		}
	}

	want := each * times // exactly 18.75
	if daily, _ := first.Spent(budgetTenant); daily != want {
		t.Fatalf("before restart daily = %v, want exactly %v", daily, want)
	}

	// The process ends. Everything in memory goes with it.
	second, err := NewDurableEnforcer(DurableOptions{Limits: limits, Clock: clock, Store: store})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	daily, monthly := second.Spent(budgetTenant)
	if daily != want || monthly != want {
		t.Errorf("after restart daily = %v monthly = %v, want %v: the whole point of the store", daily, monthly, want)
	}
}

// Rolling forward across a boundary the process was down for is the
// other half, and the reason restore does no rollover of its own.
func TestRestartRollsTheDailyWindowButNotTheMonthly(t *testing.T) {
	store := newMemStore()
	clock := newBudgetFakeClock()
	limits := map[TenantID]Limits{budgetTenant: {DailyUSD: 100}}

	e, err := NewDurableEnforcer(DurableOptions{Limits: limits, Clock: clock, Store: store})
	if err != nil {
		t.Fatalf("NewDurableEnforcer: %v", err)
	}
	r := budgetMustReserve(t, e, budgetTenant, Estimate{USD: 5})
	budgetMustSettle(t, e, r, 5)

	// Down for a day and an hour.
	clock.Advance(25 * time.Hour)
	after, err := NewDurableEnforcer(DurableOptions{Limits: limits, Clock: clock, Store: store})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	daily, monthly := after.Spent(budgetTenant)
	if daily != 0 {
		t.Errorf("daily = %v after crossing a day boundary while down, want 0", daily)
	}
	if monthly != 5 {
		t.Errorf("monthly = %v, want 5: a fresh day must not forgive the month", monthly)
	}

	// And down long enough for both.
	clock.Advance(40 * 24 * time.Hour)
	later, err := NewDurableEnforcer(DurableOptions{Limits: limits, Clock: clock, Store: store})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if daily, monthly = later.Spent(budgetTenant); daily != 0 || monthly != 0 {
		t.Errorf("daily = %v monthly = %v after 40 days down, want both 0", daily, monthly)
	}
}

// Restoring a claim would strand it forever, because nothing can settle
// or release a reservation whose process is gone. This fences the
// decision against a well meaning fix.
func TestCommittedIsNotRestored(t *testing.T) {
	store := newMemStore()
	clock := newBudgetFakeClock()
	limits := map[TenantID]Limits{budgetTenant: {DailyUSD: 10}}

	e, err := NewDurableEnforcer(DurableOptions{Limits: limits, Clock: clock, Store: store})
	if err != nil {
		t.Fatalf("NewDurableEnforcer: %v", err)
	}
	// Reserved and never settled, the way an in flight request dies.
	if _, err := e.Reserve(context.Background(), budgetTenant, Estimate{USD: 9}); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	after, err := NewDurableEnforcer(DurableOptions{Limits: limits, Clock: clock, Store: store})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if got := after.Committed(budgetTenant); got != 0 {
		t.Errorf("committed = %v after a restart, want 0: no Settle can ever arrive for it", got)
	}
	// And the budget it was holding is usable again.
	if _, err := after.Reserve(context.Background(), budgetTenant, Estimate{USD: 10}); err != nil {
		t.Errorf("the full cap should be reservable after a restart, got %v", err)
	}
}

func TestRateWindowsStartFreshAfterARestart(t *testing.T) {
	store := newMemStore()
	clock := newBudgetFakeClock()
	limits := map[TenantID]Limits{budgetTenant: {RequestsPerMinute: 2}}

	e, err := NewDurableEnforcer(DurableOptions{Limits: limits, Clock: clock, Store: store})
	if err != nil {
		t.Fatalf("NewDurableEnforcer: %v", err)
	}
	budgetMustReserve(t, e, budgetTenant, Estimate{})
	budgetMustReserve(t, e, budgetTenant, Estimate{})
	r3, err3 := e.Reserve(context.Background(), budgetTenant, Estimate{})
	budgetDenied(t, r3, err3, DenyRequestRate)

	// Deliberate, and documented on SpendRow: the rate windows are not
	// persisted, because they are incremented in Reserve and persisting
	// them would put a disk write on the admission path.
	after, err := NewDurableEnforcer(DurableOptions{Limits: limits, Clock: clock, Store: store})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if _, err := after.Reserve(context.Background(), budgetTenant, Estimate{}); err != nil {
		t.Errorf("the rate window should start fresh after a restart, got %v", err)
	}
}

// A tenant dropped from the config keeps its row, so putting it back
// does not hand it a fresh budget.
func TestRestoreSkipsUnconfiguredTenantsAndKeepsTheirRows(t *testing.T) {
	store := newMemStore()
	clock := newBudgetFakeClock()
	both := map[TenantID]Limits{budgetTenant: {DailyUSD: 100}, budgetOtherTenant: {DailyUSD: 100}}

	e, err := NewDurableEnforcer(DurableOptions{Limits: both, Clock: clock, Store: store})
	if err != nil {
		t.Fatalf("NewDurableEnforcer: %v", err)
	}
	r := budgetMustReserve(t, e, budgetOtherTenant, Estimate{USD: 7})
	budgetMustSettle(t, e, r, 7)

	// Restart with that tenant removed from the config.
	only := map[TenantID]Limits{budgetTenant: {DailyUSD: 100}}
	if _, err = NewDurableEnforcer(DurableOptions{Limits: only, Clock: clock, Store: store}); err != nil {
		t.Fatalf("restart without the tenant: %v", err)
	}
	if _, ok := store.row(budgetOtherTenant); !ok {
		t.Fatal("the dropped tenant's row was deleted; re-adding it would grant a fresh budget")
	}

	// Put it back, and its spend is still there.
	restored, err := NewDurableEnforcer(DurableOptions{Limits: both, Clock: clock, Store: store})
	if err != nil {
		t.Fatalf("restart with the tenant back: %v", err)
	}
	if daily, _ := restored.Spent(budgetOtherTenant); daily != 7 {
		t.Errorf("daily = %v after removing and re-adding the tenant, want 7", daily)
	}
}

// A store that cannot answer is the condition DenyStoreDegraded and the
// per tenant fail_closed flag were built for, and until now nothing in a
// running gateway could produce it.
func TestSaveFailureDeniesFailClosedAndClearsOnRecovery(t *testing.T) {
	store := newMemStore()
	clock := newBudgetFakeClock()
	var lost []error
	limits := map[TenantID]Limits{
		budgetTenant:      {DailyUSD: 100, FailClosed: true},
		budgetOtherTenant: {DailyUSD: 100},
	}

	e, err := NewDurableEnforcer(DurableOptions{
		Limits: limits, Clock: clock, Store: store,
		OnDurabilityLost: func(err error) { lost = append(lost, err) },
	})
	if err != nil {
		t.Fatalf("NewDurableEnforcer: %v", err)
	}

	r := budgetMustReserve(t, e, budgetTenant, Estimate{USD: 1})
	diskFull := errors.New("no space left on device")
	store.fail(diskFull)

	if err := e.Settle(context.Background(), r, providers.Usage{}, 1); err == nil {
		t.Fatal("Settle reported success when the write failed")
	} else if !errors.Is(err, diskFull) {
		t.Errorf("Settle error = %v, want it to carry the cause", err)
	}
	if len(lost) != 1 {
		t.Errorf("OnDurabilityLost called %d times, want 1", len(lost))
	}

	// The figures are still right. Only their survival is at risk.
	if daily, _ := e.Spent(budgetTenant); daily != 1 {
		t.Errorf("daily = %v after a failed write, want the settlement still counted", daily)
	}

	// A hard cap refuses rather than spending what it cannot count.
	rd, errd := e.Reserve(context.Background(), budgetTenant, Estimate{USD: 1})
	budgetDenied(t, rd, errd, DenyStoreDegraded)
	// A tenant that did not ask to be stopped keeps being served.
	if _, err := e.Reserve(context.Background(), budgetOtherTenant, Estimate{USD: 1}); err != nil {
		t.Errorf("a fail open tenant was denied while the store was degraded: %v", err)
	}

	// Recovery needs no restart: the next write that lands clears it.
	store.fail(nil)
	r2 := budgetMustReserve(t, e, budgetOtherTenant, Estimate{USD: 1})
	budgetMustSettle(t, e, r2, 1)
	if _, err := e.Reserve(context.Background(), budgetTenant, Estimate{USD: 1}); err != nil {
		t.Errorf("the strict tenant was still denied after recovery: %v", err)
	}
}

// The snapshot written is the one taken under the lock, so two
// settlements landing out of order cannot move a total backwards.
func TestStaleSnapshotsDoNotOverwriteNewerTotals(t *testing.T) {
	store := newMemStore()
	e, err := NewDurableEnforcer(DurableOptions{
		Limits: map[TenantID]Limits{budgetTenant: {DailyUSD: 100}},
		Clock:  newBudgetFakeClock(),
		Store:  store,
	})
	if err != nil {
		t.Fatalf("NewDurableEnforcer: %v", err)
	}

	newer := SpendRow{Tenant: budgetTenant, DailyUSD: 10}
	older := SpendRow{Tenant: budgetTenant, DailyUSD: 4}
	if err := e.persist(context.Background(), newer, 2); err != nil {
		t.Fatalf("persist newer: %v", err)
	}
	if err := e.persist(context.Background(), older, 1); err != nil {
		t.Fatalf("persist older: %v", err)
	}

	got, ok := store.row(budgetTenant)
	if !ok {
		t.Fatal("no row written")
	}
	if got.DailyUSD != 10 {
		t.Errorf("stored daily = %v, want 10: an older snapshot overwrote a newer one", got.DailyUSD)
	}
}

// A cancelled request context must not cancel the write. A client that
// hangs up mid answer cancels the request, and the money was spent
// either way.
func TestSettleWritesEvenWhenTheRequestContextIsCancelled(t *testing.T) {
	store := newMemStore()
	e, err := NewDurableEnforcer(DurableOptions{
		Limits: map[TenantID]Limits{budgetTenant: {DailyUSD: 100}},
		Clock:  newBudgetFakeClock(),
		Store:  store,
	})
	if err != nil {
		t.Fatalf("NewDurableEnforcer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r, err := e.Reserve(ctx, budgetTenant, Estimate{USD: 2})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	cancel() // the client hung up

	if err := e.Settle(ctx, r, providers.Usage{}, 2); err != nil {
		t.Fatalf("Settle on a cancelled context: %v", err)
	}
	row, ok := store.row(budgetTenant)
	if !ok || row.DailyUSD != 2 {
		t.Errorf("row = %+v ok = %v, want the settlement persisted despite the cancellation", row, ok)
	}
}

// Release changes only committed, which is not persisted, so it must do
// no I/O at all.
func TestReleaseDoesNotWriteToTheStore(t *testing.T) {
	store := newMemStore()
	e, err := NewDurableEnforcer(DurableOptions{
		Limits: map[TenantID]Limits{budgetTenant: {DailyUSD: 100}},
		Clock:  newBudgetFakeClock(),
		Store:  store,
	})
	if err != nil {
		t.Fatalf("NewDurableEnforcer: %v", err)
	}
	r := budgetMustReserve(t, e, budgetTenant, Estimate{USD: 1})
	budgetMustRelease(t, e, r)

	store.mu.Lock()
	saves := store.saves
	store.mu.Unlock()
	if saves != 0 {
		t.Errorf("Release triggered %d writes, want 0", saves)
	}
}
