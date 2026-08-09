package budget

import (
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openTestStore opens a store in a temp directory. Close is registered
// first: on Windows, t.TempDir cleanup fails while a SQLite file is
// still open, and the resulting error names the directory rather than
// the test that leaked it.
func openTestStore(t *testing.T) (*SQLiteStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "budget.db")
	store, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

func TestSQLiteStoreRoundTrip(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()

	start := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)
	row := SpendRow{
		Tenant:       "acme",
		DailyUSD:     18.75,
		MonthlyUSD:   41.5,
		DailyStart:   start,
		MonthlyStart: start.Add(-72 * time.Hour),
	}
	if err := store.Save(ctx, row); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("loaded %d rows, want 1", len(got))
	}
	if got[0].Tenant != row.Tenant {
		t.Errorf("tenant = %q, want %q", got[0].Tenant, row.Tenant)
	}
	if !got[0].DailyStart.Equal(row.DailyStart) || !got[0].MonthlyStart.Equal(row.MonthlyStart) {
		t.Errorf("timestamps = %v / %v, want %v / %v",
			got[0].DailyStart, got[0].MonthlyStart, row.DailyStart, row.MonthlyStart)
	}
}

// Money must survive bit for bit, not merely close enough. Epsilon
// equality would hide a storage path that rounds, and the contract suite
// asserts exact sums.
func TestSQLiteStoreRoundTripsFloatsBitForBit(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()

	values := []float64{0.1, 0.125, 1e-9, 18.75, 5e7, 0.00007277, math.MaxFloat64 / 1e300}
	for i, v := range values {
		tenant := TenantID("t" + string(rune('a'+i)))
		if err := store.Save(ctx, SpendRow{Tenant: tenant, DailyUSD: v, MonthlyUSD: v}); err != nil {
			t.Fatalf("Save %v: %v", v, err)
		}
	}

	rows, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	byTenant := make(map[TenantID]SpendRow, len(rows))
	for _, r := range rows {
		byTenant[r.Tenant] = r
	}
	for i, v := range values {
		tenant := TenantID("t" + string(rune('a'+i)))
		got := byTenant[tenant].DailyUSD
		if math.Float64bits(got) != math.Float64bits(v) {
			t.Errorf("%v round tripped as %v (bits %x vs %x)", v, got, math.Float64bits(got), math.Float64bits(v))
		}
	}
}

func TestSQLiteStoreUpsertReplacesTheRow(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()

	for _, usd := range []float64{1, 2, 3} {
		if err := store.Save(ctx, SpendRow{Tenant: "acme", DailyUSD: usd}); err != nil {
			t.Fatalf("Save %v: %v", usd, err)
		}
	}
	rows, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("loaded %d rows, want 1: saves must replace, not append", len(rows))
	}
	if rows[0].DailyUSD != 3 {
		t.Errorf("daily = %v, want the latest value 3", rows[0].DailyUSD)
	}
}

func TestMissingFileIsAFirstRunNotAFailure(t *testing.T) {
	store, path := openTestStore(t)

	rows, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load on a fresh store: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a fresh store holds %d rows, want 0", len(rows))
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the file was not created: %v", err)
	}
}

func TestSchemaIsCreatedAtTheCurrentVersion(t *testing.T) {
	_, path := openTestStore(t)

	db, err := sql.Open(sqliteDriver, "file:"+path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db.Close() }()

	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}
}

// A file from a newer binary must not be opened, because a downgrade
// would silently drop columns the newer one is still writing.
func TestNewerSchemaVersionRefusesToStart(t *testing.T) {
	store, path := openTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err := sql.Open(sqliteDriver, "file:"+path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	_ = db.Close()

	reopened, err := OpenSQLiteStore(path)
	if err == nil {
		_ = reopened.Close()
		t.Fatal("a newer schema version was accepted")
	}
	if !strings.Contains(err.Error(), "schema version 99") {
		t.Errorf("error = %v, want it to name the version found", err)
	}
}

// A corrupt money file is refused rather than repaired, and is left
// where it is so an operator can decide what to do with it.
func TestCorruptFileRefusesToStartAndIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "budget.db")

	// A plausible SQLite header followed by rubbish, so the failure is an
	// integrity error rather than "this is not a database at all".
	corrupt := append([]byte("SQLite format 3\x00"), make([]byte, 4096)...)
	for i := 16; i < len(corrupt); i++ {
		corrupt[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}

	store, err := OpenSQLiteStore(path)
	if err == nil {
		_ = store.Close()
		t.Fatal("a corrupt store was opened")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to name the path so an operator can act on it", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("the corrupt file was removed: %v", statErr)
	}
}

// The store must never consult a clock of its own, or the enforcer's
// injected one stops being authoritative and every window test becomes a
// coin flip on real time.
func TestStoreWritesOnlyTimestampsItWasGiven(t *testing.T) {
	store, path := openTestStore(t)
	ctx := context.Background()

	// An instant far from now, which a wall clock could not produce.
	start := time.Date(2001, 9, 11, 0, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, SpendRow{Tenant: "acme", DailyStart: start, MonthlyStart: start}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err := sql.Open(sqliteDriver, "file:"+path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db.Close() }()

	var updatedAt int64
	if err := db.QueryRow("SELECT updated_at FROM tenant_spend WHERE tenant = 'acme'").Scan(&updatedAt); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	if updatedAt != start.UnixNano() {
		t.Errorf("updated_at = %v, want the supplied instant %v: the store reached for a clock",
			time.Unix(0, updatedAt).UTC(), start)
	}
}

// The same contract, replayed a third time, against a real file. The
// concurrency tests in that suite drive 200 goroutines through this
// store and treat any error as a failure, which is what makes it an
// executable proof that SQLITE_BUSY cannot arise on this path.
func TestSQLiteBackedEnforcerRunsTheWholeContract(t *testing.T) {
	original := budgetNew
	budgetNew = func(t *testing.T, limits map[TenantID]Limits, clock Clock) *MemEnforcer {
		t.Helper()
		store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "budget.db"))
		if err != nil {
			t.Fatalf("OpenSQLiteStore: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		e, err := NewDurableEnforcer(DurableOptions{Limits: limits, Clock: clock, Store: store})
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

// The end to end property, through a real file: spend, close, reopen.
func TestSpendSurvivesARestartThroughSQLite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "budget.db")
	clock := newBudgetFakeClock()
	limits := map[TenantID]Limits{budgetTenant: {DailyUSD: 100}}

	store, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	first, err := NewDurableEnforcer(DurableOptions{Limits: limits, Clock: clock, Store: store})
	if err != nil {
		t.Fatalf("NewDurableEnforcer: %v", err)
	}
	for i := 0; i < 150; i++ {
		r := budgetMustReserve(t, first, budgetTenant, Estimate{USD: 0.125})
		budgetMustSettle(t, first, r, 0.125)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	second, err := NewDurableEnforcer(DurableOptions{Limits: limits, Clock: clock, Store: reopened})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}

	daily, monthly := second.Spent(budgetTenant)
	if daily != 18.75 || monthly != 18.75 {
		t.Errorf("after restart daily = %v monthly = %v, want exactly 18.75", daily, monthly)
	}
}
