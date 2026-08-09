package budget

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	// Pure Go SQLite. A cgo driver would not link under CGO_ENABLED=0 and
	// would not run in the distroless static image this ships in.
	_ "modernc.org/sqlite"
)

// schemaVersion is the version this binary understands. Bump it and
// append to migrations; never edit a shipped migration.
const schemaVersion = 1

// sqliteDriver is the name modernc.org/sqlite registers itself under.
const sqliteDriver = "sqlite"

// SQLiteStore keeps settled tenant totals in a local SQLite file.
//
// The database never does arithmetic on money. Go computes the running
// total and hands over a snapshot, so storage is a byte copy and the
// exactness argument is trivial: SQLite REAL is IEEE-754 binary64, the
// same as Go float64. Integer cents would be worse than awkward here,
// because a settlement of 0.125 USD is 12.5 cents and has no integer
// cent representation at all.
//
// Timestamps are stored as unix nanoseconds. The store is never given a
// clock and never asks the database for one: no CURRENT_TIMESTAMP, no
// strftime('now'). Every instant it writes arrives as a parameter, which
// is what keeps the enforcer's injected clock authoritative.
type SQLiteStore struct {
	db   *sql.DB
	save *sql.Stmt
}

// OpenSQLiteStore opens or creates the store at path.
//
// A missing file is a first run. A file that fails an integrity check,
// or that was written by a newer schema, refuses to open rather than
// being repaired or ignored: starting fresh would silently forgive every
// tenant's spend, which looks exactly like a cap that is working.
func OpenSQLiteStore(path string) (*SQLiteStore, error) {
	// WAL so a reader never blocks the writer. synchronous=NORMAL means
	// commits are not fsynced and the WAL is flushed at checkpoint, which
	// is the same durability posture FileLedger already documents: a
	// process crash loses nothing, a power cut can lose the last few
	// seconds. busy_timeout is belt and braces for an operator poking at
	// the file with the sqlite3 CLI; it never applies to our own path.
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)"

	db, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	// One connection is what makes SQLITE_BUSY structurally impossible in
	// process: database/sql serializes every statement, so there is never
	// a second writer nor a reader racing one. Multi node is out of scope,
	// so nothing else opens this file for writing.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), storeOpenTimeout)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := checkIntegrity(ctx, db, path); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrate(ctx, db, path); err != nil {
		_ = db.Close()
		return nil, err
	}

	save, err := db.PrepareContext(ctx, upsertSQL)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("prepare upsert: %w", err)
	}
	return &SQLiteStore{db: db, save: save}, nil
}

// storeOpenTimeout bounds the work done at startup. An integrity check
// on a file holding one row per tenant is microseconds; this exists so a
// pathological filesystem fails the boot instead of hanging it.
const storeOpenTimeout = 30 * time.Second

// checkIntegrity refuses a corrupt file rather than repairing it. A
// money file is not something to quietly rebuild: an operator who is
// told will move it aside knowing every window restarts from zero, which
// is a decision they should get to make.
func checkIntegrity(ctx context.Context, db *sql.DB, path string) error {
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("integrity check on %s: %w", path, err)
	}
	if result != "ok" {
		return fmt.Errorf(
			"budget store at %s failed its integrity check (%s); move it aside to start with clean counters, "+
				"which restarts every tenant's window from zero", path, result)
	}
	return nil
}

// migrations takes the file from version i to i+1. Append only.
var migrations = []func(context.Context, *sql.Tx) error{
	migrate0to1,
}

func migrate(ctx context.Context, db *sql.DB, path string) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version of %s: %w", path, err)
	}

	if version > schemaVersion {
		// A newer binary wrote this. Opening it would mean silently
		// dropping columns that binary is still writing.
		return fmt.Errorf(
			"budget store at %s is schema version %d, this binary understands %d",
			path, version, schemaVersion)
	}

	for v := version; v < schemaVersion; v++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", v+1, err)
		}
		if err := migrations[v](ctx, tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d on %s: %w", v+1, path, err)
		}
		// The version bump rides in the same transaction as the change it
		// describes, so a half applied migration is not reachable.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("set schema version %d on %s: %w", v+1, path, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", v+1, err)
		}
	}
	return nil
}

// migrate0to1 creates the only table. STRICT so a REAL column cannot
// quietly hold text, which SQLite's default affinity rules permit.
func migrate0to1(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS tenant_spend (
    tenant        TEXT    NOT NULL PRIMARY KEY,
    daily_usd     REAL    NOT NULL,
    daily_start   INTEGER NOT NULL,
    monthly_usd   REAL    NOT NULL,
    monthly_start INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
) STRICT`)
	return err
}

const upsertSQL = `
INSERT INTO tenant_spend (tenant, daily_usd, daily_start, monthly_usd, monthly_start, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(tenant) DO UPDATE SET
  daily_usd=excluded.daily_usd,
  daily_start=excluded.daily_start,
  monthly_usd=excluded.monthly_usd,
  monthly_start=excluded.monthly_start,
  updated_at=excluded.updated_at`

// Save replaces one tenant's row.
//
// updated_at is operator forensics only, for answering "is this file
// stale". It is taken from the row's own daily start rather than from a
// wall clock, because this type deliberately has no clock of its own.
func (s *SQLiteStore) Save(ctx context.Context, row SpendRow) error {
	_, err := s.save.ExecContext(ctx,
		string(row.Tenant),
		row.DailyUSD,
		row.DailyStart.UnixNano(),
		row.MonthlyUSD,
		row.MonthlyStart.UnixNano(),
		row.DailyStart.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("save tenant %q: %w", row.Tenant, err)
	}
	return nil
}

// Load returns every row. Windows are not rolled forward here: the
// enforcer applies its own rollover on first touch, so a row from five
// seconds ago and one from five weeks ago take the same path.
func (s *SQLiteStore) Load(ctx context.Context) ([]SpendRow, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT tenant, daily_usd, daily_start, monthly_usd, monthly_start FROM tenant_spend")
	if err != nil {
		return nil, fmt.Errorf("load tenant spend: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SpendRow
	for rows.Next() {
		var (
			tenant       string
			dailyUSD     float64
			dailyStart   int64
			monthlyUSD   float64
			monthlyStart int64
		)
		if err := rows.Scan(&tenant, &dailyUSD, &dailyStart, &monthlyUSD, &monthlyStart); err != nil {
			return nil, fmt.Errorf("scan tenant spend: %w", err)
		}
		out = append(out, SpendRow{
			Tenant:       TenantID(tenant),
			DailyUSD:     dailyUSD,
			MonthlyUSD:   monthlyUSD,
			DailyStart:   time.Unix(0, dailyStart).UTC(),
			MonthlyStart: time.Unix(0, monthlyStart).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tenant spend: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) Close() error {
	var errs []error
	if s.save != nil {
		errs = append(errs, s.save.Close())
	}
	if s.db != nil {
		errs = append(errs, s.db.Close())
	}
	return errors.Join(errs...)
}
