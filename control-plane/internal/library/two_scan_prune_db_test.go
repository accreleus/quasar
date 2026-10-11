package library

// two_scan_prune_db_test.go — schema.md amendment 25 (#521): an observation a successful
// scan did not list is marked, and pruned only by a scan queued at least one scan interval
// after the mark.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

// testInterval is the scan interval these tests hand the reconciler.
const testInterval = 6 * time.Hour

var (
	redoutEntry = ReportEntry{ExternalID: "517710", Name: "Redout: Enhanced Edition"}
	tinyEntry   = ReportEntry{ExternalID: "3179810", Name: "Tiny Dangerous Dungeons Remake"}
	portalEntry = ReportEntry{ExternalID: "620", Name: "Portal 2"}
)

// timePasses ages every missing mark and scan row by d: the reconciler compares database
// timestamps, and the database clock cannot be moved.
func (f fixture) timePasses(t *testing.T, d time.Duration) {
	t.Helper()
	ctx := context.Background()
	must(t, execT(ctx, f.pool, `UPDATE library_observations
		SET missing_since = missing_since - make_interval(secs => $1)`, d.Seconds()))
	must(t, execT(ctx, f.pool, `UPDATE library_scans
		SET created_at  = created_at  - make_interval(secs => $1),
		    claimed_at  = claimed_at  - make_interval(secs => $1),
		    reported_at = reported_at - make_interval(secs => $1)`, d.Seconds()))
}

// missingSince reads the mark on the user's observation of appID on host. held is false
// once the row is pruned; a nil mark is an observation the last scan listed.
func (f fixture) missingSince(t *testing.T, host, appID string) (mark *time.Time, held bool) {
	t.Helper()
	err := f.pool.QueryRow(context.Background(), `
		SELECT missing_since FROM library_observations
		 WHERE user_id = $1::uuid AND host_id = $2::uuid AND external_id = $3`,
		f.user, host, appID).Scan(&mark)
	return mark, err == nil
}

// scanVia queues one scan through the real queue, claims it as the fixture host and
// reconciles entries.
func (f fixture) scanVia(t *testing.T, enqueue func() int, entries ...ReportEntry) ReconcileResult {
	t.Helper()
	ctx := context.Background()
	if n := enqueue(); n != 1 {
		t.Fatalf("queued %d scans, want 1", n)
	}
	claimed, err := f.store.ClaimPending(ctx, f.host)
	must(t, err)
	if len(claimed) != 1 {
		t.Fatalf("claimed %d scans, want 1", len(claimed))
	}
	res, err := f.store.Reconcile(ctx, claimed[0].ScanID, f.host, entries, nil, testInterval)
	must(t, err)
	return res
}

func (f fixture) scheduled(t *testing.T) func() int {
	return func() int {
		t.Helper()
		n, err := f.store.Enqueue(context.Background(), testInterval)
		must(t, err)
		return n
	}
}

func (f fixture) scanNow(t *testing.T) func() int {
	return func() int {
		t.Helper()
		res, err := f.store.ForceEnqueue(context.Background(), nil, nil)
		must(t, err)
		return res.Queued
	}
}

// TestOneMissDoesNotPrune is the #521 reproduction: a successful, non-empty, uncapped
// report that omits a known game must not prune it or revoke its entitlement.
func TestOneMissDoesNotPrune(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	_, err := f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host, []ReportEntry{redoutEntry, portalEntry}, nil, testInterval)
	must(t, err)
	tileID, _, _ := f.tile(t, "517710")

	res, err := f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host, []ReportEntry{portalEntry}, nil, testInterval)
	must(t, err)
	if res.Revoked != 0 || res.Missing != 1 || res.Pruned != 0 {
		t.Errorf("after one miss: Revoked = %d Missing = %d Pruned = %d, want 0, 1, 0",
			res.Revoked, res.Missing, res.Pruned)
	}
	if mark, held := f.missingSince(t, f.host, "517710"); !held || mark == nil {
		t.Errorf("the missed game: held = %v mark = %v, want the row kept and marked", held, mark)
	}
	if mark, _ := f.missingSince(t, f.host, "620"); mark != nil {
		t.Errorf("the listed game is marked missing (%v)", mark)
	}
	if _, has := f.entitlementGrantedBy(t, f.user, tileID); !has {
		t.Error("one scan that omitted an installed game revoked its entitlement")
	}
	if _, enabled, ok := f.tile(t, "517710"); !ok || !enabled {
		t.Error("one miss disabled or removed the tile")
	}
	// The admin "Seen, not published" read still lists a marked observation.
	must(t, execT(ctx, pool, `UPDATE apps SET enabled = false WHERE id = $1::uuid`, tileID))
	unpublished, err := f.store.Unpublished(ctx, f.parent)
	must(t, err)
	if len(unpublished) != 1 || unpublished[0].ExternalID != "517710" {
		t.Errorf("unpublished = %+v, want the marked observation listed", unpublished)
	}
}

