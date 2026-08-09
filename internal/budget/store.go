package budget

import (
	"context"
	"time"
)

// Store persists the part of a tenant's accounting that has to outlive
// the process.
//
// It is a mirror, not the source of truth. Every decision is still made
// in memory under one mutex, which is what keeps Reserve free of I/O and
// keeps Spent and Committed answerable without an error return they have
// no way to report. The store is written when a settlement changes the
// running totals, and read once at startup.
//
// One process may open a given store. Two gateways pointed at one is not
// supported and is not checked for.
type Store interface {
	// Load returns every row the store holds. Windows are not rolled
	// forward here: the enforcer applies its own rollover on first touch,
	// so a row from five seconds ago and one from five days ago take the
	// same path.
	Load(ctx context.Context) ([]SpendRow, error)
	// Save writes one tenant's totals, replacing whatever was there.
	Save(ctx context.Context, row SpendRow) error
	// Close releases the underlying handle.
	Close() error
}

// SpendRow is one tenant's durable state. It is deliberately small, and
// what it leaves out matters as much as what it holds.
//
// Committed is absent because an outstanding reservation cannot survive
// the process that issued it. A *Reservation lives on the request
// goroutine's stack and is never serialized, so after a restart no
// Settle or Release can arrive for one. Persisting a claim would mean
// restoring money that nothing will ever return: a tenant that crashed
// with nine dollars of a ten dollar cap in flight would sit at ninety
// percent until the window rolled. Restoring zero is not an
// approximation, it is correct.
//
// The rate limit windows are absent for a different reason. They are
// incremented in Reserve, so persisting them would put a disk write on
// the admission path, which is the one place this design refuses to go.
// The cost of losing them is at most one minute at twice the configured
// rate, once per restart, and not something a caller can trigger.
type SpendRow struct {
	Tenant TenantID
	// DailyUSD and MonthlyUSD are settled spend, never estimates.
	DailyUSD     float64
	MonthlyUSD   float64
	DailyStart   time.Time
	MonthlyStart time.Time
}
