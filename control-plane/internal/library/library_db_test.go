package library

// library_db_test.go — the Phase 4 integration gates (spec §13 "Phase 4").
//
// TEST_DATABASE_URL-gated, like every other DB test in this repo: without it
// these SKIP, which means a green `go-check` does not mean they ran. Use
// `scripts/dev/dev.sh go-test-db`.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/internal/storage"
	"github.com/accreleus/quasar/control-plane/migrations"
)

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	if err := migrate.Run(migrations.FS, dbURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM library_scans; DELETE FROM library_observations;
		DELETE FROM library_appid_rules;
		DELETE FROM session_metrics; DELETE FROM user_homes;
		DELETE FROM sessions; DELETE FROM gpus; DELETE FROM hosts;
		DELETE FROM entitlements; DELETE FROM apps;
		DELETE FROM auth_tokens; DELETE FROM users;
		UPDATE instance_settings SET library_discovery_enabled = false, storage_provider = 'auto';
	`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- fixture -----------------------------------------------------------------

type fixture struct {
	pool     *pgxpool.Pool
	store    *Store
	user     string
	other    string
	parent   string // the Steam provider app
	host     string
	nodeName string
	secret   string
}

func newFixture(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()
	f := fixture{pool: pool, store: NewStore(pool), nodeName: "node-lib", secret: "s3cr3t"}

	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash, role)
		VALUES ('lib-a@t.local','lib-a','x','user') RETURNING id::text`).Scan(&f.user))
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash, role)
		VALUES ('lib-b@t.local','lib-b','x','user') RETURNING id::text`).Scan(&f.other))

	// The provider app. library_provider='steam' is the FUNCTIONAL trigger;
	// kind is deliberately left at its default 'game' so these tests also
	// demonstrate §4.5.3 — no server path may branch on kind, so discovery must
	// work identically whether or not an operator set Kind=Launcher.
	//
	// THE LAUNCH DEFAULTS HERE ARE DELIBERATELY NON-EMPTY AND NON-DEFAULT, and
	// that is a fixture decision with a scar behind it. This fixture used to leave
	// default_profile_id NULL and profile_policy at 'inherit', and BOTH of those
	// made the fixture unable to see a real bug:
	//
	//   - default_profile_id is TEXT holding a launch-profile SLUG. The reconciler
	//     bound it as $5::uuid. NULL casts to uuid without complaint, so with a
	//     NULL fixture every test passed; the first operator to set a real profile
	//     turned every reconcile of that home into a 22P02.
	//   - profile_policy's COLUMN DEFAULT is 'inherit'. A fixture that also says
	//     'inherit' cannot distinguish "the tile copied the parent's policy" from
	//     "the INSERT dropped the value and the column default filled it in" —
	//     the assertion in TestReconcileObservedSet was passing either way.
	//
	// So the realistic shape is now the DEFAULT for every test in this package
	// rather than a special case one test opts into. '1440p120' is a real slug
	// shape and 'prefer' is a real non-default policy.
	must(t, pool.QueryRow(ctx, `INSERT INTO apps (name, library_provider, managed_home, default_profile_id, profile_policy)
		VALUES ('Steam', 'steam', true, '1440p120', 'prefer') RETURNING id::text`).Scan(&f.parent))

	h := sha256.Sum256([]byte(f.secret))
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, node_secret_hash)
		VALUES ($1, 'online', $2) RETURNING id::text`,
		f.nodeName, hex.EncodeToString(h[:])).Scan(&f.host))

	must(t, execT(ctx, pool, `INSERT INTO user_homes (user_id, app_id, host_id, provider, ref)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'local', '/homes/opaque-a')`, f.user, f.parent, f.host))
	return f
}

func execT(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) error {
	_, err := pool.Exec(ctx, sql, args...)
	return err
}

// homeFor gives `user` a local home on the fixture host, so the janitor will
// enqueue a scan for them too.
func (f fixture) homeFor(t *testing.T, user, ref string) {
	t.Helper()
	must(t, execT(context.Background(), f.pool, `INSERT INTO user_homes (user_id, app_id, host_id, provider, ref)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'local', $4)`, user, f.parent, f.host, ref))
}

// claimedScan inserts a scan already in 'claimed', which is what the reconciler
// requires. Bypassing ClaimPending here is deliberate: the claim path has its own
// tests and these are about §7.7.
func (f fixture) claimedScan(t *testing.T, user string) string {
	t.Helper()
	var id string
	must(t, f.pool.QueryRow(context.Background(), `
		INSERT INTO library_scans (user_id, app_id, host_id, state, claimed_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'claimed', now()) RETURNING id::text`,
		user, f.parent, f.host).Scan(&id))
	return id
}

// report turns the Tower manifest fixture (denylist_test.go) into a scan report.
func observedEntries() []ReportEntry {
	out := make([]ReportEntry, 0, len(observedManifests))
	for _, m := range observedManifests {
		out = append(out, ReportEntry{
			ExternalID: m.appID,
			Name:       m.name,
			// Collected and unused (§7.3). Populated here precisely so a future
			// change that starts reading them shows up as a behaviour change in
			// these tests rather than silently working.
			InstallDir: "common/" + m.appID,
			SizeOnDisk: 1234,
			StateFlags: int64(m.stateFlags),
		})
	}
	return out
}

func (f fixture) tile(t *testing.T, appID string) (id string, enabled bool, ok bool) {
	t.Helper()
	return f.tileOf(t, f.parent, appID)
}

// tileOf is tile against an explicitly named provider app, for the tests that
// build their own parent instead of using the fixture's.
func (f fixture) tileOf(t *testing.T, parentID, appID string) (id string, enabled bool, ok bool) {
	t.Helper()
	err := f.pool.QueryRow(context.Background(), `
		SELECT id::text, enabled FROM apps
		 WHERE parent_app_id = $1::uuid AND external_source = 'steam' AND external_id = $2`,
		parentID, appID).Scan(&id, &enabled)
	if err != nil {
		return "", false, false
	}
	return id, enabled, true
}

func (f fixture) entitlementGrantedBy(t *testing.T, user, tileID string) (string, bool) {
	t.Helper()
	var by string
	err := f.pool.QueryRow(context.Background(), `
		SELECT granted_by FROM entitlements
		 WHERE subject_type='user' AND subject_id=$1::uuid AND app_id=$2::uuid`, user, tileID).Scan(&by)
	if err != nil {
		return "", false
	}
	return by, true
}

