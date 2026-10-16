package crud

// Amendment 38 (#720): per-title facts on the app read shapes and
// GET /v1/me/play-history. DB-backed: run via `make test-db`.

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"
)

type twoUsers struct {
	hlFixture
	otherToken  string
	otherUserID string
	adminToken  string
}

func newTwoUsers(t *testing.T) twoUsers {
	t.Helper()
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	login := func(email, name, pw string) (string, string) {
		u, err := authSvc.Register(ctx, email, name, pw)
		if err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		tok, err := authSvc.Login(ctx, email, pw, "")
		if err != nil {
			t.Fatalf("login %s: %v", name, err)
		}
		return u.ID, tok.Plaintext
	}
	aID, aTok := login("a@test.local", "usera", "quasar-fixture-pw-11")
	bID, bTok := login("b@test.local", "userb", "quasar-fixture-pw-12")
	login("admin@test.local", "admin", "quasar-fixture-pw-01")
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = 'admin@test.local'`); err != nil {
		t.Fatal(err)
	}
	adminTok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatal(err)
	}
	return twoUsers{
		hlFixture:  hlFixture{pool: pool, srvURL: srv.URL, token: aTok, userID: aID},
		otherToken: bTok, otherUserID: bID, adminToken: adminTok.Plaintext,
	}
}

func (f twoUsers) playHistory(t *testing.T, token string) []map[string]any {
	t.Helper()
	resp, body := getReq(t, f.srvURL+"/v1/me/play-history", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("play-history = %d (%v)", resp.StatusCode, body)
	}
	list, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items is not an array: %#v", body["items"])
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		out = append(out, e.(map[string]any))
	}
	return out
}

func (f twoUsers) sessionFor(t *testing.T, userID, appID, state string, started, ended *time.Time) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO sessions (user_id, app_id, state, started_at, ended_at, width, height, fps, bitrate_kbps)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, 1280, 720, 60, 6000)`,
		userID, appID, state, started, ended); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func parseTime(t *testing.T, v any) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339Nano, v.(string))
	if err != nil {
		t.Fatalf("time %v: %v", v, err)
	}
	return tm
}

func near(a, b time.Time) bool { return a.Sub(b).Abs() < 5*time.Second }

func TestPlayHistoryCountsSessionsThatRanAndNotFailed(t *testing.T) {
	f := newTwoUsers(t)
	now := time.Now()
	app := f.seedApp(t, "Counted", now.Add(-time.Hour))
	h := func(d time.Duration) *time.Time { return ptime(now.Add(d)) }

	f.sessionFor(t, f.userID, app, "stopped", h(-3*time.Hour), h(-2*time.Hour))       // 3600 s
	f.sessionFor(t, f.userID, app, "stopped", h(-90*time.Minute), h(-90*time.Minute)) // 0 s, still a session
	f.sessionFor(t, f.userID, app, "failed", h(-5*time.Hour), h(-4*time.Hour))        // ran, then failed: excluded
	f.sessionFor(t, f.userID, app, "stopped", nil, h(-time.Minute))                   // never reached running
	f.sessionFor(t, f.userID, app, "running", h(-10*time.Minute), nil)                // live: 600 s so far

	items := f.playHistory(t, f.token)
	if len(items) != 1 || items[0]["app_id"] != app {
		t.Fatalf("items %v", items)
	}
	it := items[0]
	if it["sessions"] != float64(3) {
		t.Fatalf("sessions %v, want 3", it["sessions"])
	}
	if total := it["total_seconds"].(float64); total < 4195 || total > 4205 {
		t.Fatalf("total_seconds %v, want ~4200", total)
	}
	if !near(parseTime(t, it["last_played_at"]), now) {
		t.Fatalf("last_played_at %v, want now while a session is live", it["last_played_at"])
	}
}

func TestPlayHistoryClampsAnUnreconciledSessionTo24Hours(t *testing.T) {
	f := newTwoUsers(t)
	started := time.Now().Add(-72 * time.Hour)
	app := f.seedApp(t, "Orphaned", started)
	f.sessionFor(t, f.userID, app, "running", &started, nil)

	it := f.playHistory(t, f.token)[0]
	if it["total_seconds"] != float64(86400) || !near(parseTime(t, it["last_played_at"]), started.Add(24*time.Hour)) {
		t.Fatalf("%v", it)
	}
}

