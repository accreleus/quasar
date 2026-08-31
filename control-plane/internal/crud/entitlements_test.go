package crud

// entitlements_test.go — steam-library-discovery Phase 2 (spec §6, §13 "Gate 2").
//
// THIS FILE IS THE DELIVERABLE AS MUCH AS THE CODE IS. Two of the spec's risk
// rows describe Phase 2 failing in ways that pass every other automated gate:
// an empty entitlements table emptying every library, and a duplicate
// entitlement corrupting an offset page boundary. Neither is caught by any test
// that existed before this file.
//
// The three tests Gate 2 names by name are:
//   - TestMigration0043BackfillIsNeutralPerUser (item 1, first clause)
//   - TestDuplicateEntitlementPagesWithoutDuplicatesOrSkips (item 1, second)
//   - the direct POST /v1/sessions rejection lives in
//     internal/session/entitlement_launch_db_test.go, because that is where the
//     scheduling transaction it proves is enforced.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/accreleus/quasar/control-plane/migrations"
)

// version42 is the last migration BEFORE entitlements; version43 is this phase.
const (
	version42 = 42
	version43 = 43
)

// --- scratch-database harness -------------------------------------------------
//
// The neutrality and constraint tests migrate a database step by step (and the
// round-trip migrates DOWN), which must never happen to the database every other
// package shares under `-p 1`. Each gets its own throwaway database.

func entScratchDB(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	name := fmt.Sprintf("quasar_ent43_%d_%d", time.Now().UnixNano()%1e9, rand.Intn(1000)) //nolint:gosec — test fixture naming

	adminURL, err := entReplaceDBName(base, "postgres")
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
			"This test needs CREATEDB on the TEST_DATABASE_URL role: it migrates step by step "+
			"(and down), which must never happen to a database other tests share.", name, err)
	}
	t.Cleanup(func() {
		admin2, err := sql.Open("pgx", adminURL)
		if err != nil {
			return
		}
		defer admin2.Close()
		_, _ = admin2.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})

	url, err := entReplaceDBName(base, name)
	if err != nil {
		t.Fatalf("derive scratch url: %v", err)
	}
	return url
}

func entReplaceDBName(rawURL, name string) (string, error) {
	i := strings.LastIndex(rawURL, "/")
	if i < 0 {
		return "", fmt.Errorf("no path in %q", rawURL)
	}
	rest := rawURL[i+1:]
	q := ""
	if j := strings.Index(rest, "?"); j >= 0 {
		q = rest[j:]
	}
	return rawURL[:i+1] + name + q, nil
}

func entMigrator(t *testing.T, url string) *migrate.Migrate {
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

func entMigrateTo(t *testing.T, m *migrate.Migrate, version uint) {
	t.Helper()
	if err := m.Migrate(version); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to %d: %v", version, err)
	}
}

// entMigrateUp runs the remaining migrations to head. Used where a test has to
// stop at an intermediate version to take a snapshot and then needs the CURRENT
// schema back before calling production code.
func entMigrateUp(t *testing.T, m *migrate.Migrate) {
	t.Helper()
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate up to head: %v", err)
	}
}

