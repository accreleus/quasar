package session

// entitlement_launch_db_test.go — steam-library-discovery Phase 2, the LAUNCH
// gate (spec §6.3, §6.5; Gate 2 item 1's third named test).
//
// THE FILTERED LIST IS UX. THIS IS THE AUTHORIZATION BOUNDARY. Every test here
// goes through POST /v1/sessions with a real bearer token rather than calling
// the store, because the claim being proved is precisely "a client that ignores
// the filtered library and posts an app id directly is refused" — a store-level
// test would prove the predicate works while leaving the question of whether
// anything actually calls it unanswered.
//
// The rejection lands BEFORE placement, which is why none of these tests need a
// GPU with room: an unentitled launch never reaches admission at all.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// entLaunchFixture is a seeded fleet (host + GPU with real capacity) plus two
// registered users — one plain, one admin — and one app that NOBODY is entitled
// to. The app is deliberately launchable in every other respect.
type entLaunchFixture struct {
	base                string
	userTok, adminTok   string
	userID, adminID     string
	appID               string
	openAppID           string // an 'all'-entitled app, the positive control
	pool                *pgxpool.Pool
	hostID, gpuIDunused string
}

func newEntLaunchFixture(t *testing.T, pool *pgxpool.Pool) entLaunchFixture {
	t.Helper()
	ctx := context.Background()
	srv, authSvc, _ := newMetricsServer(t, pool)

	f := entLaunchFixture{base: srv.URL, pool: pool}
	if _, err := authSvc.Register(ctx, "entuser@t.local", "entuser", "quasar-fixture-pw-08"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	if _, err := authSvc.Register(ctx, "entadmin@t.local", "entadmin", "quasar-fixture-pw-08"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	must(t, execEnt(ctx, pool, `UPDATE users SET role='admin' WHERE email='entadmin@t.local'`))
	f.userTok = loginTok(t, authSvc, "entuser@t.local", "quasar-fixture-pw-08")
	f.adminTok = loginTok(t, authSvc, "entadmin@t.local", "quasar-fixture-pw-08")
	must(t, pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email='entuser@t.local'`).Scan(&f.userID))
	must(t, pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email='entadmin@t.local'`).Scan(&f.adminID))

	must(t, pool.QueryRow(ctx, `INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height, default_fps, default_bitrate_kbps)
		VALUES ('ent-locked', 1024, 1, 1280, 720, 60, 6000) RETURNING id::text`).Scan(&f.appID))
	must(t, pool.QueryRow(ctx, `INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height, default_fps, default_bitrate_kbps)
		VALUES ('ent-open', 1024, 1, 1280, 720, 60, 6000) RETURNING id::text`).Scan(&f.openAppID))
	entitleAll(t, pool, f.openAppID) // f.appID deliberately gets NOTHING

	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, capacity_detection)
		VALUES ('host-ent','online','ok') RETURNING id::text`).Scan(&f.hostID))
	must(t, pool.QueryRow(ctx, `INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
		VALUES ($1, 0, 16384, 8) RETURNING id::text`, f.hostID).Scan(&f.gpuIDunused))
	return f
}

func execEnt(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) error {
	_, err := pool.Exec(ctx, sql, args...)
	return err
}

func launchStatus(t *testing.T, base, tok, appID string) (int, string) {
	t.Helper()
	resp := doJSON(t, "POST", base+"/v1/sessions", tok, map[string]any{"app_id": appID})
	defer resp.Body.Close()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	code := body.Error.Code
	if code == "" {
		code = body.Code
	}
	return resp.StatusCode, code
}

// TestLaunchRejectsUnentitledApp is the test the spec calls "the one that proves
// enforcement is not UI-gated" (Gate 2 item 1). A direct POST /v1/sessions,
// naming a real, enabled, perfectly launchable app, with a valid token, on a
// fleet with capacity to spare — refused with 403 because the caller holds no
// entitlement.
func TestLaunchRejectsUnentitledApp(t *testing.T) {
	pool := testDB(t)
	f := newEntLaunchFixture(t, pool)

	status, _ := launchStatus(t, f.base, f.userTok, f.appID)
	if status != http.StatusForbidden {
		t.Fatalf("POST /v1/sessions on an unentitled app: got %d, want 403", status)
	}

	// Nothing was reserved, and no row was written: the check runs before
	// placement, so a refused launch must leave the fleet exactly as it was.
	var sessions int
	must(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM sessions WHERE app_id::text = $1`, f.appID).Scan(&sessions))
	if sessions != 0 {
		t.Errorf("a refused launch persisted %d session row(s)", sessions)
	}

	// Positive control: the SAME user, the SAME fleet, an 'all'-entitled app.
	// Without this the 403 above could be any unrelated failure.
	if status, code := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusCreated {
		t.Fatalf("POST /v1/sessions on an entitled app: got %d (%s), want 201", status, code)
	}
}

