package session

// migration_0036_db_test.go — the two instruments that buy down UI-P4's risk.
//
// THEY ARE DIFFERENT TESTS AND PROVE DIFFERENT THINGS.
//
//   * TestMigration0036RoundTrip proves the migration is REVERSIBLE: a
//     deterministic full-row dump of every affected table, taken before `up` and
//     after `down`, is byte-identical.
//   * TestMigration0036BehaviourNeutrality proves the migration is INVISIBLE:
//     the effective (width, height, fps, bitrate, codec, abr_floor, playout0)
//     tuple is unchanged for the full cross-product of (app × probe × host).
//
// Conflating them is how a "green up/down test" ships a quality regression.
//
// Both run against a TOWER-SHAPED dataset in a SCRATCH database, never the
// shared test database — the round trip migrates DOWN, and doing that to a
// database other tests share would be destructive. An EMPTY database
// round-trips trivially and has no apps, so the neutrality diff would be
// vacuously green; that is why the dataset is seeded first.
//
// DEVIATION FROM THE RE-SPEC, STATED RATHER THAN HIDDEN: §6.1 asks for
// `pg_dump --data-only`. This uses Postgres' own composite-row-to-text rendering
// (`SELECT t::text FROM tbl t ORDER BY 1`) instead. It is the same instrument —
// a canonical, byte-comparable textual rendering of every column of every row,
// including columns that the migration adds or drops — and it does not require a
// pg_dump binary on the machine running `go test`, which the developer laptop
// this runs on does not reliably have.

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/accreleus/quasar/control-plane/internal/profile"
	"github.com/accreleus/quasar/control-plane/migrations"
)

// version35 is the last migration before UI-P4; version36 is UI-P4's expand half.
const (
	version35 = 35
	version36 = 36
)

// affectedTables is the set §6.1 names. `sessions` is included because 0036 adds
// a column to it, and a dropped-and-re-added column is exactly the kind of thing
// a table-level dump catches and a column-level one does not.
var affectedTables = []string{
	"stream_profiles", "stream_profile_policy", "user_profile_preferences", "apps", "sessions",
}

// scratchDB creates a throwaway database on the same server as TEST_DATABASE_URL
// and returns its URL. It is dropped on cleanup.
func scratchDB(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	name := fmt.Sprintf("quasar_m0036_%d_%d", time.Now().UnixNano()%1e9, rand.Intn(1000)) //nolint:gosec — test fixture naming

	adminURL, err := replaceDBName(base, "postgres")
	if err != nil {
		t.Fatalf("derive admin url: %v", err)
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("connect to the postgres database: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create scratch database %s: %v\n"+
			"This test needs CREATEDB on the TEST_DATABASE_URL role: it migrates DOWN, which must never "+
			"happen to a database other tests share.", name, err)
	}
	t.Cleanup(func() {
		admin2, err := sql.Open("pgx", adminURL)
		if err != nil {
			return
		}
		defer admin2.Close()
		_, _ = admin2.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})

	url, err := replaceDBName(base, name)
	if err != nil {
		t.Fatalf("derive scratch url: %v", err)
	}
	return url
}

// replaceDBName swaps the database name in a postgres URL, keeping everything
// else (credentials, host, query parameters) intact.
func replaceDBName(rawURL, name string) (string, error) {
	i := strings.LastIndex(rawURL, "/")
	if i < 0 {
		return "", fmt.Errorf("no path in %q", rawURL)
	}
	rest := rawURL[i+1:]
	query := ""
	if q := strings.Index(rest, "?"); q >= 0 {
		query = rest[q:]
	}
	return rawURL[:i+1] + name + query, nil
}

// newMigrator builds a golang-migrate instance over the embedded migrations, so
// the test can step to a specific version. Production code only ever calls
// migrate.Run (all the way up); stepping stays here rather than widening the
// production API for a test's benefit.
func newMigrator(t *testing.T, url string) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open migrations db: %v", err)
	}
	driver, err := pgxdriver.WithInstance(db, &pgxdriver.Config{MigrationsTable: "schema_migrations"})
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "quasar", driver)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	return m
}