func entPool(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// --- Gate 2, item 1: BACKFILL NEUTRALITY -------------------------------------

// listAppsPre0043 is a VERBATIM copy of listApps' query as it stood at migration
// 0042 — same projection, same joins, same ORDER BY, and the pre-Phase-2 WHERE
// clause with NO entitlement predicate.
//
// It has to be a copy. The "before" snapshot must be taken at schema version 42,
// where the entitlements table does not exist yet, so the production listApps
// (which now references it) cannot run at all. Calling the real function for both
// halves of the comparison is impossible by construction; the honest alternative
// is to reproduce exactly what it used to return and compare THAT against what it
// returns now.
func listAppsPre0043(t *testing.T, pool *pgxpool.Pool, callerID string, limit int32) []App {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT apps.id::text, name, description, cover_url, hero_url, kind,
		       external_source, external_id,
		       default_vram_mb, default_encode_slots,
		       default_width, default_height, default_fps, default_bitrate_kbps,
		       enabled, default_profile_id, profile_policy,
		       COALESCE(dp.width, default_width),
		       COALESCE(dp.height, default_height),
		       COALESCE(dp.fps, default_fps),
		       COALESCE(dp.nominal_bitrate_kbps, default_bitrate_kbps),
		       apps.created_at, apps.updated_at,
		       (fav.app_id IS NOT NULL)
		FROM apps
		LEFT JOIN stream_profile_policy spp ON true
		LEFT JOIN launch_profile_rungs dpr ON dpr.position = 1 AND dpr.launch_profile_id = CASE
			WHEN apps.profile_policy IN ('prefer', 'force') AND apps.default_profile_id IS NOT NULL THEN apps.default_profile_id
			ELSE spp.global_default_profile_id
		END
		LEFT JOIN stream_profiles dp ON dp.id = dpr.stream_profile_id
		LEFT JOIN user_app_favourites fav ON fav.app_id = apps.id AND fav.user_id = $3::uuid
		WHERE apps.enabled = true
		ORDER BY apps.created_at DESC
		LIMIT $1 OFFSET $2
	`, limit, 0, callerID)
	if err != nil {
		t.Fatalf("pre-0043 list: %v", err)
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.ID, &a.Name, &a.Description, &a.CoverURL, &a.HeroURL, &a.Kind,
			&a.ExternalSource, &a.ExternalID,
			&a.DefaultVramMB, &a.DefaultEncodeSlots,
			&a.DefaultWidth, &a.DefaultHeight, &a.DefaultFPS, &a.DefaultBitratekbps,
			&a.Enabled, &a.DefaultProfileID, &a.ProfilePolicy,
			&a.DisplayWidth, &a.DisplayHeight, &a.DisplayFPS, &a.DisplayBitratekbps,
			&a.CreatedAt, &a.UpdatedAt, &a.Favourite); err != nil {
			t.Fatalf("pre-0043 scan: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pre-0043 rows: %v", err)
	}
	return out
}

func snapshotJSON(t *testing.T, apps []App) string {
	t.Helper()
	b, err := json.Marshal(apps)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	return string(b)
}

// TestMigration0043BackfillIsNeutralPerUser is Gate 2's headline acceptance
// criterion: "immediately after migration, every user sees exactly the apps they
// saw before" (§13). Everything else in Phase 2 is a refinement of this.
//
// It is deliberately PER USER and a BYTE COMPARE of the serialized list, not a
// count or a set of ids: `favourite` is caller-scoped, so a filter bug that
// happened to preserve cardinality while re-ordering or mis-joining would still
// be caught.
func TestMigration0043BackfillIsNeutralPerUser(t *testing.T) {
	url := entScratchDB(t)
	m := entMigrator(t, url)
	entMigrateTo(t, m, version42)

	ctx := context.Background()
	pool := entPool(t, url)

	// A pre-migration catalogue with the shapes that actually vary the query:
	// enabled and disabled apps, and two users with different favourites.
	var alice, bob string
	must43(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('alice@t.local','alice','x') RETURNING id::text`).Scan(&alice))
	must43(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('bob@t.local','bob','x') RETURNING id::text`).Scan(&bob))

	appIDs := make([]string, 0, 5)
	for i, spec := range []struct {
		name    string
		enabled bool
	}{
		{"alpha", true}, {"beta", true}, {"gamma", false}, {"delta", true}, {"epsilon", true},
	} {
		var id string
		must43(t, pool.QueryRow(ctx, `INSERT INTO apps (name, enabled, created_at)
			VALUES ($1, $2, now() - make_interval(mins => $3::int)) RETURNING id::text`,
			spec.name, spec.enabled, i).Scan(&id))
		appIDs = append(appIDs, id)
	}
	// Different favourites per user, so the two snapshots genuinely differ from
	// each other and a cross-user leak would show up.
	must43(t, exec43(ctx, pool, `INSERT INTO user_app_favourites (user_id, app_id) VALUES ($1::uuid, $2::uuid)`, alice, appIDs[0]))
	must43(t, exec43(ctx, pool, `INSERT INTO user_app_favourites (user_id, app_id) VALUES ($1::uuid, $2::uuid)`, bob, appIDs[1]))

	before := map[string]string{
		alice: snapshotJSON(t, listAppsPre0043(t, pool, alice, 100)),
		bob:   snapshotJSON(t, listAppsPre0043(t, pool, bob, 100)),
	}
	// Guard the guard: a snapshot of an empty catalogue would make this test pass
	// vacuously no matter how broken the filter is.
	for u, snap := range before {
		if snap == "null" || snap == "[]" {
			t.Fatalf("pre-migration snapshot for %s is empty; the fixture did not seed", u)
		}
	}

	entMigrateTo(t, m, version43)

	// …then the REST OF THE WAY TO HEAD, before calling the production listApps.
	//
	// The "before" half of this comparison can only be taken at 42 (the pre-0043
	// query is a verbatim copy for exactly that reason), but the "after" half runs
	// the REAL listApps, and the real listApps is always HEAD-schema code: Phase 3
	// added apps.parent_app_id to its projection, so pinning the database at 43
	// makes it fail to compile against the schema rather than fail the assertion.
	//
	// Migrating to head rather than to a hardcoded 44 is what stops this breaking
	// again on every subsequent migration. It does not weaken the claim: the
	// property under test is "the entitlement filter returns what the unfiltered
	// query returned", and every migration after 0043 that changed the answer would
	// fail this test — which is the right outcome, not a false alarm.
	entMigrateUp(t, m)

	s := &store{pool: pool}
	for _, u := range []string{alice, bob} {
		got, _, err := s.listApps(ctx, u, "", 100)
		if err != nil {
			t.Fatalf("post-migration list for %s: %v", u, err)
		}
		if after := snapshotJSON(t, got); after != before[u] {
			t.Errorf("BACKFILL NOT NEUTRAL for user %s.\n before: %s\n  after: %s", u, before[u], after)
		}
	}

	// The backfill must cover the DISABLED app too. It is invisible either way
	// today (the filter is ANDed with enabled = true), so nothing above would
	// notice — but skipping it would mean re-enabling that app silently failed to
	// bring it back, months later, with nothing to point at.
	var backfilled int
	must43(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM entitlements WHERE subject_type='all' AND granted_by='migration'`).Scan(&backfilled))
	if backfilled != len(appIDs) {
		t.Errorf("backfill covered %d apps, want %d (every app in the catalogue, disabled included)", backfilled, len(appIDs))
	}
}

