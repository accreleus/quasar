package session

// Multi-codec DB integration tests (spec §5.3). Require a real Postgres
// (TEST_DATABASE_URL, provided by scripts/dev/dev.sh go-test-db) and reuse the
// probe-consumer harness (seed1080pApp / newFakeDispatcher / testLogger). They
// verify, end-to-end against the 0031/0036 migrations:
//   - sessions.codec defaults to 'h264' and round-trips through the store
//   - a HEVC-enabled profile + capable host + capable device resolves to h265,
//     persists, and is carried on the session_assign wire spec
//   - the host-encoder and device-decode clamps each degrade to the h264 floor
//   - a rung's `codec` column round-trips through the admin store path
//   - Store.HostCodecs reads the wire codec set with an h264-only default

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/profile"
	"github.com/jackc/pgx/v5/pgxpool"
)

// enableChainCodecs rebuilds a launch profile's rung chain in the given codec
// order, cloning the geometry of the chain's existing h264 rung so only the
// CODEC varies.
//
// UI-P4 replaced the stream_profiles.codecs status enum with this: a codec is
// offered because a RUNG using it exists in the chain, and withdrawn by removing
// that rung. The old `enableProfileCodecs` (flip codecs[].status to launchable)
// is exactly the rollout switch the amendment retired.
func enableChainCodecs(t *testing.T, pool *pgxpool.Pool, launchProfileID string, codecs ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`DELETE FROM launch_profile_rungs WHERE launch_profile_id = $1`, launchProfileID); err != nil {
		t.Fatalf("clear chain: %v", err)
	}
	for i, c := range codecs {
		rungID := launchProfileID + "-" + c
		if _, err := pool.Exec(ctx, `
			INSERT INTO stream_profiles (
			    id, display_name, width, height, fps, h264_profile,
			    nominal_bitrate_kbps, min_offer_bandwidth_kbps, recommended_offer_bandwidth_kbps,
			    headroom_factor, abr_floor_kbps, max_startup_rtt_ms, min_decode_height,
			    high_refresh_display, hardware_encoder_required, browser_client, playout0_ms,
			    visibility, sort_order, codec)
			SELECT $2, $3 || ' ' || p.display_name, p.width, p.height, p.fps, p.h264_profile,
			       p.nominal_bitrate_kbps, p.min_offer_bandwidth_kbps, p.recommended_offer_bandwidth_kbps,
			       p.headroom_factor, p.abr_floor_kbps, p.max_startup_rtt_ms, p.min_decode_height,
			       p.high_refresh_display, p.hardware_encoder_required, p.browser_client, p.playout0_ms,
			       'internal', p.sort_order, $3
			FROM stream_profiles p WHERE p.id = $1 || '-h264'
			ON CONFLICT (id) DO UPDATE SET codec = EXCLUDED.codec
		`, launchProfileID, rungID, c); err != nil {
			t.Fatalf("seed %s rung: %v", c, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO launch_profile_rungs (launch_profile_id, stream_profile_id, position)
			VALUES ($1, $2, $3)
		`, launchProfileID, rungID, i+1); err != nil {
			t.Fatalf("attach %s rung: %v", c, err)
		}
	}
}

// setHostCodecs sets the wire codec set a host advertises (hosts.codecs JSONB).
func setHostCodecs(t *testing.T, pool *pgxpool.Pool, hostID, codecsJSON string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE hosts SET codecs = $2::jsonb WHERE id::text = $1`, hostID, codecsJSON); err != nil {
		t.Fatalf("set host codecs: %v", err)
	}
}

// upsertCodecProbe inserts a fresh probe advertising the given decode codecs.
func upsertCodecProbe(t *testing.T, pool *pgxpool.Pool, userID string, hevc, av1 bool) {
	t.Helper()
	caps := `{"bandwidth_kbps":50000,"rtt_ms":5,"max_decode_height":2160,"measured_at":"` +
		time.Now().UTC().Format(time.RFC3339) + `","codecs":{"h264":true,"hevc":` +
		boolJSON(hevc) + `,"av1":` + boolJSON(av1) + `}}`
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO user_devices (user_id, device_key, capabilities)
		VALUES ($1::uuid, 'codec-device', $2::jsonb)
		ON CONFLICT (user_id, device_key) DO UPDATE
		    SET capabilities = EXCLUDED.capabilities, last_seen_at = now()
	`, userID, caps); err != nil {
		t.Fatalf("upsert codec probe: %v", err)
	}
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestSessionCodecDefaultsToH264: a plain launch defaults sessions.codec to
// 'h264', round-trips through a fresh read, and omits codec on the wire.
func TestSessionCodecDefaultsToH264(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.Codec != "h264" {
		t.Errorf("session codec: got %q, want h264 (default)", res.Session.Codec)
	}
	got, err := store.Get(ctx, res.Session.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Codec != "h264" {
		t.Errorf("persisted codec: got %q, want h264", got.Codec)
	}

	waitFor(t, func() bool { return len(disp.types()) >= 1 })
	assign := disp.lastAssignCmd()
	if assign == nil {
		t.Fatal("no session_assign dispatched")
	}
	// The default h264 is omitted on the wire (byte-identical to pre-multi-codec).
	if assign.Stream.Codec != "" {
		t.Errorf("assign stream codec: got %q, want \"\" (h264 omitted)", assign.Stream.Codec)
	}
}

// TestSessionCodecResolvesHEVC: HEVC-enabled profile + h265-capable host +
// hevc-capable device ⇒ h265 resolved, persisted, and sent on the wire.
func TestSessionCodecResolvesHEVC(t *testing.T) {
	pool := testDB(t)
	userID, appID, hostID := seed1080pApp(t, pool)
	store := NewStore(pool)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	// hevc preferred over h264 in this profile; host encodes h265; device decodes it.
	enableChainCodecs(t, pool, "1080p60", "hevc", "h264")
	setHostCodecs(t, pool, hostID, `["h264","h265"]`)
	upsertCodecProbe(t, pool, userID, true, false)

	// Admin bypasses eligibility (no bandwidth gate); codec resolution still runs.
	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60", IsAdmin: true})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.Codec != "h265" {
		t.Fatalf("session codec: got %q, want h265", res.Session.Codec)
	}
	got, err := store.Get(ctx, res.Session.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Codec != "h265" {
		t.Errorf("persisted codec: got %q, want h265", got.Codec)
	}
	waitFor(t, func() bool { return len(disp.types()) >= 1 })
	if assign := disp.lastAssignCmd(); assign == nil || assign.Stream.Codec != "h265" {
		t.Errorf("assign stream codec: got %v, want h265", assign)
	}
}

// TestSessionCodecHostClamp: profile+device want HEVC but the host cannot encode
// h265 (no codecs reported ⇒ h264-only) ⇒ resolves to the h264 floor.
func TestSessionCodecHostClamp(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	coord := newTestCoordinator(t, NewStore(pool), newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	enableChainCodecs(t, pool, "1080p60", "hevc", "h264")
	// hosts.codecs left NULL ⇒ h264-only.
	upsertCodecProbe(t, pool, userID, true, true)

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60", IsAdmin: true})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.Codec != "h264" {
		t.Errorf("session codec: got %q, want h264 (host clamp)", res.Session.Codec)
	}
}

// TestSessionCodecDeviceClamp: profile+host support HEVC but the device probe
// does not advertise hevc decode ⇒ resolves to the h264 floor (black-stream guard).
func TestSessionCodecDeviceClamp(t *testing.T) {
	pool := testDB(t)
	userID, appID, hostID := seed1080pApp(t, pool)
	coord := newTestCoordinator(t, NewStore(pool), newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	enableChainCodecs(t, pool, "1080p60", "hevc", "h264")
	setHostCodecs(t, pool, hostID, `["h264","h265"]`)
	upsertCodecProbe(t, pool, userID, false, false) // device cannot decode hevc

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60", IsAdmin: true})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.Codec != "h264" {
		t.Errorf("session codec: got %q, want h264 (device clamp)", res.Session.Codec)
	}
}

// TestSessionCodecHistoryClamp (0032): a recorded h265 decode failure for this
// (user, device, profile) makes the launch resolver skip h265 — the session
// degrades to the h264 floor even though profile, host, and probe all say HEVC.
// An explicit override still forces h265 (the re-test path).
func TestSessionCodecHistoryClamp(t *testing.T) {
	pool := testDB(t)
	userID, appID, hostID := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	enableChainCodecs(t, pool, "1080p60", "hevc", "h264")
	setHostCodecs(t, pool, hostID, `["h264","h265"]`)
	upsertCodecProbe(t, pool, userID, true, false)
	// 'codec-device' is the latest (only) device — the same key the resolver's
	// history lookup uses via LatestDeviceKey.
	// Recorded at LAUNCH-PROFILE grain — the LEGACY row shape. RungFailures folds
	// it in as "ban every hevc rung of this chain", which is exactly its old
	// meaning, so a pre-UI-P4 history row keeps working with no data migration.
	if err := store.RecordProfileOutcome(ctx, userID, "codec-device", "1080p60", "h265", outcomeFail, strptr("client_unsupported")); err != nil {
		t.Fatalf("record h265 fail: %v", err)
	}

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60", IsAdmin: true})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.Codec != "h264" {
		t.Errorf("session codec: got %q, want h264 (decode-failure history clamp)", res.Session.Codec)
	}

	// Free the per-user quota + reservation before the second launch.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET state = 'stopped' WHERE id::text = $1`, res.Session.ID); err != nil {
		t.Fatalf("stop first session: %v", err)
	}

	// Override bypasses the history clamp: forced h265 launches (re-test path).
	h265 := "h265"
	res, err = coord.LaunchByProfile(ctx, userID, LaunchParams{
		AppID: appID, ProfileID: "1080p60", IsAdmin: true,
		Override: StreamOverride{Codec: &h265},
	})
	if err != nil {
		t.Fatalf("override launch: %v", err)
	}
	if res.Session.Codec != "h265" {
		t.Errorf("override session codec: got %q, want h265 (override bypasses history)", res.Session.Codec)
	}
}