func countT(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	must(t, pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

// --- §7.7 over the real Tower manifest set -----------------------------------

// TestReconcileObservedSet is Gate 4's headline acceptance: a scan of the live
// Tower home observes 9 appids, auto-publishes the 4 games and suppresses the 5
// Valve tools, and the tile's entitlement lands in the SAME transaction that
// created it.
func TestReconcileObservedSet(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	scan := f.claimedScan(t, f.user)
	res, err := f.store.Reconcile(ctx, scan, f.host, observedEntries(), nil)
	must(t, err)

	if res.Observed != 9 || res.Suppressed != 5 || res.Created != 4 || res.Granted != 4 {
		t.Fatalf("reconcile = %+v; want 9 observed / 5 suppressed / 4 created / 4 granted", res)
	}

	// EVERY observed appid has an observation, including the suppressed five —
	// that is what replaces the first draft's "Filtered" tab (§7.6).
	if n := countT(t, pool, `SELECT count(*) FROM library_observations WHERE user_id=$1::uuid`, f.user); n != 9 {
		t.Errorf("observations = %d, want 9 (suppressed appids must be recorded too)", n)
	}

	for _, m := range observedManifests {
		id, enabled, ok := f.tile(t, m.appID)
		if m.isTool {
			if ok {
				t.Errorf("%s (%s) got a tile; a Valve tool must be suppressed", m.name, m.appID)
			}
			continue
		}
		if !ok {
			t.Fatalf("%s (%s) got no tile; a real game must auto-publish", m.name, m.appID)
		}
		if !enabled {
			t.Errorf("%s tile is disabled; a freshly published tile must be enabled", m.name)
		}
		by, has := f.entitlementGrantedBy(t, f.user, id)
		if !has || by != "provider" {
			t.Errorf("%s: entitlement granted_by=%q present=%v; want a provider grant in the same transaction",
				m.name, by, has)
		}
		// NO 'all' ENTITLEMENT (§7.7). A discovered tile reaches the users who
		// have it installed and nobody else; an 'all' row would defeat the whole
		// entitlement model on the largest catalogue in the instance.
		if n := countT(t, pool, `SELECT count(*) FROM entitlements WHERE app_id=$1::uuid AND subject_type='all'`, id); n != 0 {
			t.Errorf("%s tile has %d 'all' entitlements; want 0", m.name, n)
		}
	}

	// Tile shape (§1.2 / apps_derived_shape_ck): identity only, launch defaults
	// copied from the parent.
	//
	// policy is asserted against the FIXTURE'S value ('prefer'), not against the
	// column default ('inherit'). Those used to be the same string, which made
	// this assertion unable to fail — see the note on newFixture.
	gameID, _, _ := f.tile(t, "517710")
	var kind, origin, policy, provider string
	var profileID *string
	var runtime []byte
	var managedHome bool
	must(t, pool.QueryRow(ctx, `SELECT kind, origin, default_profile_id, profile_policy, library_provider, runtime_spec, managed_home
		FROM apps WHERE id=$1::uuid`, gameID).Scan(&kind, &origin, &profileID, &policy, &provider, &runtime, &managedHome))
	if kind != "game" || origin != "discovered" || policy != "prefer" || provider != "" ||
		string(runtime) != "{}" || managedHome {
		t.Errorf("tile shape = kind %q origin %q policy %q provider %q runtime %s managed_home %v; "+
			"want game/discovered/prefer/\"\"/{}/false", kind, origin, policy, provider, runtime, managedHome)
	}
	if profileID == nil || *profileID != "1440p120" {
		t.Errorf("tile default_profile_id = %v, want the parent's slug %q", profileID, "1440p120")
	}
}

// --- scan-observability + backfill amendment (2026-08-01) -------------------

// TestReconcileStoresOutcomeCountsOnTheScanRow is the migration 0048 gate:
// the counts ReconcileResult returns must be the SAME counts persisted on the
// scan row, in the same transaction that marks it 'reported' — the
// LibraryStatus.recent_scans validation surface this replaces the reconcile
// log line as the only copy of.
func TestReconcileStoresOutcomeCountsOnTheScanRow(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	scan := f.claimedScan(t, f.user)
	res, err := f.store.Reconcile(ctx, scan, f.host, observedEntries(), nil)
	must(t, err)
	if res.Observed != 9 || res.Suppressed != 5 || res.Created != 4 || res.Granted != 4 {
		t.Fatalf("reconcile = %+v; want the same 9/5/4/4 TestReconcileObservedSet asserts", res)
	}

	var observed, suppressed, created, disabled, granted, revoked, rejected, backfilled int
	var state string
	must(t, pool.QueryRow(ctx, `
		SELECT state, observed, suppressed, created, disabled, granted, revoked, rejected, backfilled
		  FROM library_scans WHERE id::text = $1`, scan).
		Scan(&state, &observed, &suppressed, &created, &disabled, &granted, &revoked, &rejected, &backfilled))

	if state != "reported" {
		t.Fatalf("scan state = %q, want reported", state)
	}
	if observed != res.Observed || suppressed != res.Suppressed || created != res.Created ||
		disabled != res.Disabled || granted != res.Granted || revoked != res.Revoked ||
		rejected != res.Rejected || backfilled != res.Backfilled {
		t.Errorf("stored counts = observed=%d suppressed=%d created=%d disabled=%d granted=%d "+
			"revoked=%d rejected=%d backfilled=%d; want them identical to the returned ReconcileResult %+v",
			observed, suppressed, created, disabled, granted, revoked, rejected, backfilled, res)
	}
}

// TestReconcileFailedScanKeepsZeroCounts asserts the OTHER half of "pre-0048
// rows read zero, which is 'not recorded'": a scan MarkFailed terminates
// never ran the reconciler, so its counts stay at the column DEFAULT (0) —
// and unlike a pre-0048 row, that zero is the genuinely correct answer (a
// failed walk really did produce/observe/publish nothing), not a gap in the
// data.
func TestReconcileFailedScanKeepsZeroCounts(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	scan := f.claimedScan(t, f.user)
	must(t, f.store.MarkFailed(ctx, scan, f.host, "walk refused"))

	var state string
	var observed, backfilled int
	must(t, pool.QueryRow(ctx, `SELECT state, observed, backfilled FROM library_scans WHERE id::text = $1`, scan).
		Scan(&state, &observed, &backfilled))
	if state != "failed" {
		t.Fatalf("scan state = %q, want failed", state)
	}
	if observed != 0 || backfilled != 0 {
		t.Errorf("failed scan counts = observed=%d backfilled=%d, want 0/0", observed, backfilled)
	}
}

// seedDiscoveredTile inserts a bare discovered tile under f.parent, the shape
// backfillCandidates looks for (origin='discovered', a Steam external_id).
func (f fixture) seedDiscoveredTile(t *testing.T, appID, description string) string {
	t.Helper()
	var id string
	must(t, f.pool.QueryRow(context.Background(), `
		INSERT INTO apps (name, kind, parent_app_id, external_source, external_id, origin, enabled, description)
		VALUES ($1, 'game', $2::uuid, $3, $4, 'discovered', true, $5)
		RETURNING id::text`,
		"placeholder", f.parent, SourceSteam, appID, description).Scan(&id))
	return id
}

// TestReconcileBackfillsBlankDescription is the headline backfill gate: an
// existing discovered tile with description=” this scan re-observes gets
// filled from the appdetails source, and the fill is counted.
func TestReconcileBackfillsBlankDescription(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	tileID := f.seedDiscoveredTile(t, "517710", "")

	scan := f.claimedScan(t, f.user)
	res, err := f.store.Reconcile(ctx, scan, f.host,
		[]ReportEntry{{ExternalID: "517710", Name: "Redout"}},
		map[string]AppDetail{"517710": {IsGame: true, ShortDescription: "A fast racer."}})
	must(t, err)

	if res.Backfilled != 1 {
		t.Fatalf("res.Backfilled = %d, want 1", res.Backfilled)
	}
	var desc string
	must(t, pool.QueryRow(ctx, `SELECT description FROM apps WHERE id::text = $1`, tileID).Scan(&desc))
	if desc != "A fast racer." {
		t.Errorf("tile description = %q, want the fetched short_description", desc)
	}
}

// TestReconcileBackfillNeverOverwritesANonEmptyDescription is the
// fill-blanks-only guarantee: an operator's hand-written (or previously
// backfilled) description must survive every future scan.
func TestReconcileBackfillNeverOverwritesANonEmptyDescription(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	tileID := f.seedDiscoveredTile(t, "517710", "An operator wrote this.")

	scan := f.claimedScan(t, f.user)
	res, err := f.store.Reconcile(ctx, scan, f.host,
		[]ReportEntry{{ExternalID: "517710", Name: "Redout"}},
		map[string]AppDetail{"517710": {IsGame: true, ShortDescription: "Valve's answer."}})
	must(t, err)

	if res.Backfilled != 0 {
		t.Errorf("res.Backfilled = %d, want 0 (the field was already non-empty)", res.Backfilled)
	}
	var desc string
	must(t, pool.QueryRow(ctx, `SELECT description FROM apps WHERE id::text = $1`, tileID).Scan(&desc))
	if desc != "An operator wrote this." {
		t.Errorf("tile description = %q, want the operator's edit untouched", desc)
	}
}

// TestReconcileBackfillWithNoAppDetailsDoesNothing is the switch-off half,
// exercised at the store layer: a nil appDetails map (what the handler passes
// when appdetails_lookup is off) must leave every candidate untouched and
// count zero — the same "not consulted, not filled" posture the suppression
// rung uses.
func TestReconcileBackfillWithNoAppDetailsDoesNothing(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	tileID := f.seedDiscoveredTile(t, "517710", "")

	scan := f.claimedScan(t, f.user)
	res, err := f.store.Reconcile(ctx, scan, f.host,
		[]ReportEntry{{ExternalID: "517710", Name: "Redout"}}, nil)
	must(t, err)

	if res.Backfilled != 0 {
		t.Errorf("res.Backfilled = %d, want 0 with no appdetails consulted", res.Backfilled)
	}
	var desc string
	must(t, pool.QueryRow(ctx, `SELECT description FROM apps WHERE id::text = $1`, tileID).Scan(&desc))
	if desc != "" {
		t.Errorf("tile description = %q, want still blank", desc)
	}
}

// TestScanReportBackfillEndToEndWithSwitchOff drives the full HTTP path with
// appdetails_lookup OFF at the resolver — the SAME gate the suppression rung
// uses (h.resolver.AppDetailsEnabled, checked once in handleScanReport before
// either PublishableAppIDs, BackfillCandidates, or Fetch ever run). Mirrors
// production wiring (cmd/quasar-control/app.go): the *AppDetails instance
// itself is constructed enabled=true unconditionally; the real gate is the
// resolver. So this must never call the mock Valve server, and a blank
// description must stay blank with a stored backfilled count of zero.
func TestScanReportBackfillEndToEndWithSwitchOff(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	hits := 0
	valve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"517710":{"success":true,"data":{"type":"game","short_description":"A fast racer."}}}`))
	}))
	defer valve.Close()

	f.seedDiscoveredTile(t, "517710", "")
	must(t, execT(ctx, pool, `INSERT INTO library_scans (user_id, app_id, host_id, state)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'pending')`, f.user, f.parent, f.host))

	details := NewAppDetails(true, quietLogger()) // production wiring: always enabled here
	details.endpoint = valve.URL
	set := &fakeSettings{enabled: true, appDetailsEnabled: false} // the resolver-level switch: OFF
	h := NewHandler(f.store, testStorageManager(f, set), set, details, newTestResolver(set), quietLogger())
	mux := http.NewServeMux()
	h.Register(mux, func(next http.Handler) http.Handler { return next })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp := agentReq(t, "GET", srv.URL+"/v1/agent/library/scan-pending", f.nodeName, f.secret, nil)
	var pending struct {
		Scans []PendingScan `json:"scans"`
	}
	must(t, json.NewDecoder(resp.Body).Decode(&pending))
	resp.Body.Close()
	if len(pending.Scans) != 1 {
		t.Fatalf("claimed %d scans, want 1", len(pending.Scans))
	}

	resp = agentReq(t, "POST", srv.URL+"/v1/agent/library/scan-report", f.nodeName, f.secret,
		ScanReport{ScanID: pending.Scans[0].ScanID, OK: true,
			Entries: []ReportEntry{{ExternalID: "517710", Name: "Redout"}}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scan-report = %d, want 200", resp.StatusCode)
	}

	if hits != 0 {
		t.Errorf("appdetails switch is off (AppDetails.enabled=false) but the mock Valve server got %d hits, want 0", hits)
	}
	var desc string
	var backfilled int
	must(t, pool.QueryRow(ctx, `SELECT description, backfilled FROM apps a, library_scans sc
		WHERE a.external_id = '517710' AND sc.id = $1::uuid`, pending.Scans[0].ScanID).Scan(&desc, &backfilled))
	if desc != "" {
		t.Errorf("tile description = %q, want still blank with the switch off", desc)
	}
	if backfilled != 0 {
		t.Errorf("stored backfilled count = %d, want 0 with the switch off", backfilled)
	}
}

// TestDerivedTileInheritsARealLaunchProfileSlug IS THE REGRESSION GATE for the
// ::uuid cast that broke production.
//
// apps.default_profile_id is TEXT and holds a launch-profile SLUG ("1440p120"),
// NOT a uuid. The derived-tile INSERT in Reconcile step 3 bound it as $5::uuid.
// Every fixture in this package left the parent's default_profile_id NULL, and
// NULL casts to uuid without complaint, so the wrong cast was invisible for
// exactly as long as no operator had set a launch profile on their Steam app.
// The moment one did, every reconcile of that home died with:
//
//	create derived tile: ERROR: invalid input syntax for type uuid: "1440p120" (SQLSTATE 22P02)
//
// ...which aborted the transaction, left the scan stranded in 'claimed', and —
// through the partial library_scans_open_uk — blocked every later enqueue for
// that (user, app, host) triple.
//
// THIS TEST BUILDS ITS OWN PROVIDER APP rather than leaning on newFixture. The
// shared fixture now carries a realistic slug too (that is the broader fix), but
// a regression gate for a specific defect should not be able to be disarmed by an
// unrelated edit to a fixture three hundred lines away.
func TestDerivedTileInheritsARealLaunchProfileSlug(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	const (
		wantSlug   = "1440p120"
		wantPolicy = "prefer"
	)

	// A provider app shaped the way a real one is once an operator has picked a
	// launch profile for it: a SLUG in a TEXT column, and a non-default policy.
	var parent string
	must(t, pool.QueryRow(ctx, `INSERT INTO apps (name, library_provider, managed_home, default_profile_id, profile_policy)
		VALUES ('Steam (profiled)', 'steam', true, $1, $2) RETURNING id::text`,
		wantSlug, wantPolicy).Scan(&parent))
	must(t, execT(ctx, pool, `INSERT INTO user_homes (user_id, app_id, host_id, provider, ref)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'local', '/homes/opaque-profiled')`, f.user, parent, f.host))

	var scan string
	must(t, pool.QueryRow(ctx, `INSERT INTO library_scans (user_id, app_id, host_id, state, claimed_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'claimed', now()) RETURNING id::text`,
		f.user, parent, f.host).Scan(&scan))

	// A single unambiguous game, so a failure here is about the INSERT's types and
	// nothing else.
	res, err := f.store.Reconcile(ctx, scan, f.host, []ReportEntry{
		{ExternalID: "517710", Name: "Redout: Enhanced Edition"},
	}, nil)
	if err != nil {
		t.Fatalf("reconcile failed with a real launch-profile slug on the parent: %v\n"+
			"default_profile_id is TEXT holding a slug; binding it as ::uuid is a 22P02 "+
			"the moment the column is non-empty", err)
	}
	if res.Created != 1 {
		t.Fatalf("reconcile created %d tiles, want 1", res.Created)
	}

	tileID, enabled, ok := f.tileOf(t, parent, "517710")
	if !ok {
		t.Fatal("no derived tile was created for the published appid")
	}
	if !enabled {
		t.Error("a freshly published tile must be enabled")
	}

	// §7.7: the tile copies the parent's launch defaults ONCE, at creation, and it
	// must copy them VERBATIM — the slug is a foreign key into the launch-profile
	// vocabulary, so a mangled or dropped one silently launches the game on the
	// wrong profile.
	var gotSlug *string
	var gotPolicy string
	must(t, pool.QueryRow(ctx, `SELECT default_profile_id, profile_policy FROM apps WHERE id=$1::uuid`,
		tileID).Scan(&gotSlug, &gotPolicy))
	if gotSlug == nil {
		t.Fatalf("tile default_profile_id is NULL; want the parent's slug %q copied at creation", wantSlug)
	}
	if *gotSlug != wantSlug {
		t.Errorf("tile default_profile_id = %q, want the parent's slug %q verbatim", *gotSlug, wantSlug)
	}
	if gotPolicy != wantPolicy {
		t.Errorf("tile profile_policy = %q, want the parent's %q", gotPolicy, wantPolicy)
	}
}

// TestSecondUserSeesOnlyTheirOwn — the tile is deduplicated fleet-wide (one row
// per (parent, appid)) but ENTITLED per user. This is what bounds the catalogue
// at the union of installed appids rather than the product of users and games.
func TestSecondUserSeesOnlyTheirOwn(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	f.homeFor(t, f.other, "/homes/opaque-b")

	// User A has Redout; user B has Tiny Dangerous Dungeons. Both scans see the
	// same Proton, which is exactly §8.4's point.
	scanA := f.claimedScan(t, f.user)
	_, err := f.store.Reconcile(ctx, scanA, f.host, []ReportEntry{
		{ExternalID: "517710", Name: "Redout: Enhanced Edition"},
		{ExternalID: "1493710", Name: "Proton Experimental"},
	}, nil)
	must(t, err)

	scanB := f.claimedScan(t, f.other)
	resB, err := f.store.Reconcile(ctx, scanB, f.host, []ReportEntry{
		{ExternalID: "3179810", Name: "Tiny Dangerous Dungeons Remake"},
		{ExternalID: "1493710", Name: "Proton Experimental"},
	}, nil)
	must(t, err)
	if resB.Created != 1 {
		t.Errorf("user B created %d tiles; want 1 (the shared Proton must not create one, "+
			"and Redout already exists)", resB.Created)
	}

	redout, _, ok := f.tile(t, "517710")
	if !ok {
		t.Fatal("Redout tile missing")
	}
	if _, has := f.entitlementGrantedBy(t, f.user, redout); !has {
		t.Error("user A is not entitled to their own game")
	}
	if by, has := f.entitlementGrantedBy(t, f.other, redout); has {
		t.Errorf("user B is entitled (granted_by=%q) to a game they do not have installed; "+
			"the tile must be visible ONLY to users with an observation", by)
	}
	// One row per game fleet-wide, not one per (user, game).
	if n := countT(t, pool, `SELECT count(*) FROM apps WHERE parent_app_id=$1::uuid`, f.parent); n != 2 {
		t.Errorf("tiles = %d, want 2 (global dedup by (parent, external_id))", n)
	}
}

// TestIgnoreIsDurableAndIsNotADelete is the §8.2 acceptance in full: ignoring a
// published tile disables it, revokes its provider entitlements and writes the
// rule; a subsequent scan re-observing the same appid does NOT re-enable it, does
// NOT re-create it, and does NOT re-grant. The app row, its artwork and any
// favourites all survive.
func TestIgnoreIsDurableAndIsNotADelete(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	entries := []ReportEntry{{ExternalID: "517710", Name: "Redout: Enhanced Edition"}}
	_, err := f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host, entries, nil)
	must(t, err)
	tileID, enabled, ok := f.tile(t, "517710")
	if !ok || !enabled {
		t.Fatalf("expected an enabled tile before the Ignore (ok=%v enabled=%v)", ok, enabled)
	}

	// The two things a DELETE would destroy irreversibly.
	must(t, execT(ctx, pool, `INSERT INTO app_artwork (app_id, source) VALUES ($1::uuid, 'manual')`, tileID))
	must(t, execT(ctx, pool, `INSERT INTO user_app_favourites (user_id, app_id) VALUES ($1::uuid, $2::uuid)`,
		f.user, tileID))

	res, err := f.store.SetRule(ctx, f.parent, SourceSteam, "517710", RuleIgnore, "junk", nil)
	must(t, err)
	if !res.Disabled || res.Revoked != 1 {
		t.Errorf("SetRule(ignore) = disabled %v revoked %d; want true / 1", res.Disabled, res.Revoked)
	}

	// A subsequent scan re-observing the same appid on disk.
	_, err = f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host, entries, nil)
	must(t, err)

	id2, enabled2, ok2 := f.tile(t, "517710")
	if !ok2 {
		t.Fatal("the tile was DELETED; suppression is a hide, never a delete")
	}
	if id2 != tileID {
		t.Fatalf("the tile was re-created with a new id (%s -> %s); the rule row must make the "+
			"reconciler skip the appid entirely", tileID, id2)
	}
	if enabled2 {
		t.Error("the scan re-enabled an ignored tile; §7.7 step 3 must never modify an existing tile")
	}
	if by, has := f.entitlementGrantedBy(t, f.user, tileID); has {
		t.Errorf("the scan re-granted a provider entitlement (granted_by=%q) for an ignored appid; "+
			"the suppression filter on step 5 is load-bearing", by)
	}
	if n := countT(t, pool, `SELECT count(*) FROM app_artwork WHERE app_id=$1::uuid`, tileID); n != 1 {
		t.Errorf("app_artwork rows = %d, want 1 — an Ignore must not cascade", n)
	}
	if n := countT(t, pool, `SELECT count(*) FROM user_app_favourites WHERE app_id=$1::uuid`, tileID); n != 1 {
		t.Errorf("user_app_favourites rows = %d, want 1 — an Ignore must not cascade", n)
	}

	// The "Seen, not published" read is how an admin finds it again.
	items, err := f.store.Unpublished(ctx, f.parent)
	must(t, err)
	found := false
	for _, it := range items {
		if it.ExternalID == "517710" {
			found = true
			if it.SuppressedBy != LayerRuleIgnore {
				t.Errorf("suppressed_by = %q, want %q", it.SuppressedBy, LayerRuleIgnore)
			}
			if !it.HasTile {
				t.Error("has_tile = false; a disabled tile still exists and its favourites matter")
			}
		}
	}
	if !found {
		t.Error("the ignored appid does not appear in 'Seen, not published'; it would be invisible")
	}
}

// TestScanNeverReEnablesADisabledTile is the SECOND of §8.2's two independent
// durability guarantees, and it had no coverage until the Phase 4 review.
//
// §8.2 demands two, deliberately redundant, because either alone is a
// resurrection bug:
//
//  1. the `library_appid_rules` row makes §7.7 step 3 skip the appid, so nothing
//     is re-created — covered by TestIgnoreIsDurableAndIsNotADelete; AND
//  2. step 3 NEVER MODIFIES AN EXISTING TILE, so `enabled = false` is never
//     flipped back "even if the rule row were somehow lost" — covered HERE.
//
// TestIgnoreIsDurableAndIsNotADelete cannot cover (2), and the reason is worth
// understanding rather than assuming: after an Ignore the appid carries
// rule='ignore', so Decide puts it in `suppress` and it never reaches step 3's
// INSERT at all. Mutating that INSERT's ON CONFLICT DO NOTHING to
// DO UPDATE SET enabled = true leaves that test green. This one goes red,
// because it disables a tile with NO rule row — which is also the real scenario:
// an admin who disabled a tile by hand for an unrelated reason must not have a
// background job silently undo it on the next sweep.
func TestScanNeverReEnablesADisabledTile(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	entries := []ReportEntry{{ExternalID: "517710", Name: "Redout: Enhanced Edition"}}
	_, err := f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host, entries, nil)
	must(t, err)
	tileID, enabled, ok := f.tile(t, "517710")
	if !ok || !enabled {
		t.Fatalf("expected an enabled tile to start from (ok=%v enabled=%v)", ok, enabled)
	}

	// An admin disables the tile BY HAND. Deliberately NO library_appid_rules row:
	// guarantee (1) must be absent so that only guarantee (2) can hold the line.
	must(t, execT(ctx, pool, `UPDATE apps SET enabled = false WHERE id = $1::uuid`, tileID))
	if n := countT(t, pool, `SELECT count(*) FROM library_appid_rules`); n != 0 {
		t.Fatalf("this test is only meaningful with no rule row; found %d", n)
	}

	// The game is still installed, so the very next scan reports it again and the
	// ladder puts it squarely in `publish` — it reaches step 3's INSERT.
	res, err := f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host, entries, nil)
	must(t, err)
	if res.Created != 0 {
		t.Errorf("created = %d, want 0: the tile already exists", res.Created)
	}

	id2, enabled2, ok2 := f.tile(t, "517710")
	if !ok2 || id2 != tileID {
		t.Fatalf("the tile was re-created (%s -> %s, ok=%v); ON CONFLICT must find the existing row",
			tileID, id2, ok2)
	}
	if enabled2 {
		t.Fatal("A SCAN RE-ENABLED A HAND-DISABLED TILE. §7.7 step 3 must never modify an " +
			"existing tile: ON CONFLICT DO NOTHING, never DO UPDATE. This is the second of " +
			"§8.2's two independent durability guarantees and the only test that covers it.")
	}

	// Nothing else about the row was touched either — a DO UPDATE that set only
	// `name` would still be a step-3 modification and still wrong.
	var name, origin string
	must(t, pool.QueryRow(ctx, `SELECT name, origin FROM apps WHERE id=$1::uuid`, tileID).Scan(&name, &origin))
	if name != "Redout: Enhanced Edition" || origin != "discovered" {
		t.Errorf("tile fields changed by a scan: name %q origin %q", name, origin)
	}
}

// TestEntitlementSurvivesAHomeMovingHost is the flap-prevention guarantee, and
// it likewise had no coverage until the Phase 4 review: there was no multi-host
// test at all.
//
// §7.7 step 5's revoke asks "does this user still have an observation ON ANY
// HOST", with NO host predicate. To a future contributor that query looks like
// it is missing a `WHERE host_id = …`, and adding one passes every other test in
// this package. What it actually does is make a user who moved a game from host
// A to host B lose the entitlement on A's sweep and regain it on B's — the tile
// flapping in and out of their library on alternating sweeps, forever, with
// nothing to catch it.
//
// Adding `AND o.host_id = $4::uuid` to that NOT EXISTS turns this test red.
func TestEntitlementSurvivesAHomeMovingHost(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	// A second host, and a home for the same user + same provider app on it.
	var hostB string
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, node_secret_hash)
		VALUES ('node-lib-b','online','deadbeef') RETURNING id::text`).Scan(&hostB))
	must(t, execT(ctx, pool, `INSERT INTO user_homes (user_id, app_id, host_id, provider, ref)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'local', '/homes/opaque-a-on-b')`, f.user, f.parent, hostB))

	scanOn := func(host string) string {
		var id string
		must(t, pool.QueryRow(ctx, `INSERT INTO library_scans (user_id, app_id, host_id, state, claimed_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, 'claimed', now()) RETURNING id::text`,
			f.user, f.parent, host).Scan(&id))
		return id
	}
	entries := []ReportEntry{{ExternalID: "517710", Name: "Redout: Enhanced Edition"}}

	// The game is installed on host A.
	_, err := f.store.Reconcile(ctx, scanOn(f.host), f.host, entries, nil)
	must(t, err)
	tileID, _, ok := f.tile(t, "517710")
	if !ok {
		t.Fatal("no tile after the host-A scan")
	}
	if _, has := f.entitlementGrantedBy(t, f.user, tileID); !has {
		t.Fatal("no entitlement after the host-A scan")
	}

	// The user installs it on host B too. Host B's scan sees it.
	_, err = f.store.Reconcile(ctx, scanOn(hostB), hostB, entries, nil)
	must(t, err)
	if _, has := f.entitlementGrantedBy(t, f.user, tileID); !has {
		t.Fatal("the entitlement vanished when a SECOND host also reported the game")
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_observations
		WHERE user_id=$1::uuid AND external_id='517710'`, f.user); n != 2 {
		t.Fatalf("observations = %d, want 2 (one per host — they are independent sets)", n)
	}

	// Now the user removes it from host A only. Host A's sweep runs and must NOT
	// revoke: the observation on host B is still there.
	res, err := f.store.Reconcile(ctx, scanOn(f.host), f.host, []ReportEntry{}, nil)
	must(t, err)
	if res.Revoked != 0 {
		t.Errorf("host A's sweep revoked %d entitlement(s) while host B still has the game "+
			"installed; the revoke's NOT EXISTS must have NO host predicate", res.Revoked)
	}
	if _, has := f.entitlementGrantedBy(t, f.user, tileID); !has {
		t.Fatal("THE ENTITLEMENT WAS REVOKED BY THE SWEEP OF A HOST THE GAME MOVED OFF. " +
			"§7.7 step 5's revoke asks 'is there an observation on ANY host' precisely so a " +
			"game moving between hosts does not flap in and out of the user's library.")
	}
	// Host A's own observation is gone — the per-host sets are still independent.
	if n := countT(t, pool, `SELECT count(*) FROM library_observations
		WHERE user_id=$1::uuid AND external_id='517710' AND host_id=$2::uuid`, f.user, f.host); n != 0 {
		t.Errorf("host A's observation survived its own successful scan (%d rows)", n)
	}

	// Only when the LAST host stops reporting it does the entitlement go.
	_, err = f.store.Reconcile(ctx, scanOn(hostB), hostB, []ReportEntry{}, nil)
	must(t, err)
	if _, has := f.entitlementGrantedBy(t, f.user, tileID); has {
		t.Error("the entitlement survived the game disappearing from EVERY host")
	}
	if _, _, ok := f.tile(t, "517710"); !ok {
		t.Error("the app row was deleted; only the entitlement is revoked")
	}
}

// TestLibraryRoutesRejectANonProviderApp — a rule keyed on an ordinary app is
// unreachable by the reconciler forever, so accepting one would return 200 for a
// write that can never have an effect. That is a trap for exactly the operator
// who is already hunting for why a rule "isn't working".
func TestLibraryRoutesRejectANonProviderApp(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	// A perfectly ordinary app. Kind is set to 'launcher' deliberately: §4.5.3
	// says kind is presentation and library_provider is the functional trigger,
	// so styling an app as a Launcher must NOT make it pass.
	var plain string
	must(t, pool.QueryRow(ctx, `INSERT INTO apps (name, kind) VALUES ('Just an app', 'launcher')
		RETURNING id::text`).Scan(&plain))

	srv := newTestServer(t, f, &fakeSettings{enabled: true}, NewAppDetails(false, quietLogger()))
	put := func(app string) (int, string) {
		body, _ := json.Marshal(map[string]string{"rule": "ignore"})
		req, err := http.NewRequest("PUT",
			srv.URL+"/v1/admin/apps/"+app+"/library/rules/517710", strings.NewReader(string(body)))
		must(t, err)
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		defer resp.Body.Close()
		var b map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&b)
		msg, _ := b["error"].(map[string]any)
		text, _ := msg["message"].(string)
		return resp.StatusCode, text
	}

	status, msg := put(plain)
	if status != http.StatusBadRequest {
		t.Fatalf("PUT a rule on a non-provider app = %d, want 400 (never 200: the rule would "+
			"be stored, acknowledged, and permanently inert)", status)
	}
	if !strings.Contains(strings.ToLower(msg), "library provider") {
		t.Errorf("error message = %q; it must name WHAT IS WRONG so the operator knows the fix", msg)
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_appid_rules`); n != 0 {
		t.Errorf("a rejected request wrote %d rule rows, want 0", n)
	}

	// The same request against the real provider app succeeds — without this the
	// 400 above could be any unrelated failure.
	if status, _ := put(f.parent); status != http.StatusOK {
		t.Fatalf("PUT a rule on the provider app = %d, want 200", status)
	}

	// The read routes and DELETE reject it too, and an unknown app is still 404.
	for _, path := range []string{"/library/unpublished", "/library/rules"} {
		resp, err := http.Get(srv.URL + "/v1/admin/apps/" + plain + path)
		must(t, err)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s on a non-provider app = %d, want 400", path, resp.StatusCode)
		}
	}
	req, err := http.NewRequest("DELETE", srv.URL+"/v1/admin/apps/"+plain+"/library/rules/517710", nil)
	must(t, err)
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("DELETE a rule on a non-provider app = %d, want 400", resp.StatusCode)
	}
	resp, err = http.Get(srv.URL + "/v1/admin/apps/00000000-0000-0000-0000-0000000000aa/library/rules")
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET rules on an unknown app = %d, want 404 — a real app that is not a "+
			"provider is 400, a nonexistent one is still 404", resp.StatusCode)
	}
}

