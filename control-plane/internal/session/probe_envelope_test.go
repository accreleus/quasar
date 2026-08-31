package session

// SPT-07 tests: probe envelope (capability probe → session envelope at launch).
//
// Unit tests (pure, no DB):
//   - buildProbeEnvelope cases: nil, unmeasured, LAN, metro-WAN, hostile-WAN
//   - applyEnvelopeToBitrate: ceiling < resolved bitrate → clamp; ceiling ≥ → passthrough
//   - applyEnvelopeToPlayout0: bump applied correctly
//
// Integration tests (require Postgres — TEST_DATABASE_URL):
//   - (a) low-downlink probe → session bitrate below unconstrained tier ceiling
//   - (b) LAN / high-downlink device → starting bitrate unaffected (tier ceiling wins)
//   - (c) missing / unmeasured / stale probe → today's tier defaults exactly
//   - (d) admin or explicit override → envelope skipped
//
// Pattern follows probe_consumer_test.go (same package, shared testDB / seed helpers).

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// --- unit tests (pure, no DB, always run) --------------------------------------

func TestBuildProbeEnvelope_nil(t *testing.T) {
	env := buildProbeEnvelope(nil)
	if env.SafeCeilingKbps != 0 || env.Playout0BumpMs != 0 {
		t.Errorf("nil probe: expected zero envelope, got %+v", env)
	}
}

func TestBuildProbeEnvelope_unmeasured(t *testing.T) {
	env := buildProbeEnvelope(&DeviceProbe{}) // all-zero fields = unmeasured
	if env.SafeCeilingKbps != 0 || env.Playout0BumpMs != 0 {
		t.Errorf("unmeasured probe: expected zero envelope, got %+v", env)
	}
}

func TestBuildProbeEnvelope_LAN(t *testing.T) {
	// rtt=5 ms (≤ rttLANMs=15) → no playout0 bump. bw=20000 → ceiling=14000.
	env := buildProbeEnvelope(&DeviceProbe{BandwidthKbps: 20000, RTTMs: 5})
	if env.SafeCeilingKbps != 14000 {
		t.Errorf("safe_ceiling: got %d, want 14000 (20000 * 0.70)", env.SafeCeilingKbps)
	}
	if env.Playout0BumpMs != 0 {
		t.Errorf("playout0_bump: got %d, want 0 (LAN, no bump)", env.Playout0BumpMs)
	}
}

func TestBuildProbeEnvelope_metroWAN(t *testing.T) {
	// rtt=30 ms (> rttLANMs, ≤ rttMetroMs) → metro bump.
	env := buildProbeEnvelope(&DeviceProbe{BandwidthKbps: 20000, RTTMs: 30})
	if env.SafeCeilingKbps != 14000 {
		t.Errorf("safe_ceiling: got %d, want 14000", env.SafeCeilingKbps)
	}
	if env.Playout0BumpMs != rttMetroPlayout0Bump {
		t.Errorf("playout0_bump: got %d, want %d (metro-WAN bump)", env.Playout0BumpMs, rttMetroPlayout0Bump)
	}
}

func TestBuildProbeEnvelope_hostileWAN(t *testing.T) {
	// rtt=100 ms (> rttMetroMs) → hostile-WAN bump.
	env := buildProbeEnvelope(&DeviceProbe{BandwidthKbps: 20000, RTTMs: 100})
	if env.SafeCeilingKbps != 14000 {
		t.Errorf("safe_ceiling: got %d, want 14000", env.SafeCeilingKbps)
	}
	if env.Playout0BumpMs != rttHostilePlayout0Bump {
		t.Errorf("playout0_bump: got %d, want %d (hostile-WAN bump)", env.Playout0BumpMs, rttHostilePlayout0Bump)
	}
}

func TestApplyEnvelopeToBitrate_clamped(t *testing.T) {
	env := ProbeEnvelope{SafeCeilingKbps: 8000}
	if got := applyEnvelopeToBitrate(12000, env); got != 8000 {
		t.Errorf("got %d, want 8000 (clamped to safe ceiling)", got)
	}
}

func TestApplyEnvelopeToBitrate_passthrough(t *testing.T) {
	// Resolved bitrate already below ceiling: no clamp.
	env := ProbeEnvelope{SafeCeilingKbps: 15000}
	if got := applyEnvelopeToBitrate(12000, env); got != 12000 {
		t.Errorf("got %d, want 12000 (below ceiling, passthrough)", got)
	}
}

func TestApplyEnvelopeToBitrate_noCeiling(t *testing.T) {
	// Zero safe_ceiling → no constraint.
	env := ProbeEnvelope{}
	if got := applyEnvelopeToBitrate(12000, env); got != 12000 {
		t.Errorf("got %d, want 12000 (no ceiling)", got)
	}
}

