package session

// AS10-03 integration tests: launch by selected profile id at POST /v1/sessions.
//
// These require a real Postgres (TEST_DATABASE_URL) and reuse the probe-consumer
// harness (seed1080pApp / upsertProbe / newFakeDispatcher). They verify:
//   - a profile_id resolves to the profile's concrete stream params + persists the id
//   - an ineligible profile (measured bandwidth below its minimum) is rejected
//   - admin and explicit-override launches bypass the eligibility gate
//   - an explicit override beats the profile field-by-field
//   - an unknown profile_id is rejected; a debug profile is not user-selectable
//   - the legacy (no-profile) path persists a NULL profile_id

import (
	"context"
	"errors"
	"testing"
	"time"
)

// legacyOverride is the documented escape hatch that reaches the LEGACY (tier)
// launch path: a POST /v1/sessions carrying an explicit `stream` block and NO
// profile_id (launcher.go's `lp.ProfileID == "" && !lp.Override.any()` guard).
//
// UI-P4 RETIRED profile_policy='custom', which used to be the other way in
// (ResolveDefaultProfile returned "" for it). These tests used to force an app
// to 'custom'; migration 0036 asserts zero such apps and narrows the CHECK, so
// that is now a constraint violation rather than a test fixture. The escape
// hatch below is reachable for ANY app and is in the frozen contract — which is
// also why apps.default_width/height/fps/bitrate_kbps stay: they are this path's
// CEILING via capAndPick.
func legacyOverride() StreamOverride {
	w, h, fps, kbps := int32(1280), int32(720), int32(60), int32(6000)
	return StreamOverride{Width: &w, Height: &h, FPS: &fps, BitrateKbps: &kbps}
}

// TestLaunchByProfileResolvesParams: profile_id=1080p60 with no probe resolves to
// the profile's concrete stream values and persists profile_id on the session.
func TestLaunchByProfileResolvesParams(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	s := res.Session
	// 1080p60: 1920x1080@60, bitrate 12000 (not the app default 15000), h264 high, playout0 50.
	if s.Width != 1920 || s.Height != 1080 || s.FPS != 60 {
		t.Errorf("res: got %dx%d@%d, want 1920x1080@60", s.Width, s.Height, s.FPS)
	}
	if s.BitrateKbps != 12000 {
		t.Errorf("bitrate: got %d, want 12000 (1080p60 nominal, not app default)", s.BitrateKbps)
	}
	// The browser transport negotiates H.264 down to the constrained-baseline floor
	// (Chrome's WebRTC receiver rejects High on both VA and NVENC), even though the
	// 1080p60 profile's nominal H264Profile is "high". profile_id still records the
	// selection; a native client / explicit override can lift it.
	if s.H264Profile != "constrained-baseline" {
		t.Errorf("h264_profile: got %q, want constrained-baseline (browser floor)", s.H264Profile)
	}
	if s.Playout0Ms != 50 {
		t.Errorf("playout0_ms: got %d, want 50", s.Playout0Ms)
	}
	if s.ProfileID == nil || *s.ProfileID != "1080p60" {
		t.Errorf("profile_id: got %v, want 1080p60", s.ProfileID)
	}

	// Persisted: a fresh read carries the same profile_id.
	got, err := store.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ProfileID == nil || *got.ProfileID != "1080p60" {
		t.Errorf("persisted profile_id: got %v, want 1080p60", got.ProfileID)
	}
}

// TestLaunchByProfileIneligibleRejected: a fresh probe whose bandwidth is below the
// profile minimum makes that profile ineligible → 409 (ErrProfileIneligible), no row.
func TestLaunchByProfileIneligibleRejected(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	// 4k120 needs ≥ 90000 kbps; 10000 is well below → bandwidth_too_low hard-fail.
	upsertProbe(t, pool, userID, 10000, 5, 2160, time.Now())

	_, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "4k120"})
	if !errors.Is(err, ErrProfileIneligible) {
		t.Fatalf("launch 4k120 on a 10mbps link: got err=%v, want ErrProfileIneligible", err)
	}
}

// TestLaunchByProfileAdminBypassesEligibility: an admin caller launches an
// otherwise-ineligible profile; resolution still comes from the profile.
func TestLaunchByProfileAdminBypassesEligibility(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	upsertProbe(t, pool, userID, 10000, 5, 2160, time.Now())

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "4k120", IsAdmin: true})
	if err != nil {
		t.Fatalf("admin launch 4k120: %v", err)
	}
	if res.Session.Width != 3840 || res.Session.Height != 2160 || res.Session.FPS != 120 {
		t.Errorf("res: got %dx%d@%d, want 3840x2160@120",
			res.Session.Width, res.Session.Height, res.Session.FPS)
	}
	if res.Session.ProfileID == nil || *res.Session.ProfileID != "4k120" {
		t.Errorf("profile_id: got %v, want 4k120", res.Session.ProfileID)
	}
}

