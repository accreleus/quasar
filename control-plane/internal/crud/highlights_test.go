package crud

// Integration tests for GET /v1/me/highlights (home-rail amendment, 2026-08-05).
// Require Postgres: run via `make test-db` (or scripts/dev/dev.sh go-test-db), which
// sets TEST_DATABASE_URL. Without it testDB t.Skip()s and NONE of this runs — a
// green `go test` with no database proves nothing about this file.
//
// Uses the same testDB / newTestServer / getReq helpers as handler_test.go (same
// package). Apps, entitlements and sessions are seeded with direct SQL rather than
// through the admin API because every test here turns on exact timestamps
// (apps.created_at ordering, session windows, the play-time clamp), and the API
// stamps now() for all of them.

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// --- fixtures ---------------------------------------------------------------

type hlFixture struct {
	pool   *pgxpool.Pool
	srvURL string
	token  string
	userID string
}

// newHighlightsFixture registers one ordinary (non-admin) user and returns their
// bearer token. The rail is a user surface: nothing in this file needs an admin,
// and that is itself part of the contract (RequireAuth only, no admin variant).
func newHighlightsFixture(t *testing.T) hlFixture {
	t.Helper()
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	user, err := authSvc.Register(ctx, "rail@test.local", "rail", "quasar-fixture-pw-04")
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	tok, err := authSvc.Login(ctx, "rail@test.local", "quasar-fixture-pw-04", "")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}
	return hlFixture{pool: pool, srvURL: srv.URL, token: tok.Plaintext, userID: user.ID}
}