// TestAllowBeatsTheBuiltInDenylist — rung 1 over rung 3, end to end.
func TestAllowBeatsTheBuiltInDenylist(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	// Proton Experimental: on the built-in appid list AND matching a name prefix.
	entries := []ReportEntry{{ExternalID: "1493710", Name: "Proton Experimental"}}
	_, err := f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host, entries, nil)
	must(t, err)
	if _, _, ok := f.tile(t, "1493710"); ok {
		t.Fatal("the built-in denylist did not suppress Proton Experimental")
	}

	_, err = f.store.SetRule(ctx, f.parent, SourceSteam, "1493710", RuleAllow, "not junk after all", nil)
	must(t, err)

	_, err = f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host, entries, nil)
	must(t, err)
	id, enabled, ok := f.tile(t, "1493710")
	if !ok || !enabled {
		t.Fatalf("allow did not publish on the next scan (ok=%v enabled=%v); a wrongly-denylisted "+
			"game must be recoverable without a release", ok, enabled)
	}
	if by, has := f.entitlementGrantedBy(t, f.user, id); !has || by != "provider" {
		t.Errorf("allowed appid: entitlement granted_by=%q present=%v; want a provider grant", by, has)
	}
}

// TestUninstallRevokesProviderNotAdmin — the revoke path, and its one exception.
func TestUninstallRevokesProviderNotAdmin(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	_, err := f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host, []ReportEntry{
		{ExternalID: "517710", Name: "Redout: Enhanced Edition"},
		{ExternalID: "3179810", Name: "Tiny Dangerous Dungeons Remake"},
	}, nil)
	must(t, err)
	redout, _, _ := f.tile(t, "517710")
	tiny, _, _ := f.tile(t, "3179810")

	// An admin grants the OTHER user Redout by hand. The sync must never touch it.
	must(t, execT(ctx, pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('user', $1::uuid, $2::uuid, 'admin')`, f.other, redout))

	// The user uninstalls Redout and rescans.
	res, err := f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host, []ReportEntry{
		{ExternalID: "3179810", Name: "Tiny Dangerous Dungeons Remake"},
	}, nil)
	must(t, err)
	if res.Revoked != 1 {
		t.Errorf("revoked = %d, want 1", res.Revoked)
	}
	if _, has := f.entitlementGrantedBy(t, f.user, redout); has {
		t.Error("the scanning user kept a provider entitlement for an uninstalled game")
	}
	if _, _, ok := f.tile(t, "517710"); !ok {
		t.Error("the app row was deleted on uninstall; only the entitlement is revoked")
	}
	if by, has := f.entitlementGrantedBy(t, f.other, redout); !has || by != "admin" {
		t.Errorf("the admin grant is granted_by=%q present=%v; the sync must never touch "+
			"granted_by='admin' rows", by, has)
	}
	if _, has := f.entitlementGrantedBy(t, f.user, tiny); !has {
		t.Error("a still-installed game lost its entitlement")
	}
	// The observation for the uninstalled game is gone too — that is the only
	// place a game "disappearing" is recognised.
	if n := countT(t, pool, `SELECT count(*) FROM library_observations
		WHERE user_id=$1::uuid AND external_id='517710'`, f.user); n != 0 {
		t.Errorf("stale observations = %d, want 0", n)
	}
}

// TestFailedScanRevokesNothing — the guard against a transient error
// mass-revoking a fleet's libraries.
func TestFailedScanRevokesNothing(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	entries := []ReportEntry{{ExternalID: "517710", Name: "Redout: Enhanced Edition"}}
	_, err := f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host, entries, nil)
	must(t, err)
	tileID, _, _ := f.tile(t, "517710")

	scan := f.claimedScan(t, f.user)
	must(t, f.store.MarkFailed(ctx, scan, f.host, "home not mounted"))

	if _, has := f.entitlementGrantedBy(t, f.user, tileID); !has {
		t.Error("a FAILED scan revoked an entitlement; absence is what a transient error looks like")
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_observations WHERE user_id=$1::uuid`, f.user); n != 1 {
		t.Errorf("observations after a failed scan = %d, want 1 (unchanged)", n)
	}
	if _, enabled, ok := f.tile(t, "517710"); !ok || !enabled {
		t.Error("a failed scan changed a tile")
	}
	var state, msg string
	must(t, pool.QueryRow(ctx, `SELECT state, error FROM library_scans WHERE id=$1::uuid`, scan).Scan(&state, &msg))
	if state != "failed" || msg == "" {
		t.Errorf("scan state = %q error = %q; want failed with the reason recorded", state, msg)
	}
}

