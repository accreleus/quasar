package session

// #553: a launch of a discovered game and the scan report that prunes that game
// take the game's entitlement and the host row in opposite orders. The launch
// holds the entitlement and asks for the host at its recheck; the report holds
// the host through a new observation's foreign key and asks for the entitlement.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/library"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestLaunchRacingAPruneOfItsGameIsNeverAnInternalError closes the cycle both
// ways. Postgres aborts whichever side blocked first, unless the machine stalls
// past deadlock_timeout between the two, so each outcome is checked for what it
// must leave behind and neither is required.
func TestLaunchRacingAPruneOfItsGameIsNeverAnInternalError(t *testing.T) {
	for _, first := range []string{"launch", "report"} {
		t.Run(first+" blocks first", func(t *testing.T) {
			pool := testDB(t)
			ctx, cancel := context.WithTimeout(context.Background(), 4*lockOrderWait)
			defer cancel()
			s := seed(t, pool, 8)
			parent := seedSteamApp(t, pool, `{"gpu":true}`)
			provisionHome(t, pool, s.userID, parent, s.hostID)
			lib := library.NewStore(pool)
			claimedScan := func() string {
				var id string
				must(t, pool.QueryRow(ctx, `INSERT INTO library_scans (user_id, app_id, host_id, state, claimed_at)
					VALUES ($1::uuid, $2::uuid, $3::uuid, 'claimed', now()) RETURNING id::text`,
					s.userID, parent, s.hostID).Scan(&id))
				return id
			}
			gone := library.ReportEntry{ExternalID: "517710", Name: "Redout"}
			kept := library.ReportEntry{ExternalID: "620", Name: "Portal 2"}
			found := library.ReportEntry{ExternalID: "3179810", Name: "Tiny Dangerous Dungeons Remake"}

			// One scan sees the game and the next misses it; the miss is aged past
			// the interval, so the report under test prunes it.
			_, err := lib.Reconcile(ctx, claimedScan(), s.hostID, []library.ReportEntry{gone, kept}, nil, time.Hour)
			must(t, err)
			_, err = lib.Reconcile(ctx, claimedScan(), s.hostID, []library.ReportEntry{kept}, nil, time.Hour)
			must(t, err)
			_, err = pool.Exec(ctx, `UPDATE library_observations SET missing_since = missing_since - interval '2 hours'
				WHERE external_id = $1`, gone.ExternalID)
			must(t, err)
			var tile string
			must(t, pool.QueryRow(ctx, `SELECT id::text FROM apps WHERE parent_app_id = $1::uuid AND external_id = $2`,
				parent, gone.ExternalID).Scan(&tile))

			// The launch parks holding the game's entitlement, before its recheck;
			// the report parks holding the host row, before its entitlement delete.
			launchPool, launchGate := gatedPool(t, recheckHead)
			reportPool, reportGate := gatedPool(t, "DELETE FROM entitlements e")
			p := launchParams(s)
			p.AppID, p.HomeAppID, p.ManagedHome = tile, parent, true
			launched, reported := make(chan error, 1), make(chan error, 1)
			launchGate.armed.Store(true)
			go func() {
				_, err := NewStore(launchPool).ScheduleAndCreate(ctx, p)
				launched <- err
			}()
			launchGate.awaitReached(t)
			scan := claimedScan()
			reportGate.armed.Store(true)
			go func() {
				_, err := library.NewStore(reportPool).Reconcile(ctx, scan, s.hostID, []library.ReportEntry{kept, found}, nil, time.Hour)
				reported <- err
			}()
			reportGate.awaitReached(t)

			if first == "launch" {
				launchGate.open()
				if rel := tupleWaitedOn(t, pool, recheckHead); rel != "hosts" {
					t.Fatalf("the recheck waits on a %s row, want the host the report holds", rel)
				}
				reportGate.open()
			} else {
				reportGate.open()
				if rel := tupleWaitedOn(t, pool, "DELETE FROM entitlements e"); rel != "entitlements" {
					t.Fatalf("the report waits on a %s row, want the entitlement the launch holds", rel)
				}
				launchGate.open()
			}
			launchErr, reportErr := <-launched, <-reported

			var state string
			var foundSeen, entitled bool
			must(t, pool.QueryRow(ctx, `SELECT sc.state,
				EXISTS (SELECT 1 FROM library_observations WHERE external_id = $2),
				EXISTS (SELECT 1 FROM entitlements WHERE app_id = $3::uuid)
				FROM library_scans sc WHERE sc.id = $1::uuid`, scan, found.ExternalID, tile).
				Scan(&state, &foundSeen, &entitled))
			var pgErr *pgconn.PgError
			switch {
			case reportErr == nil:
				// The prune committed and the game is gone: a refusal, not a 500.
				t.Logf("%s blocked first: the launch was the victim", first)
				if !errors.Is(launchErr, ErrNotEntitled) {
					t.Fatalf("launch of a game pruned under it: %v, want ErrNotEntitled", launchErr)
				}
				if state != "reported" || !foundSeen || entitled {
					t.Fatalf("report applied: scan %s, new game observed = %v, pruned game entitled = %v", state, foundSeen, entitled)
				}
			case errors.As(reportErr, &pgErr) && pgErr.Code == "40P01":
				// The report lost whole: the game stands, and so does the launch.
				t.Logf("%s blocked first: the report was the victim", first)
				if launchErr != nil {
					t.Fatalf("launch after the report rolled back: %v", launchErr)
				}
				if state != "claimed" || foundSeen || !entitled {
					t.Fatalf("report rolled back: scan %s, new game observed = %v, game entitled = %v", state, foundSeen, entitled)
				}
			default:
				t.Fatalf("report: %v", reportErr)
			}
		})
	}
}
