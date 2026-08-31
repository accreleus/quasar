package library

import (
	"context"
	"log/slog"
)

// Janitor is the discovery pass body (§11.1). It owns no ticker or goroutine:
// internal/jobs/library.discovery (app.go) is the trigger, RunOnce is its RunFunc body.
type Janitor struct {
	store    *Store
	settings SettingsReader
	// resolver resolves the scan interval per pass via the same resolver the admin status
	// endpoint reads (resolve.go), so the scheduler and the panel can't disagree.
	resolver *Resolver
	log      *slog.Logger
	// inertLogged suppresses a repeated "discovery is inert because X" line for the same
	// reason across passes.
	inertLogged string
}

// NewJanitor builds the discovery janitor. Interval is resolved per pass, never cached at
// construction, so an edited interval setting takes effect on the next pass with no restart.
func NewJanitor(store *Store, set SettingsReader, resolver *Resolver, log *slog.Logger) *Janitor {
	return &Janitor{store: store, settings: set, resolver: resolver, log: log}
}

// RunResult is the outcome of one janitor pass, recorded by the library.discovery job as its
// run summary.
type RunResult struct {
	Enqueued          int
	ReturnedAbandoned int
	Expired           int
	Pruned            int
}

// RunOnce performs one janitor pass and reports what it did. skipReason is empty when the
// pass ran (zero matched rows is a normal outcome, not a skip); non-empty names one of the
// three gates stopping the pass before any row-creating query — maps to jobs.Skipped(reason)
// in the RunFunc wrapper.
//
// Exported so a test or the RunFunc can drive one deterministic pass instead of waiting on
// a ticker.
func (j *Janitor) RunOnce(ctx context.Context) (RunResult, string) {
	var res RunResult

	// Step 1 gate: read per pass, never cached at construction, so a setting flipped in the
	// admin UI takes effect on the next pass with no restart. Off means no scan rows, no
	// agent requests, no third-party calls.
	enabled, err := j.settings.LibraryDiscoveryEnabled(ctx)
	if err != nil {
		j.log.Warn("library discovery: could not read the instance setting", "err", err)
		return res, ""
	}
	if !enabled {
		j.noteInert(reasonDiscoveryOff)
		return res, reasonDiscoveryOff
	}

	// interval <= 0 can in practice only come from QUASAR_LIBRARY_SCAN_INTERVAL (the hard
	// kill switch): the DB column is bounded 15..10080 by both a CHECK and the PATCH handler.
	interval, _, err := j.resolver.ScanInterval(ctx)
	if err != nil {
		j.log.Warn("library discovery: could not resolve the scan interval", "err", err)
		return res, ""
	}
	if interval <= 0 {
		j.noteInert(reasonIntervalZero)
		return res, reasonIntervalZero
	}

	// No storage_provider=="volume" gate here any more (#473: the setting can no longer
	// hold that value). A rootless instance is caught only by the admin status endpoint's
	// resolver-based inertReason; this pass needs no equivalent since ExpireStranded/Enqueue
	// below simply match zero rows with no local-driver home to walk.

	// Reports but does not return (not a skipReason): unlike the gates above, this only
	// predicts the enqueue will match nothing, a normal zero-count success, not a skip.
	// Returning early would also skip ExpireStranded, which matters when an operator unmarks
	// their last provider app and leaves pending scans stranded against the open-scan unique
	// index. A failed read logs a warning rather than claiming "no provider app".
	hasProvider, provErr := j.store.HasProviderApp(ctx)
	switch {
	case provErr != nil:
		j.log.Warn("library discovery: could not check for a library-provider app", "err", provErr)
		j.inertLogged = ""
	case !hasProvider:
		j.noteInert(reasonNoProviderApp)
	default:
		j.inertLogged = ""
	}

	// STEP 2: reap abandoned claims back to pending.
	if n, err := j.store.ReapClaimed(ctx, ClaimTTL); err != nil {
		j.log.Warn("library discovery: reap failed", "err", err)
	} else if n > 0 {
		j.log.Info("library discovery: returned abandoned scans to pending", "count", n)
		res.ReturnedAbandoned = n
	}

	// Fail pending scans whose home stopped being scannable, so the open-scan unique index
	// doesn't block that triple forever. Not in §11.1.
	if n, err := j.store.ExpireStranded(ctx); err != nil {
		j.log.Warn("library discovery: stranded-scan expiry failed", "err", err)
	} else if n > 0 {
		j.log.Info("library discovery: expired scans whose home is gone", "count", n)
		res.Expired = n
	}

	// STEP 3: enqueue.
	if n, err := j.store.Enqueue(ctx, interval); err != nil {
		j.log.Warn("library discovery: enqueue failed", "err", err)
	} else if n > 0 {
		j.log.Info("library discovery: enqueued scans", "count", n)
		res.Enqueued = n
	}

	// §11.1 step 4 ("reconcile any reported scans not yet reconciled") has nothing to do here
	// by design: Store.Reconcile commits reconciliation in the same transaction that sets
	// state='reported', so no third state exists. A failed reconcile rolls back to 'claimed',
	// which step 2 above returns to 'pending' after the ClaimTTL window.

	// Log hygiene, not scheduling: bound the completed-scan log.
	if n, err := j.store.PruneTerminal(ctx); err != nil {
		j.log.Warn("library discovery: prune failed", "err", err)
	} else if n > 0 {
		j.log.Info("library discovery: pruned old scan rows", "count", n)
		res.Pruned = n
	}

	return res, ""
}

// noteInert logs why discovery did nothing, once per distinct reason.
func (j *Janitor) noteInert(reason string) {
	if j.inertLogged == reason {
		return
	}
	j.inertLogged = reason
	j.log.Info("library discovery: inert", "reason", reason)
}