// TestIngestRejectsBadAppIDsAndKeepsTheRest — §10 validation point 2.
func TestIngestRejectsBadAppIDsAndKeepsTheRest(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	res, err := f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host, []ReportEntry{
		{ExternalID: "517710", Name: "Redout: Enhanced Edition"},
		{ExternalID: "517710; rm -rf /", Name: "injection"},
		{ExternalID: "0", Name: "zero"},
		{ExternalID: "-1", Name: "negative"},
		{ExternalID: "", Name: "empty"},
		{ExternalID: "9999999999", Name: "past 2^32"},
	}, nil)
	must(t, err)
	if res.Observed != 1 || res.Rejected != 5 {
		t.Fatalf("reconcile = %+v; want 1 observed / 5 rejected — one bad manifest must not "+
			"cost a user their whole library", res)
	}
	if _, _, ok := f.tile(t, "517710"); !ok {
		t.Error("the valid entry was not published alongside the rejected ones")
	}
}

// --- the pull channel --------------------------------------------------------

// TestClaimIsDisjointUnderConcurrency — FOR UPDATE SKIP LOCKED. Two agents
// claiming simultaneously must get DISJOINT sets, not a blocked one and a
// duplicate.
func TestClaimIsDisjointUnderConcurrency(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	// Ten users, ten homes, ten pending scans on one host.
	for i := 0; i < 10; i++ {
		var u string
		must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash, role)
			VALUES ($1, $2, 'x', 'user') RETURNING id::text`,
			"c"+string(rune('a'+i))+"@t.local", "c"+string(rune('a'+i))).Scan(&u))
		f.homeFor(t, u, "/homes/c"+string(rune('a'+i)))
		must(t, execT(ctx, pool, `INSERT INTO library_scans (user_id, app_id, host_id, state)
			VALUES ($1::uuid, $2::uuid, $3::uuid, 'pending')`, u, f.parent, f.host))
	}

	var wg sync.WaitGroup
	results := make([][]PendingScan, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = f.store.ClaimPending(ctx, f.host)
		}(i)
	}
	wg.Wait()
	must(t, errs[0])
	must(t, errs[1])

	seen := map[string]int{}
	for _, batch := range results {
		for _, s := range batch {
			seen[s.ScanID]++
			if s.RootPath == "" {
				t.Error("a claimed scan has an empty root_path; the agent would have nothing to walk")
			}
			if len(s.RelativeRoots) == 0 || s.MaxEntries == 0 || s.MaxManifestBytes == 0 {
				t.Errorf("scan job is missing its bounds: %+v", s)
			}
		}
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("scan %s was claimed %d times; SKIP LOCKED must produce disjoint sets", id, n)
		}
	}
	if len(seen) != 10 {
		t.Errorf("claimed %d distinct scans, want 10", len(seen))
	}
	if countT(t, pool, `SELECT count(*) FROM library_scans WHERE state='pending'`) != 0 {
		t.Error("pending scans remain after both agents claimed")
	}
}

// TestAbandonedClaimReturnsToPending — the 30-minute reap window, which is the
// ONLY recovery for an agent that died mid-walk.
func TestAbandonedClaimReturnsToPending(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	must(t, execT(ctx, pool, `INSERT INTO library_scans (user_id, app_id, host_id, state)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'pending')`, f.user, f.parent, f.host))

	claimed, err := f.store.ClaimPending(ctx, f.host)
	must(t, err)
	if len(claimed) != 1 {
		t.Fatalf("claimed %d scans, want 1", len(claimed))
	}

	// A fresh claim is NOT reaped.
	n, err := f.store.ReapClaimed(ctx, ClaimTTL)
	must(t, err)
	if n != 0 {
		t.Errorf("reaped %d fresh claims, want 0", n)
	}

	must(t, execT(ctx, pool, `UPDATE library_scans SET claimed_at = now() - interval '31 minutes'`))
	n, err = f.store.ReapClaimed(ctx, ClaimTTL)
	must(t, err)
	if n != 1 {
		t.Fatalf("reaped %d abandoned claims, want 1", n)
	}
	again, err := f.store.ClaimPending(ctx, f.host)
	must(t, err)
	if len(again) != 1 || again[0].ScanID != claimed[0].ScanID {
		t.Errorf("the reaped scan was not re-claimable: %+v", again)
	}
}

// TestClaimIsScopedToTheCallingHost — an agent must never see another host's
// scan, because the root_path would be a path on a machine it does not own.
func TestClaimIsScopedToTheCallingHost(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	var other string
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status) VALUES ('node-other','online')
		RETURNING id::text`).Scan(&other))
	must(t, execT(ctx, pool, `INSERT INTO library_scans (user_id, app_id, host_id, state)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'pending')`, f.user, f.parent, f.host))

	got, err := f.store.ClaimPending(ctx, other)
	must(t, err)
	if len(got) != 0 {
		t.Fatalf("host B claimed %d of host A's scans; the claim must be host-scoped", len(got))
	}
}

