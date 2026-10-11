package session

// #553: a launch of a discovered game and the scan report that prunes that game
// take the game's entitlement and the host row in opposite orders. The launch
// holds the entitlement from its entitlement check and asks for the host at its
// recheck; the report holds the host through a new observation's foreign key
// and asks for the entitlement in its stale-entitlement delete.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/library"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres checks for a deadlock once per lock wait, deadlock_timeout after the
// wait begins, and aborts the backend that finds one. So the last backend to
// block is the victim, whatever the machine's load, when it alone checks soon.
const (
	deadlockSoon  = "50ms"
	deadlockNever = "10min"
)

const (
	// How candidateQuery starts: the launch's last statement before its GPU lock.
	candidateHead = "SELECT g.id::text, g.host_id::text, g.index"
	// How the report's stale-entitlement delete starts.
	staleEntitlementDelete = "DELETE FROM entitlements e"
)

// pruneRace is a user whose discovered game the next scan report prunes while
// it finds a new one.
type pruneRace struct {
	pool         *pgxpool.Pool
	s            seedIDs
	parent, tile string // the provider app and the pruned game's tile
	scan         string // the claimed scan the report answers
	gone, found  library.ReportEntry
	report       []library.ReportEntry
}

func newPruneRace(t *testing.T, pool *pgxpool.Pool) pruneRace {
	t.Helper()
	ctx := context.Background()
	r := pruneRace{
		pool:  pool,
		s:     seed(t, pool, 8),
		gone:  library.ReportEntry{ExternalID: "517710", Name: "Redout"},
		found: library.ReportEntry{ExternalID: "3179810", Name: "Tiny Dangerous Dungeons Remake"},
	}
	kept := library.ReportEntry{ExternalID: "620", Name: "Portal 2"}
	r.report = []library.ReportEntry{kept, r.found}
	r.parent = seedSteamApp(t, pool, `{"gpu":true}`)
	provisionHome(t, pool, r.s.userID, r.parent, r.s.hostID)
	claimedScan := func() string {
		var id string
		must(t, pool.QueryRow(ctx, `INSERT INTO library_scans (user_id, app_id, host_id, state, claimed_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, 'claimed', now()) RETURNING id::text`,
			r.s.userID, r.parent, r.s.hostID).Scan(&id))
		return id
	}

	// One scan sees the game and the next misses it; the miss is aged past the
	// interval, so the report under test prunes it.
	lib := library.NewStore(pool)
	_, err := lib.Reconcile(ctx, claimedScan(), r.s.hostID, []library.ReportEntry{r.gone, kept}, nil, time.Hour)
	must(t, err)
	_, err = lib.Reconcile(ctx, claimedScan(), r.s.hostID, []library.ReportEntry{kept}, nil, time.Hour)
	must(t, err)
	_, err = pool.Exec(ctx, `UPDATE library_observations SET missing_since = missing_since - interval '2 hours'
		WHERE external_id = $1`, r.gone.ExternalID)
	must(t, err)
	must(t, pool.QueryRow(ctx, `SELECT id::text FROM apps WHERE parent_app_id = $1::uuid AND external_id = $2`,
		r.parent, r.gone.ExternalID).Scan(&r.tile))
	r.scan = claimedScan()
	return r
}

// park starts the launch of the pruned game and the report, and returns once
// each sits at its gate: the launch holding the game's entitlement, the report
// holding the host row.
func (r pruneRace) park(t *testing.T, ctx context.Context, launchPool *pgxpool.Pool, launchGate *stmtGate,
	reportPool *pgxpool.Pool, reportGate *stmtGate) (launched, reported chan error) {
	t.Helper()
	launched, reported = make(chan error, 1), make(chan error, 1)
	p := launchParams(r.s)
	p.AppID, p.HomeAppID, p.ManagedHome = r.tile, r.parent, true
	launchGate.armed.Store(true)
	go func() {
		_, err := NewStore(launchPool).ScheduleAndCreate(ctx, p)
		launched <- err
	}()
	launchGate.awaitReached(t)
	reportGate.armed.Store(true)
	go func() {
		_, err := library.NewStore(reportPool).Reconcile(ctx, r.scan, r.s.hostID, r.report, nil, time.Hour)
		reported <- err
	}()
	reportGate.awaitReached(t)
	return launched, reported
}