// TestLaunchHasNoAdminBypass is §6.5 at the launch gate, operator-confirmed:
// an admin is refused exactly like anyone else, and the remedy is a grant.
//
// This is the code path the spec says nobody tests. It is tested here.
func TestLaunchHasNoAdminBypass(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	f := newEntLaunchFixture(t, pool)

	if status, _ := launchStatus(t, f.base, f.adminTok, f.appID); status != http.StatusForbidden {
		t.Fatalf("an ADMIN launched an app they hold no entitlement for: got %d, want 403", status)
	}

	// The documented remedy: grant, then launch. (Written directly here — the
	// admin HTTP surface lives in internal/crud and is tested there.)
	must(t, execEnt(ctx, pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('user', $1::uuid, $2::uuid, 'admin')`, f.adminID, f.appID))
	if status, code := launchStatus(t, f.base, f.adminTok, f.appID); status != http.StatusCreated {
		t.Fatalf("after granting itself the entitlement the admin still could not launch: %d (%s)", status, code)
	}
	// And the grant was PERSONAL: the plain user is still refused.
	if status, _ := launchStatus(t, f.base, f.userTok, f.appID); status != http.StatusForbidden {
		t.Errorf("a personal grant to the admin also unlocked the app for another user: got %d, want 403", status)
	}
}

// TestLaunchEntitlementPrecedesQuota pins the gate ORDER. A user who is BOTH
// unentitled and at their session limit must be told they may not launch this
// app (403), not that they have too many sessions (409) — the latter is both
// wrong and sends them to fix the wrong thing.
func TestLaunchEntitlementPrecedesQuota(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	f := newEntLaunchFixture(t, pool)

	must(t, execEnt(ctx, pool, `UPDATE users SET max_concurrent_sessions = 1 WHERE id::text = $1`, f.userID))
	// Fill the quota with a legitimate launch on the entitled app.
	if status, code := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusCreated {
		t.Fatalf("seed launch: got %d (%s), want 201", status, code)
	}
	// At quota AND unentitled ⇒ 403, not 409.
	if status, _ := launchStatus(t, f.base, f.userTok, f.appID); status != http.StatusForbidden {
		t.Errorf("at-quota + unentitled: got %d, want 403 (authorization precedes resource accounting)", status)
	}
	// Sanity: at quota AND entitled really is the 409 (so the assertion above is
	// about ordering, not about the quota gate being broken).
	if status, _ := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusConflict {
		t.Errorf("at-quota + entitled: got %d, want 409", status)
	}
}

// TestLaunchAcceptsEitherEntitlementShape — a personal grant alone is enough
// (no 'all' row anywhere), and holding BOTH is not a problem at the launch gate
// either (the FOR SHARE lookup takes the first match; two matches are not an
// error).
func TestLaunchAcceptsEitherEntitlementShape(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	f := newEntLaunchFixture(t, pool)

	must(t, execEnt(ctx, pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('user', $1::uuid, $2::uuid, 'admin')`, f.userID, f.appID))
	if status, code := launchStatus(t, f.base, f.userTok, f.appID); status != http.StatusCreated {
		t.Fatalf("personal entitlement alone did not permit a launch: %d (%s)", status, code)
	}

	// Now add the 'all' row too: the user holds both shapes for one app.
	must(t, execEnt(ctx, pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('all', NULL, $1::uuid, 'migration')`, f.appID))
	if status, code := launchStatus(t, f.base, f.userTok, f.appID); status != http.StatusCreated {
		t.Fatalf("holding BOTH entitlement shapes broke the launch: %d (%s)", status, code)
	}
}

// TestSwapRejectsUnentitledApp closes the two-request bypass. Swap replaces the
// app a live session runs; without a gate on the TARGET, a user launches
// something they are entitled to and then swaps to something they are not, and
// the launch check never sees the second app.
//
// Not named in §6.3's list — see Store.IsEntitled for why it is here anyway.
func TestSwapRejectsUnentitledApp(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	f := newEntLaunchFixture(t, pool)

	resp := doJSON(t, "POST", f.base+"/v1/sessions", f.userTok, map[string]any{"app_id": f.openAppID})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("seed launch: got %d, want 201", resp.StatusCode)
	}
	var created struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	// Drive it to running so it is swappable (the same shortcut swap_test uses).
	must(t, execEnt(ctx, pool, `UPDATE sessions SET state='running' WHERE id::text = $1`, created.Session.ID))

	sw := doJSON(t, "POST", f.base+"/v1/sessions/"+created.Session.ID+"/swap", f.userTok,
		map[string]any{"app_id": f.appID})
	code := sw.StatusCode
	sw.Body.Close()
	if code != http.StatusForbidden {
		t.Fatalf("swap into an unentitled app: got %d, want 403", code)
	}

	// The session still runs its original app — a refused swap is a no-op.
	var appID string
	must(t, pool.QueryRow(ctx, `SELECT app_id::text FROM sessions WHERE id::text = $1`, created.Session.ID).Scan(&appID))
	if appID != f.openAppID {
		t.Errorf("a refused swap changed the session's app to %s", appID)
	}

	// Granting the target makes the same swap succeed, so the 403 above is the
	// entitlement gate and not some unrelated swap precondition.
	must(t, execEnt(ctx, pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('all', NULL, $1::uuid, 'migration')`, f.appID))
	sw = doJSON(t, "POST", f.base+"/v1/sessions/"+created.Session.ID+"/swap", f.userTok,
		map[string]any{"app_id": f.appID})
	code = sw.StatusCode
	sw.Body.Close()
	if code != http.StatusAccepted {
		t.Errorf("swap into a newly entitled app: got %d, want 202", code)
	}
}