// --- the janitor -------------------------------------------------------------

// fakeSettings drives the janitor deterministically without an instance_settings
// round trip, and lets a test flip the switch between passes — which is the
// behaviour §11.1 step 1 exists to guarantee.
type fakeSettings struct {
	enabled  bool
	provider string
	// intervalMinutes / appDetailsEnabled back the admin-libraries amendment's
	// DATABASE side of resolution (ResolverSettingsReader). Zero-value
	// intervalMinutes reads as the column default (360), matching what an
	// unseeded/untouched instance_settings row actually returns.
	intervalMinutes   int
	appDetailsEnabled bool
}

func (s *fakeSettings) LibraryDiscoveryEnabled(context.Context) (bool, error) { return s.enabled, nil }
func (s *fakeSettings) StorageProvider(context.Context) (string, error) {
	if s.provider == "" {
		return "local", nil
	}
	return s.provider, nil
}
func (s *fakeSettings) LibraryDiscoveryIntervalMinutes(context.Context) (int, error) {
	if s.intervalMinutes == 0 {
		return 360, nil
	}
	return s.intervalMinutes, nil
}
func (s *fakeSettings) LibraryDiscoveryAppDetailsEnabled(context.Context) (bool, error) {
	return s.appDetailsEnabled, nil
}

// newTestResolver builds a Resolver with no env override (the database column
// on set, read via ResolverSettingsReader, always wins in tests unless a case
// explicitly wants the override path — see TestResolver* below).
func newTestResolver(set ResolverSettingsReader) *Resolver {
	return NewResolver(set, false, 0, false, false)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestJanitorDoesNothingWhenTheSettingIsOff — the SHIP-DARK acceptance: no scan
// rows at all.
func TestJanitorDoesNothingWhenTheSettingIsOff(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	set := &fakeSettings{enabled: false}
	j := NewJanitor(f.store, set, newTestResolver(set), quietLogger())
	res, skip := j.RunOnce(ctx)
	if n := countT(t, pool, `SELECT count(*) FROM library_scans`); n != 0 {
		t.Fatalf("scan rows with the setting off = %d, want 0", n)
	}
	// skipReason is what the library.discovery job (internal/jobs) maps onto
	// jobs.Skipped(reason) — WP2 §8.2's "configured off" case.
	if skip != reasonDiscoveryOff {
		t.Fatalf("skip reason = %q, want %q", skip, reasonDiscoveryOff)
	}
	if res != (RunResult{}) {
		t.Fatalf("result = %+v, want the zero value on a skipped pass", res)
	}

	// THE SETTING IS READ PER PASS. Flipping it must take effect on the next pass
	// with no restart — the artwork lesson.
	set.enabled = true
	res, skip = j.RunOnce(ctx)
	if n := countT(t, pool, `SELECT count(*) FROM library_scans WHERE state='pending'`); n != 1 {
		t.Fatalf("pending scans after enabling = %d, want 1 (the flag must be read per pass)", n)
	}
	if skip != "" {
		t.Fatalf("skip reason = %q, want \"\" (an enabled pass ran)", skip)
	}
	if res.Enqueued != 1 {
		t.Fatalf("result.Enqueued = %d, want 1", res.Enqueued)
	}
}

// TestJanitorLegacyVolumeSettingNoLongerBlanketBlocksLocalHomes — §7.5,
// updated for #473 (hard removal, operator direction 2026-08-25). This used
// to be TestJanitorIsInertOnAVolumeProvider and asserted an explicit SKIP
// with a stated reason: the janitor had a dedicated `provider == "volume"`
// gate that returned early and blocked EVERY home instance-wide, regardless
// of what that individual home's own provider actually was. That gate is
// gone along with the driver — a legacy "volume" instance SETTING (settings
// validation rejects writing it going forward; this simulates a stale
// row/double bypassing that) is no longer a janitor-level inert condition at
// all. newFixture seeds one 'local'-provider home, and it now gets enqueued
// exactly as it would under any other setting value — per-home truth
// (`uh.provider = 'local'` in Enqueue's own SQL), not the instance-wide
// setting, decides what gets scanned. The pass is a normal SUCCEEDED run
// (not a skip): a skip reason is for "nothing CAN run", and something did.
func TestJanitorLegacyVolumeSettingNoLongerBlanketBlocksLocalHomes(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	j := NewJanitor(f.store, &fakeSettings{enabled: true, provider: "volume"}, newTestResolver(&fakeSettings{enabled: true, provider: "volume"}), quietLogger())
	_, skip := j.RunOnce(context.Background())
	if n := countT(t, pool, `SELECT count(*) FROM library_scans`); n != 1 {
		t.Fatalf("scan rows for the fixture's local-provider home = %d, want 1 (a legacy instance setting must not blanket-block it)", n)
	}
	if skip != "" {
		t.Fatalf("skip reason = %q, want empty — the janitor no longer has a volume-specific gate", skip)
	}
}

// TestJanitorEnqueueIsIdempotentAndIntervalled.
func TestJanitorEnqueueIsIdempotentAndIntervalled(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	j := NewJanitor(f.store, &fakeSettings{enabled: true}, newTestResolver(&fakeSettings{enabled: true}), quietLogger())

	j.RunOnce(ctx)
	j.RunOnce(ctx)
	if n := countT(t, pool, `SELECT count(*) FROM library_scans`); n != 1 {
		t.Fatalf("scan rows after two passes = %d, want 1 (an open scan blocks a second)", n)
	}

	// Complete it; a successful scan inside the interval must not re-enqueue.
	must(t, execT(ctx, pool, `UPDATE library_scans SET state='reported', reported_at=now()`))
	j.RunOnce(ctx)
	if n := countT(t, pool, `SELECT count(*) FROM library_scans WHERE state='pending'`); n != 0 {
		t.Errorf("re-enqueued %d scans inside the interval, want 0", n)
	}

	// ...and must re-enqueue once the interval has elapsed.
	must(t, execT(ctx, pool, `UPDATE library_scans SET reported_at = now() - interval '7 hours'`))
	j.RunOnce(ctx)
	if n := countT(t, pool, `SELECT count(*) FROM library_scans WHERE state='pending'`); n != 1 {
		t.Errorf("pending scans after the interval elapsed = %d, want 1", n)
	}
}

// TestJanitorIgnoresNonProviderApps — kind is presentation, library_provider is
// the trigger (§4.5.3).
func TestJanitorIgnoresNonProviderApps(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	var plain string
	must(t, pool.QueryRow(ctx, `INSERT INTO apps (name, managed_home) VALUES ('Plain app', true)
		RETURNING id::text`).Scan(&plain))
	must(t, execT(ctx, pool, `INSERT INTO user_homes (user_id, app_id, host_id, provider, ref)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'local', '/homes/plain')`, f.user, plain, f.host))

	NewJanitor(f.store, &fakeSettings{enabled: true}, newTestResolver(&fakeSettings{enabled: true}), quietLogger()).RunOnce(ctx)
	if n := countT(t, pool, `SELECT count(*) FROM library_scans WHERE app_id=$1::uuid`, plain); n != 0 {
		t.Errorf("enqueued %d scans for a non-provider app, want 0", n)
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_scans WHERE app_id=$1::uuid`, f.parent); n != 1 {
		t.Errorf("enqueued %d scans for the provider app, want 1", n)
	}
}

// TestJanitorExpiresStrandedScans — a pending scan whose home is gone would
// otherwise occupy library_scans_open_uk forever and block that triple.
func TestJanitorExpiresStrandedScans(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	j := NewJanitor(f.store, &fakeSettings{enabled: true}, newTestResolver(&fakeSettings{enabled: true}), quietLogger())

	j.RunOnce(ctx)
	must(t, execT(ctx, pool, `UPDATE user_homes SET gc_after = now()`))
	j.RunOnce(ctx)
	if n := countT(t, pool, `SELECT count(*) FROM library_scans WHERE state='pending'`); n != 0 {
		t.Errorf("stranded pending scans = %d, want 0", n)
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_scans WHERE state='failed'`); n != 1 {
		t.Errorf("expired scans recorded as failed = %d, want 1", n)
	}
}

// --- the HTTP surface --------------------------------------------------------

// newTestServer wires the library handler for the HTTP-surface tests.
//
// THE STORAGE MANAGER IS A REAL ONE WITH A REAL ROOT, and that stopped being
// optional on 2026-08-10. It used to be storage.NewVolume(f.pool) — an
// always-volume stub whose DriverResolver role was incidental (it was passed for
// its AgentAuthenticator half). That pairing is not a state production can reach:
// an always-volume driver under a 'local' setting describes nothing. It went
// unnoticed only because inertReason consulted the resolver for 'auto' alone, so
// the stub's answer was never read on a 'local' case.
//
// Now that a rootless instance is inert whatever the setting says, the stub
// would make every 'local' case in this file report as inert. Handing the
// manager the same `set` production hands it (settingsStore) plus a fixed root
// makes the fixture describe a real deployment: whatever the setting says, the
// hosts have somewhere to put a home. Tests that want a ROOTLESS instance say so
// explicitly — that is what inertreason_auto_db_test.go's newAutoServer is for.
// testStorageManager is the ROOTED manager described above: the same settings
// reader the handler gets, over a host root that always resolves. One helper so
// every fixture in this package agrees, and so a rootless fixture has to be
// written deliberately rather than reached by accident.
func testStorageManager(f fixture, set *fakeSettings) *storage.Manager {
	return storage.New(f.pool, set, storage.HostRootResolverFunc(
		func(context.Context, string) (string, error) { return "/data/quasar-homes", nil }))
}

func newTestServer(t *testing.T, f fixture, set *fakeSettings, details *AppDetails) *httptest.Server {
	t.Helper()
	h := NewHandler(f.store, testStorageManager(f, set), set, details, newTestResolver(set), quietLogger())
	mux := http.NewServeMux()
	h.Register(mux, func(next http.Handler) http.Handler { return next })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func agentReq(t *testing.T, method, url, node, secret string, body any) *http.Response {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		must(t, err)
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, url, rdr)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("X-Quasar-Node", node)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	return resp
}

// TestAgentPullCarriesNoUser is THE test a reviewer will look for. It asserts
// the property §7.3 states and protocol/agent-api.md's P2-01 verdict requires:
// no user id, no username and no user-derived field anywhere in either
// direction. It asserts on the RAW JSON, not on the struct, because a struct
// with the right fields proves nothing about what a `map[string]any` or a future
// added field would serialize.
func TestAgentPullCarriesNoUser(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	// Give the user a recognisable username and email so a leak would be visible.
	must(t, execT(ctx, pool, `UPDATE users SET username='leaky-username', email='leaky@t.local'
		WHERE id=$1::uuid`, f.user))
	must(t, execT(ctx, pool, `INSERT INTO library_scans (user_id, app_id, host_id, state)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'pending')`, f.user, f.parent, f.host))

	srv := newTestServer(t, f, &fakeSettings{enabled: true}, NewAppDetails(false, quietLogger()))
	resp := agentReq(t, "GET", srv.URL+"/v1/agent/library/scan-pending", f.nodeName, f.secret, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scan-pending = %d, want 200", resp.StatusCode)
	}
	var raw map[string]json.RawMessage
	must(t, json.NewDecoder(resp.Body).Decode(&raw))
	payload := string(raw["scans"])

	for _, forbidden := range []string{"leaky-username", "leaky@t.local", f.user, "user_id", "username", "email"} {
		if strings.Contains(payload, forbidden) {
			t.Errorf("the scan-pending payload contains %q:\n%s\n\nTHE AGENT MUST NEVER LEARN A USER "+
				"(§7.3). The scan_id -> (user, app, host) mapping is resolved control-plane side.",
				forbidden, payload)
		}
	}
	// ...and the fields that ARE there.
	var scans []map[string]any
	must(t, json.Unmarshal(raw["scans"], &scans))
	if len(scans) != 1 {
		t.Fatalf("scans = %d, want 1", len(scans))
	}
	want := map[string]bool{"scan_id": true, "root_path": true, "relative_roots": true,
		"max_entries": true, "max_manifest_bytes": true}
	for k := range scans[0] {
		if !want[k] {
			t.Errorf("unexpected field %q on the wire; §7.3 fixes this payload exactly", k)
		}
	}
	for k := range want {
		if _, ok := scans[0][k]; !ok {
			t.Errorf("missing field %q from the §7.3 payload", k)
		}
	}
}

// TestAgentAuthAndOwnership — the same node-secret scheme as the GC reaper, and
// a scan the caller's host does not own is a 404 rather than a 403.
func TestAgentAuthAndOwnership(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	srv := newTestServer(t, f, &fakeSettings{enabled: true}, NewAppDetails(false, quietLogger()))

	resp := agentReq(t, "GET", srv.URL+"/v1/agent/library/scan-pending", f.nodeName, "wrong-secret", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad secret = %d, want 401", resp.StatusCode)
	}
	resp = agentReq(t, "GET", srv.URL+"/v1/agent/library/scan-pending", "unknown-node", f.secret, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown node = %d, want 401", resp.StatusCode)
	}

	// A claimed scan on another host.
	var other string
	must(t, pool.QueryRow(context.Background(), `INSERT INTO hosts (node_name, status, node_secret_hash)
		VALUES ('node-two','online','deadbeef') RETURNING id::text`).Scan(&other))
	var foreign string
	must(t, pool.QueryRow(context.Background(), `INSERT INTO library_scans (user_id, app_id, host_id, state, claimed_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'claimed', now()) RETURNING id::text`,
		f.user, f.parent, other).Scan(&foreign))

	resp = agentReq(t, "POST", srv.URL+"/v1/agent/library/scan-report", f.nodeName, f.secret,
		ScanReport{ScanID: foreign, OK: true, Entries: []ReportEntry{{ExternalID: "517710", Name: "Redout"}}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("reporting another host's scan = %d, want 404 (never 403: a 403 would confirm "+
			"the scan exists)", resp.StatusCode)
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_observations`); n != 0 {
		t.Errorf("a cross-host report wrote %d observations, want 0", n)
	}
}

// TestScanPendingIsEmptyWhenDiscoveryIsOff — the second half of the SHIP-DARK
// acceptance: not just "no new rows" but "no work handed out".
func TestScanPendingIsEmptyWhenDiscoveryIsOff(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	must(t, execT(context.Background(), pool, `INSERT INTO library_scans (user_id, app_id, host_id, state)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'pending')`, f.user, f.parent, f.host))

	srv := newTestServer(t, f, &fakeSettings{enabled: false}, NewAppDetails(false, quietLogger()))
	resp := agentReq(t, "GET", srv.URL+"/v1/agent/library/scan-pending", f.nodeName, f.secret, nil)
	defer resp.Body.Close()
	var body struct {
		Scans []PendingScan `json:"scans"`
	}
	must(t, json.NewDecoder(resp.Body).Decode(&body))
	if len(body.Scans) != 0 {
		t.Fatalf("scan-pending handed out %d jobs with discovery off, want 0", len(body.Scans))
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_scans WHERE state='claimed'`); n != 0 {
		t.Error("a scan was claimed with discovery off")
	}
}

// TestScanReportEndToEnd drives the whole pull channel over HTTP.
func TestScanReportEndToEnd(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	must(t, execT(context.Background(), pool, `INSERT INTO library_scans (user_id, app_id, host_id, state)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'pending')`, f.user, f.parent, f.host))

	srv := newTestServer(t, f, &fakeSettings{enabled: true}, NewAppDetails(false, quietLogger()))
	resp := agentReq(t, "GET", srv.URL+"/v1/agent/library/scan-pending", f.nodeName, f.secret, nil)
	var pending struct {
		Scans []PendingScan `json:"scans"`
	}
	must(t, json.NewDecoder(resp.Body).Decode(&pending))
	resp.Body.Close()
	if len(pending.Scans) != 1 {
		t.Fatalf("claimed %d scans, want 1", len(pending.Scans))
	}
	if pending.Scans[0].RootPath != "/homes/opaque-a" {
		t.Errorf("root_path = %q, want the user's home ref", pending.Scans[0].RootPath)
	}

	resp = agentReq(t, "POST", srv.URL+"/v1/agent/library/scan-report", f.nodeName, f.secret,
		ScanReport{ScanID: pending.Scans[0].ScanID, OK: true, Entries: observedEntries()})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scan-report = %d, want 200", resp.StatusCode)
	}
	if n := countT(t, pool, `SELECT count(*) FROM apps WHERE parent_app_id=$1::uuid`, f.parent); n != 4 {
		t.Errorf("published tiles = %d, want 4", n)
	}
	// A duplicate report is a 409, not a second reconcile.
	resp = agentReq(t, "POST", srv.URL+"/v1/agent/library/scan-report", f.nodeName, f.secret,
		ScanReport{ScanID: pending.Scans[0].ScanID, OK: true, Entries: observedEntries()})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate report = %d, want 409", resp.StatusCode)
	}
}

