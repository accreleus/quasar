package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
)

// ErrInvalidUIPreference is returned when a client sends a value outside the
// documented vocabulary. Deliberately NOT clamped to a default: a bad value is
// a client bug, and silently fixing it would hide that bug on every device the
// user owns.
var ErrInvalidUIPreference = errors.New("invalid ui preference value")

// UIPreferences is the domain view of a user's client presentation state. The
// server validates it and never acts on it — nothing in the session pipeline,
// scheduler or encode path reads this.
//
// Extra carries top-level document keys this server does not recognise (a
// future client surface beyond session_overlay). It is populated on every
// read and folded back to the top level by MarshalJSON, so a GET/PATCH
// response always matches exactly what is stored — an unrecognised top-level
// key is forward-compat state, never a validation error and never silently
// dropped from the API response (see PatchUIPreferences and the contrast with
// session_overlay's closed vocabulary below).
type UIPreferences struct {
	SessionOverlay map[string]any `json:"session_overlay"`
	Extra          map[string]any `json:"-"`
}

// MarshalJSON flattens Extra back to the document's top level alongside
// session_overlay, rather than nesting it under a synthetic "extra" key or
// renaming anything — the JSON emitted must be indistinguishable from what a
// client would see if this server had a typed field for every key it wrote.
func (p UIPreferences) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(p.Extra)+1)
	maps.Copy(out, p.Extra)
	out["session_overlay"] = p.SessionOverlay
	return json.Marshal(out)
}

// allowed enum values, keyed by field. Mirrors protocol/openapi.yaml's
// SessionOverlayPreferences; a value not listed here is a 400.
var sessionOverlayEnums = map[string][]string{
	"strip_preset": {"full", "minimal", "metrics", "custom"},
	// All four docks: the overlay has always drawn left/right, the enum listed
	// two. semantics: control-api.md §UI v3 console
	"strip_position":  {"top", "bottom", "left", "right"},
	"strip_auto_hide": {"on_capture", "always_visible", "never_visible"},
}

// stripItemKeys are the booleans inside session_overlay.strip_items.
// signal/identity/codec/metrics/hint are readouts; capture/exit are ACTIONS
// (protocol/openapi.yaml SessionOverlayPreferences.strip_items) — the server
// still just validates the vocabulary, it never interprets what the values
// mean to the client.
var stripItemKeys = map[string]bool{
	"signal": true, "identity": true, "codec": true, "metrics": true, "hint": true,
	"capture": true, "exit": true, "mic": true, "fullscreen": true,
}

// sessionOverlayKnownKeys is the closed set of keys the OpenAPI contract
// allows directly inside session_overlay — protocol/openapi.yaml sets
// additionalProperties: false on SessionOverlayPreferences. This is a
// NARROWER rule than the one governing the surrounding document: a top-level
// key this server doesn't recognise is preserved verbatim as forward
// compatibility for a future client surface (see UIPreferences.Extra and
// PatchUIPreferences), but a key sitting directly inside session_overlay is
// validated against this closed vocabulary and rejected if unlisted, exactly
// like an out-of-vocabulary enum value. The two rules look contradictory side
// by side; they aren't — only session_overlay's shape is frozen by the
// contract, the document around it isn't.
var sessionOverlayKnownKeys = map[string]bool{
	"strip_preset":    true,
	"strip_position":  true,
	"strip_auto_hide": true,
	"strip_items":     true,
}

// ValidateSessionOverlay checks a partial session_overlay object. Absent fields
// are legal (PATCH is partial); present ones must be in vocabulary, and no key
// outside sessionOverlayKnownKeys is accepted.
func ValidateSessionOverlay(v map[string]any) error {
	for k := range v {
		if !sessionOverlayKnownKeys[k] {
			return fmt.Errorf("%w: unknown session_overlay key %q", ErrInvalidUIPreference, k)
		}
	}
	for field, allowed := range sessionOverlayEnums {
		raw, ok := v[field]
		if !ok {
			continue
		}
		s, ok := raw.(string)
		if !ok {
			return fmt.Errorf("%w: %s must be a string", ErrInvalidUIPreference, field)
		}
		if !slices.Contains(allowed, s) {
			return fmt.Errorf("%w: %s=%q", ErrInvalidUIPreference, field, s)
		}
	}
	if raw, ok := v["strip_items"]; ok {
		items, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: strip_items must be an object", ErrInvalidUIPreference)
		}
		for k, val := range items {
			if !stripItemKeys[k] {
				return fmt.Errorf("%w: unknown strip item %q", ErrInvalidUIPreference, k)
			}
			if _, ok := val.(bool); !ok {
				return fmt.Errorf("%w: strip_items.%s must be a boolean", ErrInvalidUIPreference, k)
			}
		}
	}
	return nil
}

