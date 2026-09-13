// device_scope.go — which device a per-client decision is made for.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
)

// The decision sites that resolve a scope, one per read, named in the fallback log.
const (
	scopeSiteProfiles    = "me.profiles"
	scopeSiteEligibility = "launch.eligibility"
	scopeSiteH264Lift    = "launch.h264_lift"
	scopeSiteEnvelope    = "launch.envelope"
	scopeSiteTier        = "launch.tier"
	scopeSiteRung        = "launch.rung"
	scopeSiteHealth      = "health.outcome"
)

// DeviceScope is one client's stored identity: the device_key its certification
// history is keyed by, and its capability probe (nil when absent or stale). The
// two must describe the same device, which is why they are read together.
type DeviceScope struct {
	DeviceKey string
	Probe     *DeviceProbe
	Fallback  bool // answers for the account's latest-seen device, not the caller's
}

// ResolveDeviceScope reads the scope a decision applies to. deviceID is the
// user_devices id carried by the caller's token binding or by sessions.device_id;
// when it names no row of this user — empty, malformed, foreign or deleted — the
// scope falls back to the account's most recently seen device, flagged and logged
// once against site. An account with no device row resolves to the zero scope,
// which gates nothing. A read error is returned: callers must proceed with no
// probe and no history rather than with the coarse per-user key.
//
// #203: scoping this on the requesting device is what stops a browser being
// answered with a native client's probe on a shared account.
func (s *Store) ResolveDeviceScope(ctx context.Context, userID, deviceID, site string) (DeviceScope, error) {
	if isValidUUID(deviceID) {
		sc, found, err := s.deviceScopeByID(ctx, userID, deviceID)
		if err != nil {
			return DeviceScope{}, err
		}
		if found {
			return sc, nil
		}
	}
	slog.Default().Info("device-scope fallback: no device binding on this request, "+
		"answering from the account's most recently seen device",
		"site", site, "user_id", userID)
	sc, err := s.latestDeviceScope(ctx, userID)
	if err != nil {
		return DeviceScope{}, err
	}
	sc.Fallback = true
	return sc, nil
}

// deviceScopeByID reads one device. The user_id predicate is the owner check, not
// a filter: another account's device id must resolve as absent.
func (s *Store) deviceScopeByID(ctx context.Context, userID, deviceID string) (DeviceScope, bool, error) {
	var sc DeviceScope
	var rawCaps []byte
	err := s.pool.QueryRow(ctx, `
		SELECT device_key, capabilities FROM user_devices
		WHERE user_id = $1::uuid AND id = $2::uuid
	`, userID, deviceID).Scan(&sc.DeviceKey, &rawCaps)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceScope{}, false, nil
	}
	if err != nil {
		return DeviceScope{}, false, fmt.Errorf("query device scope: %w", err)
	}
	sc.Probe = parseDeviceProbe(rawCaps)
	return sc, true, nil
}

// latestDeviceScope is the fallback read: the account's most recently seen device.
func (s *Store) latestDeviceScope(ctx context.Context, userID string) (DeviceScope, error) {
	var sc DeviceScope
	var rawCaps []byte
	err := s.pool.QueryRow(ctx, `
		SELECT device_key, capabilities FROM user_devices
		WHERE user_id = $1::uuid
		ORDER BY last_seen_at DESC
		LIMIT 1
	`, userID).Scan(&sc.DeviceKey, &rawCaps)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceScope{}, nil
	}
	if err != nil {
		return DeviceScope{}, fmt.Errorf("query latest device scope: %w", err)
	}
	sc.Probe = parseDeviceProbe(rawCaps)
	return sc, nil
}