func migrateTo(t *testing.T, m *migrate.Migrate, version uint) {
	t.Helper()
	if err := m.Migrate(version); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to %d: %v", version, err)
	}
}

// dumpTables renders every row of every named table as canonical text, ordered
// deterministically, so two dumps are byte-comparable.
func dumpTables(t *testing.T, pool *pgxpool.Pool, tables []string) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder
	for _, tbl := range tables {
		b.WriteString("== " + tbl + "\n")
		rows, err := pool.Query(ctx, fmt.Sprintf(`SELECT t::text FROM %s t`, tbl))
		if err != nil {
			t.Fatalf("dump %s: %v", tbl, err)
		}
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatalf("scan %s: %v", tbl, err)
			}
			lines = append(lines, line)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate %s: %v", tbl, err)
		}
		sort.Strings(lines) // stable ordering, per §6.1
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

// --- the Tower-shaped dataset ------------------------------------------------

type prodFixture struct {
	userNoPref  string
	userWithPre string
	apps        []prodApp
}

type prodApp struct {
	id     string
	name   string
	policy string
	pinned *string
}

// seedProdShaped builds the dataset §6.1/§7 require, at migration version 35.
//
// The three codec-list shapes the fan-out rule has to handle are ALL present, so
// neither test can pass by accident on a uniform database:
//
//   - ONE MATERIALISED list with several launchable entries (1440p60), with h264
//     FIRST — today's stored order, and the case where reordering during the
//     migration would silently flip an AV1-enabled host's behaviour.
//   - SEVERAL NULLs (every other profile) — the Tower/ship-dark state, which
//     resolves to the in-code default and yields exactly one h264 rung.
//   - ONE list with ZERO launchable entries (720p60) — which must still
//     synthesise an h264 floor rung, because that profile streams h264 today via
//     the resolver's guaranteed fallback.
//   - ONE list that is NON-EMPTY after filtering but holds NO launchable h264
//     (4k120: h264 unsupported, av1 launchable) — the fan-out gap. Its filtered
//     list is not empty, so the zero-launchable clause does not fire, and without
//     the append clause the chain's only rung is AV1: the resolver's floor
//     degenerates to rungs[len-1] and dispatches AV1 to a host that may not
//     encode it, while write validation's h264 floor rule 400s every PATCH, so
//     the chain can never be repaired through the API.
func seedProdShaped(t *testing.T, pool *pgxpool.Pool) prodFixture {
	t.Helper()
	ctx := context.Background()
	var f prodFixture

	if _, err := pool.Exec(ctx, `
		UPDATE stream_profiles SET codecs = '[{"codec":"h264","status":"launchable"},
		                                      {"codec":"hevc","status":"launchable"},
		                                      {"codec":"av1","status":"launchable"}]'::jsonb
		 WHERE id = '1440p60';
		UPDATE stream_profiles SET codecs = '[{"codec":"hevc","status":"future"},
		                                      {"codec":"av1","status":"unsupported"}]'::jsonb
		 WHERE id = '720p60';
		UPDATE stream_profiles SET codecs = '[{"codec":"h264","status":"unsupported"},
		                                      {"codec":"av1","status":"launchable"}]'::jsonb
		 WHERE id = '4k120';
	`); err != nil {
		t.Fatalf("seed codec lists: %v", err)
	}

	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('nopref@test.local','nopref','x') RETURNING id::text`).Scan(&f.userNoPref))
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('withpref@test.local','withpref','x') RETURNING id::text`).Scan(&f.userWithPre))

	// One user pins a preference; the global default stays NULL (Tower's state).
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_profile_preferences (user_id, default_profile_id)
		VALUES ($1::uuid, '1080p60')`, f.userWithPre); err != nil {
		t.Fatalf("seed user preference: %v", err)
	}

	pin := func(s string) *string { return &s }
	specs := []prodApp{
		{name: "inherit-app", policy: "inherit"},
		{name: "prefer-app", policy: "prefer", pinned: pin("1440p60")},
		{name: "force-app", policy: "force", pinned: pin("720p60")},
		{name: "inherit-with-ignored-pin", policy: "inherit", pinned: pin("4k60")},
	}
	for _, a := range specs {
		var id string
		must(t, pool.QueryRow(ctx, `INSERT INTO apps
			(name, default_vram_mb, default_encode_slots, default_width, default_height,
			 default_fps, default_bitrate_kbps, profile_policy, default_profile_id)
			VALUES ($1, 1024, 1, 1920, 1080, 60, 15000, $2, $3) RETURNING id::text`,
			a.name, a.policy, a.pinned).Scan(&id))
		a.id = id
		f.apps = append(f.apps, a)
	}

	// A host + GPU + a couple of sessions, so the `sessions` dump is non-empty and
	// the added/dropped stream_profile_id column is actually exercised.
	var hostID, gpuID string
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, capacity_detection)
		VALUES ('host-a','online','ok') RETURNING id::text`).Scan(&hostID))
	must(t, pool.QueryRow(ctx, `INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
		VALUES ($1, 0, 24576, 8) RETURNING id::text`, hostID).Scan(&gpuID))
	for i, pid := range []string{"1080p60", "1440p60"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO sessions (user_id, app_id, host_id, gpu_id, state,
			    width, height, fps, bitrate_kbps, h264_profile, profile_id, playout0_ms, reserved_encode_slots)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'stopped',
			        1920, 1080, 60, 12000, 'constrained-baseline', $5, 50, 0)`,
			f.userNoPref, f.apps[i].id, hostID, gpuID, pid); err != nil {
			t.Fatalf("seed session: %v", err)
		}
	}
	return f
}

// --- the round trip ----------------------------------------------------------

// TestMigration0036RoundTrip proves 0036 is reversible against a Tower-shaped
// dataset: the dump before `up` and after `down` must be byte-identical.
//
// An EMPTY database round-trips trivially and proves nothing, which is why the
// fixture is seeded first and why the codec-list shapes above are deliberately
// varied — the fan-out is LOSSY (a single h264 rung cannot be distinguished from
// "codecs was NULL" versus "codecs was the default list stored explicitly", and
// future/unsupported entries leave no trace at all), so a computed collapse
// would silently reconstruct the wrong rows and this dump is what catches it.
func TestMigration0036RoundTrip(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, version35)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()

	seedProdShaped(t, pool)
	before := dumpTables(t, pool, affectedTables)

	migrateTo(t, m, version36)

	// Sanity: the migration actually did something, so a no-op cannot pass.
	var rungs, chains int
	if err := pool.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM launch_profile_rungs), (SELECT count(*) FROM launch_profiles)`).
		Scan(&rungs, &chains); err != nil {
		t.Fatalf("post-up counts: %v", err)
	}
	if chains != 8 {
		t.Fatalf("post-up launch profiles = %d, want 8", chains)
	}
	// 5 NULL-codecs profiles ⇒ 1 rung each, 1440p60's materialised 3-launchable
	// list ⇒ 3 rungs, 720p60's zero-launchable list ⇒ 1 synthesised h264 floor,
	// 4k120's h264-unsupported+av1-launchable list ⇒ 2 (the av1 rung + the
	// APPENDED h264 floor).
	if rungs != 5+3+1+2 {
		t.Fatalf("post-up rungs = %d, want 11 (5 NULL + 3 materialised + 1 synthesised floor + 2 appended-floor)", rungs)
	}

	migrateTo(t, m, version35)
	after := dumpTables(t, pool, affectedTables)

	if before != after {
		t.Fatalf("round trip is NOT byte-identical.\n--- before up ---\n%s\n--- after down ---\n%s",
			before, after)
	}
}

