package session

// AS-02 integration tests: probe consumer at POST /v1/sessions.
//
// These tests require a real Postgres (TEST_DATABASE_URL). They verify:
//   - probe present → tier selected, session stamped with tier params
//   - probe stale (> 30 days) → default tier
//   - probe absent (no user_devices row) → default tier
//   - explicit launch override beats tier selection
//   - app default_* values cap the tier (a 720p app never launches 1080p)
//
// Pattern follows lifecycle_test.go (same package, shared testDB / seed helpers).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// --- seed helpers for probe consumer tests -----------------------------------

// upsertProbe inserts or updates a user_devices row with the given capability
// fields. measuredAt controls whether the probe appears fresh or stale.
func upsertProbe(t *testing.T, pool *pgxpool.Pool, userID string, bwKbps, rttMs, maxDecodeHeight int32, measuredAt time.Time) {
	t.Helper()
	ctx := context.Background()
	caps, err := json.Marshal(map[string]any{
		"bandwidth_kbps":    bwKbps,
		"rtt_ms":            rttMs,
		"max_decode_height": maxDecodeHeight,
		"measured_at":       measuredAt.UTC().Format(time.RFC3339),
		"codecs":            map[string]bool{"h264": true},
	})
	if err != nil {
		t.Fatalf("marshal caps: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_devices (user_id, device_key, capabilities)
		VALUES ($1::uuid, 'probe-device', $2)
		ON CONFLICT (user_id, device_key) DO UPDATE
		    SET capabilities = EXCLUDED.capabilities,
		        last_seen_at = now()
	`, userID, caps); err != nil {
		t.Fatalf("upsert probe: %v", err)
	}
}

// seed1080pApp inserts an app whose defaults allow 1080p60/15000kbps, so the
// full 1080p60 / 1080p60-lan tier can pass through uncapped.
func seed1080pApp(t *testing.T, pool *pgxpool.Pool) (userID, appID, hostID string) {
	t.Helper()
	ctx := context.Background()
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('probe@test.local','probeuser','x') RETURNING id::text`).Scan(&userID))
	must(t, pool.QueryRow(ctx, `INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height, default_fps, default_bitrate_kbps)
		VALUES ('hd-probe-app', 1024, 1, 1920, 1080, 60, 15000) RETURNING id::text`).Scan(&appID))
	entitleAll(t, pool, appID)
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, capacity_detection)
		VALUES ('host-probe','online','ok') RETURNING id::text`).Scan(&hostID))
	var gpuID string
	must(t, pool.QueryRow(ctx, `INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
		VALUES ($1, 0, 16384, 4) RETURNING id::text`, hostID).Scan(&gpuID))
	return
}

// --- probe consumer integration tests ----------------------------------------

// TestProbeConsumerProbeStale: a probe whose measured_at is older than 30 days
// is treated as absent → default tier = playout0=50.
func TestProbeConsumerProbeStale(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	// Insert a stale probe (31 days old, past the 30-day cut).
	staleTime := time.Now().Add(-31 * 24 * time.Hour)
	upsertProbe(t, pool, s.userID, 50000, 5, 2160, staleTime)

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// Stale probe → default tier (1080p60): playout0_ms = 50.
	if res.Session.Playout0Ms != 50 {
		t.Errorf("playout0_ms: got %d, want 50 (default tier, stale probe)", res.Session.Playout0Ms)
	}
}

// TestProbeConsumerProbeAbsent: no user_devices row → default tier = playout0=50.
func TestProbeConsumerProbeAbsent(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	// No device row inserted for this user.
	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// No probe → default tier: playout0_ms = 50.
	if res.Session.Playout0Ms != 50 {
		t.Errorf("playout0_ms: got %d, want 50 (default tier, no probe)", res.Session.Playout0Ms)
	}
}

// TestProbeConsumerRecommendedProfile: an omitted-profile launch on an
// 'inherit'-policy app (no app/user/global default configured) resolves the
// device-recommended *profile* — the terminal fallback of the CP-01 resolution
// chain (override → app policy → user pref → global default → recommendation;
// see docs/design/plans/2026-06-24-launch-profile-host-settings-ui-plan.html and
// migration 0015). The legacy AS-02 tier ladder no longer drives an omitted-
// profile launch; the recommended profile takes its place.
//
// For a strong LAN probe (bw=50000, rtt=5, decode=2160) the highest fully-
// eligible user-facing profile is 1440p60 (4k120/4k60/1440p120 are ineligible or
// merely risky). seed1080pApp's default_width/height (1920x1080) no longer cap
// it — capping is now expressed via profile_policy/default_profile_id, not the
// legacy app default_* dimensions (see TestProbeConsumerAppForcedProfileCaps).
func TestProbeConsumerRecommendedProfile(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	// Excellent LAN probe → 1440p60 is the highest fully-eligible profile.
	upsertProbe(t, pool, userID, 50000, 5, 2160, time.Now())

	res, err := coord.Launch(ctx, userID, appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// 1440p60: width=2560, height=1440, fps=60, nominal bitrate=20000, playout0=50.
	if res.Session.Width != 2560 || res.Session.Height != 1440 || res.Session.FPS != 60 {
		t.Errorf("res: got %dx%d@%d, want 2560x1440@60 (1440p60 recommended)",
			res.Session.Width, res.Session.Height, res.Session.FPS)
	}
	if res.Session.BitrateKbps != 20000 {
		t.Errorf("bitrate: got %d, want 20000 (1440p60 nominal)", res.Session.BitrateKbps)
	}
	if res.Session.Playout0Ms != 50 {
		t.Errorf("playout0_ms: got %d, want 50 (1440p60)", res.Session.Playout0Ms)
	}
	if res.Session.ProfileID == nil || *res.Session.ProfileID != "1440p60" {
		t.Errorf("profile_id: got %v, want 1440p60 (recommended profile resolved)", res.Session.ProfileID)
	}
}

// TestProbeConsumerOverrideBeatsProbe: an explicit bitrate override beats the
// tier selection for that field; un-overridden fields still come from the tier.
func TestProbeConsumerOverrideBeatsProbe(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	// LAN probe → 1080p60-lan (bitrate=12000, playout0=50).
	upsertProbe(t, pool, userID, 50000, 5, 2160, time.Now())

	// Override: force bitrate to 4000 (below tier and app default).
	bw := int32(4000)
	res, err := coord.Launch(ctx, userID, appID, StreamOverride{BitrateKbps: &bw})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// Explicit override beats tier: bitrate = 4000.
	if res.Session.BitrateKbps != 4000 {
		t.Errorf("bitrate: got %d, want 4000 (override beats tier)", res.Session.BitrateKbps)
	}
	// Non-overridden playout: still from 1080p60-lan tier = 50.
	if res.Session.Playout0Ms != 50 {
		t.Errorf("playout0_ms: got %d, want 50 (tier, not overridden)", res.Session.Playout0Ms)
	}
}

// TestProbeConsumerAppForcedProfileCaps: an admin caps an app to 720p even for a
// client whose probe would otherwise recommend 1440p. Under the CP-01 model the
// cap is expressed by app policy (profile_policy='force' + default_profile_id),
// which sits above the device recommendation in the resolution chain — NOT by the
// legacy app default_width/height dimensions, which no longer gate profile
// resolution. A 'force'-policy app pins every omitted-profile launch to its
// configured profile regardless of the probe.
func TestProbeConsumerAppForcedProfileCaps(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	// App force-pinned to the 720p60 profile via app policy.
	var userID, hostID string
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('cap2@test.local','cap2user','x') RETURNING id::text`).Scan(&userID))
	var appID string
	must(t, pool.QueryRow(ctx, `INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height, default_fps, default_bitrate_kbps,
		 profile_policy, default_profile_id)
		VALUES ('cap2-app', 1024, 1, 1280, 720, 60, 5000, 'force', '720p60') RETURNING id::text`).Scan(&appID))
	entitleAll(t, pool, appID)
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, capacity_detection)
		VALUES ('host-cap2','online','ok') RETURNING id::text`).Scan(&hostID))
	var gpuID string
	must(t, pool.QueryRow(ctx, `INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
		VALUES ($1, 0, 16384, 4) RETURNING id::text`, hostID).Scan(&gpuID))
	_ = gpuID

	store := NewStore(pool)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())

	// Excellent probe: absent the app cap this would recommend 1440p60.
	upsertProbe(t, pool, userID, 50000, 5, 2160, time.Now())

	res, err := coord.Launch(ctx, userID, appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// App policy pins to 720p60: 1280×720@60, nominal bitrate 8000, playout0 75.
	if res.Session.Width != 1280 || res.Session.Height != 720 {
		t.Errorf("res: got %dx%d, want 1280x720 (app pins to 720p60)",
			res.Session.Width, res.Session.Height)
	}
	if res.Session.BitrateKbps != 8000 {
		t.Errorf("bitrate: got %d, want 8000 (720p60 nominal)", res.Session.BitrateKbps)
	}
	if res.Session.Playout0Ms != 75 {
		t.Errorf("playout0_ms: got %d, want 75 (720p60)", res.Session.Playout0Ms)
	}
	if res.Session.ProfileID == nil || *res.Session.ProfileID != "720p60" {
		t.Errorf("profile_id: got %v, want 720p60 (forced by app policy)", res.Session.ProfileID)
	}
}

// TestLatestProbeStoreMethod exercises Store.LatestProbe at the store layer:
// absent row, stale measured_at, and fresh measured_at.
func TestLatestProbeStoreMethod(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	var userID string
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('lp@test.local','lpuser','x') RETURNING id::text`).Scan(&userID))

	t.Run("absent: no device row", func(t *testing.T) {
		dp, err := store.LatestProbe(ctx, userID)
		if err != nil {
			t.Fatalf("LatestProbe: %v", err)
		}
		if dp != nil {
			t.Errorf("expected nil probe, got %+v", dp)
		}
	})

	t.Run("stale: measured_at > 30 days old", func(t *testing.T) {
		staleTime := time.Now().Add(-31 * 24 * time.Hour)
		caps, _ := json.Marshal(map[string]any{
			"bandwidth_kbps": 50000, "rtt_ms": 5,
			"measured_at": staleTime.UTC().Format(time.RFC3339),
		})
		if _, err := pool.Exec(ctx, `
			INSERT INTO user_devices (user_id, device_key, capabilities)
			VALUES ($1::uuid, 'stale-key', $2)
			ON CONFLICT (user_id, device_key) DO UPDATE
			    SET capabilities = EXCLUDED.capabilities
		`, userID, caps); err != nil {
			t.Fatalf("seed stale device: %v", err)
		}
		dp, err := store.LatestProbe(ctx, userID)
		if err != nil {
			t.Fatalf("LatestProbe: %v", err)
		}
		if dp != nil {
			t.Errorf("expected nil probe for stale row, got %+v", dp)
		}
	})

	t.Run("fresh: recent measured_at", func(t *testing.T) {
		freshTime := time.Now()
		caps, _ := json.Marshal(map[string]any{
			"bandwidth_kbps":    40000,
			"rtt_ms":            8,
			"max_decode_height": 2160,
			"display":           map[string]any{"refresh_hz": 119.88},
			"measured_at":       freshTime.UTC().Format(time.RFC3339),
		})
		if _, err := pool.Exec(ctx, `
			INSERT INTO user_devices (user_id, device_key, capabilities)
			VALUES ($1::uuid, 'fresh-key', $2)
			ON CONFLICT (user_id, device_key) DO UPDATE
			    SET capabilities = EXCLUDED.capabilities,
			        last_seen_at = now()
		`, userID, caps); err != nil {
			t.Fatalf("seed fresh device: %v", err)
		}
		dp, err := store.LatestProbe(ctx, userID)
		if err != nil {
			t.Fatalf("LatestProbe: %v", err)
		}
		if dp == nil {
			t.Fatal("expected non-nil probe for fresh row")
		}
		if dp.BandwidthKbps != 40000 {
			t.Errorf("bandwidth_kbps: got %d, want 40000", dp.BandwidthKbps)
		}
		if dp.RTTMs != 8 {
			t.Errorf("rtt_ms: got %d, want 8", dp.RTTMs)
		}
		if dp.MaxDecodeHeight != 2160 {
			t.Errorf("max_decode_height: got %d, want 2160", dp.MaxDecodeHeight)
		}
		if dp.DisplayRefreshHz != 119.88 {
			t.Errorf("display refresh: got %v, want 119.88", dp.DisplayRefreshHz)
		}
	})

	t.Run("bogus bandwidth below 100 kbps treated as unmeasured (#146)", func(t *testing.T) {
		// Pre-fix probes timed a GET of the tiny /health body and stored
		// single-digit kbps; that must read as "unmeasured", not floor-tier.
		caps, _ := json.Marshal(map[string]any{
			"bandwidth_kbps": 2,
			"rtt_ms":         107,
			"measured_at":    time.Now().UTC().Format(time.RFC3339),
		})
		if _, err := pool.Exec(ctx, `
			INSERT INTO user_devices (user_id, device_key, capabilities)
			VALUES ($1::uuid, 'bogus-bw-key', $2)
			ON CONFLICT (user_id, device_key) DO UPDATE
			    SET capabilities = EXCLUDED.capabilities,
			        last_seen_at = now() + interval '1 second'
		`, userID, caps); err != nil {
			t.Fatalf("seed bogus device: %v", err)
		}
		dp, err := store.LatestProbe(ctx, userID)
		if err != nil {
			t.Fatalf("LatestProbe: %v", err)
		}
		if dp == nil {
			t.Fatal("expected non-nil probe (row is fresh)")
		}
		if dp.BandwidthKbps != 0 {
			t.Errorf("bandwidth_kbps: got %d, want 0 (bogus value sanitized)", dp.BandwidthKbps)
		}
	})
}
