package session

// DB tests for the codec preference (#305): an Auto launch ranks a GPU that
// encodes a better codec ahead of a freer one, and never excludes a candidate.
// Require Postgres (make test-db).

import (
	"context"
	"testing"
)

var avHevcH264 = []string{"av1", "h265", "h264"}

func preferring(s seedIDs, pref []string) CreateParams {
	p := launchParams(s)
	p.CodecPreference = pref
	return p
}

// TestAutoPrefersTheAV1GPUWhileItHasASlot is the ticket's acceptance through
// the whole launch path: GPU 0 (one slot) encodes av1, GPU 1 (four slots) does
// not. Spread alone picks GPU 1; the preference picks GPU 0 while it has a slot,
// and the session gets av1. Once GPU 0 is full the launch lands on GPU 1 and
// resolves h265 there (#303).
func TestAutoPrefersTheAV1GPUWhileItHasASlot(t *testing.T) {
	pool := testDB(t)
	userID, appID, hostID := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	gpu1ID := seedSecondGPU(t, pool, hostID, 16384, 4)
	if _, err := pool.Exec(ctx, `UPDATE gpus SET encode_slots_total = 1 WHERE host_id::text = $1 AND index = 0`, hostID); err != nil {
		t.Fatalf("shrink gpu 0: %v", err)
	}
	setHostCodecsRaw(t, pool, hostID, `["h264","h265","av1"]`)
	setGPUCodecsRaw(t, pool, hostID, 0, `["h264","h265","av1"]`)
	setGPUCodecsRaw(t, pool, hostID, 1, `["h264","h265"]`)
	setQuota(t, pool, userID, 20)
	enableChainCodecs(t, pool, "1080p60", "av1", "hevc", "h264")

	// The fixture's point: with no preference, spread sends the launch to GPU 1.
	spread, err := store.ScheduleAndCreate(ctx, launchParams(seedIDs{userID: userID, appID: appID}))
	if err != nil {
		t.Fatalf("unpreferred launch: %v", err)
	}
	if spread.GPUID == nil || *spread.GPUID != gpu1ID {
		t.Fatalf("fixture: spread should pick GPU 1, got index %v", derefI32(spread.GPUIndex))
	}
	releaseSession(t, pool, spread.ID)

	upsertCodecProbe(t, pool, userID, true, true)
	launch := func() *Session {
		t.Helper()
		res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60", IsAdmin: true})
		if err != nil {
			t.Fatalf("launch: %v", err)
		}
		return &res.Session
	}

	first := launch()
	if first.GPUIndex == nil || *first.GPUIndex != 0 {
		t.Fatalf("Auto with an AV1-capable device placed on index %v, want GPU 0 (the only av1 GPU)", derefI32(first.GPUIndex))
	}
	if first.Codec != "av1" {
		t.Errorf("session on GPU 0 codec = %q, want av1", first.Codec)
	}

	second := launch()
	if second.GPUIndex == nil || *second.GPUIndex != 1 {
		t.Fatalf("with GPU 0 full, placed on index %v, want GPU 1", derefI32(second.GPUIndex))
	}
	if second.Codec != "h265" {
		t.Errorf("session on GPU 1 codec = %q, want h265 (GPU 1 has no av1)", second.Codec)
	}
}

// TestAutoWithNoProbeIsSpread: without a device probe only h264 survives the
// client clamps, so there is no preference and spread decides.
func TestAutoWithNoProbeIsSpread(t *testing.T) {
	pool := testDB(t)
	userID, appID, hostID := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	seedSecondGPU(t, pool, hostID, 16384, 4)
	if _, err := pool.Exec(ctx, `UPDATE gpus SET encode_slots_total = 1 WHERE host_id::text = $1 AND index = 0`, hostID); err != nil {
		t.Fatalf("shrink gpu 0: %v", err)
	}
	setGPUCodecsRaw(t, pool, hostID, 0, `["h264","h265","av1"]`)
	setGPUCodecsRaw(t, pool, hostID, 1, `["h264","h265"]`)
	enableChainCodecs(t, pool, "1080p60", "av1", "hevc", "h264")

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60", IsAdmin: true})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.GPUIndex == nil || *res.Session.GPUIndex != 1 {
		t.Fatalf("no-probe launch placed on index %v, want GPU 1 (spread)", derefI32(res.Session.GPUIndex))
	}
	if res.Session.Codec != "h264" {
		t.Errorf("no-probe codec = %q, want h264", res.Session.Codec)
	}
}