// TestMigration0036FanOutRule asserts the three load-bearing halves of the
// fan-out rule directly, at the SQL level, against the same dataset.
func TestMigration0036FanOutRule(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, version35)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()
	seedProdShaped(t, pool)
	migrateTo(t, m, version36)

	store := NewStore(pool)
	ctx := context.Background()

	// (1) NULL codecs ⇒ EXACTLY ONE h264 rung. This is the shipped state.
	lp, err := store.GetLaunchProfile(ctx, "1080p60")
	if err != nil {
		t.Fatalf("get 1080p60: %v", err)
	}
	if got := rungIDs(lp.Rungs); len(got) != 1 || got[0] != "1080p60-h264" {
		t.Errorf("NULL-codecs fan-out = %v, want [1080p60-h264]", got)
	}

	// (2) A materialised list fans out one rung per LAUNCHABLE entry in STORED
	//     ORDER — INCLUDING h264 first. Reordering h264 to last here would flip an
	//     AV1-enabled host+client from h264 to AV1 inside a supposedly
	//     behaviour-neutral migration.
	lp, err = store.GetLaunchProfile(ctx, "1440p60")
	if err != nil {
		t.Fatalf("get 1440p60: %v", err)
	}
	want := []string{"1440p60-h264", "1440p60-hevc", "1440p60-av1"}
	if got := rungIDs(lp.Rungs); !equalStrings(got, want) {
		t.Errorf("materialised fan-out = %v, want %v (stored order, verbatim)", got, want)
	}

	// (3) ZERO launchable entries still synthesise an h264 floor rung — that
	//     profile streams h264 today via the resolver's guaranteed fallback, and a
	//     rung-less chain would turn that silent, correct fallback into a launch
	//     with nothing to dispatch.
	lp, err = store.GetLaunchProfile(ctx, "720p60")
	if err != nil {
		t.Fatalf("get 720p60: %v", err)
	}
	if got := rungIDs(lp.Rungs); len(got) != 1 || got[0] != "720p60-h264" {
		t.Errorf("zero-launchable fan-out = %v, want the synthesised [720p60-h264]", got)
	}

	// (3b) A NON-EMPTY launchable list with NO h264 gets the floor APPENDED — the
	//      gap the zero-launchable clause does not cover. Order matters twice: the
	//      av1 rung keeps its stored position (behaviour neutrality), and the
	//      floor lands LAST, which is where resolveRung's unconditional fallback
	//      looks for it. Without this the chain's only rung is av1: the floor
	//      degenerates to rungs[len-1] and dispatches av1 to a host that may not
	//      encode it, and write validation 400s every PATCH, so it can never be
	//      repaired through the API.
	lp, err = store.GetLaunchProfile(ctx, "4k120")
	if err != nil {
		t.Fatalf("get 4k120: %v", err)
	}
	wantAppended := []string{"4k120-av1", "4k120-h264"}
	if got := rungIDs(lp.Rungs); !equalStrings(got, wantAppended) {
		t.Errorf("no-launchable-h264 fan-out = %v, want %v (stored order, then the APPENDED h264 floor)",
			got, wantAppended)
	}

	// (4) The global default is deliberately NOT populated.
	policy, err := store.GetProfilePolicy(ctx)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if policy.GlobalDefaultProfileID != nil {
		t.Errorf("global_default_profile_id = %v, want NULL — 0036 must not change every inherit app's resolution in one invisible step",
			*policy.GlobalDefaultProfileID)
	}

	// (5) The legacy rows and the codecs column SURVIVE (expand/contract).
	var legacy int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM stream_profiles WHERE codec IS NULL`).Scan(&legacy); err != nil {
		t.Fatalf("count legacy: %v", err)
	}
	if legacy != 8 {
		t.Errorf("legacy stream_profiles rows = %d, want 8 — a code-level revert reads exactly these", legacy)
	}
}

// TestMigration0036RefusesCustomApps: the `custom` gate fails the migration and
// names the offending apps rather than guessing a conversion. `custom` cannot be
// converted behaviour-neutrally — such an app resolves NO profile today and
// lands on the legacy tier path, where the effective settings are
// min(tier from the probe, the app defaults), not simply the app defaults.
func TestMigration0036RefusesCustomApps(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, version35)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()
	seedProdShaped(t, pool)

	if _, err := pool.Exec(context.Background(),
		`INSERT INTO apps (name, profile_policy) VALUES ('legacy-custom-app', 'custom')`); err != nil {
		t.Fatalf("seed custom app: %v", err)
	}

	err = m.Migrate(version36)
	if err == nil {
		t.Fatal("0036 applied with a profile_policy='custom' app present; it must refuse")
	}
	if !strings.Contains(err.Error(), "legacy-custom-app") {
		t.Errorf("migration error must NAME the offending apps so an operator can act; got: %v", err)
	}

	// The transaction rolled back: nothing was applied.
	var launchProfiles int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'launch_profiles'`).Scan(&launchProfiles); err != nil {
		t.Fatalf("check rollback: %v", err)
	}
	if launchProfiles != 0 {
		t.Error("launch_profiles exists after a refused migration; the up migration must be atomic")
	}
}