// TestNextScheduledScanPrunesAnUninstall: the janitor's own next scan, one interval on, is
// far enough from the mark to confirm it. It pins Enqueue's recency predicate and the
// prune's distance predicate to the same arithmetic.
func TestNextScheduledScanPrunesAnUninstall(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)

	f.scanVia(t, f.scheduled(t), redoutEntry, portalEntry)
	tileID, _, _ := f.tile(t, "517710")
	f.timePasses(t, testInterval)
	f.scanVia(t, f.scheduled(t), portalEntry)
	if _, held := f.missingSince(t, f.host, "517710"); !held {
		t.Fatal("the first scan to miss the game pruned it")
	}

	f.timePasses(t, testInterval)
	res := f.scanVia(t, f.scheduled(t), portalEntry)
	if res.Pruned != 1 || res.Revoked != 1 {
		t.Errorf("confirming scan: Pruned = %d Revoked = %d, want 1 and 1", res.Pruned, res.Revoked)
	}
	if _, held := f.missingSince(t, f.host, "517710"); held {
		t.Error("the uninstalled game is still observed after the confirming scan")
	}
	if _, has := f.entitlementGrantedBy(t, f.user, tileID); has {
		t.Error("the uninstalled game kept its provider entitlement after the confirming scan")
	}
	if _, _, ok := f.tile(t, "517710"); !ok {
		t.Error("the app row was deleted; only the entitlement is revoked")
	}
}

// TestScanNowTwiceDoesNotPrune: a mount that is away for a moment is missed by every scan
// taken in that moment, so a scan queued less than one interval after the mark neither
// confirms it nor moves it.
func TestScanNowTwiceDoesNotPrune(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)

	f.scanVia(t, f.scheduled(t), redoutEntry, portalEntry)
	f.scanVia(t, f.scanNow(t), portalEntry)
	first, _ := f.missingSince(t, f.host, "517710")
	if first == nil {
		t.Fatal("the missed game was not marked")
	}
	for range 2 {
		if res := f.scanVia(t, f.scanNow(t), portalEntry); res.Pruned != 0 || res.Missing != 0 {
			t.Errorf("scan now right after the mark: Pruned = %d Missing = %d, want 0 and 0", res.Pruned, res.Missing)
		}
	}
	if mark, held := f.missingSince(t, f.host, "517710"); !held || mark == nil || !mark.Equal(*first) {
		t.Fatalf("after two more misses: held = %v mark = %v, want the first mark %v kept", held, mark, first)
	}
	// The scheduler does not queue one either: the last successful scan is too recent.
	if n := f.scheduled(t)(); n != 0 {
		t.Errorf("the scheduler queued %d scans inside the interval, want 0", n)
	}

	f.timePasses(t, testInterval-time.Minute)
	f.scanVia(t, f.scanNow(t), portalEntry)
	if _, held := f.missingSince(t, f.host, "517710"); !held {
		t.Fatal("a scan queued a minute short of the interval pruned the game")
	}

	f.timePasses(t, 2*time.Minute)
	if res := f.scanVia(t, f.scanNow(t), portalEntry); res.Pruned != 1 {
		t.Errorf("a scan queued past the interval: Pruned = %d, want 1", res.Pruned)
	}
}

