// Package leakcheck holds the shared go.uber.org/goleak option set used by the
// control-plane integration tests (#391 D-1).
//
// It exists so the ignore list is written ONCE and reviewed as one thing. Every
// entry below must name a goroutine that is legitimately long-lived — a
// process-scoped background worker owned by a library, not by us. An ignore is
// never the right answer for our own goroutine: our goroutines have owners, and
// an owner that cannot stop its goroutine is the defect this guard is for.
//
// Only test files import this package; it is not linked into the server binary.
package leakcheck

import "go.uber.org/goleak"

// Options returns the baseline ignore set shared by every package that wires
// goleak.VerifyTestMain.
func Options() []goleak.Option {
	return []goleak.Option{
		// pgx's pool runs a background health-check/reaper goroutine for the life
		// of the pool. The tests share one process-wide pool built in setup and
		// deliberately never closed (closing it between packages would tear down
		// the shared fixture), so this worker is legitimately still running at
		// teardown.
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
		// puddle is pgx's underlying resource pool; its acquire/destruct workers
		// belong to the same never-closed pool.
		goleak.IgnoreTopFunction("github.com/jackc/puddle/v2.(*Pool[...]).backgroundHealthCheck"),
		// database/sql's connection opener is started by the first sql.DB in the
		// process (golang-migrate opens one to apply migrations in setup) and runs
		// until that DB is closed. Same rationale as the pgx pool.
		goleak.IgnoreTopFunction("database/sql.(*DB).connectionOpener"),
		// Go's own timer/select internals occasionally show up as the top frame of
		// a goroutine that is mid-exit when the profile is taken. goleak retries,
		// but these are never our leak.
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
	}
}

// With returns the baseline options plus extra package-specific ones.
func With(extra ...goleak.Option) []goleak.Option {
	return append(Options(), extra...)
}
