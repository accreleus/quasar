package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrProfileOverrideDisabled is returned when a non-admin caller explicitly
// chooses a profile but policy says this app/global config must decide.
var ErrProfileOverrideDisabled = errors.New("stream profile override disabled")

type ProfilePolicySettings struct {
	GlobalDefaultProfileID *string `json:"global_default_profile_id"`
	UserOverridesAllowed   bool    `json:"user_overrides_allowed"`
}

type UserProfilePreferences struct {
	DefaultProfileID *string `json:"default_profile_id"`
}

func (s *Store) GetProfilePolicy(ctx context.Context) (ProfilePolicySettings, error) {
	var p ProfilePolicySettings
	err := s.pool.QueryRow(ctx, `
		SELECT global_default_profile_id, user_overrides_allowed
		FROM stream_profile_policy
		WHERE id = true
	`).Scan(&p.GlobalDefaultProfileID, &p.UserOverridesAllowed)
	if err != nil {
		return ProfilePolicySettings{}, fmt.Errorf("get profile policy: %w", err)
	}
	return p, nil
}

func (s *Store) UpdateProfilePolicy(ctx context.Context, globalDefaultProfileID *string, userOverridesAllowed bool, adminUserID string) (ProfilePolicySettings, error) {
	if globalDefaultProfileID != nil && *globalDefaultProfileID != "" {
		// Must check launch_profiles (migration 0036's FK target): a rung id from
		// stream_profiles would pass here and fail at write time as a 500.
		ok, err := s.LaunchProfileExists(ctx, *globalDefaultProfileID)
		if err != nil {
			return ProfilePolicySettings{}, err
		}
		if !ok {
			return ProfilePolicySettings{}, ErrProfileUnknown
		}
	}
	var p ProfilePolicySettings
	err := s.pool.QueryRow(ctx, `
		UPDATE stream_profile_policy
		SET global_default_profile_id = NULLIF($1, ''),
		    user_overrides_allowed = $2,
		    updated_by = NULLIF($3, '')::uuid
		WHERE id = true
		RETURNING global_default_profile_id, user_overrides_allowed
	`, strOrEmpty(globalDefaultProfileID), userOverridesAllowed, adminUserID).Scan(&p.GlobalDefaultProfileID, &p.UserOverridesAllowed)
	if err != nil {
		return ProfilePolicySettings{}, fmt.Errorf("update profile policy: %w", err)
	}
	return p, nil
}

func (s *Store) GetUserProfilePreferences(ctx context.Context, userID string) (UserProfilePreferences, error) {
	var prefs UserProfilePreferences
	err := s.pool.QueryRow(ctx, `
		SELECT default_profile_id
		FROM user_profile_preferences
		WHERE user_id::text = $1
	`, userID).Scan(&prefs.DefaultProfileID)
	if errors.Is(err, pgx.ErrNoRows) {
		return prefs, nil
	}
	if err != nil {
		return UserProfilePreferences{}, fmt.Errorf("get user profile preferences: %w", err)
	}
	return prefs, nil
}

func (s *Store) UpdateUserProfilePreferences(ctx context.Context, userID string, defaultProfileID *string) (UserProfilePreferences, error) {
	if defaultProfileID != nil && *defaultProfileID != "" {
		// Must check launch_profiles, same as UpdateProfilePolicy.
		ok, err := s.LaunchProfileExists(ctx, *defaultProfileID)
		if err != nil {
			return UserProfilePreferences{}, err
		}
		if !ok {
			return UserProfilePreferences{}, ErrProfileUnknown
		}
	}
	var prefs UserProfilePreferences
	err := s.pool.QueryRow(ctx, `
		INSERT INTO user_profile_preferences (user_id, default_profile_id)
		VALUES ($1::uuid, NULLIF($2, ''))
		ON CONFLICT (user_id) DO UPDATE
		SET default_profile_id = EXCLUDED.default_profile_id
		RETURNING default_profile_id
	`, userID, strOrEmpty(defaultProfileID)).Scan(&prefs.DefaultProfileID)
	if err != nil {
		return UserProfilePreferences{}, fmt.Errorf("update user profile preferences: %w", err)
	}
	return prefs, nil
}

func (s *Store) ProfileOverrideAllowed(ctx context.Context, app LaunchApp, isAdmin bool) (bool, error) {
	if isAdmin {
		return true, nil
	}
	if app.ProfilePolicy == "force" {
		return false, nil
	}
	policy, err := s.GetProfilePolicy(ctx)
	if err != nil {
		return false, err
	}
	return policy.UserOverridesAllowed, nil
}

// ResolveDefaultProfile picks the launch profile a no-profile_id launch uses:
// app pin (prefer/force) -> user preference -> global default -> recommendation.
// `restriction` is the app's launchable allow-list; a user preference or global
// default outside it is skipped, falling through to the next source (ultimately
// recommendedID, computed over the filtered catalogue) rather than launching an
// app-restricted profile at an account/instance default that ignores the
// restriction. The app's own pin is never skipped.
func (s *Store) ResolveDefaultProfile(ctx context.Context, userID string, app LaunchApp, recommendedID string, restriction AppProfileRestriction) (string, error) {
	if (app.ProfilePolicy == "prefer" || app.ProfilePolicy == "force") && app.DefaultProfileID != nil && *app.DefaultProfileID != "" {
		return *app.DefaultProfileID, nil
	}
	prefs, err := s.GetUserProfilePreferences(ctx, userID)
	if err != nil {
		return "", err
	}
	if prefs.DefaultProfileID != nil && *prefs.DefaultProfileID != "" && restriction.Permits(*prefs.DefaultProfileID) {
		return *prefs.DefaultProfileID, nil
	}
	policy, err := s.GetProfilePolicy(ctx)
	if err != nil {
		return "", err
	}
	if policy.GlobalDefaultProfileID != nil && *policy.GlobalDefaultProfileID != "" && restriction.Permits(*policy.GlobalDefaultProfileID) {
		return *policy.GlobalDefaultProfileID, nil
	}
	// "" when the filtered catalogue is empty; falls through to the legacy tier
	// path, capped by app defaults.
	return recommendedID, nil
}

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