func TestApplyEnvelopeToPlayout0(t *testing.T) {
	env := ProbeEnvelope{Playout0BumpMs: 25}
	if got := applyEnvelopeToPlayout0(50, env); got != 75 {
		t.Errorf("got %d, want 75 (50 + 25 metro bump)", got)
	}
}

// --- integration tests (require Postgres) --------------------------------------

// upsertProbeUnmeasuredBoth inserts a device probe with bogus bandwidth (→ 0
// after the #146 guard) and absent RTT (0) so that neither the safe_ceiling nor
// the playout0 bump applies. Used to test the "fully unmeasured" no-op path.
func upsertProbeUnmeasuredBoth(t *testing.T, pool *pgxpool.Pool, userID string, measuredAt time.Time) {
	t.Helper()
	// bw=2 → sanitized to 0 by the <100 kbps guard; rtt=0 → unmeasured (both axes no-op).
	upsertProbe(t, pool, userID, 2, 0, 0, measuredAt)
}

// TestEnvelopeLowDownlink (scenario a): a probe with low bandwidth → session
// bitrate is clamped BELOW the profile's nominal value.
//
// bw=10000 kbps → safe_ceiling = 7000 kbps (floor(10000 * 0.70)).
// 720p60 nominal = 8000 kbps > 7000 → clamped to 7000.
// 10000 kbps passes 720p60 eligibility (MinOfferBandwidthKbps=9600).
func TestEnvelopeLowDownlink(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	// LAN rtt=8 ms (no playout bump), bw=10000 (ceiling=7000 < 720p60 nominal 8000).
	upsertProbe(t, pool, userID, 10000, 8, 1080, time.Now())

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "720p60"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// safe_ceiling = int32(float64(10000) * 0.70) = 7000.
	wantBitrate := int32(7000)
	if res.Session.BitrateKbps != wantBitrate {
		t.Errorf("bitrate: got %d, want %d (safe_ceiling clamp)", res.Session.BitrateKbps, wantBitrate)
	}
	// LAN rtt=8 ≤ 15 ms → no playout0 bump. 720p60 baseline=75.
	if res.Session.Playout0Ms != 75 {
		t.Errorf("playout0_ms: got %d, want 75 (LAN probe, no bump)", res.Session.Playout0Ms)
	}
}

// TestEnvelopeHighDownlinkLAN (scenario b): excellent LAN probe → bitrate NOT
// clamped (safe_ceiling > resolved nominal), playout0 unchanged.
//
// bw=50000 kbps → safe_ceiling=35000 kbps. 1080p60 nominal=12000 < 35000 → no clamp.
// LAN rtt=5 ms → no playout0 bump.
func TestEnvelopeHighDownlinkLAN(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	upsertProbe(t, pool, userID, 50000, 5, 2160, time.Now())

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// safe_ceiling=35000 > nominal=12000 → no clamp.
	if res.Session.BitrateKbps != 12000 {
		t.Errorf("bitrate: got %d, want 12000 (LAN, ceiling above nominal)", res.Session.BitrateKbps)
	}
	// LAN rtt=5 ms → no playout0 bump. 1080p60 baseline=50.
	if res.Session.Playout0Ms != 50 {
		t.Errorf("playout0_ms: got %d, want 50 (LAN, no bump)", res.Session.Playout0Ms)
	}
}

// TestEnvelopeMetroWANBump: metro-WAN probe → playout0 bumped + bitrate clamped.
// bw=15000 → ceiling=10500 (15000*0.70) < 1080p60 nominal 12000 → clamp.
// rtt=30 ms (> 15, ≤ 50) → metro bump +25 ms.
func TestEnvelopeMetroWANBump(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	upsertProbe(t, pool, userID, 15000, 30, 1080, time.Now())

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// int32(float64(15000) * 0.70) = 10500.
	if res.Session.BitrateKbps != 10500 {
		t.Errorf("bitrate: got %d, want 10500 (15000*0.70)", res.Session.BitrateKbps)
	}
	// 1080p60 playout0=50 + metro bump 25 = 75.
	if res.Session.Playout0Ms != 75 {
		t.Errorf("playout0_ms: got %d, want 75 (base 50 + metro bump 25)", res.Session.Playout0Ms)
	}
}

// TestEnvelopeHostileWANBump: hostile-WAN probe (rtt=80 ms) → larger playout0 bump.
// bw=50000 → ceiling=35000 > 12000 → no bitrate clamp.
// rtt=80 ms (> rttMetroMs) → hostile bump +50 ms.
func TestEnvelopeHostileWANBump(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	upsertProbe(t, pool, userID, 50000, 80, 1080, time.Now())

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// ceiling 35000 > nominal 12000 → no clamp.
	if res.Session.BitrateKbps != 12000 {
		t.Errorf("bitrate: got %d, want 12000 (bw well above ceiling)", res.Session.BitrateKbps)
	}
	// 1080p60 playout0=50 + hostile bump 50 = 100.
	if res.Session.Playout0Ms != 100 {
		t.Errorf("playout0_ms: got %d, want 100 (base 50 + hostile bump 50)", res.Session.Playout0Ms)
	}
}