// TestAdminRuleRouteValidatesTheAppID — §10 point 3: this table is
// admin-writable and takes an appid straight from an HTTP body.
func TestAdminRuleRouteValidatesTheAppID(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	srv := newTestServer(t, f, &fakeSettings{enabled: true}, NewAppDetails(false, quietLogger()))

	for _, bad := range []string{"0", "007", "abc", "9999999999"} {
		body, _ := json.Marshal(map[string]string{"rule": "ignore"})
		req, err := http.NewRequest("PUT",
			srv.URL+"/v1/admin/apps/"+f.parent+"/library/rules/"+bad, strings.NewReader(string(body)))
		must(t, err)
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT rule with external_id %q = %d, want 400", bad, resp.StatusCode)
		}
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_appid_rules`); n != 0 {
		t.Errorf("bad appids wrote %d rules, want 0", n)
	}

	// ...and a bad rule value.
	body, _ := json.Marshal(map[string]string{"rule": "delete"})
	req, err := http.NewRequest("PUT",
		srv.URL+"/v1/admin/apps/"+f.parent+"/library/rules/517710", strings.NewReader(string(body)))
	must(t, err)
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT rule with rule=delete = %d, want 400", resp.StatusCode)
	}
}

// TestAdminStatusSurfacesTheInertReason — §7.5. With auto-publish, "nothing
// appeared" and "nothing ran" look identical unless something says which.
func TestAdminStatusSurfacesTheInertReason(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)

	cases := []struct {
		set  *fakeSettings
		want string
	}{
		{&fakeSettings{enabled: false}, "switched off"},
		// #473 hard removal (2026-08-25): a legacy "volume" provider value no
		// longer gets a volume-specific reason — testStorageManager's resolver
		// rejects it unconditionally (ErrVolumeDriverRemoved), which reads to
		// noHostHasStorageRoot as "unresolved", the same bucket a rootless
		// instance falls into.
		{&fakeSettings{enabled: true, provider: "volume"}, "storage root"},
		{&fakeSettings{enabled: true, provider: "local"}, ""},
	}
	for _, c := range cases {
		srv := newTestServer(t, f, c.set, NewAppDetails(false, quietLogger()))
		resp, err := http.Get(srv.URL + "/v1/admin/library/status")
		must(t, err)
		var body struct {
			InertReason string `json:"inert_reason"`
		}
		must(t, json.NewDecoder(resp.Body).Decode(&body))
		resp.Body.Close()
		if c.want == "" {
			if body.InertReason != "" {
				t.Errorf("inert_reason = %q, want empty for a live configuration", body.InertReason)
			}
			continue
		}
		if !strings.Contains(body.InertReason, c.want) {
			t.Errorf("inert_reason = %q, want it to mention %q", body.InertReason, c.want)
		}
	}
}

// --- the fourth inert reason: nobody marked the provider app ------------------
//
// THE FIRST-RUN STATE. An operator flips library_discovery_enabled on, no app
// carries library_provider='steam', the enqueue matches zero rows, and NOTHING
// is written or said. §7.5's principle is that under auto-publish "nothing
// appeared" and "nothing ran" are indistinguishable, so the reason must be
// surfaced — on both admin surfaces and once in the log.

// unmarkProvider takes the fixture's Steam app back to the state a fresh
// instance is in: a real app, not marked as a library provider.
func (f fixture) unmarkProvider(t *testing.T) {
	t.Helper()
	must(t, execT(context.Background(), f.pool,
		`UPDATE apps SET library_provider = '' WHERE id::text = $1`, f.parent))
}

// captureLogger returns a logger and the buffer it writes to, so a test can
// assert on what the janitor actually TOLD the operator rather than on a state
// field that no human ever reads.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})), &buf
}

// TestJanitorSaysWhenNoAppIsAProvider — the log half. Said ONCE (noteInert
// dedupes), and it must name the remedy: an operator told only that discovery is
// inert has been moved from silence to a sentence, which is barely better.
func TestJanitorSaysWhenNoAppIsAProvider(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	f.unmarkProvider(t)
	ctx := context.Background()

	log, buf := captureLogger()
	j := NewJanitor(f.store, &fakeSettings{enabled: true}, newTestResolver(&fakeSettings{enabled: true}), log)
	j.RunOnce(ctx)

	if !strings.Contains(buf.String(), "no app is marked as a library provider") {
		t.Fatalf("janitor log did not say why discovery was inert:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "Identity section of the app editor") {
		t.Errorf("the reason does not name the remedy, so an operator still cannot act on it:\n%s", buf.String())
	}
	// Not a hard error and not a behaviour change: the pass still runs, it simply
	// enqueues nothing, which stays a normal outcome.
	if n := countT(t, pool, `SELECT count(*) FROM library_scans`); n != 0 {
		t.Errorf("scan rows with no provider app = %d, want 0", n)
	}

	// Said once, not every six hours forever.
	before := strings.Count(buf.String(), "discovery is inert")
	j.RunOnce(ctx)
	if after := strings.Count(buf.String(), "discovery is inert"); after != before {
		t.Errorf("the inert line was repeated on the second pass (%d then %d) — noteInert exists to say it once", before, after)
	}
}

// TestJanitorReasonClearsOnceAnAppIsMarked — the reason is a REPORT, not a gate.
// Marking the app makes it go away and the enqueue proceeds on the very next
// pass, with no restart, because §11.1 step 1 re-reads per pass.
func TestJanitorReasonClearsOnceAnAppIsMarked(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	f.unmarkProvider(t)
	ctx := context.Background()

	log, buf := captureLogger()
	j := NewJanitor(f.store, &fakeSettings{enabled: true}, newTestResolver(&fakeSettings{enabled: true}), log)
	j.RunOnce(ctx)
	if !strings.Contains(buf.String(), "no app is marked as a library provider") {
		t.Fatalf("setup: expected the inert reason on the first pass:\n%s", buf.String())
	}

	buf.Reset()
	must(t, execT(ctx, pool,
		`UPDATE apps SET library_provider = 'steam' WHERE id::text = $1`, f.parent))
	j.RunOnce(ctx)

	if strings.Contains(buf.String(), "no app is marked as a library provider") {
		t.Errorf("the reason survived the app being marked:\n%s", buf.String())
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_scans`); n != 1 {
		t.Fatalf("scans enqueued after marking the app = %d, want 1", n)
	}
}

