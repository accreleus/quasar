package settings

import (
	"net/http"
	"testing"
)

func TestSteamPreparationSettingsRevisionAndValidation(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)
	initial := get(t).Settings
	if !initial.SteamPreparationEnabled || initial.SteamPreparationRevision != "1" {
		t.Fatalf("defaults: %+v", initial)
	}
	code, off := patch(t, `{"steam_preparation_enabled":false}`)
	if code != http.StatusOK || off.Settings.SteamPreparationEnabled || off.Settings.SteamPreparationRevision != "2" {
		t.Fatalf("disable: %d %+v", code, off)
	}
	_, noop := patch(t, `{"steam_preparation_enabled":false}`)
	if noop.Settings.SteamPreparationRevision != "2" {
		t.Fatal("no-op bumped revision")
	}
	for _, body := range []string{`{"steam_preparation_enabled":null}`, `{"steam_preparation_enabled":"true"}`, `{"steam_preparation_revision":"9"}`} {
		code, _ = patch(t, body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s returned %d", body, code)
		}
	}
	_, unrelated := patch(t, `{"mic_capture_enabled":true}`)
	if unrelated.Settings.SteamPreparationRevision != "2" || unrelated.Settings.SteamPreparationEnabled {
		t.Fatal("unrelated settings changed policy")
	}
}