// --- behaviour neutrality -----------------------------------------------------

// effective is the tuple §7 diffs. playout0 and abr_floor are in it DELIBERATELY:
// they are the two values the deleted in-code catalog corrupted, and they are
// invisible in a resolution-only diff.
type effective struct {
	ProfileID   string
	Width       int32
	Height      int32
	FPS         int32
	BitrateKbps int32
	Codec       string
	ABRFloor    int32
	Playout0Ms  int32
	Rejected    bool // eligibility refused the launch
}

func (e effective) String() string {
	if e.Rejected {
		return "REJECTED"
	}
	return fmt.Sprintf("%s %dx%d@%d %dkbps codec=%s floor=%d playout0=%d",
		e.ProfileID, e.Width, e.Height, e.FPS, e.BitrateKbps, e.Codec, e.ABRFloor, e.Playout0Ms)
}

// probeCase is one column of the probe matrix.
type probeCase struct {
	name string
	// nil ⇒ the no-probe path.
	probe *DeviceProbe
}

func probeMatrix() []probeCase {
	return []probeCase{
		{"no-probe", nil},
		{"full-capability", &DeviceProbe{BandwidthKbps: 200000, RTTMs: 5, MaxDecodeHeight: 2160, DisplayRefreshHz: 144, HEVC: true, AV1: true}},
		{"1080p-decode-capped", &DeviceProbe{BandwidthKbps: 200000, RTTMs: 5, MaxDecodeHeight: 1080, DisplayRefreshHz: 60, HEVC: true, AV1: true}},
		{"low-bandwidth", &DeviceProbe{BandwidthKbps: 5000, RTTMs: 30, MaxDecodeHeight: 1080, HEVC: true, AV1: true}},
		{"high-rtt", &DeviceProbe{BandwidthKbps: 200000, RTTMs: 90, MaxDecodeHeight: 2160, DisplayRefreshHz: 144, HEVC: true, AV1: true}},
		{"no-h265", &DeviceProbe{BandwidthKbps: 200000, RTTMs: 5, MaxDecodeHeight: 2160, DisplayRefreshHz: 144, HEVC: false, AV1: true}},
		{"no-av1", &DeviceProbe{BandwidthKbps: 200000, RTTMs: 5, MaxDecodeHeight: 2160, DisplayRefreshHz: 144, HEVC: true, AV1: false}},
	}
}

