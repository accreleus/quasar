package session

// cert_launch_order_test.go — how the SPT-06 certification cap and UI-P4 rung
// resolution COMPOSE on the launch path after migration 0041.
//
// The cap used to run BEFORE rung resolution. It cannot any more: a cert is keyed
// on the rung, and the rung is not known until the walk has run. applyPostPlacement
// now resolves, caps, and RE-resolves over the capped chain. These tests pin the
// three properties that reordering had to preserve or establish:
//
//  1. the SPT-06 behaviour that shipped still fires (an unsafe h264 cert on
//     today's single-rung data still caps 1080p60 → 720p60);
//  2. a cert for a codec the session did NOT resolve does not touch it — the
//     defect being fixed, observed end-to-end through LaunchByProfile rather than
//     at the store;
//  3. after a cap, the session actually carries the LOWER chain's rung, so the
//     cap and the walk compose rather than one overwriting the other.
//
// They need Postgres (TEST_DATABASE_URL).

import (
	"context"
	"testing"
)

// launchCertRow builds an unsafe cert for one rung on the session's placed host.
func launchCertRow(hostID, profileID, rungID string, bitrate int, verdict string) EncoderCertRow {
	r := certSeedRow(hostID, 0, "va", profileID, rungID, bitrate, verdict)
	r.EncodeP95 = 25.0
	return r
}

// TestLaunchCertCapStillCapsH264 is the no-regression case: the exact behaviour
// SPT-06 shipped, through the reordered launch path, on the data every deployment
// actually has (one h264 rung per chain).
func TestLaunchCertCapStillCapsH264(t *testing.T) {
	pool := testDB(t)
	userID, appID, hostID := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	// 1080p60's nominal bitrate is 12000; the cap looks the cert up at the bitrate
	// the rung would actually dispatch at.
	must(t, store.UpsertEncoderCert(ctx,
		launchCertRow(hostID, "1080p60", "1080p60-h264", 12000, VerdictUnsafe)))

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	s := res.Session

	if s.ProfileID == nil || *s.ProfileID != "720p60" {
		t.Errorf("launch profile: got %v want 720p60 (unsafe 1080p60 cert must cap)", s.ProfileID)
	}
	// AND the walk ran over the CAPPED chain: the session carries its rung, not the
	// original chain's. This is what proves the two mechanisms composed.
	if s.StreamProfileID == nil || *s.StreamProfileID != "720p60-h264" {
		t.Errorf("resolved rung: got %v want 720p60-h264", s.StreamProfileID)
	}
	if s.Width != 1280 || s.Height != 720 {
		t.Errorf("resolution: got %dx%d want 1280x720 (the capped chain's rung)", s.Width, s.Height)
	}

	// The persisted row agrees with what was dispatched — one write, both values.
	got, err := store.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ProfileID == nil || *got.ProfileID != "720p60" ||
		got.StreamProfileID == nil || *got.StreamProfileID != "720p60-h264" {
		t.Errorf("persisted ids: got %v / %v want 720p60 / 720p60-h264", got.ProfileID, got.StreamProfileID)
	}
	if got.Width != 1280 || got.BitrateKbps != s.BitrateKbps {
		t.Errorf("persisted stream disagrees with the dispatched one: %dx%d @%d vs @%d",
			got.Width, got.Height, got.BitrateKbps, s.BitrateKbps)
	}
}

// TestLaunchCertCapIgnoresOtherCodecsRung is the defect, end to end. An AV1 rung
// of the 1080p60 chain is certified `unsafe`; the launching client has no AV1
// decode probe, so the walk resolves the H.264 rung, which has never been
// measured. The launch must NOT be capped.
//
// Before 0041 that cert row was keyed (host, gpu, encoder, "1080p60", bitrate) —
// indistinguishable from an h264 verdict — so this launch was silently downgraded
// to 720p60 on the strength of a measurement of a codec it was not going to use.
func TestLaunchCertCapIgnoresOtherCodecsRung(t *testing.T) {
	pool := testDB(t)
	userID, appID, hostID := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	av1Rung := seedExtraRung(t, pool, "1080p60", "av1")
	must(t, store.UpsertEncoderCert(ctx,
		launchCertRow(hostID, "1080p60", av1Rung, 12000, VerdictUnsafe)))

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	s := res.Session

	if s.StreamProfileID == nil || *s.StreamProfileID != "1080p60-h264" {
		t.Fatalf("resolved rung: got %v want 1080p60-h264 (no AV1 decode probe ⇒ h264)", s.StreamProfileID)
	}
	if s.ProfileID == nil || *s.ProfileID != "1080p60" {
		t.Errorf("launch profile: got %v want 1080p60 — an AV1 verdict must not cap an H.264 launch", s.ProfileID)
	}
	if s.Width != 1920 || s.Height != 1080 {
		t.Errorf("resolution: got %dx%d want 1920x1080 (uncapped)", s.Width, s.Height)
	}
	if s.Codec != wireCodecH264 {
		t.Errorf("codec: got %q want h264", s.Codec)
	}
}

// TestLaunchCertCapUncertifiedIsOptimistic guards the posture the cap has always
// had and that the reorder must not have changed: no row means uncertified, which
// means no cap. Paired with the test above it separates "not capped because the
// cert was scoped away" from "never caps anything".
func TestLaunchCertCapUncertifiedIsOptimistic(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.ProfileID == nil || *res.Session.ProfileID != "1080p60" {
		t.Errorf("launch profile: got %v want 1080p60 (uncertified ⇒ optimistic)", res.Session.ProfileID)
	}
	if res.Session.StreamProfileID == nil || *res.Session.StreamProfileID != "1080p60-h264" {
		t.Errorf("resolved rung: got %v want 1080p60-h264", res.Session.StreamProfileID)
	}
}

// TestLaunchCertCapSkippedForAdmin pins the AS10-03 convention through the new
// ordering: an admin launch bypasses the cap entirely, so an unsafe cert on the
// resolved rung changes nothing. The rung still resolves (the walk is not
// conditional on the cap), which is what makes the reorder safe.
func TestLaunchCertCapSkippedForAdmin(t *testing.T) {
	pool := testDB(t)
	userID, appID, hostID := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	must(t, store.UpsertEncoderCert(ctx,
		launchCertRow(hostID, "1080p60", "1080p60-h264", 12000, VerdictUnsafe)))

	res, err := coord.LaunchByProfile(ctx, userID,
		LaunchParams{AppID: appID, ProfileID: "1080p60", IsAdmin: true})
	if err != nil {
		t.Fatalf("admin launch: %v", err)
	}
	s := res.Session
	if s.ProfileID == nil || *s.ProfileID != "1080p60" {
		t.Errorf("admin launch profile: got %v want 1080p60 (cap must not apply)", s.ProfileID)
	}
	if s.StreamProfileID == nil || *s.StreamProfileID != "1080p60-h264" {
		t.Errorf("admin resolved rung: got %v want 1080p60-h264", s.StreamProfileID)
	}
}
