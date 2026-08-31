package session

import (
	"log/slog"
	"testing"
)

// newTestCoordinator is NewCoordinator with the shutdown every test owes it
// (#406). A Coordinator holds a lifecycle context that its background watchdogs
// (watchStartToRunning) derive from; without Close those watchdogs stay parked
// for the full startToRunningTimeout after their test has finished, which is why
// the package carried a goleak ignore for them. Not a production leak — the
// server has one Coordinator and closes it — purely test hygiene, but the ignore
// it forced blinded the guard to any real regression in the same function.
//
// Every test in this package builds its coordinator through here so the cleanup
// cannot be forgotten at the next call site added.
func newTestCoordinator(t *testing.T, store *Store, d Dispatcher, log *slog.Logger, opts ...CoordinatorOption) *Coordinator {
	t.Helper()
	c := NewCoordinator(store, d, log, opts...)
	t.Cleanup(c.Close)
	return c
}