// TestInertReasonPrecedence — ordering is deliberate, not incidental. With the
// switch off AND no provider app, the answer is the SWITCH: the instance-level
// facts make the provider question moot, and reporting "no provider app" to
// someone who has the feature off would send them configuring an app for
// nothing.
func TestInertReasonPrecedence(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	f.unmarkProvider(t)

	log, buf := captureLogger()
	NewJanitor(f.store, &fakeSettings{enabled: false}, newTestResolver(&fakeSettings{enabled: false}), log).RunOnce(context.Background())
	if !strings.Contains(buf.String(), reasonDiscoveryOff) {
		t.Errorf("janitor reported %q, want the switch:\n%s", buf.String(), reasonDiscoveryOff)
	}
	if strings.Contains(buf.String(), reasonNoProviderApp) {
		t.Errorf("janitor reported the provider reason while the switch was off:\n%s", buf.String())
	}

	// The same order on the admin surface, which is where an operator actually
	// reads it.
	srv := newTestServer(t, f, &fakeSettings{enabled: false}, NewAppDetails(false, quietLogger()))
	if got := statusInertReason(t, srv); got != reasonDiscoveryOff {
		t.Errorf("status inert_reason = %q, want the switch reason", got)
	}
}

// TestBothAdminSurfacesReportTheSameNoProviderReason — the two surfaces share
// one helper precisely so their wording cannot drift, so the assertion is
// against the shared constant rather than a literal copied into the test. A
// literal here would still pass on the day the two surfaces diverged.
func TestBothAdminSurfacesReportTheSameNoProviderReason(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	f.unmarkProvider(t)
	set := &fakeSettings{enabled: true}

	status := statusInertReason(t, newTestServer(t, f, set, NewAppDetails(false, quietLogger())))
	if status != reasonNoProviderApp {
		t.Errorf("GET status inert_reason = %q, want the shared reason %q", status, reasonNoProviderApp)
	}

	code, scan := postScan(t, newScanServer(t, f, set, 6*time.Hour), "")
	if code != http.StatusOK {
		t.Fatalf("force scan status %d, want 200 — an inert instance is a 200 with a reason, not a 4xx", code)
	}
	if scan.InertReason != reasonNoProviderApp {
		t.Errorf("force scan inert_reason = %q, want the shared reason %q", scan.InertReason, reasonNoProviderApp)
	}
	if scan.Queued != 0 {
		t.Errorf("queued %d with no provider app, want 0", scan.Queued)
	}
	if n := countT(t, pool, `SELECT count(*) FROM library_scans`); n != 0 {
		t.Errorf("scan rows written = %d, want 0", n)
	}
}

func statusInertReason(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp, err := http.Get(srv.URL + "/v1/admin/library/status")
	must(t, err)
	defer resp.Body.Close()
	var body struct {
		InertReason string `json:"inert_reason"`
	}
	must(t, json.NewDecoder(resp.Body).Decode(&body))
	return body.InertReason
}

// TestAppDetailsIsOffByDefaultAndNeverOverridesARule — §8.3's containment,
// asserted at the reconciler rather than over the network.
func TestAppDetailsNeverOverridesARule(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	// An operator has explicitly allowed a Valve tool. Even if the (opt-in)
	// appdetails rung says "not a game", the rule must win.
	_, err := f.store.SetRule(ctx, f.parent, SourceSteam, "1493710", RuleAllow, "", nil)
	must(t, err)

	_, err = f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host,
		[]ReportEntry{{ExternalID: "1493710", Name: "Proton Experimental"}},
		map[string]AppDetail{"1493710": {IsGame: false}}) // "Steam says: not a game"
	must(t, err)
	if _, enabled, ok := f.tile(t, "1493710"); !ok || !enabled {
		t.Fatal("the appdetails rung overrode an operator's allow rule; §8.3 forbids it")
	}

	// ...but it DOES suppress an appid the ladder reached by its default rung.
	_, err = f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host,
		[]ReportEntry{{ExternalID: "1493710", Name: "Proton Experimental"},
			{ExternalID: "999888", Name: "Some Soundtrack"}},
		map[string]AppDetail{"999888": {IsGame: false}})
	must(t, err)
	if _, _, ok := f.tile(t, "999888"); ok {
		t.Error("the appdetails rung did not suppress a non-game the ladder would have published")
	}

	// PublishableAppIDs is the set the lookup may be asked about, and an
	// operator-decided appid must not be in it.
	ids, err := f.store.PublishableAppIDs(ctx, f.parent, []ReportEntry{
		{ExternalID: "1493710", Name: "Proton Experimental"}, // allow rule -> excluded
		{ExternalID: "2180100", Name: "Proton Hotfix"},       // built-in suppress -> excluded
		{ExternalID: "517710", Name: "Redout"},               // default publish -> included
	})
	must(t, err)
	if len(ids) != 1 || ids[0] != "517710" {
		t.Errorf("PublishableAppIDs = %v, want [517710] only", ids)
	}
}