// seedApp inserts an enabled app plus the ('all', NULL) entitlement that
// createApp's default `entitle: "all"` would have written, so the app is visible
// to the fixture user. createdAt drives the recently_added ordering.
func (f hlFixture) seedApp(t *testing.T, name string, createdAt time.Time) string {
	t.Helper()
	var appID string
	err := f.pool.QueryRow(context.Background(), `
		INSERT INTO apps (name, created_at,
			default_vram_mb, default_encode_slots,
			default_width, default_height, default_fps, default_bitrate_kbps)
		VALUES ($1, $2, 1024, 1, 1280, 720, 60, 6000)
		RETURNING id::text`, name, createdAt).Scan(&appID)
	if err != nil {
		t.Fatalf("seed app %q: %v", name, err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('all', NULL, $1::uuid, 'admin')`, appID); err != nil {
		t.Fatalf("seed entitlement for %q: %v", name, err)
	}
	return appID
}

// revokeEntitlements deletes every entitlement for an app — the storage effect of
// an admin revoking access via DELETE /v1/admin/apps/{id}/entitlements/{eid}.
// Session rows are deliberately left alone: that a session OUTLIVES the
// entitlement that authorised it is the whole premise of the filter.
func (f hlFixture) revokeEntitlements(t *testing.T, appID string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`DELETE FROM entitlements WHERE app_id = $1::uuid`, appID); err != nil {
		t.Fatalf("revoke entitlements: %v", err)
	}
}

// seedSession inserts one session for the fixture user. started/ended may be nil.
// createdAt orders the live pick.
func (f hlFixture) seedSession(t *testing.T, appID, state string, createdAt time.Time, started, ended *time.Time) string {
	t.Helper()
	var id string
	err := f.pool.QueryRow(context.Background(), `
		INSERT INTO sessions (user_id, app_id, state, created_at, started_at, ended_at,
			width, height, fps, bitrate_kbps)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, 1280, 720, 60, 6000)
		RETURNING id::text`,
		f.userID, appID, state, createdAt, started, ended).Scan(&id)
	if err != nil {
		t.Fatalf("seed session (%s): %v", state, err)
	}
	return id
}

// highlights calls GET /v1/me/highlights[?query] and returns the status plus the
// decoded items array.
func (f hlFixture) highlights(t *testing.T, query string) (int, []map[string]any) {
	t.Helper()
	url := f.srvURL + "/v1/me/highlights" + query
	resp, body := getReq(t, url, f.token)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	raw, ok := body["items"]
	if !ok {
		t.Fatalf("response has no items key: %v", body)
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("items is not an array: %#v", raw)
	}
	items := make([]map[string]any, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("item is not an object: %#v", e)
		}
		items = append(items, m)
	}
	return resp.StatusCode, items
}

func ptime(tm time.Time) *time.Time { return &tm }

// appIDs pulls the app_id column out of a rail, in order.
func appIDs(items []map[string]any) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, fmt.Sprint(it["app_id"]))
	}
	return out
}

// --- tests ------------------------------------------------------------------

// TestHighlightsEmptyHistoryIsEmptyRail is the contract's empty-history rule:
// "`items` … is empty for a user who has never launched anything". The apps exist
// and are entitled and freshly created — recently_added has every input it needs —
// and the rail is STILL empty, because recently_added is a filler for a user with
// history, not a fallback rail for a user without one.
func TestHighlightsEmptyHistoryIsEmptyRail(t *testing.T) {
	f := newHighlightsFixture(t)
	now := time.Now()
	f.seedApp(t, "brand-new-a", now.Add(-time.Hour))
	f.seedApp(t, "brand-new-b", now.Add(-2*time.Hour))

	status, items := f.highlights(t, "")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	if len(items) != 0 {
		t.Fatalf("empty history must produce an empty rail, got %d item(s): %v", len(items), items)
	}
}

// TestHighlightsLiveCarriesSessionFields: the live reason populates session_id and
// session_started_at; every other reason leaves BOTH null. Both keys are always
// present (the contract has them in `required`), so absent is a failure too.
func TestHighlightsLiveCarriesSessionFields(t *testing.T) {
	f := newHighlightsFixture(t)
	now := time.Now()

	liveApp := f.seedApp(t, "live-app", now.Add(-72*time.Hour))
	pastApp := f.seedApp(t, "past-app", now.Add(-72*time.Hour))

	liveSessionID := f.seedSession(t, liveApp, "running", now.Add(-30*time.Minute),
		ptime(now.Add(-30*time.Minute)), nil)
	f.seedSession(t, pastApp, "stopped", now.Add(-5*time.Hour),
		ptime(now.Add(-5*time.Hour)), ptime(now.Add(-4*time.Hour)))

	status, items := f.highlights(t, "")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 items, got %d: %v", len(items), items)
	}

	live := items[0]
	if live["app_id"] != liveApp {
		t.Fatalf("live app must rank first, got %v", appIDs(items))
	}
	if live["reason"] != reasonLive {
		t.Fatalf("reason: want %q, got %v", reasonLive, live["reason"])
	}
	if live["session_id"] != liveSessionID {
		t.Fatalf("session_id: want %q, got %v", liveSessionID, live["session_id"])
	}
	if live["session_started_at"] == nil {
		t.Fatal("session_started_at must be populated for reason=live")
	}

	other := items[1]
	if other["reason"] == reasonLive {
		t.Fatalf("second item must not be live: %v", other)
	}
	for _, k := range []string{"session_id", "session_started_at"} {
		v, present := other[k]
		if !present {
			t.Fatalf("%s must always be serialized (it is in the contract's required list): %v", k, other)
		}
		if v != nil {
			t.Fatalf("%s must be null for reason=%v, got %v", k, other["reason"], v)
		}
	}
}

// TestHighlightsOneCardPerApp: an app that is BOTH live and the week's most-played
// appears exactly once, as live. This is the duplicate case the spec's §4 item 7
// left open and the contract closed.
func TestHighlightsOneCardPerApp(t *testing.T) {
	f := newHighlightsFixture(t)
	now := time.Now()

	busy := f.seedApp(t, "busy-app", now.Add(-72*time.Hour))
	// Lots of play time in the window …
	f.seedSession(t, busy, "stopped", now.Add(-48*time.Hour),
		ptime(now.Add(-48*time.Hour)), ptime(now.Add(-42*time.Hour)))
	// … and live right now.
	f.seedSession(t, busy, "running", now.Add(-10*time.Minute),
		ptime(now.Add(-10*time.Minute)), nil)

	status, items := f.highlights(t, "")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	n := 0
	for _, it := range items {
		if it["app_id"] == busy {
			n++
			if it["reason"] != reasonLive {
				t.Fatalf("the live reason must win: got %v", it["reason"])
			}
		}
	}
	if n != 1 {
		t.Fatalf("one card per app: got %d cards for the same app: %v", n, items)
	}
}

// TestHighlightsExcludesRevokedEntitlement is THE security-shaped test.
//
// Session rows outlive the entitlement that authorised them — revoking does not
// delete history — so a rail that ranks over `sessions` without re-applying the
// entitled + enabled predicate resurrects a revoked title onto the user's home
// page with its play time attached.
func TestHighlightsExcludesRevokedEntitlement(t *testing.T) {
	f := newHighlightsFixture(t)
	now := time.Now()

	revoked := f.seedApp(t, "soon-revoked", now.Add(-72*time.Hour))
	kept := f.seedApp(t, "still-mine", now.Add(-72*time.Hour))

	// Real history for both: several hours on the app that is about to be revoked.
	f.seedSession(t, revoked, "stopped", now.Add(-30*time.Hour),
		ptime(now.Add(-30*time.Hour)), ptime(now.Add(-26*time.Hour)))
	f.seedSession(t, revoked, "running", now.Add(-5*time.Minute),
		ptime(now.Add(-5*time.Minute)), nil)
	f.seedSession(t, kept, "stopped", now.Add(-20*time.Hour),
		ptime(now.Add(-20*time.Hour)), ptime(now.Add(-19*time.Hour)))

	// Before: the app is on the rail.
	status, items := f.highlights(t, "")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	found := false
	for _, id := range appIDs(items) {
		if id == revoked {
			found = true
		}
	}
	if !found {
		t.Fatalf("precondition: the entitled app should be on the rail, got %v", appIDs(items))
	}

	f.revokeEntitlements(t, revoked)

	// After: gone — sessions untouched, entitlement gone, card gone.
	status, items = f.highlights(t, "")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	for _, it := range items {
		if it["app_id"] == revoked {
			t.Fatalf("a REVOKED app is on the rail (reason=%v, play_seconds=%v) — the entitlement filter is missing",
				it["reason"], it["play_seconds"])
		}
	}
	// And the still-entitled app is unaffected: the filter narrows, it does not
	// empty the rail.
	if len(items) == 0 {
		t.Fatal("the still-entitled app must remain on the rail")
	}
	if items[0]["app_id"] != kept {
		t.Fatalf("want the still-entitled app, got %v", appIDs(items))
	}

	// Belt and braces: the session rows really are still there, so the exclusion
	// came from the predicate and not from the fixture deleting history.
	var sessions int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sessions WHERE app_id = $1::uuid`, revoked).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 2 {
		t.Fatalf("the revoke must not delete history: want 2 session rows, got %d", sessions)
	}
}

