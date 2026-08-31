package session

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// AS10-11 — DB integration: the profile-certification history store + the
// coordinator's EvaluateClientHealth consumption path (records a fail on a
// sustained client degradation, clears it on a sustained smooth run, and never
// records while hidden or network-degraded). Needs Postgres (exercises the 0013
// migration). Run via scripts/dev/dev.sh go-test-db.

// runningProfileSession launches, ties to a LAUNCH profile and its resolved
// rung, and drives to running.
func runningProfileSession(t *testing.T, store *Store, pool *pgxpool.Pool, s seedIDs, profileID string) Session {
	t.Helper()
	ctx := context.Background()
	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE sessions SET profile_id = $2, stream_profile_id = $3 WHERE id::text = $1`,
		sess.ID, profileID, profileID+"-h264"); err != nil {
		t.Fatalf("set profile_id: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateStarting, nil, nil); err != nil {
		t.Fatalf("→ starting: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateRunning, nil, nil); err != nil {
		t.Fatalf("→ running: %v", err)
	}
	return sess
}

func TestCertHistoryRecordAndOverride(t *testing.T) {
	pool := testDB(t)
	store, _, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	// Fail then pass at the same (user, device, profile): latest-outcome-wins.
	// codec='h264' — the floor — profile-blocks exactly like a pre-0032 fail.
	if err := store.RecordProfileOutcome(ctx, s.userID, "dev-1", "4k120", "h264", outcomeFail, strptr("decode_degrading")); err != nil {
		t.Fatalf("record fail: %v", err)
	}
	fails, err := store.ProfileFailures(ctx, s.userID, "dev-1")
	if err != nil {
		t.Fatalf("failures: %v", err)
	}
	if !fails["4k120"] {
		t.Fatal("expected 4k120 in failures after a fail")
	}

	if err := store.RecordProfileOutcome(ctx, s.userID, "dev-1", "4k120", "h264", outcomePass, nil); err != nil {
		t.Fatalf("record pass: %v", err)
	}
	fails, _ = store.ProfileFailures(ctx, s.userID, "dev-1")
	if fails["4k120"] {
		t.Fatal("a sustained pass must clear a prior fail (latest-outcome-wins)")
	}
}

// TestRungFailuresLegacyAndRungLevelTogether — UI-P4 clamp 4 at rung grain
// (re-spec §4.4). The read is a UNION of two shapes and this exercises BOTH in
// one launch profile, which is the case that a single-shape implementation
// silently gets wrong.
//
//   - a LEGACY row keyed (launch profile, codec) bans EVERY rung of that chain
//     using that codec — precisely its pre-UI-P4 meaning. All 147 rows live on
//     Tower are this shape, which is why no history migration is needed.
//   - a RUNG-LEVEL row keyed by rung id bans exactly that rung, and crucially
//     NOT its sibling that shares a codec at a different resolution. A codec-only
//     key cannot express that, and decode failure IS resolution-dependent.
func TestRungFailuresLegacyAndRungLevelTogether(t *testing.T) {
	pool := testDB(t)
	store, _, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	// A chain with two AV1 rungs at different resolutions plus an HEVC rung and
	// the h264 floor. Two rungs sharing a codec at different resolutions is the
	// normal shape of a real chain and is legal under the floor rule.
	lp := seedChain(t, pool, "mixed", []chainRung{
		{id: "mixed-av1-2160", codec: "av1", w: 3840, h: 2160},
		{id: "mixed-av1-1080", codec: "av1", w: 1920, h: 1080},
		{id: "mixed-hevc-1080", codec: "hevc", w: 1920, h: 1080},
		{id: "mixed-h264-1080", codec: "h264", w: 1920, h: 1080},
	})

	// (1) A RUNG-LEVEL decode fail on the 4K AV1 rung.
	if err := store.RecordProfileOutcome(ctx, s.userID, "dev-1", "mixed-av1-2160", "av1", outcomeFail, strptr("client_unsupported")); err != nil {
		t.Fatalf("record rung fail: %v", err)
	}
	// (2) A LEGACY launch-profile-level fail for h265.
	if err := store.RecordProfileOutcome(ctx, s.userID, "dev-1", "mixed", "h265", outcomeFail, strptr("client_unsupported")); err != nil {
		t.Fatalf("record legacy fail: %v", err)
	}

	banned, err := store.RungFailures(ctx, s.userID, "dev-1", lp)
	if err != nil {
		t.Fatalf("rung failures: %v", err)
	}
	if !banned["mixed-av1-2160"] {
		t.Error("the rung-level AV1 4K fail must ban that rung")
	}
	if banned["mixed-av1-1080"] {
		t.Error("a 4K AV1 decode failure must NOT ban the 1080p AV1 rung — decode failure is resolution-dependent")
	}
	if !banned["mixed-hevc-1080"] {
		t.Error("a legacy (launch profile, h265) fail must ban every hevc rung of that chain")
	}
	if banned["mixed-h264-1080"] {
		t.Error("nothing failed h264; the floor rung must stay available")
	}

	// (3) Neither shape is profile-blocking: eligibility must still offer the chain.
	fails, err := store.ProfileFailures(ctx, s.userID, "dev-1")
	if err != nil {
		t.Fatalf("profile failures: %v", err)
	}
	if fails["mixed"] {
		t.Error("codec-scoped decode fails must NOT make the launch profile ineligible")
	}

	// (4) A PASS clears BOTH shapes for the codec that ran. An av1 pass clears the
	// rung-level av1 row; the h265 legacy row is untouched.
	if err := store.RecordProfileOutcome(ctx, s.userID, "dev-1", "mixed", "av1", outcomePass, nil); err != nil {
		t.Fatalf("record av1 pass: %v", err)
	}
	banned, _ = store.RungFailures(ctx, s.userID, "dev-1", lp)
	if banned["mixed-av1-2160"] {
		t.Error("a sustained av1 pass must clear the rung-level av1 fail (proof by success)")
	}
	if !banned["mixed-hevc-1080"] {
		t.Error("an av1 pass must NOT clear the h265 legacy fail")
	}

	// (5) An h265 pass clears the legacy row too.
	if err := store.RecordProfileOutcome(ctx, s.userID, "dev-1", "mixed", "h265", outcomePass, nil); err != nil {
		t.Fatalf("record h265 pass: %v", err)
	}
	banned, _ = store.RungFailures(ctx, s.userID, "dev-1", lp)
	if len(banned) != 0 {
		t.Fatalf("expected no bans after passes on both codecs, got %v", banned)
	}
}

// TestProfileLevelFailStillBlocksEligibility — a presentation-side (” codec)
// fail is codec- and resolution-independent and still blanks the whole chain.
func TestProfileLevelFailStillBlocksEligibility(t *testing.T) {
	pool := testDB(t)
	store, _, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	if err := store.RecordProfileOutcome(ctx, s.userID, "dev-1", "1080p60", "", outcomeFail, strptr("presentation_degrading")); err != nil {
		t.Fatalf("record profile-level fail: %v", err)
	}
	fails, _ := store.ProfileFailures(ctx, s.userID, "dev-1")
	if !fails["1080p60"] {
		t.Fatal("a profile-level ('') fail must make the launch profile ineligible")
	}

	if err := store.RecordProfileOutcome(ctx, s.userID, "dev-1", "1080p60", "h264", outcomePass, nil); err != nil {
		t.Fatalf("record h264 pass: %v", err)
	}
	fails, _ = store.ProfileFailures(ctx, s.userID, "dev-1")
	if fails["1080p60"] {
		t.Fatal("a sustained pass must clear the profile-level fail")
	}
}

// TestEvaluateClientHealthCodecScopedFail — end-to-end through the evaluator: a
// sustained client_unsupported on an h265 session records the fail against the
// RESOLVED RUNG (UI-P4 grain), the launch profile stays eligible, and the
// resolver's clamp-4 input reflects it.
func TestEvaluateClientHealthCodecScopedFail(t *testing.T) {
	pool := testDB(t)
	store, coord, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	lp := seedChain(t, pool, "hevc-chain", []chainRung{
		{id: "hevc-chain-hevc", codec: "hevc", w: 1920, h: 1080},
		{id: "hevc-chain-h264", codec: "h264", w: 1920, h: 1080},
	})

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE sessions SET profile_id = 'hevc-chain', stream_profile_id = 'hevc-chain-hevc', codec = 'h265' WHERE id::text = $1`,
		sess.ID); err != nil {
		t.Fatalf("set profile/rung/codec: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateStarting, nil, nil); err != nil {
		t.Fatalf("→ starting: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateRunning, nil, nil); err != nil {
		t.Fatalf("→ running: %v", err)
	}

	coord.health.mu.Lock()
	coord.health.clientRuns[sess.ID] = &clientHealthRun{class: ClientHealthUnsupported, since: time.Now().Add(-time.Minute)}
	coord.health.mu.Unlock()
	coord.EvaluateClientHealth(ctx, sess.ID, ClientHealthSample{Class: ClientHealthUnsupported, DeviceKey: "dev-1"})

	fails, _ := store.ProfileFailures(ctx, s.userID, "dev-1")
	if fails["hevc-chain"] {
		t.Fatal("h265 client_unsupported must not blank the launch profile")
	}
	banned, _ := store.RungFailures(ctx, s.userID, "dev-1", lp)
	if !banned["hevc-chain-hevc"] {
		t.Fatalf("expected the resolved HEVC rung to be banned, got %v", banned)
	}
	if banned["hevc-chain-h264"] {
		t.Fatal("the h264 floor rung must never be banned by an h265 decode failure")
	}
}

func TestEvaluateClientHealthRecordsFailAndRecovers(t *testing.T) {
	pool := testDB(t)
	store, coord, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess := runningProfileSession(t, store, pool, s, "1080p60")

	// Seed a sustained decode-degrading run, then a degraded sample → fail recorded
	// + client_decode_degrading state.
	coord.health.mu.Lock()
	coord.health.clientRuns[sess.ID] = &clientHealthRun{class: ClientHealthDecode, since: time.Now().Add(-time.Minute)}
	coord.health.mu.Unlock()
	coord.EvaluateClientHealth(ctx, sess.ID, ClientHealthSample{Class: ClientHealthDecode, DeviceKey: "dev-1"})

	got, _ := store.Get(ctx, sess.ID)
	if got.HealthState != HealthClientDecodeDegrading {
		t.Fatalf("health_state: got %q want client_decode_degrading", got.HealthState)
	}
	// The decode-side fail is written at RUNG grain (UI-P4), and ProfileFailures
	// maps an h264 rung failure back to its launch profile — if the FLOOR itself
	// fails to decode, no codec can save the chain on this device, which is
	// exactly the pre-UI-P4 meaning.
	banned, _ := store.RungFailures(ctx, s.userID, "dev-1", mustGetLaunchProfile(t, store, "1080p60"))
	if !banned["1080p60-h264"] {
		t.Fatalf("sustained decode-degrading must record a fail against the resolved rung, got %v", banned)
	}
	fails, _ := store.ProfileFailures(ctx, s.userID, "dev-1")
	if !fails["1080p60"] {
		t.Fatal("an h264 (floor) decode failure must still block the launch profile in eligibility")
	}

	// Sustained smooth → clears to healthy + certifies a pass.
	coord.health.mu.Lock()
	coord.health.clientRuns[sess.ID] = &clientHealthRun{class: ClientHealthSmooth, since: time.Now().Add(-time.Minute)}
	coord.health.mu.Unlock()
	coord.EvaluateClientHealth(ctx, sess.ID, ClientHealthSample{Class: ClientHealthSmooth, DeviceKey: "dev-1"})

	got, _ = store.Get(ctx, sess.ID)
	if got.HealthState != HealthHealthy {
		t.Fatalf("health_state after recovery: got %q want healthy", got.HealthState)
	}
	fails, _ = store.ProfileFailures(ctx, s.userID, "dev-1")
	if fails["1080p60"] {
		t.Fatal("sustained smooth must clear the cert fail (recovery-on-success)")
	}
	banned, _ = store.RungFailures(ctx, s.userID, "dev-1", mustGetLaunchProfile(t, store, "1080p60"))
	if banned["1080p60-h264"] {
		t.Fatal("a pass must clear the rung-level row too, not just the launch-profile-level one")
	}
}

// TestProfileFailuresH264ArmOnlyBlocksOnTheFloorRung — the h264 rung→launch
// profile arm of ProfileFailures must fire ONLY for the rung the floor would
// hand back.
//
// The arm exists because the resolver's unconditional floor bypasses clamp 4: if
// the FLOOR rung fails decode, the device would be handed the very rung it just
// failed, forever, with no eligibility signal — so the chain has to go
// ineligible. That justification does NOT extend to a non-floor h264 rung. In a
// hand-built chain like [4k60-h264, 1080p60-h264], clamp 4 correctly skips the
// failed 4K rung and lands on the working 1080p one; blanking the whole chain
// kills a launch that would have worked, and it does so precisely for the
// multi-rung configurations the restructure exists to enable.
func TestProfileFailuresH264ArmOnlyBlocksOnTheFloorRung(t *testing.T) {
	pool := testDB(t)
	store, _, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	// Two h264 rungs at different resolutions. Legal under the floor rule, and the
	// exact shape an operator builds when they want 4K on capable clients and a
	// 1080p fallback on everything else.
	lp := seedChain(t, pool, "twoh264", []chainRung{
		{id: "twoh264-4k", codec: "h264", w: 3840, h: 2160, minDec: 2160},
		{id: "twoh264-1080", codec: "h264", w: 1920, h: 1080, minDec: 1080},
	})

	// (1) A decode fail on the NON-floor 4K rung.
	if err := store.RecordProfileOutcome(ctx, s.userID, "dev-1", "twoh264-4k", "h264", outcomeFail, strptr("decode_degrading")); err != nil {
		t.Fatalf("record 4k rung fail: %v", err)
	}

	fails, err := store.ProfileFailures(ctx, s.userID, "dev-1")
	if err != nil {
		t.Fatalf("profile failures: %v", err)
	}
	if fails["twoh264"] {
		t.Error("an h264 fail on a NON-floor rung must NOT blank the launch profile — " +
			"clamp 4 skips that rung and the chain still resolves to its working h264 floor")
	}

	// …and clamp 4 does its job at the right grain: the 4K rung is banned, the
	// 1080p floor is not.
	banned, err := store.RungFailures(ctx, s.userID, "dev-1", lp)
	if err != nil {
		t.Fatalf("rung failures: %v", err)
	}
	if !banned["twoh264-4k"] {
		t.Error("the rung-level h264 fail must still ban the 4K rung (clamp 4)")
	}
	if banned["twoh264-1080"] {
		t.Error("a 4K h264 decode failure must NOT ban the 1080p h264 rung")
	}

	// (2) A decode fail on the FLOOR rung (the last h264 rung) DOES blank the
	//     chain — that is the case the arm was written for, and it must survive
	//     the restriction.
	if err := store.RecordProfileOutcome(ctx, s.userID, "dev-1", "twoh264-1080", "h264", outcomeFail, strptr("decode_degrading")); err != nil {
		t.Fatalf("record floor rung fail: %v", err)
	}
	fails, _ = store.ProfileFailures(ctx, s.userID, "dev-1")
	if !fails["twoh264"] {
		t.Fatal("an h264 fail on the FLOOR rung must blank the launch profile — the floor " +
			"bypasses clamp 4, so the device would be handed the rung it just failed, forever")
	}

	// (3) A sustained pass on the floor rung clears it again.
	if err := store.RecordProfileOutcome(ctx, s.userID, "dev-1", "twoh264-1080", "h264", outcomePass, nil); err != nil {
		t.Fatalf("record floor rung pass: %v", err)
	}
	fails, _ = store.ProfileFailures(ctx, s.userID, "dev-1")
	if fails["twoh264"] {
		t.Error("a sustained pass on the floor rung must clear the block (latest-outcome-wins)")
	}
}

// mustGetLaunchProfile reads a chain with its rungs, failing the test on error.
func mustGetLaunchProfile(t *testing.T, store *Store, id string) profile.LaunchProfile {
	t.Helper()
	lp, err := store.GetLaunchProfile(context.Background(), id)
	if err != nil {
		t.Fatalf("get launch profile %s: %v", id, err)
	}
	return lp
}

func TestEvaluateClientHealthHiddenNeverRecords(t *testing.T) {
	pool := testDB(t)
	store, coord, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess := runningProfileSession(t, store, pool, s, "1080p60")

	// A long degraded run, but the sample is hidden → no fail, no state flip.
	coord.health.mu.Lock()
	coord.health.clientRuns[sess.ID] = &clientHealthRun{class: ClientHealthDecode, since: time.Now().Add(-time.Hour)}
	coord.health.mu.Unlock()
	coord.EvaluateClientHealth(ctx, sess.ID, ClientHealthSample{
		Class: ClientHealthDecode, DeviceKey: "dev-1", IsHidden: true,
	})

	got, _ := store.Get(ctx, sess.ID)
	if got.HealthState != HealthHealthy {
		t.Fatalf("hidden must not flip state: got %q want healthy", got.HealthState)
	}
	fails, _ := store.ProfileFailures(ctx, s.userID, "dev-1")
	if fails["1080p60"] {
		t.Fatal("a hidden tab must NEVER record a cert fail (#1 false-positive guard)")
	}
}