// TestCodecPreferenceNeverExcludes: the preference is a sort key. A launch
// whose preferred GPU is unavailable, or that no GPU matches at all, still lands.
func TestCodecPreferenceNeverExcludes(t *testing.T) {
	t.Run("the preferred GPU is readiness-blocked", func(t *testing.T) {
		pool := testDB(t)
		store := NewStore(pool)
		s, gpu1ID := seedAV1OnGPU0(t, pool)
		reportReadiness(t, pool, s.hostID, 5, false, false)
		blockGPU(t, pool, s.gpuID, true)

		sess, err := store.ScheduleAndCreate(context.Background(), preferring(s, avHevcH264))
		if err != nil {
			t.Fatalf("launch with the av1 GPU blocked: %v", err)
		}
		if sess.GPUID == nil || *sess.GPUID != gpu1ID {
			t.Fatalf("placed on %v, want GPU 1", deref(sess.GPUID))
		}
	})

	t.Run("the preferred GPU has no slots", func(t *testing.T) {
		pool := testDB(t)
		store := NewStore(pool)
		s, gpu1ID := seedAV1OnGPU0(t, pool)
		if _, err := pool.Exec(context.Background(),
			`UPDATE gpus SET encode_slots_total = 0 WHERE id::text = $1`, s.gpuID); err != nil {
			t.Fatalf("zero gpu 0: %v", err)
		}

		sess, err := store.ScheduleAndCreate(context.Background(), preferring(s, avHevcH264))
		if err != nil {
			t.Fatalf("launch with the av1 GPU unusable: %v", err)
		}
		if sess.GPUID == nil || *sess.GPUID != gpu1ID {
			t.Fatalf("placed on %v, want GPU 1", deref(sess.GPUID))
		}
	})

	t.Run("no GPU encodes any preferred codec", func(t *testing.T) {
		pool := testDB(t)
		store := NewStore(pool)
		s, gpu1ID := seedAV1OnGPU0(t, pool)

		sess, err := store.ScheduleAndCreate(context.Background(), preferring(s, []string{"vp9"}))
		if err != nil {
			t.Fatalf("launch preferring a codec nobody encodes: %v", err)
		}
		// Every GPU ties on the key, so spread decides.
		if sess.GPUID == nil || *sess.GPUID != gpu1ID {
			t.Fatalf("placed on %v, want GPU 1 (spread)", deref(sess.GPUID))
		}
	})
}

// TestEmptyCodecPreferenceIsSpread: nil and empty both leave the order to spread.
func TestEmptyCodecPreferenceIsSpread(t *testing.T) {
	for _, pref := range [][]string{nil, {}} {
		pool := testDB(t)
		store := NewStore(pool)
		s, gpu1ID := seedAV1OnGPU0(t, pool)

		sess, err := store.ScheduleAndCreate(context.Background(), preferring(s, pref))
		if err != nil {
			t.Fatalf("launch with preference %#v: %v", pref, err)
		}
		if sess.GPUID == nil || *sess.GPUID != gpu1ID {
			t.Fatalf("preference %#v placed on %v, want GPU 1 (spread)", pref, deref(sess.GPUID))
		}
	}
}

// TestLocalityBeatsCodecPreference: a home on another host is a different
// install, so the locality key ranks ahead of the preference.
func TestLocalityBeatsCodecPreference(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool, WithPlacementPolicy(PolicyLocality))
	s := seed(t, pool, 4)
	h2, _ := seedSecondHost(t, pool, 16384, 2)
	setGPUCodecsRaw(t, pool, s.hostID, 0, `["h264"]`)
	setGPUCodecsRaw(t, pool, h2, 0, `["h264","h265","av1"]`)
	setQuota(t, pool, s.userID, 10)
	ctx := context.Background()

	// Control: with no home, the preference sends the launch to host-2 even
	// though host-1 is freer.
	homeless := seedManagedApp2(t, pool)
	p := managedLaunchParams(s, homeless)
	p.CodecPreference = avHevcH264
	sess, err := store.ScheduleAndCreate(ctx, p)
	if err != nil {
		t.Fatalf("homeless launch: %v", err)
	}
	if sess.HostID == nil || *sess.HostID != h2 {
		t.Fatalf("homeless launch placed on %v, want host-2 (the av1 host)", deref(sess.HostID))
	}

	homed := seedManagedApp(t, pool, `{}`)
	seedHome(t, pool, s.userID, homed, s.hostID)
	p = managedLaunchParams(s, homed)
	p.CodecPreference = avHevcH264
	sess, err = store.ScheduleAndCreate(ctx, p)
	if err != nil {
		t.Fatalf("homed launch: %v", err)
	}
	if sess.HostID == nil || *sess.HostID != s.hostID {
		t.Fatalf("homed launch placed on %v, want the home host %s", deref(sess.HostID), s.hostID)
	}
}
