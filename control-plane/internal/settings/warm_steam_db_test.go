package settings

// Amendment 39 (#759): the three warm Steam instance settings
// (control-api.md §Warm Steam, "Admin settings"). DB-backed: run via
// `make test-db`.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// restoreWarmDefaults resets the three columns after the test: instance_settings
// is one shared row, and the other settings tests read it. Call right after
// testDB so the reset runs before the pool closes.
func restoreWarmDefaults(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `
			UPDATE instance_settings
			SET steam_warm_enabled = false, steam_warm_rewarm = 'library_open', steam_warm_idle_minutes = 10`); err != nil {
			t.Errorf("restore warm Steam defaults: %v", err)
		}
	})
}

func assertWarm(t *testing.T, st Settings, enabled bool, rewarm string, idle int) {
	t.Helper()
	if st.SteamWarmEnabled != enabled || st.SteamWarmRewarm != rewarm || st.SteamWarmIdleMinutes != idle {
		t.Errorf("warm settings = (%v, %q, %d), want (%v, %q, %d)",
			st.SteamWarmEnabled, st.SteamWarmRewarm, st.SteamWarmIdleMinutes, enabled, rewarm, idle)
	}
}

func TestSteamWarmDefaults(t *testing.T) {
	pool := testDB(t)
	restoreWarmDefaults(t, pool)

	// No singleton row yet: the store answers the defaults, not an error.
	st, err := NewStore(pool).Get(context.Background())
	must(t, err)
	assertWarm(t, st, false, "library_open", 10)

	_, get := newSettingsHarness(t, pool)
	assertWarm(t, get(t).Settings, false, "library_open", 10)
}

func TestPatchSteamWarmSetsEachField(t *testing.T) {
	pool := testDB(t)
	restoreWarmDefaults(t, pool)
	patch, get := newSettingsHarness(t, pool)

	for _, step := range []struct {
		body    string
		enabled bool
		rewarm  string
		idle    int
	}{
		{`{"steam_warm_enabled":true}`, true, "library_open", 10},
		{`{"steam_warm_rewarm":"next_steam_launch"}`, true, "next_steam_launch", 10},
		{`{"steam_warm_idle_minutes":45}`, true, "next_steam_launch", 45},
		{`{"registration_mode":"open"}`, true, "next_steam_launch", 45}, // absent stays unchanged
		{`{"steam_warm_idle_minutes":1}`, true, "next_steam_launch", 1},
		{`{"steam_warm_idle_minutes":60,"steam_warm_enabled":false,"steam_warm_rewarm":"library_open"}`, false, "library_open", 60},
	} {
		code, env := patch(t, step.body)
		if code != http.StatusOK {
			t.Fatalf("PATCH %s: status %d, want 200", step.body, code)
		}
		assertWarm(t, env.Settings, step.enabled, step.rewarm, step.idle)
		assertWarm(t, get(t).Settings, step.enabled, step.rewarm, step.idle)
	}
}

func TestPatchSteamWarmRejectsInvalidAndWritesNothing(t *testing.T) {
	pool := testDB(t)
	restoreWarmDefaults(t, pool)
	patch, get := newSettingsHarness(t, pool)

	if code, _ := patch(t, `{"steam_warm_enabled":true,"steam_warm_rewarm":"next_steam_launch","steam_warm_idle_minutes":20}`); code != http.StatusOK {
		t.Fatalf("setup PATCH: status %d, want 200", code)
	}

	for _, bad := range []string{
		`{"steam_warm_rewarm":"always"}`,
		`{"steam_warm_rewarm":""}`,
		`{"steam_warm_idle_minutes":0}`,
		`{"steam_warm_idle_minutes":61}`,
		`{"steam_warm_idle_minutes":-5}`,
		// A valid field beside an invalid one is not applied either.
		`{"steam_warm_enabled":false,"steam_warm_idle_minutes":0}`,
	} {
		if code, _ := patch(t, bad); code != http.StatusBadRequest {
			t.Errorf("PATCH %s: status %d, want 400", bad, code)
		}
		assertWarm(t, get(t).Settings, true, "next_steam_launch", 20)
	}
}

func TestSteamWarmPatchAuditsChangedKeysOnly(t *testing.T) {
	pool := testDB(t)
	restoreWarmDefaults(t, pool)
	patch, _ := auditedPatch(t, pool)

	if code := patch(`{"steam_warm_enabled":true,"steam_warm_rewarm":"next_steam_launch","steam_warm_idle_minutes":25}`); code != http.StatusOK {
		t.Fatalf("PATCH: status %d, want 200", code)
	}
	if code := patch(`{"steam_warm_idle_minutes":0}`); code != http.StatusBadRequest {
		t.Fatalf("invalid PATCH: status %d, want 400", code)
	}

	rows := settingsAuditRows(t, pool)
	if len(rows) != 1 {
		t.Fatalf("recorded %d rows, want 1 (the rejected PATCH is not audited)", len(rows))
	}
	if rows[0].Action != "instance.settings.updated" {
		t.Errorf("action = %q, want instance.settings.updated", rows[0].Action)
	}
	var details struct {
		Keys []string `json:"keys"`
	}
	must(t, json.Unmarshal([]byte(rows[0].Details), &details))
	want := map[string]bool{"steam_warm_enabled": true, "steam_warm_rewarm": true, "steam_warm_idle_minutes": true}
	if len(details.Keys) != len(want) {
		t.Fatalf("keys = %v, want %v", details.Keys, want)
	}
	for _, k := range details.Keys {
		if !want[k] {
			t.Errorf("keys carries an unexpected field %q", k)
		}
	}
	if strings.Contains(rows[0].Details, "next_steam_launch") {
		t.Errorf("details leaked a value: %s", rows[0].Details)
	}
}