// TestEnvelopeMissingProbe (scenario c, missing): no user_devices row → nil probe
// → envelope is a no-op → session gets profile nominal values.
func TestEnvelopeMissingProbe(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	// No probe row inserted for this user.

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// No probe → no envelope → 1080p60: bitrate=12000, playout0=50.
	if res.Session.BitrateKbps != 12000 {
		t.Errorf("bitrate: got %d, want 12000 (no probe, profile nominal)", res.Session.BitrateKbps)
	}
	if res.Session.Playout0Ms != 50 {
		t.Errorf("playout0_ms: got %d, want 50 (no probe, profile baseline)", res.Session.Playout0Ms)
	}
}

// TestEnvelopeUnmeasuredProbe (scenario c, unmeasured): probe exists but
// bandwidth=0 (sanitized <100 kbps, #146) and rtt=0 → envelope is a no-op.
func TestEnvelopeUnmeasuredProbe(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	// bw=2 kbps (→ sanitized to 0 by #146 guard), rtt=0 (absent) → both unmeasured.
	upsertProbeUnmeasuredBoth(t, pool, userID, time.Now())

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// Unmeasured bw + rtt → no safe_ceiling, no bump → 1080p60 defaults.
	if res.Session.BitrateKbps != 12000 {
		t.Errorf("bitrate: got %d, want 12000 (unmeasured bw, no clamp)", res.Session.BitrateKbps)
	}
	if res.Session.Playout0Ms != 50 {
		t.Errorf("playout0_ms: got %d, want 50 (unmeasured rtt, no bump)", res.Session.Playout0Ms)
	}
}

// TestEnvelopeStaleProbe (scenario c, stale): probe older than probeMaxAgeDays →
// LatestProbe returns nil → envelope never built → session gets profile defaults.
func TestEnvelopeStaleProbe(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	// Low bw + hostile RTT that would trigger both envelope axes — but staleness
	// makes LatestProbe return nil, so neither applies.
	staleTime := time.Now().Add(-31 * 24 * time.Hour)
	upsertProbe(t, pool, userID, 5000, 200, 1080, staleTime)

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// Stale → nil probe → no envelope → 1080p60 defaults.
	if res.Session.BitrateKbps != 12000 {
		t.Errorf("bitrate: got %d, want 12000 (stale probe, no clamp)", res.Session.BitrateKbps)
	}
	if res.Session.Playout0Ms != 50 {
		t.Errorf("playout0_ms: got %d, want 50 (stale probe, no bump)", res.Session.Playout0Ms)
	}
}

// TestEnvelopeAdminBypassesEnvelope (scenario d): IsAdmin=true → envelope skipped
// entirely, even with a limiting probe.
func TestEnvelopeAdminBypassesEnvelope(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	// Low bw + hostile RTT would clamp bitrate and bump playout0.
	upsertProbe(t, pool, userID, 5000, 100, 1080, time.Now())

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{
		AppID:     appID,
		ProfileID: "1080p60",
		IsAdmin:   true,
	})
	if err != nil {
		t.Fatalf("admin launch: %v", err)
	}

	// Admin → envelope skipped → 1080p60 nominal values.
	if res.Session.BitrateKbps != 12000 {
		t.Errorf("bitrate: got %d, want 12000 (admin bypasses envelope)", res.Session.BitrateKbps)
	}
	if res.Session.Playout0Ms != 50 {
		t.Errorf("playout0_ms: got %d, want 50 (admin bypasses envelope)", res.Session.Playout0Ms)
	}
}

// TestEnvelopeExplicitOverrideBypassesEnvelope (scenario d): an explicit stream
// override → envelope skipped entirely.
func TestEnvelopeExplicitOverrideBypassesEnvelope(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	// Limiting probe.
	upsertProbe(t, pool, userID, 5000, 100, 1080, time.Now())

	bw := int32(12000) // explicit override
	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{
		AppID:     appID,
		ProfileID: "1080p60",
		Override:  StreamOverride{BitrateKbps: &bw},
	})
	if err != nil {
		t.Fatalf("override launch: %v", err)
	}

	// Explicit override present → envelope skipped → override value wins.
	if res.Session.BitrateKbps != 12000 {
		t.Errorf("bitrate: got %d, want 12000 (explicit override, envelope skipped)", res.Session.BitrateKbps)
	}
	// playout0 not overridden, but envelope also skipped → profile baseline=50.
	if res.Session.Playout0Ms != 50 {
		t.Errorf("playout0_ms: got %d, want 50 (envelope skipped on override)", res.Session.Playout0Ms)
	}
}