// User A never sees user B's history, and B sees only B's.
func TestPlayHistoryIsTheCallersOwn(t *testing.T) {
	f := newTwoUsers(t)
	now := time.Now()
	shared := f.seedApp(t, "Shared", now)
	onlyB := f.seedApp(t, "Only B", now)
	f.sessionFor(t, f.otherUserID, shared, "stopped", ptime(now.Add(-2*time.Hour)), ptime(now.Add(-time.Hour)))
	f.sessionFor(t, f.otherUserID, onlyB, "stopped", ptime(now.Add(-2*time.Hour)), ptime(now.Add(-time.Hour)))
	f.sessionFor(t, f.userID, shared, "stopped", ptime(now.Add(-30*time.Minute)), ptime(now.Add(-20*time.Minute)))

	a := f.playHistory(t, f.token)
	if len(a) != 1 || a[0]["app_id"] != shared || a[0]["sessions"] != float64(1) || a[0]["total_seconds"] != float64(600) {
		t.Fatalf("user A sees %v", a)
	}
	b := f.playHistory(t, f.otherToken)
	if len(b) != 2 {
		t.Fatalf("user B sees %v", b)
	}
	for _, it := range b {
		if it["total_seconds"] != float64(3600) {
			t.Fatalf("user B's item carries someone else's time: %v", it)
		}
	}
}

func TestPlayHistoryIsOrderedAndHidesRevokedTitles(t *testing.T) {
	f := newTwoUsers(t)
	now := time.Now()
	older := f.seedApp(t, "Older", now)
	newer := f.seedApp(t, "Newer", now)
	revoked := f.seedApp(t, "Revoked", now)
	f.sessionFor(t, f.userID, older, "stopped", ptime(now.Add(-5*time.Hour)), ptime(now.Add(-4*time.Hour)))
	f.sessionFor(t, f.userID, newer, "stopped", ptime(now.Add(-2*time.Hour)), ptime(now.Add(-time.Hour)))
	f.sessionFor(t, f.userID, revoked, "stopped", ptime(now.Add(-30*time.Minute)), ptime(now.Add(-10*time.Minute)))
	f.revokeEntitlements(t, revoked)

	if got := appIDs(f.playHistory(t, f.token)); !reflect.DeepEqual(got, []string{newer, older}) {
		t.Fatalf("order %v, want newer, older and no revoked title", got)
	}
}

func TestPlayHistoryEmptyAndUnauthenticated(t *testing.T) {
	f := newTwoUsers(t)
	if items := f.playHistory(t, f.token); len(items) != 0 {
		t.Fatalf("items %v", items)
	}
	if resp, _ := getReq(t, f.srvURL+"/v1/me/play-history", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", resp.StatusCode)
	}
}

func TestAppReadsCarryTitleFacts(t *testing.T) {
	f := newTwoUsers(t)
	ctx := context.Background()
	created := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	withFacts := f.seedApp(t, "Portal 2", created)
	bare := f.seedApp(t, "Bare", created)
	if _, err := f.pool.Exec(ctx, `
		UPDATE apps SET genres = '{Action,Adventure}', developer = 'Valve', publisher = 'Valve',
		       release_date = '18 Apr, 2011', controller_support = 'full',
		       logo_url = '/v1/artwork/l.png', icon_url = '/v1/artwork/i.png'
		 WHERE id::text = $1`, withFacts); err != nil {
		t.Fatal(err)
	}

	resp, body := getReq(t, f.srvURL+"/v1/apps/"+withFacts, f.token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get app = %d", resp.StatusCode)
	}
	app := body["app"].(map[string]any)
	want := map[string]any{"genres": []any{"Action", "Adventure"}, "developer": "Valve", "publisher": "Valve",
		"release_date": "18 Apr, 2011", "controller_support": "full",
		"logo_url": "/v1/artwork/l.png", "icon_url": "/v1/artwork/i.png"}
	for k, v := range want {
		if !reflect.DeepEqual(app[k], v) {
			t.Fatalf("%s = %#v, want %#v", k, app[k], v)
		}
	}
	if !parseTime(t, app["added_at"]).Equal(created) {
		t.Fatalf("added_at %v, want apps.created_at %v", app["added_at"], created)
	}

	resp, body = getReq(t, f.srvURL+"/v1/apps", f.token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list apps = %d", resp.StatusCode)
	}
	for _, raw := range body["items"].([]any) {
		it := raw.(map[string]any)
		if it["id"] != bare {
			continue
		}
		if !reflect.DeepEqual(it["genres"], []any{}) {
			t.Fatalf("unknown genres = %#v, want []", it["genres"])
		}
		for _, k := range []string{"developer", "publisher", "release_date", "controller_support", "logo_url", "icon_url"} {
			if v, present := it[k]; !present || v != nil {
				t.Fatalf("%s = %#v (present %v), want null", k, v, present)
			}
		}
	}

	resp, body = getReq(t, f.srvURL+"/v1/admin/apps", f.adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin list = %d", resp.StatusCode)
	}
	for _, raw := range body["items"].([]any) {
		if it := raw.(map[string]any); it["id"] == withFacts && it["developer"] != "Valve" {
			t.Fatalf("AdminApp lacks the facts: %v", it)
		}
	}

	// Read-only: not accepted on the write shape.
	resp, _ = patch(t, f.srvURL+"/v1/apps/"+withFacts, map[string]any{"genres": []string{"Puzzle"}}, f.adminToken)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PATCH genres = %d, want 400", resp.StatusCode)
	}
}
