package session

// app_launch_profiles.go — the per-app launchable-launch-profile allow-list and
// its server-side enforcement.
//
// Filtering the menu in the UI is not enforcement. GET /v1/me/profiles?app_id=…
// returns a filtered list as a convenience; POST /v1/sessions independently
// rejects a profile_id outside the allow-list (launcher.go). A client-side-only
// allow-list is the same defect class as a client-side admin flag (CLAUDE.md
// invariant #6).
//
// Semantics:
//   - Empty set = any launch profile the device is eligible for, which is what
//     every existing app has, so the feature ships inert.
//   - Non-empty = intersect with eligibility, never a union: it can only narrow.
//   - The app's own default under profile_policy 'prefer' is implicitly always
//     included and is resolved here rather than stored, so changing the default
//     never rewrites allow-list rows.
//   - Only meaningful for 'inherit' and 'prefer'. A 'force' app is always
//     unrestricted here and the write path refuses to store a list for one: an
//     allow-list that can never apply is a stored lie awaiting a policy change.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// ErrProfileNotLaunchableForApp: the launch profile exists and is user-visible;
// the APP's configuration refuses it, hence 409 rather than 400/403 (handler.go).
var ErrProfileNotLaunchableForApp = errors.New("launch profile not offered by this app")

// AppProfileRestriction is one app's effective launchable set, policy applied and
// implicit default folded in. The zero value is unrestricted, the safe failure
// mode for a path with no app in hand.
type AppProfileRestriction struct {
	// Restricted is false when the app offers everything eligibility permits.
	// Callers must branch on this, never on len(allowed) == 0: an empty allowed
	// set with Restricted true would mean "nothing is launchable", which is a
	// different and never-produced thing.
	Restricted bool
	allowed    map[string]bool
}

// Permits reports whether a launch profile id is offered by the app.
func (r AppProfileRestriction) Permits(id string) bool {
	if !r.Restricted {
		return true
	}
	return r.allowed[id]
}

// Filter narrows a catalogue to the allow-list, preserving order. Apply it
// BEFORE eligibility evaluation, or the implicit path resolves to a
// recommendation the app forbids.
func (r AppProfileRestriction) Filter(catalog []profile.LaunchProfile) []profile.LaunchProfile {
	if !r.Restricted {
		return catalog
	}
	out := make([]profile.LaunchProfile, 0, len(catalog))
	for _, lp := range catalog {
		if r.allowed[lp.ID] {
			out = append(out, lp)
		}
	}
	return out
}

// newAppProfileRestriction folds the stored allow-list, the policy and the app
// default into the effective set. Kept out of the queries so the three rules are
// testable without a database.
func newAppProfileRestriction(policy string, defaultProfileID *string, stored []string) AppProfileRestriction {
	// 'force' pins the profile, so no allow-list applies. Unrestricted rather than
	// "only the default", to keep the launch path's override/admin hatches
	// behaving unchanged for a forced app.
	if policy == "force" || len(stored) == 0 {
		return AppProfileRestriction{}
	}
	allowed := make(map[string]bool, len(stored)+1)
	for _, id := range stored {
		allowed[id] = true
	}
	// The app default is implicitly included and cannot be unticked — but only
	// under 'prefer', the one policy where apps.default_profile_id IS this app's
	// default. Under 'inherit' the account or global default decides, so a
	// leftover column value must not silently widen the list.
	if policy == "prefer" && defaultProfileID != nil && *defaultProfileID != "" {
		allowed[*defaultProfileID] = true
	}
	return AppProfileRestriction{Restricted: true, allowed: allowed}
}

// AppProfileRestrictionFor resolves the allow-list for an app already loaded on
// the launch path.
func (s *Store) AppProfileRestrictionFor(ctx context.Context, app LaunchApp) (AppProfileRestriction, error) {
	// 'force' short-circuits: the answer cannot depend on the rows, and the write
	// path guarantees there are none.
	if app.ProfilePolicy == "force" {
		return AppProfileRestriction{}, nil
	}
	stored, err := s.appLaunchProfileIDs(ctx, app.ID)
	if err != nil {
		return AppProfileRestriction{}, err
	}
	return newAppProfileRestriction(app.ProfilePolicy, app.DefaultProfileID, stored), nil
}

// AppProfileRestrictionByID resolves the allow-list for an app named by id under
// the same visibility rule as GET /v1/apps/{id}: absent, disabled, or not
// entitled to callerID is ErrNotFound. Used by GET /v1/me/profiles?app_id=…,
// which holds no LaunchApp and must not confirm an app the caller cannot read.
//
// The entitlement clause is part of that rule (§6.3) and lives here, not in the
// handler, which relies on the visibility claim above. Without it the endpoint is
// an existence oracle plus an allow-list dump for any authenticated caller
// holding an app UUID — and app UUIDs become per-user game tiles.
//
// A copy of entitledSQL (internal/crud/store.go, the definition of record),
// inlined rather than routed through IsEntitled so it stays one round trip.
//
// Not the authorization boundary: this is a menu. POST /v1/sessions enforces the
// allow-list and the entitlement independently.
func (s *Store) AppProfileRestrictionByID(ctx context.Context, callerID, appID string) (AppProfileRestriction, error) {
	var policy string
	var defaultProfileID *string
	err := s.pool.QueryRow(ctx, `
		SELECT profile_policy, default_profile_id
		FROM apps
		WHERE id::text = $1 AND enabled = true
		  AND EXISTS (
		      SELECT 1 FROM entitlements e
		      WHERE e.app_id = apps.id
		        AND (e.subject_type = 'all'
		             OR (e.subject_type = 'user' AND e.subject_id = $2::uuid))
		  )
	`, appID, callerID).Scan(&policy, &defaultProfileID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AppProfileRestriction{}, ErrNotFound
	}
	if err != nil {
		return AppProfileRestriction{}, fmt.Errorf("query app profile policy: %w", err)
	}
	if policy == "force" {
		return AppProfileRestriction{}, nil
	}
	stored, err := s.appLaunchProfileIDs(ctx, appID)
	if err != nil {
		return AppProfileRestriction{}, err
	}
	return newAppProfileRestriction(policy, defaultProfileID, stored), nil
}

// appLaunchProfileIDs reads the stored allow-list rows only; policy and
// implicit-default folding is newAppProfileRestriction's job.
func (s *Store) appLaunchProfileIDs(ctx context.Context, appID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT launch_profile_id
		FROM app_launch_profiles
		WHERE app_id::text = $1
		ORDER BY launch_profile_id ASC
	`, appID)
	if err != nil {
		return nil, fmt.Errorf("query app launch profiles: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan app launch profile: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AppsAllowListing returns the apps whose allow-list names a launch profile, so
// DELETE /v1/admin/launch-profiles/{id} can log which apps the FK cascade will
// widen. Not a delete gate: allow-list membership does not refuse the delete,
// unlike the three references LaunchProfileUsedByFor counts.
func (s *Store) AppsAllowListing(ctx context.Context, launchProfileID string) ([]AppRef, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id::text, a.name
		FROM app_launch_profiles alp
		JOIN apps a ON a.id = alp.app_id
		WHERE alp.launch_profile_id = $1
		ORDER BY a.name ASC
	`, launchProfileID)
	if err != nil {
		return nil, fmt.Errorf("query apps allow-listing launch profile: %w", err)
	}
	defer rows.Close()
	out := []AppRef{}
	for rows.Next() {
		var a AppRef
		if err := rows.Scan(&a.ID, &a.Name); err != nil {
			return nil, fmt.Errorf("scan app ref: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