// TestSightingClearsTheMissingMark: a scan that lists the game again clears the mark, so a
// later miss starts over.
func TestSightingClearsTheMissingMark(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)

	f.scanVia(t, f.scanNow(t), redoutEntry, portalEntry)
	f.scanVia(t, f.scanNow(t), portalEntry)
	f.timePasses(t, testInterval)
	f.scanVia(t, f.scanNow(t), redoutEntry, portalEntry)
	if mark, held := f.missingSince(t, f.host, "517710"); !held || mark != nil {
		t.Fatalf("after the game was seen again: held = %v mark = %v, want kept and unmarked", held, mark)
	}

	if res := f.scanVia(t, f.scanNow(t), portalEntry); res.Pruned != 0 || res.Missing != 1 {
		t.Errorf("first miss after a sighting: Pruned = %d Missing = %d, want 0 and 1", res.Pruned, res.Missing)
	}
	if _, held := f.missingSince(t, f.host, "517710"); !held {
		t.Error("a miss after a sighting was confirmed by the mark the sighting cleared")
	}
}

// TestFailedCappedAndEmptyReportsLeaveTheMarkAlone: a scan that cannot speak for a game
// neither confirms its mark nor clears it. A capped report still clears the mark of a game
// it lists, because a sighting is a sighting.
func TestFailedCappedAndEmptyReportsLeaveTheMarkAlone(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	f.scanVia(t, f.scanNow(t), redoutEntry, tinyEntry, portalEntry)
	redout, _, _ := f.tile(t, "517710")
	f.scanVia(t, f.scanNow(t), portalEntry)
	f.timePasses(t, testInterval)
	marked, _ := f.missingSince(t, f.host, "517710")
	if marked == nil {
		t.Fatal("the missed game was not marked")
	}
	stillMarked := func(after string) {
		t.Helper()
		mark, held := f.missingSince(t, f.host, "517710")
		if !held || mark == nil || !mark.Equal(*marked) {
			t.Fatalf("after %s: held = %v mark = %v, want the row kept with its mark %v", after, held, mark, marked)
		}
		if _, has := f.entitlementGrantedBy(t, f.user, redout); !has {
			t.Fatalf("after %s: the marked game lost its entitlement", after)
		}
	}

	// An unresolved or switched-off interval marks and never prunes.
	_, err := f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host, []ReportEntry{portalEntry}, nil, 0)
	must(t, err)
	stillMarked("a scan reconciled with no interval")

	must(t, f.store.MarkFailed(ctx, f.claimedScan(t, f.user), f.host, "home not mounted"))
	stillMarked("a failed scan")

	capped := []ReportEntry{portalEntry, tinyEntry}
	for i := 0; len(capped) < scanMaxEntries; i++ {
		capped = append(capped, ReportEntry{ExternalID: strconv.Itoa(1000 + i), Name: "Filler " + strconv.Itoa(i)})
	}
	res, err := f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host, capped, nil, testInterval)
	must(t, err)
	if !res.Capped || res.Pruned != 0 || res.Missing != 0 {
		t.Errorf("capped report: Capped = %v Pruned = %d Missing = %d, want true, 0, 0", res.Capped, res.Pruned, res.Missing)
	}
	stillMarked("a report at the entry cap")
	if mark, held := f.missingSince(t, f.host, "3179810"); !held || mark != nil {
		t.Errorf("a capped report that listed a marked game: held = %v mark = %v, want kept and unmarked", held, mark)
	}

	_, err = f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host, nil, nil, testInterval)
	must(t, err)
	stillMarked("an empty report")

	// The mark survived all of them: the next complete scan confirms it.
	res, err = f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host, []ReportEntry{portalEntry, tinyEntry}, nil, testInterval)
	must(t, err)
	if _, held := f.missingSince(t, f.host, "517710"); held || res.Revoked != 1 {
		t.Errorf("the confirming scan: held = %v Revoked = %d, want pruned and 1", held, res.Revoked)
	}
}