// TestSessionCodecOverride: an admin stream.codec override forces the codec even
// against the ship-dark default profile/host.
func TestSessionCodecOverride(t *testing.T) {
	pool := testDB(t)
	userID, appID, hostID := seed1080pApp(t, pool)
	coord := newTestCoordinator(t, NewStore(pool), newFakeDispatcher(true), testLogger())
	ctx := context.Background()
	setHostCodecs(t, pool, hostID, `["h264","h265","av1"]`)
	// Clamp 0 selects the first RUNG with the requested codec, so the chain must
	// contain one — an override is a choice among the chain's rungs, not a way to
	// conjure a codec the launch profile does not offer.
	enableChainCodecs(t, pool, "1080p60", "av1", "h264")

	av1 := "av1"
	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{
		AppID: appID, ProfileID: "1080p60", IsAdmin: true,
		Override: StreamOverride{Codec: &av1},
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.Codec != "av1" {
		t.Errorf("session codec: got %q, want av1 (override)", res.Session.Codec)
	}
}

// TestSessionCodecOverrideHostUnsupported: an override for a codec the placed
// host cannot encode fails the launch cleanly (ErrCodecUnsupportedByHost) rather
// than dispatching a doomed assignment.
func TestSessionCodecOverrideHostUnsupported(t *testing.T) {
	pool := testDB(t)
	userID, appID, hostID := seed1080pApp(t, pool)
	coord := newTestCoordinator(t, NewStore(pool), newFakeDispatcher(true), testLogger())
	ctx := context.Background()
	setHostCodecs(t, pool, hostID, `["h264"]`) // host is h264-only
	enableChainCodecs(t, pool, "1080p60", "av1", "h264")

	av1 := "av1"
	_, err := coord.LaunchByProfile(ctx, userID, LaunchParams{
		AppID: appID, ProfileID: "1080p60", IsAdmin: true,
		Override: StreamOverride{Codec: &av1},
	})
	if !errors.Is(err, ErrCodecUnsupportedByHost) {
		t.Fatalf("override av1 on h264-only host: got err=%v, want ErrCodecUnsupportedByHost", err)
	}
}

// TestRungCodecRoundTrip — UI-P4: `codec` is the authoritative single codec on a
// rung, and the admin write path round-trips it.
//
// It also asserts the deliberate omission that protects a code-level revert: the
// admin UPDATE does NOT touch the legacy `codecs` column. Migration 0036 is the
// EXPAND half, so the legacy rows and that column are exactly what a
// rolled-back binary reads — materialising a merged default over a NULL would
// silently change what it sees.
func TestRungCodecRoundTrip(t *testing.T) {
	pool := testDB(t)
	seed1080pApp(t, pool)
	store := NewStore(pool)
	ctx := context.Background()

	p, err := store.GetStreamProfile(ctx, "1080p60-h264")
	if err != nil {
		t.Fatalf("get rung: %v", err)
	}
	if p.Codec != profile.CodecH264 {
		t.Fatalf("rung codec = %q, want h264", p.Codec)
	}

	p.Codec = profile.CodecAV1
	if _, err := store.UpdateStreamProfile(ctx, p); err != nil {
		t.Fatalf("update rung: %v", err)
	}
	got, err := store.GetStreamProfile(ctx, "1080p60-h264")
	if err != nil {
		t.Fatalf("re-get rung: %v", err)
	}
	if got.Codec != profile.CodecAV1 {
		t.Errorf("rung codec after edit = %q, want av1", got.Codec)
	}

	// The legacy `codecs` column on the LEGACY row is untouched by any rung edit.
	var legacyCodecs *string
	if err := pool.QueryRow(ctx, `SELECT codecs::text FROM stream_profiles WHERE id = '1080p60'`).Scan(&legacyCodecs); err != nil {
		t.Fatalf("read legacy codecs: %v", err)
	}
	if legacyCodecs != nil {
		t.Errorf("legacy stream_profiles.codecs = %q, want NULL (0036 keeps it untouched so a code-level revert still finds its data)", *legacyCodecs)
	}
}

// TestLegacyCodecsColumnStillMergesToTheInCodeDefault — the legacy rows and the
// `codecs` column survive migration 0036 by design (expand/contract). Nothing on
// the launch path reads them, but a rolled-back binary does, so the merge must
// still behave.
func TestLegacyCodecsColumnStillMergesToTheInCodeDefault(t *testing.T) {
	pool := testDB(t)
	seed1080pApp(t, pool)
	store := NewStore(pool)
	ctx := context.Background()

	p, err := store.GetStreamProfile(ctx, "1080p60")
	if err != nil {
		t.Fatalf("get legacy row: %v", err)
	}
	assertCodecs(t, p.Codecs, "legacy NULL", "h264:launchable", "hevc:future", "av1:future")
	if p.Codec != "" {
		t.Errorf("legacy row codec = %q, want empty (only rungs carry a codec)", p.Codec)
	}
}

// TestHostCodecsDefault: a host with NULL codecs reads back as h264-only.
func TestHostCodecsDefault(t *testing.T) {
	pool := testDB(t)
	_, _, hostID := seed1080pApp(t, pool)
	store := NewStore(pool)
	ctx := context.Background()

	got, err := store.HostCodecs(ctx, hostID)
	if err != nil {
		t.Fatalf("host codecs: %v", err)
	}
	if len(got) != 1 || got[0] != "h264" {
		t.Errorf("host codecs (NULL): got %v, want [h264]", got)
	}

	setHostCodecs(t, pool, hostID, `["h264","h265","av1"]`)
	got, err = store.HostCodecs(ctx, hostID)
	if err != nil {
		t.Fatalf("host codecs: %v", err)
	}
	if len(got) != 3 || got[0] != "h264" || got[1] != "h265" || got[2] != "av1" {
		t.Errorf("host codecs: got %v, want [h264 h265 av1]", got)
	}
}

// TestHostCodecPixelRates (#506) is the readback half of the throughput hint: the
// stored object projects down to codec→Mpix/s, and every "we do not know" shape
// comes back nil — which is what makes clamp 6 fail open.
func TestHostCodecPixelRates(t *testing.T) {
	pool := testDB(t)
	_, _, hostID := seed1080pApp(t, pool)
	store := NewStore(pool)
	ctx := context.Background()

	// NULL column (pre-amendment agent) ⇒ nil ⇒ no clamp.
	got, err := store.HostCodecPixelRates(ctx, hostID)
	if err != nil {
		t.Fatalf("host codec pixel rates: %v", err)
	}
	if got != nil {
		t.Errorf("NULL codec_pixel_rates: got %v, want nil (unknown, not zero)", got)
	}

	set := func(js string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE hosts SET codec_pixel_rates=$2::jsonb WHERE id=$1`, hostID, js); err != nil {
			t.Fatalf("set codec_pixel_rates: %v", err)
		}
	}

	set(`{"h265":{"max_pixel_rate_mpix_s":395},"h264":{"max_pixel_rate_mpix_s":1400}}`)
	got, err = store.HostCodecPixelRates(ctx, hostID)
	if err != nil {
		t.Fatalf("host codec pixel rates: %v", err)
	}
	if got["h265"] != 395 || got["h264"] != 1400 {
		t.Errorf("codec pixel rates: got %v, want h265=395 h264=1400", got)
	}

	// Per-ENTRY tolerance: one unusable codec must not cost the others. A newer
	// agent reporting a codec this control plane cannot read is the case that
	// matters, and discarding the whole map there would be the wrong trade.
	set(`{"h265":{"max_pixel_rate_mpix_s":395},"av1":{"max_pixel_rate_mpix_s":0}}`)
	got, err = store.HostCodecPixelRates(ctx, hostID)
	if err != nil {
		t.Fatalf("host codec pixel rates: %v", err)
	}
	if got["h265"] != 395 {
		t.Errorf("h265 rate lost to a sibling's bad entry: %v", got)
	}
	if _, ok := got["av1"]; ok {
		t.Errorf("a zero rate was kept (%v) — zero means 'cannot vouch', not 'zero pixels/s'", got)
	}

	// An explicit {} is a real report meaning "no hints"; it must read as unknown,
	// not as an empty-but-present map a caller might treat as authoritative.
	set(`{}`)
	got, err = store.HostCodecPixelRates(ctx, hostID)
	if err != nil {
		t.Fatalf("host codec pixel rates: %v", err)
	}
	if got != nil {
		t.Errorf("empty codec_pixel_rates: got %v, want nil", got)
	}
}

func assertCodecs(t *testing.T, got []profile.CodecPref, label string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s codecs: got %v, want %v", label, got, want)
	}
	for i, w := range want {
		g := string(got[i].Codec) + ":" + string(got[i].Status)
		if g != w {
			t.Errorf("%s codecs[%d]: got %q, want %q", label, i, g, w)
		}
	}
}
