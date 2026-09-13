// The release-notification target rides the existing settings envelope
// (control-api.md §"Release notifications"), so it is exercised through the same
// real RequireAuth→RequireAdmin chain every other field is.
package settings

import (
	"strings"
	"testing"
)

func TestReleaseWebhookDefaultsToOff(t *testing.T) {
	pool := testDB(t)
	_, get := newSettingsHarness(t, pool)

	st := get(t).Settings
	if st.ReleaseWebhookEnabled || st.ReleaseWebhookURL != "" {
		t.Fatalf("settings = %+v, want an unconfigured instance announcing nothing", st)
	}
}

func TestPatchReleaseWebhook(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)

	code, env := patch(t, `{"release_webhook_url":"https://hooks.example.com/a/b","release_webhook_enabled":true}`)
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if !env.Settings.ReleaseWebhookEnabled || env.Settings.ReleaseWebhookURL != "https://hooks.example.com/a/b" {
		t.Fatalf("settings = %+v, want the webhook configured", env.Settings)
	}

	// Absent means unchanged, as everywhere on this body.
	code, env = patch(t, `{"registration_mode":"open"}`)
	if code != 200 || !env.Settings.ReleaseWebhookEnabled || env.Settings.ReleaseWebhookURL == "" {
		t.Fatalf("an unrelated PATCH reset the webhook: %d %+v", code, env.Settings)
	}

	// Clearing the URL disables it in the same write: "enabled, with nowhere to
	// send" is a state nothing can act on.
	code, env = patch(t, `{"release_webhook_url":""}`)
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if env.Settings.ReleaseWebhookURL != "" || env.Settings.ReleaseWebhookEnabled {
		t.Fatalf("clearing the URL left it enabled: %+v", env.Settings)
	}
	if got := get(t).Settings; got.ReleaseWebhookEnabled {
		t.Fatal("the clear did not persist")
	}
}

func TestPatchReleaseWebhookValidationFailures(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)

	bad := []string{
		`{"release_webhook_url":"http://hooks.example.com/a"}`,
		`{"release_webhook_url":"https://user:pass@hooks.example.com/a"}`,
		`{"release_webhook_url":"/relative"}`,
		`{"release_webhook_url":"https:///nohost"}`,
		`{"release_webhook_url":"https://hooks.example.com/` + strings.Repeat("a", 2048) + `"}`,
		// Enabling with nothing to send to is a refusal, not a switch that
		// silently does nothing — including when the same body clears the URL.
		`{"release_webhook_enabled":true}`,
		`{"release_webhook_enabled":true,"release_webhook_url":""}`,
	}
	for _, body := range bad {
		code, _ := patch(t, body)
		if code != 400 {
			t.Errorf("PATCH %.80s = %d, want 400 validation_failed", body, code)
		}
	}
	st := get(t).Settings
	if st.ReleaseWebhookEnabled || st.ReleaseWebhookURL != "" {
		t.Fatalf("a rejected PATCH still wrote: %+v", st)
	}
}

func TestReleaseWebhookKeysAreAudited(t *testing.T) {
	enabled, url := true, "https://hooks.example.com/a"
	p := Patch{ReleaseWebhookEnabled: &enabled, ReleaseWebhookURL: &url}
	keys := strings.Join(p.ChangedKeys(), ",")
	if !strings.Contains(keys, "release_webhook_enabled") || !strings.Contains(keys, "release_webhook_url") {
		t.Fatalf("changed keys = %q, want both webhook keys", keys)
	}
	if p.Empty() {
		t.Fatal("a patch naming only the webhook keys must not read as empty")
	}
	// The audit records KEY NAMES ONLY, which is what keeps the URL — itself a
	// credential on Slack and Discord — out of the log.
	if strings.Contains(keys, "hooks.example.com") {
		t.Fatal("the changed-keys list carried a value")
	}
}

func TestValidReleaseWebhookURL(t *testing.T) {
	good := []string{
		"https://hooks.example.com/services/T/B/X",
		"https://ntfy.example.com/quasar",
		"https://hooks.example.com:8443/a?b=c",
	}
	for _, u := range good {
		if !ValidReleaseWebhookURL(u) {
			t.Errorf("ValidReleaseWebhookURL(%q) = false, want true", u)
		}
	}
	bad := []string{
		"", " ", "http://hooks.example.com/a", "https://u:p@hooks.example.com/a",
		"/relative", "hooks.example.com/a", "https:///a", " https://hooks.example.com/a",
		"https://hooks.example.com/" + strings.Repeat("a", MaxReleaseWebhookURLLen),
	}
	for _, u := range bad {
		if ValidReleaseWebhookURL(u) {
			t.Errorf("ValidReleaseWebhookURL(%q) = true, want false", u)
		}
	}
}