// TestConcurrentReportsCountOneMiss: a report delivered several times at once, on two hosts
// at once, is one miss per host. Each host's mark needs its own confirming scan, and the
// entitlement holds while any host's row remains, marked or not.
func TestConcurrentReportsCountOneMiss(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	var hostB string
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, node_secret_hash)
		VALUES ('node-lib-b','online','deadbeef') RETURNING id::text`).Scan(&hostB))
	scanOn := func(host string) string {
		var id string
		must(t, pool.QueryRow(ctx, `INSERT INTO library_scans (user_id, app_id, host_id, state, claimed_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, 'claimed', now()) RETURNING id::text`,
			f.user, f.parent, host).Scan(&id))
		return id
	}
	hosts := []string{f.host, hostB}
	for _, h := range hosts {
		_, err := f.store.Reconcile(ctx, scanOn(h), h, []ReportEntry{redoutEntry, portalEntry}, nil, testInterval)
		must(t, err)
	}
	tileID, _, _ := f.tile(t, "517710")

	scans := map[string]string{f.host: scanOn(f.host), hostB: scanOn(hostB)}
	// A triple has one open scan at a time: a "scan now" racing it queues nothing beside it.
	if n := f.scanNow(t)(); n != 0 {
		t.Errorf("scan now queued %d scans beside an open one, want 0", n)
	}

	const deliveries = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := map[string]int{}
	for _, h := range hosts {
		for range deliveries {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := f.store.Reconcile(ctx, scans[h], h, []ReportEntry{portalEntry}, nil, testInterval)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					accepted[h]++
				case !errors.Is(err, ErrScanNotOpen):
					t.Errorf("concurrent report on host %s: %v", h, err)
				}
			}()
		}
	}
	wg.Wait()
	for _, h := range hosts {
		if accepted[h] != 1 {
			t.Errorf("host %s reconciled %d of %d deliveries, want 1", h, accepted[h], deliveries)
		}
		if mark, held := f.missingSince(t, h, "517710"); !held || mark == nil {
			t.Errorf("host %s after one miss: held = %v mark = %v, want kept and marked", h, held, mark)
		}
	}
	if _, has := f.entitlementGrantedBy(t, f.user, tileID); !has {
		t.Fatal("one miss on each host revoked the entitlement")
	}

	// Host A's confirming scan speaks for host A only.
	f.timePasses(t, testInterval)
	_, err := f.store.Reconcile(ctx, scanOn(f.host), f.host, []ReportEntry{portalEntry}, nil, testInterval)
	must(t, err)
	if _, held := f.missingSince(t, f.host, "517710"); held {
		t.Error("host A's confirming scan did not prune host A's row")
	}
	if mark, held := f.missingSince(t, hostB, "517710"); !held || mark == nil {
		t.Errorf("host A's scan touched host B's row: held = %v mark = %v", held, mark)
	}
	if _, has := f.entitlementGrantedBy(t, f.user, tileID); !has {
		t.Error("the entitlement was revoked while host B's marked row remained")
	}

	res, err := f.store.Reconcile(ctx, scanOn(hostB), hostB, []ReportEntry{portalEntry}, nil, testInterval)
	must(t, err)
	if _, has := f.entitlementGrantedBy(t, f.user, tileID); has || res.Revoked != 1 {
		t.Errorf("after both hosts confirmed: Revoked = %d, want the entitlement gone", res.Revoked)
	}
}

// TestScanReportMeasuresTheDistanceByTheResolvedInterval: the route hands the reconciler
// the interval the scheduler runs on, not a constant.
func TestScanReportMeasuresTheDistanceByTheResolvedInterval(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	const minutes = 15
	srv := newTestServer(t, f, &fakeSettings{enabled: true, intervalMinutes: minutes}, NewAppDetails(false, quietLogger()))
	report := func(entries ...ReportEntry) {
		t.Helper()
		resp := agentReq(t, "POST", srv.URL+"/v1/agent/library/scan-report", f.nodeName, f.secret,
			ScanReport{ScanID: f.claimedScan(t, f.user), OK: true, Entries: entries})
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("scan-report = %d, want 200", resp.StatusCode)
		}
	}

	report(redoutEntry, portalEntry)
	report(portalEntry)
	f.timePasses(t, (minutes-1)*time.Minute)
	report(portalEntry)
	if _, held := f.missingSince(t, f.host, "517710"); !held {
		t.Fatal("a report one minute short of the configured interval pruned the game")
	}
	f.timePasses(t, 2*time.Minute)
	report(portalEntry)
	if _, held := f.missingSince(t, f.host, "517710"); held {
		t.Error("a report past the configured interval did not prune the game")
	}
}