// TestLaunchByProfileOverrideBypassesAndBeats: an explicit stream override bypasses
// the eligibility gate (back-compat escape) and beats the profile field-by-field.
func TestLaunchByProfileOverrideBypassesAndBeats(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	upsertProbe(t, pool, userID, 10000, 5, 2160, time.Now())

	bw := int32(8000)
	hi := "high"
	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{
		AppID:     appID,
		ProfileID: "4k120",
		Override:  StreamOverride{BitrateKbps: &bw, H264Profile: &hi},
	})
	if err != nil {
		t.Fatalf("override launch: %v", err)
	}
	// Override beats the profile's 75000 nominal.
	if res.Session.BitrateKbps != 8000 {
		t.Errorf("bitrate: got %d, want 8000 (override beats profile)", res.Session.BitrateKbps)
	}
	// An explicit h264 override is honored over the browser baseline floor.
	if res.Session.H264Profile != "high" {
		t.Errorf("h264_profile: got %q, want high (explicit override)", res.Session.H264Profile)
	}
	// Non-overridden fields still come from the profile.
	if res.Session.Width != 3840 || res.Session.FPS != 120 {
		t.Errorf("res: got %dx_@%d, want 3840x_@120", res.Session.Width, res.Session.FPS)
	}
	// The selected profile id is still recorded.
	if res.Session.ProfileID == nil || *res.Session.ProfileID != "4k120" {
		t.Errorf("profile_id: got %v, want 4k120", res.Session.ProfileID)
	}
}

// TestLaunchByProfileUnknownRejected: a profile_id absent from the catalog → 400.
func TestLaunchByProfileUnknownRejected(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	coord := newTestCoordinator(t, NewStore(pool), newFakeDispatcher(true), testLogger())

	_, err := coord.LaunchByProfile(context.Background(), userID, LaunchParams{AppID: appID, ProfileID: "8k240"})
	if !errors.Is(err, ErrProfileUnknown) {
		t.Fatalf("unknown profile: got err=%v, want ErrProfileUnknown", err)
	}
}

// TestLaunchByProfileDebugNotUserSelectable: a non-user-facing profile (720p30,
// debug) cannot be launched on the user path → ErrProfileIneligible. With an admin
// bypass it launches.
func TestLaunchByProfileDebugNotUserSelectable(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	coord := newTestCoordinator(t, NewStore(pool), newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	if _, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "720p30"}); !errors.Is(err, ErrProfileIneligible) {
		t.Fatalf("debug profile on user path: got err=%v, want ErrProfileIneligible", err)
	}
	// Admin may select it.
	if _, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "720p30", IsAdmin: true}); err != nil {
		t.Fatalf("admin debug-profile launch: %v", err)
	}
}

// TestLaunchByProfileAssignsABRFloor (AS10-04): a profile-launched session's
// session_assign carries the profile's ABR floor in the stream block.
func TestLaunchByProfileAssignsABRFloor(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, NewStore(pool), disp, testLogger())
	ctx := context.Background()

	if _, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60"}); err != nil {
		t.Fatalf("launch: %v", err)
	}

	// The assign→start handshake is dispatched asynchronously.
	waitFor(t, func() bool { return len(disp.types()) >= 1 })
	assign := disp.lastAssignCmd()
	if assign == nil {
		t.Fatal("no session_assign dispatched")
	}
	// 1080p60's catalog ABRFloorKbps is 4000.
	if assign.Stream.AbrFloorKbps != 4000 {
		t.Errorf("assign abr_floor_kbps: got %d, want 4000 (1080p60 profile floor)", assign.Stream.AbrFloorKbps)
	}
}

// TestLaunchLegacyPathOmitsABRFloor (AS10-04): a legacy (no profile_id) launch
// sends abr_floor_kbps=0 so the agent keeps its env/ratio fallback.
//
// UI-P4: the ABR floor now comes from the RESOLVED RUNG, and a legacy launch
// resolves none — so the floor stays 0 exactly as before.
func TestLaunchLegacyPathOmitsABRFloor(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, NewStore(pool), disp, testLogger())
	ctx := context.Background()

	if _, err := coord.Launch(ctx, userID, appID, legacyOverride()); err != nil {
		t.Fatalf("legacy launch: %v", err)
	}

	waitFor(t, func() bool { return len(disp.types()) >= 1 })
	assign := disp.lastAssignCmd()
	if assign == nil {
		t.Fatal("no session_assign dispatched")
	}
	if assign.Stream.AbrFloorKbps != 0 {
		t.Errorf("legacy assign abr_floor_kbps: got %d, want 0 (no profile ⇒ agent fallback)", assign.Stream.AbrFloorKbps)
	}
}

// TestLaunchLegacyPathNilProfile: the stream-override escape hatch leaves BOTH
// profile ids NULL on the session — the launch resolved neither a chain nor a
// rung, so there is nothing to record for either question.
func TestLaunchLegacyPathNilProfile(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	coord := newTestCoordinator(t, NewStore(pool), newFakeDispatcher(true), testLogger())

	res, err := coord.Launch(context.Background(), userID, appID, legacyOverride())
	if err != nil {
		t.Fatalf("legacy launch: %v", err)
	}
	if res.Session.ProfileID != nil {
		t.Errorf("profile_id: got %v, want nil (override launch, no profile)", res.Session.ProfileID)
	}
	if res.Session.StreamProfileID != nil {
		t.Errorf("stream_profile_id: got %v, want nil (no rung resolved)", res.Session.StreamProfileID)
	}
}

// TestProfilePolicyCustomIsRejected — UI-P4 amendment B2: `custom` is retired.
// Migration 0036 asserts zero such apps and narrows the DB CHECK; this is the
// backstop proving the constraint is actually in place.
func TestProfilePolicyCustomIsRejected(t *testing.T) {
	pool := testDB(t)
	_, appID, _ := seed1080pApp(t, pool)
	_, err := pool.Exec(context.Background(),
		`UPDATE apps SET profile_policy = 'custom' WHERE id::text = $1`, appID)
	if err == nil {
		t.Fatal("profile_policy = 'custom' was accepted; migration 0036 must have narrowed the CHECK")
	}
}