// hostMatrix is one entry per encoder family; placement is stubbed to these.
func hostMatrix() []struct {
	name   string
	codecs []string
} {
	return []struct {
		name   string
		codecs []string
	}{
		{"h264-only", []string{"h264"}},
		{"h264+h265", []string{"h264", "h265"}},
		{"h264+h265+av1", []string{"h264", "h265", "av1"}},
	}
}

// TestMigration0036BehaviourNeutrality is §7: the effective settings for every
// (app × probe × host) cell must be IDENTICAL before and after 0036.
//
// The "before" side is an INDEPENDENT ORACLE (legacyEffective, below) written
// against the pre-UI-P4 algorithm and reading the pre-0036 tables. It is
// deliberately NOT the code under test — the pre-0036 implementation no longer
// exists, and re-using any part of the new resolver as its own reference would
// make the diff vacuous. The "after" side drives the REAL post-0036 store and
// resolver.
//
// The round trip proves the migration is reversible. This proves it is
// invisible. They are different properties.
func TestMigration0036BehaviourNeutrality(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, version35)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()
	f := seedProdShaped(t, pool)
	store := NewStore(pool)
	ctx := context.Background()

	users := []struct {
		name string
		id   string
	}{
		{"user-no-preference", f.userNoPref},
		{"user-with-preference", f.userWithPre},
	}

	// --- BEFORE: the independent oracle over the pre-0036 tables. -------------
	legacyRows, err := legacyStreamProfiles(ctx, pool)
	if err != nil {
		t.Fatalf("read pre-0036 stream profiles: %v", err)
	}
	before := map[string]effective{}
	for _, app := range f.apps {
		for _, u := range users {
			for _, pc := range probeMatrix() {
				for _, hc := range hostMatrix() {
					key := app.name + "|" + u.name + "|" + pc.name + "|" + hc.name
					before[key] = legacyEffective(t, ctx, pool, legacyRows, app, u.id, pc.probe, hc.codecs)
				}
			}
		}
	}
	if len(before) == 0 {
		t.Fatal("the cross-product is empty — the diff would be vacuously green")
	}

	// --- apply 0036 ----------------------------------------------------------
	migrateTo(t, m, version36)

	// --- AFTER: the real post-0036 store + resolver. --------------------------
	after := map[string]effective{}
	for _, app := range f.apps {
		for _, u := range users {
			for _, pc := range probeMatrix() {
				for _, hc := range hostMatrix() {
					key := app.name + "|" + u.name + "|" + pc.name + "|" + hc.name
					after[key] = postEffective(t, ctx, store, app, u.id, pc.probe, hc.codecs)
				}
			}
		}
	}

	keys := make([]string, 0, len(before))
	for k := range before {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	diffs := 0
	for _, k := range keys {
		if before[k] != after[k] {
			diffs++
			t.Errorf("NEUTRALITY DIFF %s\n  before: %s\n   after: %s", k, before[k], after[k])
		}
	}
	if diffs == 0 {
		t.Logf("behaviour neutrality: %d cells, 0 diffs", len(keys))
	}
}