// TestHighlightsDisabledAppExcluded: the predicate is entitled AND enabled, the
// same pair GET /v1/apps applies. An admin disabling an app takes it off the rail
// too.
func TestHighlightsDisabledAppExcluded(t *testing.T) {
	f := newHighlightsFixture(t)
	now := time.Now()

	app := f.seedApp(t, "to-be-disabled", now.Add(-72*time.Hour))
	f.seedSession(t, app, "stopped", now.Add(-10*time.Hour),
		ptime(now.Add(-10*time.Hour)), ptime(now.Add(-9*time.Hour)))

	if _, err := f.pool.Exec(context.Background(),
		`UPDATE apps SET enabled = false WHERE id = $1::uuid`, app); err != nil {
		t.Fatalf("disable app: %v", err)
	}

	status, items := f.highlights(t, "")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	if len(items) != 0 {
		t.Fatalf("a disabled app must not be on the rail: %v", items)
	}
}

// TestHighlightsWindowDaysBounds: 1 and 90 are accepted, 0 / 91 / "abc" are 400
// validation_failed. Out-of-range is refused rather than clamped — a caller asking
// for a year has a wrong idea about the surface and should be told.
func TestHighlightsWindowDaysBounds(t *testing.T) {
	f := newHighlightsFixture(t)

	for _, q := range []string{"?window_days=1", "?window_days=90", "?window_days=7", ""} {
		status, _ := f.highlights(t, q)
		if status != http.StatusOK {
			t.Fatalf("%q: want 200, got %d", q, status)
		}
	}

	for _, q := range []string{"?window_days=0", "?window_days=91", "?window_days=abc",
		"?window_days=-1", "?window_days=7.5"} {
		resp, body := getReq(t, f.srvURL+"/v1/me/highlights"+q, f.token)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%q: want 400, got %d (%v)", q, resp.StatusCode, body)
		}
		errObj, ok := body["error"].(map[string]any)
		if !ok || errObj["code"] != "validation_failed" {
			t.Fatalf("%q: want code validation_failed, got %v", q, body)
		}
	}
}

