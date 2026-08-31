package session

// SPT Path-B tests: a native, high-eligible client receives the catalog rung's
// higher H.264 profile (main/high) via the *profile path* WITHOUT it counting as a
// stream override — so the SPT-07 probe envelope and SPT-06 cert cap still apply.
//
// nativeHighEligible is a pure helper (no DB) and is table-tested directly. The
// end-to-end behaviour (High emitted, envelope still applies; browser ⇒ floor;
// native without the decode profile ⇒ floor; no probe ⇒ floor; explicit override
// wins) is exercised against a real Postgres (TEST_DATABASE_URL), reusing the
// probe-consumer harness (seed1080pApp / newFakeDispatcher) like profile_launch_test.go.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// --- pure helper: nativeHighEligible ----------------------------------------

func TestNativeHighEligible(t *testing.T) {
	tests := []struct {
		name   string
		probe  *DeviceProbe
		target string
		want   bool
	}{
		{
			name:   "nil probe is never eligible",
			probe:  nil,
			target: "high",
			want:   false,
		},
		{
			name:   "browser (non-native) client never eligible",
			probe:  &DeviceProbe{ClientType: "", H264DecodeProfiles: []string{"constrained-baseline", "main", "high"}},
			target: "high",
			want:   false,
		},
		{
			name:   "native, decode list contains high",
			probe:  &DeviceProbe{ClientType: "native", H264DecodeProfiles: []string{"constrained-baseline", "main", "high"}},
			target: "high",
			want:   true,
		},
		{
			name:   "native, decode list contains main",
			probe:  &DeviceProbe{ClientType: "native", H264DecodeProfiles: []string{"constrained-baseline", "main"}},
			target: "main",
			want:   true,
		},
		{
			name:   "native, decode list lacks high -> not eligible for high",
			probe:  &DeviceProbe{ClientType: "native", H264DecodeProfiles: []string{"constrained-baseline", "main"}},
			target: "high",
			want:   false,
		},
		{
			name:   "native, no decode matrix -> main allowed (conservative fallback)",
			probe:  &DeviceProbe{ClientType: "native"},
			target: "main",
			want:   true,
		},
		{
			name:   "native, no decode matrix -> high NEVER allowed (conservative fallback)",
			probe:  &DeviceProbe{ClientType: "native"},
			target: "high",
			want:   false,
		},
		{
			name:   "constrained-baseline target is the floor: trivially eligible for native",
			probe:  &DeviceProbe{ClientType: "native"},
			target: "constrained-baseline",
			want:   true,
		},
		{
			name:   "empty target treated as floor: eligible for native",
			probe:  &DeviceProbe{ClientType: "native"},
			target: "",
			want:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nativeHighEligible(tc.probe, tc.target); got != tc.want {
				t.Fatalf("nativeHighEligible(%+v, %q) = %v, want %v", tc.probe, tc.target, got, tc.want)
			}
		})
	}
}

// --- DB integration: LaunchByProfile honours Path-B ------------------------