// legacyStreamProfile is one pre-0036 row plus its codec list.
type legacyStreamProfile struct {
	p      profile.Profile
	codecs []profile.CodecPref
}

func legacyStreamProfiles(ctx context.Context, pool *pgxpool.Pool) ([]legacyStreamProfile, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, display_name, width, height, fps, h264_profile,
		       nominal_bitrate_kbps, min_offer_bandwidth_kbps, recommended_offer_bandwidth_kbps,
		       headroom_factor, abr_floor_kbps, max_startup_rtt_ms, min_decode_height,
		       high_refresh_display, hardware_encoder_required, browser_client, playout0_ms,
		       visibility, codecs
		FROM stream_profiles
		ORDER BY sort_order ASC, height DESC, fps DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []legacyStreamProfile
	for rows.Next() {
		var p profile.Profile
		var highRefresh, browserClient, visibility string
		var codecsRaw []byte
		if err := rows.Scan(&p.ID, &p.DisplayName, &p.Width, &p.Height, &p.FPS, &p.H264Profile,
			&p.NominalBitrateKbps, &p.MinOfferBandwidthKbps, &p.RecommendedOfferBandwidthKbps,
			&p.HeadroomFactor, &p.ABRFloorKbps, &p.MaxStartupRTTMs, &p.MinDecodeHeight,
			&highRefresh, &p.HardwareEncoderRequired, &browserClient, &p.Playout0Ms,
			&visibility, &codecsRaw); err != nil {
			return nil, err
		}
		p.HighRefreshDisplay = profile.DisplayReq(highRefresh)
		p.BrowserClient = profile.BrowserSupport(browserClient)
		p.Visibility = profile.Visibility(visibility)
		out = append(out, legacyStreamProfile{p: p, codecs: mergeProfileCodecs(codecsRaw)})
	}
	return out, rows.Err()
}