// TestHighlightsWindowDaysNarrowsMostPlayed: window_days is not decorative. With a
// 90-day window the older-but-longer session wins most_played; with the default
// 7-day window it is out of scope and the recent one wins.
func TestHighlightsWindowDaysNarrowsMostPlayed(t *testing.T) {
	f := newHighlightsFixture(t)
	now := time.Now()

	old := f.seedApp(t, "old-marathon", now.Add(-200*time.Hour))
	recent := f.seedApp(t, "recent-session", now.Add(-200*time.Hour))

	// 8 hours, 30 days ago (clamped-safe, well under 24 h).
	f.seedSession(t, old, "stopped", now.Add(-30*24*time.Hour),
		ptime(now.Add(-30*24*time.Hour)), ptime(now.Add(-30*24*time.Hour+8*time.Hour)))
	// 1 hour, 2 days ago.
	f.seedSession(t, recent, "stopped", now.Add(-48*time.Hour),
		ptime(now.Add(-48*time.Hour)), ptime(now.Add(-47*time.Hour)))

	_, items := f.highlights(t, "")
	if len(items) == 0 || items[0]["reason"] != reasonMostPlayed || items[0]["app_id"] != recent {
		t.Fatalf("default window: want most_played=%s, got %v", recent, items)
	}

	_, items = f.highlights(t, "?window_days=90")
	if len(items) == 0 || items[0]["reason"] != reasonMostPlayed || items[0]["app_id"] != old {
		t.Fatalf("90-day window: want most_played=%s, got %v", old, items)
	}
}

// TestHighlightsPlaySecondsClampAndFailedExclusion covers both halves of the
// play_seconds rule:
//
//   - state='failed' contributes NOTHING, however plausible its timestamps.
//   - a NULL ended_at on a non-live row is an UNRECONCILED session, not an
//     open-ended one: it is clamped to 24 h rather than counting the days since it
//     was abandoned.
func TestHighlightsPlaySecondsClampAndFailedExclusion(t *testing.T) {
	f := newHighlightsFixture(t)
	now := time.Now()

	abandoned := f.seedApp(t, "abandoned-app", now.Add(-200*time.Hour))
	failedOnly := f.seedApp(t, "failed-only-app", now.Add(-200*time.Hour))

	// Started 20 days ago, never reconciled to a terminal state: state is terminal
	// but ended_at is NULL. Unclamped this is ~1.7 M seconds.
	f.seedSession(t, abandoned, "stopped", now.Add(-20*24*time.Hour),
		ptime(now.Add(-20*24*time.Hour)), nil)
	// A failed launch with a full, believable 3-hour timestamp pair.
	f.seedSession(t, failedOnly, "failed", now.Add(-6*time.Hour),
		ptime(now.Add(-6*time.Hour)), ptime(now.Add(-3*time.Hour)))

	// A 90-day window so the abandoned session is inside it.
	status, items := f.highlights(t, "?window_days=90")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}

	byApp := map[string]map[string]any{}
	for _, it := range items {
		byApp[fmt.Sprint(it["app_id"])] = it
	}

	ab, ok := byApp[abandoned]
	if !ok {
		t.Fatalf("the abandoned app should still be on the rail: %v", items)
	}
	got := int64(ab["play_seconds"].(float64))
	const dayseconds = 24 * 60 * 60
	if got != dayseconds {
		t.Fatalf("play_seconds: want the 24 h clamp (%d), got %d — a NULL ended_at is contributing open-ended time",
			dayseconds, got)
	}

	fa, ok := byApp[failedOnly]
	if !ok {
		t.Fatalf("the failed-only app should appear (as recently_added): %v", items)
	}
	if v := int64(fa["play_seconds"].(float64)); v != 0 {
		t.Fatalf("play_seconds for a failed-only app: want 0, got %d", v)
	}
	// A failed launch is also nothing to "resume", and it never completed, so
	// last_played_at stays null.
	if fa["reason"] == reasonResume || fa["reason"] == reasonMostPlayed {
		t.Fatalf("a failed session must not produce reason %v", fa["reason"])
	}
	if fa["last_played_at"] != nil {
		t.Fatalf("last_played_at must exclude failed sessions, got %v", fa["last_played_at"])
	}
}