// pruneOutcome is what a race left behind.
type pruneOutcome struct {
	scan                              string
	goneSeen, goneEntitled            bool
	foundSeen, foundTile, foundGrants bool
	sessions                          int // the launching user's
}

func (r pruneRace) outcome(t *testing.T) pruneOutcome {
	t.Helper()
	var o pruneOutcome
	must(t, r.pool.QueryRow(context.Background(), `
		SELECT (SELECT state FROM library_scans WHERE id = $1::uuid),
		       EXISTS (SELECT 1 FROM library_observations WHERE parent_app_id = $2::uuid AND external_id = $3),
		       EXISTS (SELECT 1 FROM entitlements WHERE app_id = $5::uuid),
		       EXISTS (SELECT 1 FROM library_observations WHERE parent_app_id = $2::uuid AND external_id = $4),
		       EXISTS (SELECT 1 FROM apps WHERE parent_app_id = $2::uuid AND external_id = $4),
		       EXISTS (SELECT 1 FROM entitlements e JOIN apps a ON a.id = e.app_id
		                WHERE a.parent_app_id = $2::uuid AND a.external_id = $4),
		       (SELECT count(*) FROM sessions WHERE user_id = $6::uuid)`,
		r.scan, r.parent, r.gone.ExternalID, r.found.ExternalID, r.tile, r.s.userID).
		Scan(&o.scan, &o.goneSeen, &o.goneEntitled, &o.foundSeen, &o.foundTile, &o.foundGrants, &o.sessions))
	return o
}

// requireApplied: the report committed, so the game is gone and its launch was
// refused, leaving no session.
func (r pruneRace) requireApplied(t *testing.T, launchErr error) {
	t.Helper()
	if !errors.Is(launchErr, ErrNotEntitled) {
		t.Fatalf("launch of a game pruned under it: %v, want ErrNotEntitled", launchErr)
	}
	want := pruneOutcome{scan: "reported", foundSeen: true, foundTile: true, foundGrants: true}
	if got := r.outcome(t); got != want {
		t.Fatalf("report applied: left %+v, want %+v", got, want)
	}
}

// requireRolledBack: the report lost whole, so the game stands and its launch
// was placed.
func (r pruneRace) requireRolledBack(t *testing.T, launchErr error) {
	t.Helper()
	if launchErr != nil {
		t.Fatalf("launch after the report rolled back: %v", launchErr)
	}
	want := pruneOutcome{scan: "claimed", goneSeen: true, goneEntitled: true, sessions: 1}
	if got := r.outcome(t); got != want {
		t.Fatalf("report rolled back: left %+v, want %+v", got, want)
	}
}

// TestLaunchRacingAPruneOfItsGameIsNeverAnInternalError closes the two-way
// cycle in both orders at the default deadlock_timeout. The side that blocked
// first is the victim unless the machine stalls past that timeout between the
// two waits, so either outcome is accepted and checked for what it must leave.
// It cannot tell a missing retry from such a stall; the two tests below can.
func TestLaunchRacingAPruneOfItsGameIsNeverAnInternalError(t *testing.T) {
	for _, first := range []string{"launch", "report"} {
		t.Run(first+" blocks first", func(t *testing.T) {
			pool := testDB(t)
			ctx, cancel := context.WithTimeout(context.Background(), 4*lockOrderWait)
			defer cancel()
			r := newPruneRace(t, pool)
			launchPool, launchGate := gatedPool(t, recheckHead)
			reportPool, reportGate := gatedPool(t, staleEntitlementDelete)
			launched, reported := r.park(t, ctx, launchPool, launchGate, reportPool, reportGate)

			if first == "launch" {
				launchGate.open()
				if rel := tupleWaitedOn(t, pool, recheckHead); rel != "hosts" {
					t.Fatalf("the recheck waits on a %s row, want the host the report holds", rel)
				}
				reportGate.open()
			} else {
				reportGate.open()
				if rel := tupleWaitedOn(t, pool, staleEntitlementDelete); rel != "entitlements" {
					t.Fatalf("the report waits on a %s row, want the entitlement the launch holds", rel)
				}
				launchGate.open()
			}
			launchErr, reportErr := <-launched, <-reported

			var pgErr *pgconn.PgError
			switch {
			case reportErr == nil:
				t.Logf("%s blocked first: the launch was the victim", first)
				r.requireApplied(t, launchErr)
			case errors.As(reportErr, &pgErr) && pgErr.Code == "40P01":
				t.Logf("%s blocked first: the report was the victim", first)
				r.requireRolledBack(t, launchErr)
			default:
				t.Fatalf("report: %v", reportErr)
			}
		})
	}
}