// getUIPreferencesRaw returns the whole stored object including keys this
// server does not recognise. Used by PatchUIPreferences to merge without loss,
// and by tests to assert that forward-compatibility holds.
func (s *store) getUIPreferencesRaw(ctx context.Context, userID string) (map[string]any, error) {
	var buf []byte
	err := s.pool.QueryRow(ctx, `
		SELECT session_overlay
		FROM user_ui_preferences
		WHERE user_id = $1::uuid
	`, userID).Scan(&buf)
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get ui preferences: %w", err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil, fmt.Errorf("decode ui preferences: %w", err)
	}
	return out, nil
}

// GetUIPreferences reads the caller's preferences. A missing row is not an
// error — it means "all defaults" — so this returns an empty overlay, and the
// client applies its own defaults over it.
func (s *store) GetUIPreferences(ctx context.Context, userID string) (UIPreferences, error) {
	raw, err := s.getUIPreferencesRaw(ctx, userID)
	if err != nil {
		return UIPreferences{}, err
	}
	return uiPreferencesFromRaw(raw), nil
}

func uiPreferencesFromRaw(raw map[string]any) UIPreferences {
	overlay, _ := raw["session_overlay"].(map[string]any)
	if overlay == nil {
		overlay = map[string]any{}
	}
	extra := make(map[string]any, len(raw))
	maps.Copy(extra, raw)
	delete(extra, "session_overlay")
	return UIPreferences{SessionOverlay: overlay, Extra: extra}
}

// PatchUIPreferences merges `patch` one level deep into the stored object and
// returns the result. Top-level keys the server does not recognise survive
// untouched, so an older control plane cannot delete a newer client's state.
func (s *store) PatchUIPreferences(ctx context.Context, userID string, patch map[string]any) (UIPreferences, error) {
	if overlay, ok := patch["session_overlay"]; ok {
		m, ok := overlay.(map[string]any)
		if !ok {
			return UIPreferences{}, fmt.Errorf("%w: session_overlay must be an object", ErrInvalidUIPreference)
		}
		if err := ValidateSessionOverlay(m); err != nil {
			return UIPreferences{}, err
		}
	}

	current, err := s.getUIPreferencesRaw(ctx, userID)
	if err != nil {
		return UIPreferences{}, err
	}
	for k, v := range patch {
		// One level deep: a known object surface merges field-by-field, anything
		// else replaces. Only session_overlay is a merge surface today.
		if k == "session_overlay" {
			existing, _ := current[k].(map[string]any)
			if existing == nil {
				existing = map[string]any{}
			}
			maps.Copy(existing, v.(map[string]any))
			current[k] = existing
			continue
		}
		current[k] = v
	}

	buf, err := json.Marshal(current)
	if err != nil {
		return UIPreferences{}, fmt.Errorf("encode ui preferences: %w", err)
	}
	var stored []byte
	err = s.pool.QueryRow(ctx, `
		INSERT INTO user_ui_preferences (user_id, session_overlay)
		VALUES ($1::uuid, $2::jsonb)
		ON CONFLICT (user_id) DO UPDATE
		SET session_overlay = EXCLUDED.session_overlay
		RETURNING session_overlay
	`, userID, buf).Scan(&stored)
	if err != nil {
		return UIPreferences{}, fmt.Errorf("update ui preferences: %w", err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(stored, &out); err != nil {
		return UIPreferences{}, fmt.Errorf("decode stored ui preferences: %w", err)
	}
	return uiPreferencesFromRaw(out), nil
}
