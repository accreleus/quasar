package session

// Amendment 49 (#845): GET/PATCH /v1/me/region-input, the nine keys at launch
// and swap, and the parked-session stop on a change. DB-backed: `make test-db`.

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

func restoreRegionDefaultsAtEnd(t *testing.T, pool *pgxpool.Pool) {
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE instance_settings SET default_timezone = 'UTC',
			default_language = 'en-US', default_region_format = NULL, default_keyboard_layout = 'us',
			default_keyboard_variant = ''`)
	})
}

func TestRegionInputRouteReadsAndMerges(t *testing.T) {
	w := newWarmServer(t)
	resp, body := w.do(t, http.MethodGet, "/v1/me/region-input", w.token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d", resp.StatusCode)
	}
	region, input := body["region"].(map[string]any), body["input"].(map[string]any)
	for _, m := range []map[string]any{region, input} {
		for k, v := range m {
			if v != nil {
				t.Errorf("%s = %v for a user who saved nothing, want null", k, v)
			}
		}
	}
	if len(region) != 3 || len(input) != 6 {
		t.Errorf("GET region %v input %v, want every field", region, input)
	}
	defaults := body["defaults"].(map[string]any)
	dr, di := defaults["region"].(map[string]any), defaults["input"].(map[string]any)
	if dr["timezone"] != "UTC" || dr["language"] != "en-US" || dr["region_format"] != nil ||
		di["match_cursor"] != "auto" || di["mouse_sensitivity"] != float64(1) || di["raw_mouse"] != false {
		t.Errorf("defaults = %v", defaults)
	}
	if kb := di["keyboard"].(map[string]any); kb["layout"] != "us" || kb["variant"] != "" {
		t.Errorf("default keyboard = %v", kb)
	}
	if _, ok := body["parked_session_stopped"]; ok {
		t.Error("GET carries parked_session_stopped")
	}

	// A GET body echoed back is accepted: defaults and parked_session_stopped are ignored.
	body["region"] = map[string]any{"language": "de-DE"}
	body["input"] = map[string]any{"keyboard": map[string]any{"layout": "de", "variant": "nodeadkeys"}, "flip_scroll": true}
	body["parked_session_stopped"] = true
	resp, body = w.do(t, http.MethodPatch, "/v1/me/region-input", w.token, body)
	if resp.StatusCode != http.StatusOK || body["parked_session_stopped"] != false {
		t.Fatalf("PATCH = %d %v", resp.StatusCode, body)
	}
	resp, body = w.do(t, http.MethodPatch, "/v1/me/region-input", w.token,
		map[string]any{"region": map[string]any{"timezone": "Europe/Berlin", "language": nil}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d", resp.StatusCode)
	}
	region, input = body["region"].(map[string]any), body["input"].(map[string]any)
	if region["timezone"] != "Europe/Berlin" || region["language"] != nil || input["flip_scroll"] != true {
		t.Errorf("merged = %v %v", region, input)
	}
	if kb, _ := input["keyboard"].(map[string]any); kb["layout"] != "de" || kb["variant"] != "nodeadkeys" {
		t.Errorf("keyboard = %v, want kept", input["keyboard"])
	}

	// Anything invalid writes nothing.
	for _, bad := range []map[string]any{
		{"region": map[string]any{"timezone": "Local"}, "input": map[string]any{"flip_scroll": false}},
		{"input": map[string]any{"flip_scroll": false, "mouse_sensitivity": 9}},
		{"input": map[string]any{"flip_scroll": false}, "extra": 1},
		{"region": map[string]any{"timezone": "UTC", "zone": "x"}},
	} {
		if resp, _ := w.do(t, http.MethodPatch, "/v1/me/region-input", w.token, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("PATCH %v = %d, want 400", bad, resp.StatusCode)
		}
	}
	_, body = w.do(t, http.MethodGet, "/v1/me/region-input", w.token, nil)
	if body["input"].(map[string]any)["flip_scroll"] != true || body["region"].(map[string]any)["timezone"] != "Europe/Berlin" {
		t.Errorf("a refused PATCH wrote: %v", body)
	}
	if resp, _ := w.do(t, http.MethodGet, "/v1/me/region-input", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no bearer = %d, want 401", resp.StatusCode)
	}
}

func TestSavingARegionValueStopsOnlyTheIdleParkedSession(t *testing.T) {
	w := newWarmServer(t)
	patch := func(body map[string]any) map[string]any {
		t.Helper()
		resp, out := w.do(t, http.MethodPatch, "/v1/me/region-input", w.token, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PATCH = %d %v", resp.StatusCode, out)
		}
		return out
	}

	k := w.park(t)
	// A client field stops nothing.
	if out := patch(map[string]any{"input": map[string]any{"mouse_sensitivity": 2}}); out["parked_session_stopped"] != false {
		t.Errorf("mouse change stopped = %v", out["parked_session_stopped"])
	}
	if got := readRow(t, w.pool, k); got.State != "running" || w.disp.stopsFor(k) != 0 {
		t.Fatalf("parked row %+v after a client field, want running and no stop", got)
	}
	// Saving the same value again is no change.
	patch(map[string]any{"region": map[string]any{"timezone": nil}})
	if w.disp.stopsFor(k) != 0 {
		t.Fatal("an unchanged value stopped the parked session")
	}
	out := patch(map[string]any{"input": map[string]any{"keyboard": map[string]any{"layout": "gb", "variant": "extd"}}})
	if out["parked_session_stopped"] != true {
		t.Errorf("keyboard change stopped = %v, want true", out["parked_session_stopped"])
	}
	if got := readRow(t, w.pool, k); got.State != "stopping" || got.StopReason != StopReasonParkedSuperseded ||
		w.disp.stopReason(k) != StopReasonParkedSuperseded {
		t.Errorf("parked row %+v stop %q, want stopping with parked_superseded", got, w.disp.stopReason(k))
	}
	var suppressed bool
	must(t, w.pool.QueryRow(context.Background(), `SELECT steam_warm_suppressed FROM users WHERE id = $1::uuid`,
		w.userID).Scan(&suppressed))
	if suppressed {
		t.Error("the stop suppressed warm Steam")
	}

	// A session in use is never stopped.
	w.coord.AgentState(context.Background(), w.hostID, stoppedQualified(k))
	play := w.runningTile(t)
	out = patch(map[string]any{"region": map[string]any{"timezone": "Asia/Tokyo"}})
	if out["parked_session_stopped"] != false {
		t.Errorf("stopped = %v with only a play session", out["parked_session_stopped"])
	}
	if got := readRow(t, w.pool, play); got.State != "running" || w.disp.stopsFor(play) != 0 {
		t.Errorf("play row %+v, want running and never stopped", got)
	}
}

func stoppedQualified(id string) agentws.SessionStateMsg {
	return agentws.SessionStateMsg{SessionID: id, State: "stopped", HomeCleanupQualified: true}
}

func TestLaunchAndSwapCarryTheResolvedRegionValues(t *testing.T) {
	f := newWarmFixture(t, 8)
	ctx := context.Background()
	restoreRegionDefaultsAtEnd(t, f.pool)
	// The app's own TZ and LANG, and a key the user's values never touch.
	must(t, exec(t, f.pool, `UPDATE apps SET runtime_spec =
		'{"image":"desk:1","env":{"TZ":"Pacific/Auckland","LANG":"C","KEEP":"1"}}' WHERE id = $1::uuid`, f.appID))
	launch := func(appID string) (string, map[string]string) {
		t.Helper()
		res, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: appID, DeviceID: f.deviceID})
		if err != nil {
			t.Fatalf("launch: %v", err)
		}
		return res.Session.ID, envOf(t, f.disp.assignFor(t, res.Session.ID).App)
	}

	// Nothing reported or saved: the install defaults, over the app's own.
	id, env := launch(f.appID)
	if env["TZ"] != "UTC" || env["LANG"] != "en_US.UTF-8" || env["LC_TIME"] != "en_US.UTF-8" ||
		env["XKB_DEFAULT_LAYOUT"] != "us" || env["KEEP"] != "1" {
		t.Errorf("defaults env = %v", env)
	}
	if v, ok := env["XKB_DEFAULT_VARIANT"]; !ok || v != "" {
		t.Errorf("XKB_DEFAULT_VARIANT = %q present=%v, want the empty basic variant", v, ok)
	}
	for _, k := range []string{"LC_ALL", "LANGUAGE"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s set", k)
		}
	}
	f.ended(t, id)

	// The browser's reported values, then an admin default for what it does not report.
	must(t, exec(t, f.pool, `UPDATE user_devices SET capabilities = '{"timezone":"America/Chicago","language":"es-MX"}'
		WHERE id = $1::uuid`, f.deviceID))
	must(t, exec(t, f.pool, `UPDATE instance_settings SET default_timezone = 'Europe/Paris',
		default_region_format = 'en-GB', default_keyboard_layout = 'fr', default_keyboard_variant = 'azerty'`))
	id, env = launch(f.appID)
	if env["TZ"] != "America/Chicago" || env["LANG"] != "es_MX.UTF-8" || env["LC_PAPER"] != "en_GB.UTF-8" ||
		env["XKB_DEFAULT_LAYOUT"] != "fr" || env["XKB_DEFAULT_VARIANT"] != "azerty" {
		t.Errorf("browser + admin env = %v", env)
	}
	f.ended(t, id)

	// The user's values beat both, on a launch and on a swap.
	if _, _, err := f.store.SaveRegionInput(ctx, f.userID, func(r *RegionInput) {
		r.Region.Timezone, r.Region.RegionFormat = strp("Asia/Seoul"), strp("ko-KR")
	}); err != nil {
		t.Fatal(err)
	}
	id, env = launch(f.steam)
	if env["TZ"] != "Asia/Seoul" || env["LANG"] != "es_MX.UTF-8" || env["LC_NUMERIC"] != "ko_KR.UTF-8" ||
		env["STEAM_STARTUP_FLAGS"] != "-bigpicture" {
		t.Errorf("user env = %v", env)
	}
	f.disp.waitFor(t, "start")
	f.running(t, id)
	if _, _, err := f.store.SaveRegionInput(ctx, f.userID, func(r *RegionInput) { r.Region.Timezone = strp("Africa/Cairo") }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.coord.Swap(ctx, id, f.appID); err != nil {
		t.Fatalf("swap: %v", err)
	}
	f.disp.waitFor(t, "swap")
	f.disp.mu.Lock()
	swapped := f.disp.swaps[len(f.disp.swaps)-1]
	f.disp.mu.Unlock()
	if env := envOf(t, swapped.App); env["TZ"] != "Africa/Cairo" || env["LC_NUMERIC"] != "ko_KR.UTF-8" || env["KEEP"] != "1" {
		t.Errorf("swap env = %v, want the values resolved at the swap", env)
	}
}

func TestAParkedSessionIsAssignedTheRegionValues(t *testing.T) {
	f := newWarmFixture(t, 8)
	must(t, exec(t, f.pool, `UPDATE user_devices SET capabilities = '{"timezone":"Europe/Madrid"}' WHERE id = $1::uuid`, f.deviceID))
	k := f.park(t)
	if env := envOf(t, f.disp.assignFor(t, k).App); env["TZ"] != "Europe/Madrid" || env["STEAM_STARTUP_FLAGS"] != "-silent" {
		t.Errorf("parked env = %v", env)
	}
}

func TestMigration0108AddsRegionInputAndDropsIt(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, 106)
	pool, err := pgxpool.New(context.Background(), url)
	must(t, err)
	t.Cleanup(pool.Close)
	ctx := context.Background()
	must(t, exec(t, pool, `INSERT INTO instance_settings (id) VALUES (true) ON CONFLICT (id) DO NOTHING`))

	migrateTo(t, m, 108)
	var tz, lang, layout, variant, cursor string
	var format *string
	var sens float64
	must(t, pool.QueryRow(ctx, `SELECT default_timezone, default_language, default_region_format,
		default_keyboard_layout, default_keyboard_variant, default_match_cursor, default_mouse_sensitivity
		FROM instance_settings`).Scan(&tz, &lang, &format, &layout, &variant, &cursor, &sens))
	if tz != "UTC" || lang != "en-US" || format != nil || layout != "us" || variant != "" || cursor != "auto" || sens != 1 {
		t.Errorf("existing singleton = %s %s %v %s %q %s %v", tz, lang, format, layout, variant, cursor, sens)
	}
	var user string
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash) VALUES ('r@x', 'r', 'x')
		RETURNING id::text`).Scan(&user))
	must(t, exec(t, pool, `INSERT INTO user_region_input (user_id, timezone) VALUES ($1::uuid, 'Europe/Oslo')`, user))
	for _, bad := range []string{
		`UPDATE user_region_input SET language = 'de_DE'`,
		`UPDATE user_region_input SET keyboard_layout = 'de'`, // the pair is set together
		`UPDATE user_region_input SET mouse_sensitivity = 5`,
		`UPDATE instance_settings SET default_match_cursor = 'x'`,
	} {
		if err := exec(t, pool, bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	must(t, exec(t, pool, `DELETE FROM users WHERE id = $1::uuid`, user))
	var left int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM user_region_input`).Scan(&left))
	if left != 0 {
		t.Error("a deleted user's settings outlived the user")
	}

	migrateTo(t, m, 106)
	var exists bool
	must(t, pool.QueryRow(ctx, `SELECT to_regclass('user_region_input') IS NOT NULL OR EXISTS (
		SELECT 1 FROM information_schema.columns WHERE table_name = 'instance_settings'
		AND column_name LIKE 'default\_%' AND column_name <> 'default_profile_id')`).Scan(&exists))
	if exists {
		t.Error("the down left the table or a default column")
	}
}