// TestMigration0043DownThenUp proves the down migration actually works — applied
// for real against Postgres, not merely present as a file — and that re-applying
// re-establishes the neutral state.
func TestMigration0043DownThenUp(t *testing.T) {
	url := entScratchDB(t)
	m := entMigrator(t, url)
	entMigrateTo(t, m, version43)

	ctx := context.Background()
	pool := entPool(t, url)

	var appID string
	must43(t, pool.QueryRow(ctx, `INSERT INTO apps (name) VALUES ('down-up') RETURNING id::text`).Scan(&appID))
	// The app was created AFTER the backfill ran, so it has no entitlement; grant
	// one so the down path has a row to drop.
	must43(t, exec43(ctx, pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('all', NULL, $1::uuid, 'admin')`, appID))

	entMigrateTo(t, m, version42)
	var exists bool
	must43(t, pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.tables WHERE table_name = 'entitlements')`).Scan(&exists))
	if exists {
		t.Fatal("entitlements table still exists after migrating down to 42")
	}

	entMigrateTo(t, m, version43)
	// Re-applying re-runs the backfill, so the app is entitled again — the
	// documented (and deliberate) consequence: a down/up cycle returns the
	// catalogue to FULLY OPEN, not to whatever narrower state was configured.
	var n int
	must43(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM entitlements WHERE app_id::text = $1 AND subject_type='all'`, appID).Scan(&n))
	if n != 1 {
		t.Errorf("after down+up the app has %d 'all' entitlements, want 1 (the re-run backfill)", n)
	}
}

// TestEntitlementShapeCheckRejectsWrongShapes pins the 0043 shape CHECK. It is
// what makes ('all', <uuid>) — "everyone, but specifically this person", which
// means nothing — and ('user', NULL) — a user row with no user — unstorable, so
// no reader has to defend against them.
func TestEntitlementShapeCheckRejectsWrongShapes(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	var userID, appID string
	must43(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('shape@t.local','shape','x') RETURNING id::text`).Scan(&userID))
	must43(t, pool.QueryRow(ctx, `INSERT INTO apps (name) VALUES ('shape-app') RETURNING id::text`).Scan(&appID))

	cases := []struct {
		name        string
		subjectType string
		subjectID   any
	}{
		{"all with a subject_id", "all", userID},
		{"user with a NULL subject_id", "user", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
				VALUES ($1, $2::uuid, $3::uuid, 'admin')`, c.subjectType, c.subjectID, appID)
			if err == nil {
				t.Fatalf("the database accepted (%s, %v)", c.subjectType, c.subjectID)
			}
			if !strings.Contains(err.Error(), "entitlements_subject_shape_ck") {
				t.Fatalf("want entitlements_subject_shape_ck, got %v", err)
			}
		})
	}
}

// TestEntitlementPartialUniqueIndexesRejectDuplicates pins BOTH indexes.
//
// The 'all' half is the one that matters and the one a plain
// UNIQUE (subject_type, subject_id, app_id) would silently fail to enforce:
// Postgres does not consider two NULL subject_ids equal, so it would happily
// store unlimited duplicate 'all' rows for one app.
func TestEntitlementPartialUniqueIndexesRejectDuplicates(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	var userID, appID string
	must43(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('uk@t.local','uk','x') RETURNING id::text`).Scan(&userID))
	must43(t, pool.QueryRow(ctx, `INSERT INTO apps (name) VALUES ('uk-app') RETURNING id::text`).Scan(&appID))

	t.Run("all", func(t *testing.T) {
		must43(t, exec43(ctx, pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
			VALUES ('all', NULL, $1::uuid, 'admin')`, appID))
		_, err := pool.Exec(ctx, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
			VALUES ('all', NULL, $1::uuid, 'admin')`, appID)
		if err == nil {
			t.Fatal("a duplicate ('all', <app>) row was accepted — the partial unique index is not doing its job")
		}
		if !strings.Contains(err.Error(), "entitlements_all_uk") {
			t.Fatalf("want entitlements_all_uk, got %v", err)
		}
	})

	t.Run("user", func(t *testing.T) {
		must43(t, exec43(ctx, pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
			VALUES ('user', $1::uuid, $2::uuid, 'admin')`, userID, appID))
		_, err := pool.Exec(ctx, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
			VALUES ('user', $1::uuid, $2::uuid, 'admin')`, userID, appID)
		if err == nil {
			t.Fatal("a duplicate ('user', <user>, <app>) row was accepted")
		}
		if !strings.Contains(err.Error(), "entitlements_user_uk") {
			t.Fatalf("want entitlements_user_uk, got %v", err)
		}
	})

	// And the two shapes must NOT collide with each other: holding both an 'all'
	// and a personal entitlement for one app is a legal, meaningful state (it is
	// exactly what the pagination test below relies on).
	var n int
	must43(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM entitlements WHERE app_id::text = $1`, appID).Scan(&n))
	if n != 2 {
		t.Fatalf("expected the 'all' and 'user' rows to coexist, got %d rows", n)
	}
}

// --- Gate 2, item 1 (second clause): DUPLICATE-ENTITLEMENT PAGINATION ---------

// TestDuplicateEntitlementPagesWithoutDuplicatesOrSkips is the EXISTS-not-JOIN
// regression test (§6.3, §17 row 3).
//
// It pages with a small limit over a catalogue big enough to cross two page
// boundaries, with the doubly-entitled app placed INSIDE the first page rather
// than at its edge — a join's duplicate row would consume a slot in that page,
// shift every subsequent offset by one, and drop the app that should have led
// page two. A single-page test cannot see any of that, which is exactly why the
// spec calls this "the kind of defect that passes every single-user test".
func TestDuplicateEntitlementPagesWithoutDuplicatesOrSkips(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	s := &store{pool: pool}

	var userID string
	must43(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('pager@t.local','pager','x') RETURNING id::text`).Scan(&userID))

	// 7 apps, deterministic order (created_at DESC ⇒ app-0 first).
	const total = 7
	ids := make([]string, total)
	for i := 0; i < total; i++ {
		must43(t, pool.QueryRow(ctx, `INSERT INTO apps (name, created_at)
			VALUES ($1, now() - make_interval(mins => $2::int)) RETURNING id::text`,
			fmt.Sprintf("page-app-%d", i), i).Scan(&ids[i]))
		must43(t, exec43(ctx, pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
			VALUES ('all', NULL, $1::uuid, 'migration')`, ids[i]))
	}
	// app-1 is ALSO granted personally: two entitlement rows, one app. It sits at
	// index 1 of a 3-per-page walk, i.e. mid-page, so a join's duplicate would push
	// app-2 onto page two and app-2 would then be visited twice or not at all.
	must43(t, exec43(ctx, pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('user', $1::uuid, $2::uuid, 'admin')`, userID, ids[1]))

	var walked []string
	cursor := ""
	for page := 0; page < 10; page++ {
		got, next, err := s.listApps(ctx, userID, cursor, 3)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, a := range got {
			walked = append(walked, a.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}

	if len(walked) != total {
		t.Fatalf("paged walk returned %d apps, want %d: %v", len(walked), total, walked)
	}
	seen := map[string]int{}
	for _, id := range walked {
		seen[id]++
	}
	for i, id := range ids {
		switch seen[id] {
		case 1: // correct
		case 0:
			t.Errorf("app-%d was SKIPPED across the page boundary", i)
		default:
			t.Errorf("app-%d appeared %d times (duplicate entitlement leaked through as a duplicate row)", i, seen[id])
		}
	}
	// And order is unchanged: created_at DESC, the same order a single-entitlement
	// catalogue produces.
	for i := range ids {
		if walked[i] != ids[i] {
			t.Fatalf("page order diverged at %d: got %s want %s (%v)", i, walked[i], ids[i], walked)
		}
	}
}

// --- the filter, end to end over HTTP ----------------------------------------

// entServer registers an admin and a plain user, returns the server and both
// tokens plus both user ids.
type entFixture struct {
	srv                  string
	adminTok, userTok    string
	adminID, userID      string
	entitledID, hiddenID string // two apps: one 'all'-entitled, one entitled to nobody
}

func newEntFixture(t *testing.T, pool *pgxpool.Pool) entFixture {
	t.Helper()
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "eadmin@t.local", "eadmin", "quasar-fixture-pw-08"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE email='eadmin@t.local'`); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	if _, err := authSvc.Register(ctx, "euser@t.local", "euser", "quasar-fixture-pw-08"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	adminTok, err := authSvc.Login(ctx, "eadmin@t.local", "quasar-fixture-pw-08", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	userTok, err := authSvc.Login(ctx, "euser@t.local", "quasar-fixture-pw-08", "")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}

	f := entFixture{srv: srv.URL, adminTok: adminTok.Plaintext, userTok: userTok.Plaintext}
	must43(t, pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email='eadmin@t.local'`).Scan(&f.adminID))
	must43(t, pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email='euser@t.local'`).Scan(&f.userID))

	// Created through the real handler, so each gets the default 'all' entitlement
	// (§6.4) — then the second one is stripped, which is how an admin restricting
	// an app actually looks in the database.
	f.entitledID = createAppViaAPI(t, f.srv, f.adminTok, "shared-app", nil)
	f.hiddenID = createAppViaAPI(t, f.srv, f.adminTok, "hidden-app", nil)
	must43(t, exec43(ctx, pool, `DELETE FROM entitlements WHERE app_id::text = $1`, f.hiddenID))
	return f
}

func createAppViaAPI(t *testing.T, base, tok, name string, entitle *string) string {
	t.Helper()
	body := map[string]any{"name": name}
	if entitle != nil {
		body["entitle"] = *entitle
	}
	resp, parsed := post(t, base+"/v1/apps", body, tok)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app %s: want 201, got %d (%v)", name, resp.StatusCode, parsed)
	}
	return parsed["app"].(map[string]any)["id"].(string)
}

func listAppIDs(t *testing.T, base, path, tok string) []string {
	t.Helper()
	resp, parsed := getReq(t, base+path, tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: want 200, got %d (%v)", path, resp.StatusCode, parsed)
	}
	items, _ := parsed["items"].([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.(map[string]any)["id"].(string))
	}
	return out
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// TestUnentitledAppIsInvisibleAndUnfavouritable covers the Phase 2 acceptance
// list's read half: absent from GET /v1/apps, 404 from GET /v1/apps/{id}, and
// (the one deliberate exception to the uniform 404) 403 from PUT favourites.
func TestUnentitledAppIsInvisibleAndUnfavouritable(t *testing.T) {
	pool := testDB(t)
	f := newEntFixture(t, pool)

	ids := listAppIDs(t, f.srv, "/v1/apps", f.userTok)
	if !contains(ids, f.entitledID) {
		t.Error("the 'all'-entitled app is missing from the user's library")
	}
	if contains(ids, f.hiddenID) {
		t.Error("an app the user holds no entitlement for is VISIBLE in GET /v1/apps")
	}

	resp, _ := getReq(t, f.srv+"/v1/apps/"+f.hiddenID, f.userTok)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /v1/apps/{unentitled}: want 404, got %d", resp.StatusCode)
	}
	resp, _ = getReq(t, f.srv+"/v1/apps/"+f.entitledID, f.userTok)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/apps/{entitled}: want 200, got %d", resp.StatusCode)
	}

	resp, _ = putReq(t, f.srv+"/v1/me/favourites/"+f.hiddenID, f.userTok)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("PUT /v1/me/favourites/{unentitled}: want 403, got %d", resp.StatusCode)
	}
	resp, _ = putReq(t, f.srv+"/v1/me/favourites/"+f.entitledID, f.userTok)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("PUT /v1/me/favourites/{entitled}: want 204, got %d", resp.StatusCode)
	}
}

// TestAdminIsSubjectToTheSameFilter is §6.5 as an assertion: there is no role
// bypass on GET /v1/apps, and the admin god view is GET /v1/admin/apps.
//
// If someone later adds `OR <caller is admin>` to entitledSQL "to make the admin
// UI work", this is the test that fails.
func TestAdminIsSubjectToTheSameFilter(t *testing.T) {
	pool := testDB(t)
	f := newEntFixture(t, pool)

	userIDs := listAppIDs(t, f.srv, "/v1/apps", f.adminTok)
	if contains(userIDs, f.hiddenID) {
		t.Error("GET /v1/apps returned an unentitled app TO AN ADMIN — §6.5 says there is no role bypass")
	}
	if !contains(userIDs, f.entitledID) {
		t.Error("GET /v1/apps hid an 'all'-entitled app from the admin")
	}

	adminIDs := listAppIDs(t, f.srv, "/v1/admin/apps", f.adminTok)
	if !contains(adminIDs, f.hiddenID) || !contains(adminIDs, f.entitledID) {
		t.Errorf("GET /v1/admin/apps must be UNFILTERED (the god view); got %v", adminIDs)
	}

	// A personal grant to the admin brings it back into their own library, which is
	// the documented remedy — one call, and audited.
	resp, parsed := post(t, f.srv+"/v1/admin/apps/"+f.hiddenID+"/entitlements",
		map[string]any{"subject_type": "user", "subject_id": f.adminID}, f.adminTok)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("grant to self: want 201, got %d (%v)", resp.StatusCode, parsed)
	}
	if !contains(listAppIDs(t, f.srv, "/v1/apps", f.adminTok), f.hiddenID) {
		t.Error("after granting themselves the entitlement the admin still cannot see the app")
	}
	// And it did NOT leak to the other user.
	if contains(listAppIDs(t, f.srv, "/v1/apps", f.userTok), f.hiddenID) {
		t.Error("a personal grant to the admin made the app visible to a different user")
	}
}

// TestCreateAppEntitlementDefault covers §6.4's corollary in both directions.
func TestCreateAppEntitlementDefault(t *testing.T) {
	pool := testDB(t)
	f := newEntFixture(t, pool)

	// Default (field absent) ⇒ visible to everyone immediately. Without this,
	// "I made an app and nobody can see it" is the new default experience.
	def := createAppViaAPI(t, f.srv, f.adminTok, "default-visible", nil)
	if !contains(listAppIDs(t, f.srv, "/v1/apps", f.userTok), def) {
		t.Error("a newly created app is invisible to a plain user; the default 'all' entitlement was not written")
	}

	none := "none"
	gated := createAppViaAPI(t, f.srv, f.adminTok, "gated", &none)
	if contains(listAppIDs(t, f.srv, "/v1/apps", f.userTok), gated) {
		t.Error(`entitle:"none" still produced a visible app`)
	}

	all := "all"
	explicit := createAppViaAPI(t, f.srv, f.adminTok, "explicit-all", &all)
	if !contains(listAppIDs(t, f.srv, "/v1/apps", f.userTok), explicit) {
		t.Error(`entitle:"all" did not produce a visible app`)
	}

	// A typo must be refused, never silently read as "none" — that would create an
	// invisible app and present as "the catalogue is broken".
	resp, _ := post(t, f.srv+"/v1/apps", map[string]any{"name": "typo", "entitle": "nome"}, f.adminTok)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf(`entitle:"nome": want 400, got %d`, resp.StatusCode)
	}
}

// TestAdminEntitlementRoutesRejectNonAdmin is the CLAUDE.md invariant #6 check
// every admin surface in this repo gets: 401 without a token, 403 with a valid
// non-admin one, and the role gate precedes any resource lookup (so the 403s
// below use ids the caller has no business learning about).
func TestAdminEntitlementRoutesRejectNonAdmin(t *testing.T) {
	pool := testDB(t)
	f := newEntFixture(t, pool)

	routes := []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/admin/apps/" + f.hiddenID + "/entitlements", nil},
		{"POST", "/v1/admin/apps/" + f.hiddenID + "/entitlements", map[string]any{"subject_type": "all"}},
		{"DELETE", "/v1/admin/apps/" + f.hiddenID + "/entitlements/" + f.hiddenID, nil},
		{"GET", "/v1/admin/users/" + f.userID + "/entitlements", nil},
	}
	for _, r := range routes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			if code := doEnt(t, r.method, f.srv+r.path, "", r.body); code != http.StatusUnauthorized {
				t.Errorf("no token: got %d, want 401", code)
			}
			if code := doEnt(t, r.method, f.srv+r.path, f.userTok, r.body); code != http.StatusForbidden {
				t.Errorf("non-admin token: got %d, want 403", code)
			}
		})
	}
	// Sanity: the admin token reaches the handlers, so the 403s are the role gate
	// and not a routing accident.
	if code := doEnt(t, "GET", f.srv+"/v1/admin/apps/"+f.hiddenID+"/entitlements", f.adminTok, nil); code != http.StatusOK {
		t.Errorf("admin GET entitlements = %d, want 200", code)
	}
}

// TestGrantRevokeRoundTrip covers the §6.6 surface, including the two things
// that are easy to get wrong: a 'provider' row (Phase 4, written by nothing yet)
// must still be revocable, and both mutations must be audited.
func TestGrantRevokeRoundTrip(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	f := newEntFixture(t, pool)

	// Grant to a user.
	resp, parsed := post(t, f.srv+"/v1/admin/apps/"+f.hiddenID+"/entitlements",
		map[string]any{"subject_type": "user", "subject_id": f.userID}, f.adminTok)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("grant: want 201, got %d (%v)", resp.StatusCode, parsed)
	}
	ent := parsed["entitlement"].(map[string]any)
	entID := ent["id"].(string)
	if ent["granted_by"] != "admin" {
		t.Errorf("granted_by = %v, want admin", ent["granted_by"])
	}
	if ent["subject_username"] != "euser" {
		t.Errorf("subject_username = %v, want euser", ent["subject_username"])
	}
	if !contains(listAppIDs(t, f.srv, "/v1/apps", f.userTok), f.hiddenID) {
		t.Error("the granted app is still not in the user's library")
	}

	// Re-granting the same pair is a 409, not a silent second row.
	resp, _ = post(t, f.srv+"/v1/admin/apps/"+f.hiddenID+"/entitlements",
		map[string]any{"subject_type": "user", "subject_id": f.userID}, f.adminTok)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate grant: want 409, got %d", resp.StatusCode)
	}

	// Both list directions see it.
	if n := len(entitlementItems(t, f.srv+"/v1/admin/apps/"+f.hiddenID+"/entitlements", f.adminTok)); n != 1 {
		t.Errorf("app entitlements: got %d, want 1", n)
	}
	if n := len(entitlementItems(t, f.srv+"/v1/admin/users/"+f.userID+"/entitlements", f.adminTok)); n != 1 {
		t.Errorf("user entitlements: got %d, want 1", n)
	}

	// Revoke.
	if code := doEnt(t, "DELETE", f.srv+"/v1/admin/apps/"+f.hiddenID+"/entitlements/"+entID, f.adminTok, nil); code != http.StatusNoContent {
		t.Fatalf("revoke: want 204, got %d", code)
	}
	if contains(listAppIDs(t, f.srv, "/v1/apps", f.userTok), f.hiddenID) {
		t.Error("the app is still visible after the entitlement was revoked")
	}
	if code := doEnt(t, "DELETE", f.srv+"/v1/admin/apps/"+f.hiddenID+"/entitlements/"+entID, f.adminTok, nil); code != http.StatusNotFound {
		t.Error("revoking an already-revoked entitlement should be 404")
	}

	// A 'provider' row — Phase 4 writes these; nothing does today. Revoking one
	// must work, because the reconciler and the admin UI both depend on it.
	var provID string
	must43(t, pool.QueryRow(ctx, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by, source_ref)
		VALUES ('user', $1::uuid, $2::uuid, 'provider', 'steam:730') RETURNING id::text`,
		f.userID, f.hiddenID).Scan(&provID))
	if code := doEnt(t, "DELETE", f.srv+"/v1/admin/apps/"+f.hiddenID+"/entitlements/"+provID, f.adminTok, nil); code != http.StatusNoContent {
		t.Errorf("revoking a granted_by='provider' row: want 204, got %d", code)
	}

	// A revoke scoped to the WRONG app is a 404, never a cross-app delete.
	var strayID string
	must43(t, pool.QueryRow(ctx, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('user', $1::uuid, $2::uuid, 'admin') RETURNING id::text`, f.userID, f.hiddenID).Scan(&strayID))
	if code := doEnt(t, "DELETE", f.srv+"/v1/admin/apps/"+f.entitledID+"/entitlements/"+strayID, f.adminTok, nil); code != http.StatusNotFound {
		t.Errorf("cross-app revoke: want 404, got %d", code)
	}
	var stillThere bool
	must43(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM entitlements WHERE id::text = $1)`, strayID).Scan(&stillThere))
	if !stillThere {
		t.Error("a revoke scoped to the wrong app DELETED the row anyway")
	}
}