// upsertNativeProbe inserts/updates a user_devices row that mimics the AS10-12
// native-client capability report: client_type="native" plus a decode.h264.profiles
// list. Bandwidth/rtt/max_decode_height are set generously so the profile passes the
// (separate) eligibility gate; this helper only varies the native/decode surface.
func upsertNativeProbe(t *testing.T, pool *pgxpool.Pool, userID string, h264Profiles []string, measuredAt time.Time) {
	t.Helper()
	ctx := context.Background()
	caps := map[string]any{
		"bandwidth_kbps":    50000,
		"rtt_ms":            5,
		"max_decode_height": 2160,
		"measured_at":       measuredAt.UTC().Format(time.RFC3339),
		"codecs":            map[string]bool{"h264": true},
		"client_type":       "native",
	}
	if h264Profiles != nil {
		caps["decode"] = map[string]any{
			"h264": map[string]any{"profiles": h264Profiles},
		}
	}
	raw, err := json.Marshal(caps)
	if err != nil {
		t.Fatalf("marshal native caps: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_devices (user_id, device_key, capabilities)
		VALUES ($1::uuid, 'native-device', $2)
		ON CONFLICT (user_id, device_key) DO UPDATE
		    SET capabilities = EXCLUDED.capabilities,
		        last_seen_at = now()
	`, userID, raw); err != nil {
		t.Fatalf("upsert native probe: %v", err)
	}
}

// TestLaunchByProfilePathBNativeHighEligible: a native client whose decode list
// includes "high" receives the 1080p60 rung's "high" H.264 profile (not the floor),
// and because no override was set the SPT-07 envelope still applies (no override =>
// envelope path runs; here it does not lower the already-resolved bitrate).
func TestLaunchByProfilePathBNativeHighEligible(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	upsertNativeProbe(t, pool, userID, []string{"constrained-baseline", "main", "high"}, time.Now())

	// The launching client declares itself native — the identity binding requires
	// BOTH the declaration and the stored native probe to agree before the lift fires.
	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60", ClientType: "native"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.H264Profile != "high" {
		t.Fatalf("h264_profile: got %q, want high (native high-eligible Path-B)", res.Session.H264Profile)
	}
	// No override was supplied, so the session carries the resolved profile id — the
	// SPT-07 envelope + SPT-06 cert-cap blocks key on profile_id != "" && !ov.any().
	if res.Session.ProfileID == nil || *res.Session.ProfileID != "1080p60" {
		t.Fatalf("profile_id: got %v, want 1080p60 (path-B must not become an override)", res.Session.ProfileID)
	}
}

// TestLaunchByProfilePathBBrowserKeepsFloor: a browser probe (no client_type) keeps
// the constrained-baseline floor — unchanged behaviour.
func TestLaunchByProfilePathBBrowserKeepsFloor(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	// Browser probe: upsertProbe writes no client_type.
	upsertProbe(t, pool, userID, 50000, 5, 2160, time.Now())

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.H264Profile != "constrained-baseline" {
		t.Fatalf("h264_profile: got %q, want constrained-baseline (browser floor)", res.Session.H264Profile)
	}
}

// TestLaunchByProfilePathBNativeWithoutDecodeProfile: a native client whose decode
// list omits "high" keeps the floor for the "high" rung.
func TestLaunchByProfilePathBNativeWithoutDecodeProfile(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	// Native, but only decodes up to main → not eligible for the "high" rung. The
	// launching client declares native, so the lift gate is reached; the stored
	// probe's decode list is what keeps it at the floor.
	upsertNativeProbe(t, pool, userID, []string{"constrained-baseline", "main"}, time.Now())

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60", ClientType: "native"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.H264Profile != "constrained-baseline" {
		t.Fatalf("h264_profile: got %q, want constrained-baseline (native lacks high decode)", res.Session.H264Profile)
	}
}

// TestLaunchByProfilePathBNoProbeKeepsFloor: no device row at all → not eligible →
// floor (unchanged browser behaviour; a probe read miss never blocks a launch).
func TestLaunchByProfilePathBNoProbeKeepsFloor(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.H264Profile != "constrained-baseline" {
		t.Fatalf("h264_profile: got %q, want constrained-baseline (no probe)", res.Session.H264Profile)
	}
}

// TestLaunchByProfilePathBCrossDevicePoisoning is the identity-binding regression:
// the user has a NATIVE probe stored as their latest device (as if they had just
// played on the native client), then launches again. The lift must key on the
// LAUNCHING client's own client_type declaration, not the stored probe:
//   - launch WITHOUT client_type (a browser tab)  ⇒ constrained-baseline floor,
//     even though a native high-capable probe is the account's latest (this is the
//     bug: Chrome cannot decode H.264 High);
//   - launch WITH client_type="native"            ⇒ lift to the rung's "high";
//   - launch WITH a garbage client_type           ⇒ floor (lenient parse: only the
//     exact string "native" opts in).
func TestLaunchByProfilePathBCrossDevicePoisoning(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	// The account's latest probe is a fully high-capable NATIVE device.
	upsertNativeProbe(t, pool, userID, []string{"constrained-baseline", "main", "high"}, time.Now())

	// 1. Browser launch (no client_type) must NOT inherit the native probe's High.
	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60"})
	if err != nil {
		t.Fatalf("browser launch: %v", err)
	}
	if res.Session.H264Profile != "constrained-baseline" {
		t.Fatalf("browser launch h264_profile: got %q, want constrained-baseline (native probe must not poison a browser launch)", res.Session.H264Profile)
	}

	// 2. Native launch (client_type=native) with the same stored probe ⇒ lift.
	res, err = coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60", ClientType: "native"})
	if err != nil {
		t.Fatalf("native launch: %v", err)
	}
	if res.Session.H264Profile != "high" {
		t.Fatalf("native launch h264_profile: got %q, want high (declared native + high-capable probe)", res.Session.H264Profile)
	}

	// 3. Garbage client_type is treated as browser (lenient parse) ⇒ floor.
	res, err = coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60", ClientType: "Native \"; DROP"})
	if err != nil {
		t.Fatalf("garbage-client-type launch: %v", err)
	}
	if res.Session.H264Profile != "constrained-baseline" {
		t.Fatalf("garbage client_type h264_profile: got %q, want constrained-baseline (only exact \"native\" opts in)", res.Session.H264Profile)
	}
}

// TestLaunchByProfilePathBExplicitOverrideWins: an explicit H.264 override is honoured
// verbatim even for a native high-eligible client — the override path is unchanged and
// Path-B never fires (ov.any() is true, so the envelope/cert-cap gates are bypassed as
// they are today).
func TestLaunchByProfilePathBExplicitOverrideWins(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	upsertNativeProbe(t, pool, userID, []string{"constrained-baseline", "main", "high"}, time.Now())

	main := "main"
	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{
		AppID:     appID,
		ProfileID: "1080p60",
		Override:  StreamOverride{H264Profile: &main},
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.H264Profile != "main" {
		t.Fatalf("h264_profile: got %q, want main (explicit override wins, not the rung's high)", res.Session.H264Profile)
	}
}
