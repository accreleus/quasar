// Unattended automatic apply (#122) rides the existing settings envelope, so it
// is exercised through the same real RequireAuth→RequireAdmin chain every other
// field is.
package settings

import "testing"

func TestPlatformAutoApplyDefaultsOff(t *testing.T) {
	pool := testDB(t)
	_, get := newSettingsHarness(t, pool)

	if get(t).Settings.PlatformAutoApply {
		t.Error("platform_auto_apply must default false — installing releases without a click is opt-in")
	}
}

func TestPatchPlatformAutoApply(t *testing.T) {
	pool := testDB(t)
	patch, _ := newSettingsHarness(t, pool)

	code, env := patch(t, `{"platform_auto_apply":true}`)
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if !env.Settings.PlatformAutoApply {
		t.Fatal("platform_auto_apply did not take")
	}

	// Absent means unchanged. A plain non-pointer decode would read false here
	// and silently switch automatic updates back off whenever an admin changed
	// any other setting.
	code, env = patch(t, `{"registration_mode":"open"}`)
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if !env.Settings.PlatformAutoApply {
		t.Error("an unrelated PATCH switched automatic updates off — pointer decode is not holding")
	}

	// And it turns back off.
	code, env = patch(t, `{"platform_auto_apply":false}`)
	if code != 200 || env.Settings.PlatformAutoApply {
		t.Fatalf("status = %d, auto_apply = %v, want 200 / false", code, env.Settings.PlatformAutoApply)
	}
}

// Unlike release_webhook_enabled, this field refuses nothing: it depends on no
// other setting being configured first, so there is no companion 400.
func TestPlatformAutoApplyNeedsNoCompanionConfiguration(t *testing.T) {
	pool := testDB(t)
	patch, _ := newSettingsHarness(t, pool)

	if code, _ := patch(t, `{"platform_auto_apply":true}`); code != 200 {
		t.Fatalf("status = %d, want 200 with nothing else configured", code)
	}
}

func TestPlatformAutoApplyInChangedKeys(t *testing.T) {
	on := true
	keys := Patch{PlatformAutoApply: &on}.ChangedKeys()
	if len(keys) != 1 || keys[0] != "platform_auto_apply" {
		t.Fatalf("ChangedKeys = %v, want [platform_auto_apply] for the audit record", keys)
	}
}
