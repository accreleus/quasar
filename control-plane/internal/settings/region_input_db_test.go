package settings

// Amendment 49 (#845): the nine Region and Input install defaults on
// /v1/admin/settings. DB-backed: run via `make test-db`.

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func restoreRegionInputDefaults(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `UPDATE instance_settings SET
			default_timezone = 'UTC', default_language = 'en-US', default_region_format = NULL,
			default_keyboard_layout = 'us', default_keyboard_variant = '', default_match_cursor = 'auto',
			default_mouse_sensitivity = 1, default_raw_mouse = false, default_flip_scroll = false,
			default_nintendo_layout = false`); err != nil {
			t.Errorf("restore region and input defaults: %v", err)
		}
	})
}

func TestRegionInputDefaultsReadAndPatch(t *testing.T) {
	pool := testDB(t)
	restoreRegionInputDefaults(t, pool)
	st, err := NewStore(pool).Get(context.Background())
	must(t, err)
	if st.RegionInputDefaults != BuiltInRegionInputDefaults {
		t.Fatalf("no singleton: %+v, want the built-in defaults", st.RegionInputDefaults)
	}
	patch, get := newSettingsHarness(t, pool)
	if got := get(t).Settings.RegionInputDefaults; got != BuiltInRegionInputDefaults {
		t.Fatalf("seeded row: %+v, want the column defaults", got)
	}

	code, env := patch(t, `{"default_timezone":"Asia/Calcutta","default_language":"de-DE",
		"default_region_format":"en-GB","default_keyboard":{"layout":"de","variant":"nodeadkeys"},
		"default_match_cursor":"on","default_mouse_sensitivity":2.5,"default_raw_mouse":true,
		"default_flip_scroll":true,"default_nintendo_layout":true}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH = %d", code)
	}
	gb := "en-GB"
	want := RegionInputDefaults{Timezone: "Asia/Calcutta", Language: "de-DE", RegionFormat: &gb,
		Keyboard: Keyboard{"de", "nodeadkeys"}, MatchCursor: "on", MouseSensitivity: 2.5,
		RawMouse: true, FlipScroll: true, NintendoLayout: true}
	check := func(got RegionInputDefaults, want RegionInputDefaults) {
		t.Helper()
		gotFmt, wantFmt := got.RegionFormat, want.RegionFormat
		got.RegionFormat, want.RegionFormat = nil, nil
		if got != want || (gotFmt == nil) != (wantFmt == nil) || (gotFmt != nil && *gotFmt != *wantFmt) {
			t.Errorf("defaults = %+v (format %v), want %+v (format %v)", got, gotFmt, want, wantFmt)
		}
	}
	check(env.Settings.RegionInputDefaults, want)
	check(get(t).Settings.RegionInputDefaults, want)

	// A default stored before the shape rule is left alone: a PATCH of another
	// field does not re-validate it, and the read returns it as stored.
	must(t, func() error {
		_, err := pool.Exec(context.Background(), `UPDATE instance_settings SET default_timezone = 'posixrules'`)
		return err
	}())
	if code, _ := patch(t, `{"default_language":"de-DE"}`); code != http.StatusOK {
		t.Fatalf("PATCH of another field with a stored posixrules default = %d, want 200", code)
	}
	if got := get(t).Settings.RegionInputDefaults.Timezone; got != "posixrules" {
		t.Errorf("stored default = %q, want it kept", got)
	}
	if code, _ := patch(t, `{"default_timezone":"Asia/Calcutta"}`); code != http.StatusOK {
		t.Fatalf("PATCH = %d", code)
	}

	// Absent = unchanged; null clears only the region format.
	if code, _ := patch(t, `{"default_region_format":null,"default_raw_mouse":false}`); code != http.StatusOK {
		t.Fatalf("PATCH = %d", code)
	}
	want.RegionFormat, want.RawMouse = nil, false
	check(get(t).Settings.RegionInputDefaults, want)

	for _, bad := range []string{
		`{"default_timezone":"Local"}`, `{"default_timezone":null}`, `{"default_language":"german"}`,
		`{"default_keyboard":{"layout":"de;rm"}}`, `{"default_mouse_sensitivity":0.1}`,
		`{"default_flip_scroll":null}`, `{"default_raw_mouse":true,"default_match_cursor":"x"}`,
	} {
		if code, _ := patch(t, bad); code != http.StatusBadRequest {
			t.Errorf("PATCH %s = %d, want 400", bad, code)
		}
	}
	check(get(t).Settings.RegionInputDefaults, want)
}
