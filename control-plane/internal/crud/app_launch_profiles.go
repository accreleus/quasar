package crud

// Write half of an app's launch-profile allow-list; the read is in store.go,
// enforcement is internal/session (launcher.go) since this package never sees
// a launch. Both write routes are registered under RequireAuth->RequireAdmin
// in handler.go; nothing here re-checks the role.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Unlike default_profile_id/runtime_preset_id, null has no meaning here; treating
// it as "clear" would silently act on a value the caller meant something by.
var errAllowListNullNotMeaningful = errors.New(
	"launchable_profile_ids: null is not meaningful — send [] to clear the allow-list, or omit the field to leave it unchanged")

// `force` pins the app's launch profile outright, so an allow-list would be a
// stored rule that silently activates if policy later reverts to `prefer`.
var errAllowListUnderForce = errors.New(
	"launchable_profile_ids cannot be set while profile_policy is force — that policy pins the app's launch profile, so no allow-list can ever apply")

// allowListPatch is a parsed launchable_profile_ids field.
//   - present=false: the field was absent ⇒ unchanged on patch, none on create.
//   - present=true, ids empty: an explicit [] ⇒ clear (unrestricted).
type allowListPatch struct {
	present bool
	ids     []string
}

// json.RawMessage does not decode an explicit null to nil, so it must be
// checked explicitly; len(raw) == 0 reliably means absent.
func parseAllowList(raw json.RawMessage) (allowListPatch, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return allowListPatch{}, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return allowListPatch{}, errAllowListNullNotMeaningful
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return allowListPatch{}, errors.New("launchable_profile_ids must be an array of launch profile ids")
	}
	// Dedupe, preserving first-seen order: the join table's PK would reject
	// duplicates anyway, and a 400 for a repeated id helps nobody.
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			return allowListPatch{}, errors.New("launchable_profile_ids must not contain an empty id")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return allowListPatch{present: true, ids: out}, nil
}

// validLaunchProfileIDs reports whether every id names a user-visible launch
// profile; same filter as validUserProfileID (apps.default_profile_id).
func (s *store) validLaunchProfileIDs(ctx context.Context, ids []string) (bool, error) {
	if len(ids) == 0 {
		return true, nil
	}
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM launch_profiles
		WHERE id = ANY($1) AND visibility = 'user'
	`, ids).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("validate launch profile ids: %w", err)
	}
	// ids is already deduped, so an exact count match proves every id resolved.
	return n == len(ids), nil
}

// setAppLaunchProfiles replaces an app's stored allow-list, delete-then-insert
// in one transaction: a partial application would silently widen the app's menu.
func (s *store) setAppLaunchProfiles(ctx context.Context, appID string, ids []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin set app launch profiles: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — no-op after commit

	if _, err := tx.Exec(ctx, `DELETE FROM app_launch_profiles WHERE app_id::text = $1`, appID); err != nil {
		return fmt.Errorf("clear app launch profiles: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `
			INSERT INTO app_launch_profiles (app_id, launch_profile_id) VALUES ($1::uuid, $2)
		`, appID, id); err != nil {
			return fmt.Errorf("insert app launch profile %q: %w", id, err)
		}
	}
	return tx.Commit(ctx)
}

// appProfilePolicy reads an app's stored profile_policy, needed to resolve the
// effective policy of a partial patch that doesn't also send a new allow-list.
func (s *store) appProfilePolicy(ctx context.Context, appID string) (string, error) {
	var policy string
	err := s.pool.QueryRow(ctx, `SELECT profile_policy FROM apps WHERE id::text = $1`, appID).Scan(&policy)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read app profile policy: %w", err)
	}
	return policy, nil
}