// legacyEffective is the ORACLE: the pre-UI-P4 algorithm, reimplemented here
// against the pre-0036 tables. Every step below is the old behaviour, written
// out rather than called, precisely so that the diff has something independent
// to compare against.
func legacyEffective(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	rows []legacyStreamProfile, app prodApp, userID string,
	dp *DeviceProbe, hostCodecs []string,
) effective {
	t.Helper()

	in := profile.EvalInput{}
	if dp != nil {
		in.Probe = &profile.Probe{
			BandwidthKbps:    dp.BandwidthKbps,
			RTTMs:            dp.RTTMs,
			MaxDecodeHeight:  dp.MaxDecodeHeight,
			DisplayRefreshHz: dp.DisplayRefreshHz,
		}
	}

	// OLD STEP 1: evaluate the user-facing catalog and pick a recommendation —
	// highest fully eligible in catalog order; with no probe, the conservative
	// default (1080p60); otherwise the lowest-demand entry, low confidence.
	type verdict struct {
		id    string
		elig  profile.Eligibility
		valid bool
	}
	var verdicts []verdict
	for _, r := range rows {
		if r.p.Visibility != profile.VisibilityUser {
			continue
		}
		// The old engine's codec check used the profile's first LAUNCHABLE codec;
		// the probe matrix never sets Probe.Codecs, so it never fired. Reproduced
		// here as a no-op for the same reason.
		pe := profile.EvaluateProfile(r.p, in)
		verdicts = append(verdicts, verdict{r.p.ID, pe.Eligibility, true})
	}
	recommended := ""
	if dp == nil {
		for _, v := range verdicts {
			if v.id == "1080p60" {
				recommended = "1080p60"
			}
		}
	} else {
		for _, v := range verdicts {
			if v.elig == profile.EligibilityEligible {
				recommended = v.id
				break
			}
		}
	}
	if recommended == "" && len(verdicts) > 0 {
		recommended = verdicts[len(verdicts)-1].id
	}

	// OLD STEP 2: ResolveDefaultProfile — app pin (prefer/force) → user preference
	// → global default → recommendation.
	profileID := ""
	switch {
	case (app.policy == "prefer" || app.policy == "force") && app.pinned != nil:
		profileID = *app.pinned
	default:
		var pref *string
		if err := pool.QueryRow(ctx,
			`SELECT default_profile_id FROM user_profile_preferences WHERE user_id::text = $1`, userID).Scan(&pref); err == nil && pref != nil {
			profileID = *pref
		}
		if profileID == "" {
			var global *string
			if err := pool.QueryRow(ctx,
				`SELECT global_default_profile_id FROM stream_profile_policy WHERE id = true`).Scan(&global); err == nil && global != nil {
				profileID = *global
			}
		}
		if profileID == "" {
			profileID = recommended
		}
	}

	var sel legacyStreamProfile
	for _, r := range rows {
		if r.p.ID == profileID {
			sel = r
		}
	}
	if sel.p.ID == "" {
		t.Fatalf("oracle: resolved profile %q not in the pre-0036 catalog", profileID)
	}

	// OLD STEP 3: the eligibility gate (non-admin, no override).
	if profile.EvaluateProfile(sel.p, in).Eligibility == profile.EligibilityIneligible {
		return effective{Rejected: true}
	}

	// OLD STEP 4: the profile's concrete values, then the SPT-07 envelope.
	e := effective{
		ProfileID: sel.p.ID, Width: sel.p.Width, Height: sel.p.Height, FPS: sel.p.FPS,
		BitrateKbps: sel.p.NominalBitrateKbps, ABRFloor: sel.p.ABRFloorKbps, Playout0Ms: sel.p.Playout0Ms,
	}
	env := buildProbeEnvelope(dp)
	if env.SafeCeilingKbps > 0 {
		e.BitrateKbps = applyEnvelopeToBitrate(e.BitrateKbps, env)
	}
	if env.Playout0BumpMs > 0 {
		e.Playout0Ms = applyEnvelopeToPlayout0(e.Playout0Ms, env)
	}

	// OLD STEP 5: resolveCodec — first launchable candidate surviving the host and
	// device clamps, with a guaranteed h264 floor.
	e.Codec = wireCodecH264
	host := map[string]bool{}
	for _, c := range hostCodecs {
		host[c] = true
	}
	if len(host) == 0 {
		host[wireCodecH264] = true
	}
	for _, cp := range sel.codecs {
		if cp.Status != profile.CodecLaunchable {
			continue
		}
		wire, ok := catalogToWire(cp.Codec)
		if !ok || !host[wire] {
			continue
		}
		switch wire {
		case wireCodecH265:
			if dp == nil || !dp.HEVC {
				continue
			}
		case wireCodecAV1:
			if dp == nil || !dp.AV1 {
				continue
			}
		}
		e.Codec = wire
		break
	}
	return e
}