// TestEntitlementsCascade — deleting a user or an app removes its entitlements
// (Phase 2 acceptance). Both FKs, both directions.
func TestEntitlementsCascade(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	var userID, appA, appB string
	must43(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('casc@t.local','casc','x') RETURNING id::text`).Scan(&userID))
	must43(t, pool.QueryRow(ctx, `INSERT INTO apps (name) VALUES ('casc-a') RETURNING id::text`).Scan(&appA))
	must43(t, pool.QueryRow(ctx, `INSERT INTO apps (name) VALUES ('casc-b') RETURNING id::text`).Scan(&appB))
	must43(t, exec43(ctx, pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('user', $1::uuid, $2::uuid, 'admin')`, userID, appA))
	must43(t, exec43(ctx, pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('user', $1::uuid, $2::uuid, 'admin')`, userID, appB))
	must43(t, exec43(ctx, pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('all', NULL, $1::uuid, 'migration')`, appB))

	must43(t, exec43(ctx, pool, `DELETE FROM apps WHERE id::text = $1`, appA))
	var n int
	must43(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM entitlements WHERE app_id::text = $1`, appA).Scan(&n))
	if n != 0 {
		t.Errorf("deleting an app left %d entitlement(s) behind", n)
	}

	must43(t, exec43(ctx, pool, `DELETE FROM users WHERE id::text = $1`, userID))
	must43(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM entitlements WHERE subject_id::text = $1`, userID).Scan(&n))
	if n != 0 {
		t.Errorf("deleting a user left %d entitlement(s) behind", n)
	}
	// The app's 'all' row is untouched by the user delete — the two shapes are
	// independent facts.
	must43(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM entitlements WHERE app_id::text = $1`, appB).Scan(&n))
	if n != 1 {
		t.Errorf("the app's 'all' entitlement did not survive deleting an unrelated user (got %d rows)", n)
	}

	// granted_by_user is SET NULL, not CASCADE: deleting the operator who granted
	// an entitlement must not silently revoke it.
	var granter, appC, entC string
	must43(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('granter@t.local','granter','x') RETURNING id::text`).Scan(&granter))
	must43(t, pool.QueryRow(ctx, `INSERT INTO apps (name) VALUES ('casc-c') RETURNING id::text`).Scan(&appC))
	must43(t, pool.QueryRow(ctx, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by, granted_by_user)
		VALUES ('all', NULL, $1::uuid, 'admin', $2::uuid) RETURNING id::text`, appC, granter).Scan(&entC))
	must43(t, exec43(ctx, pool, `DELETE FROM users WHERE id::text = $1`, granter))
	var survivor *string
	must43(t, pool.QueryRow(ctx, `SELECT granted_by_user::text FROM entitlements WHERE id::text = $1`, entC).Scan(&survivor))
	if survivor != nil {
		t.Errorf("granted_by_user = %v after the granter was deleted, want NULL", *survivor)
	}
}

// --- small helpers -----------------------------------------------------------

func must43(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
}

func exec43(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) error {
	_, err := pool.Exec(ctx, sql, args...)
	return err
}

func doEnt(t *testing.T, method, url, tok string, body any) int {
	t.Helper()
	switch method {
	case "GET":
		resp, _ := getReq(t, url, tok)
		return resp.StatusCode
	case "POST":
		resp, _ := post(t, url, body, tok)
		return resp.StatusCode
	case "DELETE":
		return deleteReq(t, url, tok).StatusCode
	}
	t.Fatalf("unsupported method %s", method)
	return 0
}

func entitlementItems(t *testing.T, url, tok string) []any {
	t.Helper()
	resp, parsed := getReq(t, url, tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: want 200, got %d (%v)", url, resp.StatusCode, parsed)
	}
	items, _ := parsed["items"].([]any)
	return items
}