// TestHighlightsAtMostFive: more qualifying apps than slots yields exactly five,
// and the maxItems in the contract holds.
func TestHighlightsAtMostFive(t *testing.T) {
	f := newHighlightsFixture(t)
	now := time.Now()

	for i := 0; i < 8; i++ {
		app := f.seedApp(t, fmt.Sprintf("played-app-%d", i), now.Add(-200*time.Hour))
		start := now.Add(-time.Duration(i+1) * time.Hour)
		f.seedSession(t, app, "stopped", start, ptime(start), ptime(start.Add(30*time.Minute)))
	}

	status, items := f.highlights(t, "")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	if len(items) != highlightLimit {
		t.Fatalf("want %d items, got %d", highlightLimit, len(items))
	}
	seen := map[string]bool{}
	for _, id := range appIDs(items) {
		if seen[id] {
			t.Fatalf("duplicate app on the rail: %v", appIDs(items))
		}
		seen[id] = true
	}
}

// TestHighlightsReasonPriority walks the whole ladder in one rail:
// live → most_played → resume → recently_added, one card per app, and
// recently_added filling the remaining slots only because history exists.
func TestHighlightsReasonPriority(t *testing.T) {
	f := newHighlightsFixture(t)
	now := time.Now()

	liveApp := f.seedApp(t, "z-live", now.Add(-500*time.Hour))
	mostApp := f.seedApp(t, "z-most", now.Add(-500*time.Hour))
	resumeApp := f.seedApp(t, "z-resume", now.Add(-500*time.Hour))
	// Newest app, never played — the filler.
	newApp := f.seedApp(t, "z-new", now)

	f.seedSession(t, liveApp, "running", now.Add(-time.Minute), ptime(now.Add(-time.Minute)), nil)
	// 6 hours in the window: the most played.
	f.seedSession(t, mostApp, "stopped", now.Add(-40*time.Hour),
		ptime(now.Add(-40*time.Hour)), ptime(now.Add(-34*time.Hour)))
	// 20 minutes, more recent but far less play time.
	f.seedSession(t, resumeApp, "stopped", now.Add(-3*time.Hour),
		ptime(now.Add(-3*time.Hour)), ptime(now.Add(-3*time.Hour+20*time.Minute)))

	status, items := f.highlights(t, "")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	if len(items) != 4 {
		t.Fatalf("want 4 items, got %d: %v", len(items), items)
	}
	want := []struct {
		appID  string
		reason string
	}{
		{liveApp, reasonLive},
		{mostApp, reasonMostPlayed},
		{resumeApp, reasonResume},
		{newApp, reasonRecentlyAdded},
	}
	for i, w := range want {
		if items[i]["app_id"] != w.appID || items[i]["reason"] != w.reason {
			t.Fatalf("item %d: want (%s, %s), got (%v, %v)",
				i, w.appID, w.reason, items[i]["app_id"], items[i]["reason"])
		}
	}

	// last_played_at is MAX(ended_at) over non-failed sessions and is therefore
	// null for the app whose only session is the live one.
	if items[0]["last_played_at"] != nil {
		t.Fatalf("last_played_at must be null for an app never played to completion, got %v",
			items[0]["last_played_at"])
	}
	if items[1]["last_played_at"] == nil {
		t.Fatal("last_played_at must be set for an app with a completed session")
	}
}

// TestHighlightsRequiresAuth: no bearer is 401, not an empty rail.
func TestHighlightsRequiresAuth(t *testing.T) {
	f := newHighlightsFixture(t)
	resp, _ := getReq(t, f.srvURL+"/v1/me/highlights", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

// TestHighlightsAreCallerScoped: another user's sessions never leak onto this
// caller's rail. The subject is the bearer identity and there is no user_id
// parameter, so this is structural — but it is the property that structure exists
// to provide, so it is asserted.
func TestHighlightsAreCallerScoped(t *testing.T) {
	f := newHighlightsFixture(t)
	ctx := context.Background()
	now := time.Now()

	var otherID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO users (email, username, password_hash)
		VALUES ('other@test.local','other','x') RETURNING id::text`).Scan(&otherID); err != nil {
		t.Fatalf("seed other user: %v", err)
	}

	app := f.seedApp(t, "theirs", now.Add(-72*time.Hour))
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO sessions (user_id, app_id, state, created_at, started_at, ended_at,
			width, height, fps, bitrate_kbps)
		VALUES ($1::uuid, $2::uuid, 'stopped', $3, $3, $4, 1280, 720, 60, 6000)`,
		otherID, app, now.Add(-4*time.Hour), now.Add(-3*time.Hour)); err != nil {
		t.Fatalf("seed other user's session: %v", err)
	}

	status, items := f.highlights(t, "")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	if len(items) != 0 {
		t.Fatalf("another user's history must not reach this rail: %v", items)
	}
}