// postEffective drives the REAL post-0036 code over the same cell.
func postEffective(
	t *testing.T, ctx context.Context, store *Store,
	app prodApp, userID string, dp *DeviceProbe, hostCodecs []string,
) effective {
	t.Helper()

	in := profile.EvalInput{}
	if dp != nil {
		in.Probe = &profile.Probe{
			BandwidthKbps:    dp.BandwidthKbps,
			RTTMs:            dp.RTTMs,
			MaxDecodeHeight:  dp.MaxDecodeHeight,
			DisplayRefreshHz: dp.DisplayRefreshHz,
		}
	}

	catalog, err := store.ListLaunchProfiles(ctx, true)
	if err != nil {
		t.Fatalf("list launch profiles: %v", err)
	}
	ev := profile.EvaluateLaunchProfiles(catalog, in)

	launchApp := LaunchApp{ProfilePolicy: app.policy, DefaultProfileID: app.pinned}
	// UI-P5: the zero AppProfileRestriction is "unrestricted", which is the
	// behaviour this neutrality harness is measuring — no app in the 0036 fixture
	// has an allow-list, and this must keep reproducing the pre-UI-P5 numbers.
	profileID, err := store.ResolveDefaultProfile(ctx, userID, launchApp, ev.RecommendedID, AppProfileRestriction{})
	if err != nil {
		t.Fatalf("resolve default profile: %v", err)
	}
	lp, err := store.GetLaunchProfile(ctx, profileID)
	if err != nil {
		t.Fatalf("get launch profile %q: %v", profileID, err)
	}

	if profile.EvaluateLaunchProfile(lp, in).Eligibility == profile.EligibilityIneligible {
		return effective{Rejected: true}
	}

	// Pre-schedule: the TOP rung, with the envelope applied before admission.
	top := lp.Rungs[0]
	env := buildProbeEnvelope(dp)
	bitrate := top.NominalBitrateKbps
	if env.SafeCeilingKbps > 0 {
		bitrate = applyEnvelopeToBitrate(bitrate, env)
	}

	// Post-placement: resolve the rung, then RE-APPLY the envelope to its bitrate.
	rung, _, err := resolveRung(lp.Rungs, hostCodecs, hostEncoderCaps{}, dp, nil, StreamOverride{})
	if err != nil {
		t.Fatalf("resolve rung: %v", err)
	}
	bitrate = rung.NominalBitrateKbps
	if env.SafeCeilingKbps > 0 {
		bitrate = applyEnvelopeToBitrate(bitrate, env)
	}
	playout0 := rung.Playout0Ms
	if env.Playout0BumpMs > 0 {
		playout0 = applyEnvelopeToPlayout0(playout0, env)
	}
	codec, ok := catalogToWire(rung.Codec)
	if !ok {
		codec = wireCodecH264
	}
	return effective{
		ProfileID: lp.ID, Width: rung.Width, Height: rung.Height, FPS: rung.FPS,
		BitrateKbps: bitrate, Codec: codec, ABRFloor: rung.ABRFloorKbps, Playout0Ms: playout0,
	}
}
