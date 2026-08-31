package auth

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// seedUIPrefsUser creates a bare account for these tests via the package's own
// createUser (no shared seedTestUser helper exists in this package).
func seedUIPrefsUser(t *testing.T, s *store, email, username string) string {
	t.Helper()
	u, err := s.createUser(context.Background(), email, username, "x")
	if err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}
	return u.ID
}

func TestUIPreferencesRoundTrip(t *testing.T) {
	pool := testDB(t) // existing helper; skips when TEST_DATABASE_URL is unset
	s := &store{pool: pool}
	ctx := context.Background()
	userID := seedUIPrefsUser(t, s, "ui-prefs@example.com", "uiprefs")

	// A user with no row reads as all-defaults, not an error.
	got, err := s.GetUIPreferences(ctx, userID)
	if err != nil {
		t.Fatalf("GetUIPreferences on absent row: %v", err)
	}
	if len(got.SessionOverlay) != 0 {
		t.Fatalf("absent row should read as empty overlay, got %v", got.SessionOverlay)
	}

	// A first PATCH creates the row.
	got, err = s.PatchUIPreferences(ctx, userID, map[string]any{
		"session_overlay": map[string]any{"strip_position": "top"},
	})
	if err != nil {
		t.Fatalf("first PatchUIPreferences: %v", err)
	}
	if got.SessionOverlay["strip_position"] != "top" {
		t.Fatalf("strip_position = %v, want top", got.SessionOverlay["strip_position"])
	}

	// A second PATCH merges: strip_position survives an unrelated write.
	got, err = s.PatchUIPreferences(ctx, userID, map[string]any{
		"session_overlay": map[string]any{"strip_auto_hide": "never_visible"},
	})
	if err != nil {
		t.Fatalf("second PatchUIPreferences: %v", err)
	}
	if got.SessionOverlay["strip_position"] != "top" {
		t.Fatalf("merge dropped strip_position: %v", got.SessionOverlay)
	}
	if got.SessionOverlay["strip_auto_hide"] != "never_visible" {
		t.Fatalf("strip_auto_hide = %v, want never_visible", got.SessionOverlay["strip_auto_hide"])
	}
}

func TestUIPreferencesRejectsUnknownEnum(t *testing.T) {
	pool := testDB(t)
	s := &store{pool: pool}
	userID := seedUIPrefsUser(t, s, "ui-bad@example.com", "uibad")

	// "left" became a valid dock in the UI v3 amendment; "middle" is the value
	// that is still out of vocabulary.
	_, err := s.PatchUIPreferences(context.Background(), userID, map[string]any{
		"session_overlay": map[string]any{"strip_position": "middle"},
	})
	if !errors.Is(err, ErrInvalidUIPreference) {
		t.Fatalf("err = %v, want ErrInvalidUIPreference", err)
	}
}

func TestUIPreferencesPreservesUnknownKeys(t *testing.T) {
	pool := testDB(t)
	s := &store{pool: pool}
	ctx := context.Background()
	userID := seedUIPrefsUser(t, s, "ui-future@example.com", "uifuture")

	// A newer client writes a surface this server has never heard of.
	if _, err := s.PatchUIPreferences(ctx, userID, map[string]any{
		"library_density": "compact",
	}); err != nil {
		t.Fatalf("PatchUIPreferences with unknown top-level key: %v", err)
	}
	// An older-surface write must not delete it.
	if _, err := s.PatchUIPreferences(ctx, userID, map[string]any{
		"session_overlay": map[string]any{"strip_preset": "minimal"},
	}); err != nil {
		t.Fatalf("second PatchUIPreferences: %v", err)
	}
	raw, err := s.getUIPreferencesRaw(ctx, userID)
	if err != nil {
		t.Fatalf("getUIPreferencesRaw: %v", err)
	}
	if raw["library_density"] != "compact" {
		t.Fatalf("unknown key was dropped: %v", raw)
	}
}

// TestUIPreferencesRejectsUnknownSessionOverlayKey covers review finding 1:
// protocol/openapi.yaml sets additionalProperties: false on
// SessionOverlayPreferences, so a key inside session_overlay that this server
// doesn't recognise (a typo, or a field from a vocabulary the server hasn't
// been taught yet) must be a 400, never silently merged and stored — unlike
// an unknown key at the DOCUMENT's top level, which is deliberately
// preserved (see TestUIPreferencesPreservesUnknownKeys).
func TestUIPreferencesRejectsUnknownSessionOverlayKey(t *testing.T) {
	pool := testDB(t)
	s := &store{pool: pool}
	ctx := context.Background()
	userID := seedUIPrefsUser(t, s, "ui-overlay-typo@example.com", "uioverlaytypo")

	_, err := s.PatchUIPreferences(ctx, userID, map[string]any{
		"session_overlay": map[string]any{"strip_postion": "top"}, // typo, not a real key
	})
	if !errors.Is(err, ErrInvalidUIPreference) {
		t.Fatalf("err = %v, want ErrInvalidUIPreference", err)
	}

	// Nothing must have been persisted by the rejected patch.
	raw, err := s.getUIPreferencesRaw(ctx, userID)
	if err != nil {
		t.Fatalf("getUIPreferencesRaw: %v", err)
	}
	if len(raw) != 0 {
		t.Fatalf("rejected patch persisted state: %v", raw)
	}
}

// TestUIPreferencesExtraRoundTripsThroughExportedAPI covers review finding 2:
// an unrecognised top-level key must survive through the EXPORTED accessors
// (GetUIPreferences, PatchUIPreferences's return value), not just through the
// unexported getUIPreferencesRaw — Task 3 serialises UIPreferences straight
// into the HTTP response, so if it only round-tripped through the raw reader
// the key would be intact in storage but invisible on every GET/PATCH
// response, which is worse than dropping it outright.
func TestUIPreferencesExtraRoundTripsThroughExportedAPI(t *testing.T) {
	pool := testDB(t)
	s := &store{pool: pool}
	ctx := context.Background()
	userID := seedUIPrefsUser(t, s, "ui-extra-exported@example.com", "uiextraexported")

	patched, err := s.PatchUIPreferences(ctx, userID, map[string]any{
		"library_density": "compact",
	})
	if err != nil {
		t.Fatalf("PatchUIPreferences with unknown top-level key: %v", err)
	}
	if patched.Extra["library_density"] != "compact" {
		t.Fatalf("PatchUIPreferences return value dropped unknown top-level key: %+v", patched)
	}

	got, err := s.GetUIPreferences(ctx, userID)
	if err != nil {
		t.Fatalf("GetUIPreferences: %v", err)
	}
	if got.Extra["library_density"] != "compact" {
		t.Fatalf("GetUIPreferences dropped unknown top-level key: %+v", got)
	}

	// And it must appear at the JSON top level, not nested under a synthetic
	// "extra" wrapper — this is what Task 3's handler will actually emit.
	buf, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal UIPreferences: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf, &doc); err != nil {
		t.Fatalf("unmarshal for inspection: %v", err)
	}
	if doc["library_density"] != "compact" {
		t.Fatalf("JSON response dropped or misplaced unknown top-level key: %s", buf)
	}
	if _, nested := doc["extra"]; nested {
		t.Fatalf("unknown key must not be nested under an \"extra\" wrapper: %s", buf)
	}
}