// TestLaunchLosingADeadlockAtItsRecheckIsRetried makes the launch the victim of
// the two-way cycle: the report blocks first and never checks, the launch
// blocks last and checks at once.
func TestLaunchLosingADeadlockAtItsRecheckIsRetried(t *testing.T) {
	pool := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*lockOrderWait)
	defer cancel()
	r := newPruneRace(t, pool)
	launchPool, launchGate := gatedPool(t, recheckHead, deadlockSoon)
	reportPool, reportGate := gatedPool(t, staleEntitlementDelete, deadlockNever)
	launched, reported := r.park(t, ctx, launchPool, launchGate, reportPool, reportGate)

	reportGate.open()
	if rel := tupleWaitedOn(t, pool, staleEntitlementDelete); rel != "entitlements" {
		t.Fatalf("the report waits on a %s row, want the entitlement the launch holds", rel)
	}
	launchGate.open()
	launchErr, reportErr := <-launched, <-reported
	if reportErr != nil {
		t.Fatalf("report: %v", reportErr)
	}
	r.requireApplied(t, launchErr)
}

// TestLaunchLosingADeadlockAtItsGPULockIsRetried is the three-way cycle, which
// catches the launch before its recheck: it waits for the GPU lock, a second
// user's launch holds that lock and waits for the host row, and the report
// holds the host row and waits for the first launch's entitlement. The first
// launch blocks last and is the only one that checks.
func TestLaunchLosingADeadlockAtItsGPULockIsRetried(t *testing.T) {
	pool := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*lockOrderWait)
	defer cancel()
	r := newPruneRace(t, pool)
	other := seedUser(t, pool, "other553@test.local", "other553")
	launchPool, launchGate := gatedPool(t, candidateHead, deadlockSoon)
	otherPool, _ := gatedPool(t, "", deadlockNever) // its gate is never armed
	reportPool, reportGate := gatedPool(t, staleEntitlementDelete, deadlockNever)
	launched, reported := r.park(t, ctx, launchPool, launchGate, reportPool, reportGate)

	otherLaunched := make(chan error, 1)
	go func() {
		_, err := NewStore(otherPool).ScheduleAndCreate(ctx, launchParams(seedIDs{userID: other, appID: r.s.appID}))
		otherLaunched <- err
	}()
	if rel := tupleWaitedOn(t, pool, recheckHead); rel != "hosts" {
		t.Fatalf("the second launch's recheck waits on a %s row, want the host the report holds", rel)
	}
	reportGate.open()
	if rel := tupleWaitedOn(t, pool, staleEntitlementDelete); rel != "entitlements" {
		t.Fatalf("the report waits on a %s row, want the entitlement the first launch holds", rel)
	}
	launchGate.open()
	launchErr, otherErr, reportErr := <-launched, <-otherLaunched, <-reported
	if reportErr != nil {
		t.Fatalf("report: %v", reportErr)
	}
	if otherErr != nil {
		t.Fatalf("the second user's launch: %v", otherErr)
	}
	r.requireApplied(t, launchErr)
}
