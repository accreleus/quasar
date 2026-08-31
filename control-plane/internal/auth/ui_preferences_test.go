package auth

import (
	"errors"
	"testing"
)

// Pure ValidateSessionOverlay coverage — no store/Postgres involved, unlike
// the DB-backed tests in ui_preferences_db_test.go, so these run in every
// environment including a bare `go test ./...` with no TEST_DATABASE_URL.

// TestValidateSessionOverlayAcceptsCaptureAndExit covers the strip-actions
// rollout: capture/exit join signal/identity/codec/metrics/hint as legal
// strip_items keys (protocol/openapi.yaml SessionOverlayPreferences.strip_items).
func TestValidateSessionOverlayAcceptsCaptureAndExit(t *testing.T) {
	err := ValidateSessionOverlay(map[string]any{
		"strip_items": map[string]any{
			"capture": true,
			"exit":    false,
		},
	})
	if err != nil {
		t.Fatalf("ValidateSessionOverlay with capture/exit keys: %v", err)
	}
}

// TestValidateSessionOverlayAcceptsMicAndFullscreen covers the mic/fullscreen
// rollout: mic/fullscreen join the other seven as legal strip_items keys
// (protocol/openapi.yaml SessionOverlayPreferences.strip_items).
func TestValidateSessionOverlayAcceptsMicAndFullscreen(t *testing.T) {
	err := ValidateSessionOverlay(map[string]any{
		"strip_items": map[string]any{
			"mic":        true,
			"fullscreen": false,
		},
	})
	if err != nil {
		t.Fatalf("ValidateSessionOverlay with mic/fullscreen keys: %v", err)
	}
}

// TestValidateSessionOverlayRejectsUnknownStripItemKey pins the closed-vocabulary
// rule stripItemKeys documents: a key outside the known set — including a
// plausible-looking typo of one of the new action keys — is still a 400.
func TestValidateSessionOverlayRejectsUnknownStripItemKey(t *testing.T) {
	for _, bad := range []string{"quit", "captur", "exitt", "mik", "fullscren"} {
		err := ValidateSessionOverlay(map[string]any{
			"strip_items": map[string]any{bad: true},
		})
		if !errors.Is(err, ErrInvalidUIPreference) {
			t.Fatalf("strip_items key %q: err = %v, want ErrInvalidUIPreference", bad, err)
		}
	}
}

// TestValidateSessionOverlayRejectsNonBoolStripItem covers capture/exit/mic/
// fullscreen specifically, mirroring the existing type check for the older keys.
func TestValidateSessionOverlayRejectsNonBoolStripItem(t *testing.T) {
	for _, key := range []string{"capture", "exit", "mic", "fullscreen"} {
		err := ValidateSessionOverlay(map[string]any{
			"strip_items": map[string]any{key: "yes"},
		})
		if !errors.Is(err, ErrInvalidUIPreference) {
			t.Fatalf("strip_items.%s = \"yes\": err = %v, want ErrInvalidUIPreference", key, err)
		}
	}
}

// TestValidateSessionOverlayAcceptsEveryDock — the overlay draws four docks;
// the enum listed two, so a client asking for a side dock got a 400 for a layout
// the HUD can render (UI v3 amendment §8). Out-of-vocabulary is still refused,
// and still not clamped.
func TestValidateSessionOverlayAcceptsEveryDock(t *testing.T) {
	for _, pos := range []string{"top", "bottom", "left", "right"} {
		if err := ValidateSessionOverlay(map[string]any{"strip_position": pos}); err != nil {
			t.Errorf("strip_position=%q: %v", pos, err)
		}
	}
	for _, pos := range []string{"middle", "centre", "TOP", ""} {
		if err := ValidateSessionOverlay(map[string]any{"strip_position": pos}); err == nil {
			t.Errorf("strip_position=%q was accepted; out-of-vocabulary must be a 400", pos)
		}
	}
}