// TestAppDetailsDisabledMakesNoRequest — "with the setting off: no third-party
// calls", asserted by pointing the client at a server that records hits.
func TestAppDetailsDisabledMakesNoRequest(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"517710":{"success":true,"data":{"type":"game"}}}`))
	}))
	defer srv.Close()

	off := NewAppDetails(false, quietLogger())
	off.endpoint = srv.URL
	if got := off.Classify(context.Background(), []string{"517710"}); got != nil {
		t.Errorf("a disabled lookup returned %v, want nil", got)
	}
	if hits != 0 {
		t.Fatalf("a disabled lookup made %d third-party requests, want 0", hits)
	}

	on := NewAppDetails(true, quietLogger())
	on.endpoint = srv.URL
	got := on.Classify(context.Background(), []string{"517710"})
	if hits != 1 || !got["517710"] {
		t.Errorf("enabled lookup: hits=%d result=%v; want 1 hit and a 'game' verdict", hits, got)
	}
}

// --- admin-libraries amendment (2026-08-01): the status endpoint's resolved
// fields --------------------------------------------------------------------

// statusBody is the subset of GET /v1/admin/library/status this file asserts
// on for the amendment's new fields.
type statusBody struct {
	ScanIntervalSecs          float64      `json:"scan_interval_secs"`
	AppdetailsLookup          bool         `json:"appdetails_lookup"`
	IntervalOverriddenByEnv   bool         `json:"interval_overridden_by_env"`
	AppdetailsOverriddenByEnv bool         `json:"appdetails_overridden_by_env"`
	LastScanCompletedAt       *time.Time   `json:"last_scan_completed_at"`
	RecentScans               []RecentScan `json:"recent_scans"`
}

func getStatus(t *testing.T, srv *httptest.Server) statusBody {
	t.Helper()
	resp, err := http.Get(srv.URL + "/v1/admin/library/status")
	must(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status: %d, want 200", resp.StatusCode)
	}
	var body statusBody
	must(t, json.NewDecoder(resp.Body).Decode(&body))
	return body
}

// newStatusServer builds the library HTTP surface with an explicit resolver,
// so a test can force env overrides independently of the database column —
// newTestServer's resolver never overrides (see newTestResolver).
func newStatusServer(t *testing.T, f fixture, set *fakeSettings, resolver *Resolver) *httptest.Server {
	t.Helper()
	h := NewHandler(f.store, testStorageManager(f, set), set, NewAppDetails(false, quietLogger()), resolver, quietLogger())
	mux := http.NewServeMux()
	h.Register(mux, func(next http.Handler) http.Handler { return next })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestLibraryStatusReportsEnvOverrides — with both env vars set, the status
// endpoint must report the ENV values, not the database column's, and set
// both *_overridden_by_env flags — the UI's cue to grey the controls.
func TestLibraryStatusReportsEnvOverrides(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	set := &fakeSettings{enabled: true, intervalMinutes: 720, appDetailsEnabled: false}
	resolver := NewResolver(set, true, 5*time.Minute, true, true)
	srv := newStatusServer(t, f, set, resolver)

	body := getStatus(t, srv)
	if !body.IntervalOverriddenByEnv {
		t.Error("interval_overridden_by_env = false, want true (QUASAR_LIBRARY_SCAN_INTERVAL was set)")
	}
	if body.ScanIntervalSecs != (5 * time.Minute).Seconds() {
		t.Errorf("scan_interval_secs = %v, want the env value %v (not the database's 720 minutes)",
			body.ScanIntervalSecs, (5 * time.Minute).Seconds())
	}
	if !body.AppdetailsOverriddenByEnv {
		t.Error("appdetails_overridden_by_env = false, want true (QUASAR_STEAM_APPDETAILS_LOOKUP was set)")
	}
	if !body.AppdetailsLookup {
		t.Error("appdetails_lookup = false, want the env value true (not the database's false)")
	}
}

// TestLibraryStatusReportsDatabaseValuesWhenEnvUnset is the other direction:
// with neither env var set, the status endpoint reports the DATABASE column
// values and both overridden flags are false.
func TestLibraryStatusReportsDatabaseValuesWhenEnvUnset(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	set := &fakeSettings{enabled: true, intervalMinutes: 45, appDetailsEnabled: true}
	resolver := NewResolver(set, false, 0, false, false)
	srv := newStatusServer(t, f, set, resolver)

	body := getStatus(t, srv)
	if body.IntervalOverriddenByEnv {
		t.Error("interval_overridden_by_env = true, want false (env unset)")
	}
	if body.ScanIntervalSecs != (45 * time.Minute).Seconds() {
		t.Errorf("scan_interval_secs = %v, want the database value %v",
			body.ScanIntervalSecs, (45 * time.Minute).Seconds())
	}
	if body.AppdetailsOverriddenByEnv {
		t.Error("appdetails_overridden_by_env = true, want false (env unset)")
	}
	if !body.AppdetailsLookup {
		t.Error("appdetails_lookup = false, want the database value true")
	}
}

// TestLibraryStatusLastScanCompletedAt — null when nothing has ever
// completed, populated once a scan reaches a terminal state (reported).
func TestLibraryStatusLastScanCompletedAt(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	set := &fakeSettings{enabled: true}
	srv := newTestServer(t, f, set, NewAppDetails(false, quietLogger()))

	if body := getStatus(t, srv); body.LastScanCompletedAt != nil {
		t.Errorf("last_scan_completed_at = %v, want nil before any scan has completed", body.LastScanCompletedAt)
	}

	scanID := f.claimedScan(t, f.user)
	_, err := f.store.Reconcile(ctx, scanID, f.host,
		[]ReportEntry{{ExternalID: "517710", Name: "Redout"}}, nil)
	must(t, err)

	body := getStatus(t, srv)
	if body.LastScanCompletedAt == nil {
		t.Fatal("last_scan_completed_at = nil, want a timestamp after a scan reported")
	}
	// The lower bound tolerates DATABASE-vs-PROCESS clock skew rather than
	// asserting the two agree. reported_at is stamped by Postgres (now()),
	// while time.Since reads this process's clock — two different machines in
	// production, and two different clocks even locally: the ephemeral test
	// Postgres in Docker measures a consistent ~80ms AHEAD of the macOS host.
	// A bare `since < 0` therefore fails whenever the assertion runs within
	// that skew of the write, which is a race decided by machine load, and it
	// made this test flaky (~1 run in 6). The assertion's real intent is
	// "recent, not an old row", so only the upper bound is load-bearing.
	const skewTolerance = 5 * time.Second
	if since := time.Since(*body.LastScanCompletedAt); since < -skewTolerance || since > time.Minute {
		t.Errorf("last_scan_completed_at = %v (since=%v), want roughly now",
			*body.LastScanCompletedAt, since)
	}
}

// TestLibraryStatusRecentScans is the scan-observability amendment's headline
// admin-surface gate: recent_scans lists terminal scans newest first, with
// the user/host names and the stored per-outcome counts, and a scan from
// before migration 0048 existed (a hand-inserted terminal row with the
// column defaults) renders as zeros rather than being excluded or erroring.
func TestLibraryStatusRecentScans(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	set := &fakeSettings{enabled: true}
	srv := newTestServer(t, f, set, NewAppDetails(false, quietLogger()))

	// A successful scan, whose counts must round-trip exactly.
	okScan := f.claimedScan(t, f.user)
	res, err := f.store.Reconcile(ctx, okScan, f.host,
		[]ReportEntry{{ExternalID: "517710", Name: "Redout"}}, nil)
	must(t, err)

	// A failed scan, reported after the successful one so newest-first has
	// something to order.
	failedScan := f.claimedScan(t, f.user)
	must(t, f.store.MarkFailed(ctx, failedScan, f.host, "walk refused"))

	// A "pre-0048-style" row: a terminal scan whose counts were never
	// computed by Reconcile (the migration's DEFAULT 0, exactly what a row
	// written before 0048 existed reads today).
	var legacyScan string
	must(t, pool.QueryRow(ctx, `
		INSERT INTO library_scans (user_id, app_id, host_id, state, reported_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'reported', now() - interval '10 minutes')
		RETURNING id::text`, f.user, f.parent, f.host).Scan(&legacyScan))

	body := getStatus(t, srv)
	if len(body.RecentScans) != 3 {
		t.Fatalf("recent_scans = %d entries, want 3", len(body.RecentScans))
	}

	// Newest first: failedScan and okScan both stamp reported_at ~now (via
	// Reconcile/MarkFailed), legacyScan is backdated 10 minutes, so it must
	// sort last regardless of insertion order.
	last := body.RecentScans[len(body.RecentScans)-1]
	if last.State != "reported" || last.Observed != 0 || last.Suppressed != 0 ||
		last.Created != 0 || last.Disabled != 0 || last.Granted != 0 ||
		last.Revoked != 0 || last.Rejected != 0 || last.Backfilled != 0 {
		t.Errorf("oldest (pre-0048-style) entry = %+v, want state=reported and every count 0", last)
	}

	var gotOK, gotFailed *RecentScan
	for i := range body.RecentScans[:2] {
		r := &body.RecentScans[i]
		switch r.State {
		case "reported":
			gotOK = r
		case "failed":
			gotFailed = r
		}
	}
	if gotOK == nil {
		t.Fatal("no 'reported' entry among the two newest recent_scans")
	}
	if gotOK.User != "lib-a" || gotOK.Host != f.nodeName {
		t.Errorf("reported entry user/host = %q/%q, want %q/%q", gotOK.User, gotOK.Host, "lib-a", f.nodeName)
	}
	if gotOK.Observed != res.Observed || gotOK.Created != res.Created || gotOK.Granted != res.Granted {
		t.Errorf("reported entry counts = %+v, want them matching the Reconcile result %+v", gotOK, res)
	}
	if gotOK.Error != "" {
		t.Errorf("reported entry error = %q, want empty", gotOK.Error)
	}

	if gotFailed == nil {
		t.Fatal("no 'failed' entry among the two newest recent_scans")
	}
	if gotFailed.Error != "walk refused" {
		t.Errorf("failed entry error = %q, want %q", gotFailed.Error, "walk refused")
	}
	if gotFailed.Observed != 0 || gotFailed.Backfilled != 0 {
		t.Errorf("failed entry counts = %+v, want every count 0", gotFailed)
	}
}