// TestUnentitledLaunchIs403BeforeAnyProfileGate is the ORDERING fix (review
// finding 2).
//
// Two gates in LaunchByProfile return before ScheduleAndCreate is ever reached —
// ErrProfileOverrideDisabled and ErrProfileNotLaunchableForApp — so before the
// pre-check was added, a caller holding a UUID they had no entitlement for could
// tell "this app offers that profile" (some other error) from "it does not"
// (409) and enumerate the whole allow-list. The transactional FOR SHARE check
// still ran, so nothing could actually LAUNCH; what leaked was the answers.
//
// The assertion is deliberately the same 403 for every profile_id shape, so the
// endpoint returns no information at all about an app the caller cannot see.
func TestUnentitledLaunchIs403BeforeAnyProfileGate(t *testing.T) {
	pool := testDB(t)
	f := newEntLaunchFixture(t, pool)

	// The unentitled app offers ONLY 720p60. 1080p60 exists and is user-visible,
	// so it is a well-formed id the allow-list refuses — the exact input that used
	// to answer 409.
	allowLaunchProfiles(t, pool, f.appID, "720p60")

	cases := []struct {
		name string
		body map[string]any
	}{
		{"no profile_id", map[string]any{"app_id": f.appID}},
		{"an offered profile_id", map[string]any{"app_id": f.appID, "profile_id": "720p60"}},
		{"a profile_id the app does NOT offer", map[string]any{"app_id": f.appID, "profile_id": "1080p60"}},
		{"a profile_id that does not exist", map[string]any{"app_id": f.appID, "profile_id": "no-such-profile"}},
		{"an explicit stream override", map[string]any{"app_id": f.appID, "stream": map[string]any{"fps": 30}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := doJSON(t, "POST", f.base+"/v1/sessions", f.userTok, c.body)
			code := resp.StatusCode
			resp.Body.Close()
			if code != http.StatusForbidden {
				t.Errorf("got %d, want 403 — an unentitled caller learned something about this app's launch profiles", code)
			}
		})
	}

	// Control: with the entitlement granted, the profile gates come back to life
	// and the SAME disallowed profile_id is a 409 again. Without this the test
	// would also pass if the allow-list had simply stopped being enforced.
	must(t, execEnt(context.Background(), pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('all', NULL, $1::uuid, 'migration')`, f.appID))
	resp := doJSON(t, "POST", f.base+"/v1/sessions", f.userTok,
		map[string]any{"app_id": f.appID, "profile_id": "1080p60"})
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusConflict {
		t.Errorf("entitled + disallowed profile: got %d, want 409 (the allow-list gate must still work)", code)
	}
}

// TestProfilesMenuIs404ForAnUnentitledApp is review finding 3.
//
// GET /v1/me/profiles?app_id=… promises "the SAME visibility rule as
// GET /v1/apps/{id}", which since Phase 2 includes entitlement. Before the fix
// it was a working existence oracle (404 for a bogus UUID, 200 for a real one)
// plus an allow-list dump, for any authenticated caller. In Phase 4 an app UUID
// IS a per-user game tile, so "does this UUID resolve" becomes "does another
// user own this game".
func TestProfilesMenuIs404ForAnUnentitledApp(t *testing.T) {
	pool := testDB(t)
	f := newEntLaunchFixture(t, pool)
	allowLaunchProfiles(t, pool, f.appID, "720p60")

	// Unentitled app: indistinguishable from an id that does not exist.
	unentitled := doJSON(t, "GET", f.base+"/v1/me/profiles?app_id="+f.appID, f.userTok, nil)
	unentitledCode := unentitled.StatusCode
	unentitled.Body.Close()
	if unentitledCode != http.StatusNotFound {
		t.Errorf("unentitled app: got %d, want 404", unentitledCode)
	}

	bogus := doJSON(t, "GET", f.base+"/v1/me/profiles?app_id=00000000-0000-0000-0000-0000000000ff", f.userTok, nil)
	bogusCode := bogus.StatusCode
	bogus.Body.Close()
	if bogusCode != unentitledCode {
		t.Errorf("an unentitled app (%d) is distinguishable from a nonexistent one (%d) — still an existence oracle",
			unentitledCode, bogusCode)
	}

	// An ADMIN gets the same 404: no role bypass here either (§6.5).
	adminResp := doJSON(t, "GET", f.base+"/v1/me/profiles?app_id="+f.appID, f.adminTok, nil)
	adminCode := adminResp.StatusCode
	adminResp.Body.Close()
	if adminCode != http.StatusNotFound {
		t.Errorf("admin on an unentitled app: got %d, want 404", adminCode)
	}

	// Entitled: 200, and the menu is genuinely narrowed to what the app offers —
	// so the 404s above are the entitlement clause, not the endpoint being broken.
	must(t, execEnt(context.Background(), pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('all', NULL, $1::uuid, 'migration')`, f.appID))
	ok := doJSON(t, "GET", f.base+"/v1/me/profiles?app_id="+f.appID, f.userTok, nil)
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("entitled app: got %d, want 200", ok.StatusCode)
	}
	var menu struct {
		Profiles []struct {
			ID string `json:"id"`
		} `json:"profiles"`
	}
	if err := json.NewDecoder(ok.Body).Decode(&menu); err != nil {
		t.Fatalf("decode profiles: %v", err)
	}
	if len(menu.Profiles) != 1 || menu.Profiles[0].ID != "720p60" {
		t.Errorf("entitled menu = %+v, want exactly the app's allow-list [720p60]", menu.Profiles)
	}

	// The no-app_id form is unchanged: it is a per-user capability question, not
	// an app question, so it never had an entitlement to check.
	all := doJSON(t, "GET", f.base+"/v1/me/profiles", f.userTok, nil)
	allCode := all.StatusCode
	all.Body.Close()
	if allCode != http.StatusOK {
		t.Errorf("GET /v1/me/profiles with no app_id: got %d, want 200", allCode)
	}
}

// TestRevokeBlocksTheNextLaunchImmediately is the concurrency-adjacent
// assertion the FOR SHARE lock exists for, in its observable form: once a revoke
// has COMMITTED, no launch after it can succeed. (The interleaving the lock
// rules out — check passes, revoke commits, session lands — is not reproducible
// from a test without injecting a pause inside the transaction; what is testable
// is that there is no cached or stale read on the launch path.)
func TestRevokeBlocksTheNextLaunchImmediately(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	f := newEntLaunchFixture(t, pool)

	if status, _ := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusCreated {
		t.Fatalf("pre-revoke launch should succeed")
	}
	must(t, execEnt(ctx, pool, `DELETE FROM entitlements WHERE app_id::text = $1`, f.openAppID))
	if status, _ := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusForbidden {
		t.Errorf("post-revoke launch: got %d, want 403", status)
	}
}
