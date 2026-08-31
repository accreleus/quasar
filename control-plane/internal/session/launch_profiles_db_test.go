package session

// launch_profiles_db_test.go — UI-P4 launch-profile / stream-profile store
// coverage against a real Postgres (migration 0036).
//
// Also home to the assertions that used to live in internal/profile's
// profile_test.go. Those tested a Go literal that UI-P4 deleted; the same
// invariants are now asserted against the SEEDED DATABASE ROWS, which is the
// real data they were always about.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// chainRung describes one rung to seed.
type chainRung struct {
	id       string
	codec    string // catalog vocabulary: h264 | hevc | av1
	w, h     int32
	fps      int32
	minBW    int32
	minDec   int32
	hwEnc    bool
	abrFloor int32
}

// seedChain inserts a launch profile with the given rungs in order and returns
// it as the store would. Everything unset takes a sane default so a test only
// states what it is actually about.
func seedChain(t *testing.T, pool *pgxpool.Pool, id string, rungs []chainRung) profile.LaunchProfile {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO launch_profiles (id, display_name, description, visibility, sort_order)
		VALUES ($1, $1, '', 'user', 5)
		ON CONFLICT (id) DO UPDATE SET visibility = 'user'
	`, id); err != nil {
		t.Fatalf("seed launch profile %s: %v", id, err)
	}
	for i, r := range rungs {
		if r.fps == 0 {
			r.fps = 60
		}
		if r.minDec == 0 {
			r.minDec = r.h
		}
		if r.abrFloor == 0 {
			r.abrFloor = 4000
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO stream_profiles (
			    id, display_name, width, height, fps, h264_profile,
			    nominal_bitrate_kbps, min_offer_bandwidth_kbps, recommended_offer_bandwidth_kbps,
			    headroom_factor, abr_floor_kbps, max_startup_rtt_ms, min_decode_height,
			    high_refresh_display, hardware_encoder_required, browser_client, playout0_ms,
			    visibility, sort_order, codec)
			VALUES ($1, $1, $2, $3, $4, 'high', 12000, $5, 18000, 1.5, $6, 0, $7,
			        'none', $8, 'recommended', 50, 'internal', 5, $9)
			ON CONFLICT (id) DO UPDATE SET codec = EXCLUDED.codec
		`, r.id, r.w, r.h, r.fps, r.minBW, r.abrFloor, r.minDec, r.hwEnc, r.codec); err != nil {
			t.Fatalf("seed rung %s: %v", r.id, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO launch_profile_rungs (launch_profile_id, stream_profile_id, position)
			VALUES ($1, $2, $3)
			ON CONFLICT (launch_profile_id, position) DO UPDATE SET stream_profile_id = EXCLUDED.stream_profile_id
		`, id, r.id, i+1); err != nil {
			t.Fatalf("seed rung membership %s: %v", r.id, err)
		}
	}
	store := NewStore(pool)
	lp, err := store.GetLaunchProfile(ctx, id)
	if err != nil {
		t.Fatalf("read seeded chain %s: %v", id, err)
	}
	return lp
}

// TestSeededLadderShape asserts the shipped ladder's invariants against the real
// rows, post-0036. These moved here from internal/profile's deleted catalog test.
func TestSeededLadderShape(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	lps, err := store.ListLaunchProfiles(ctx, false)
	if err != nil {
		t.Fatalf("list launch profiles: %v", err)
	}
	byID := map[string]profile.LaunchProfile{}
	for _, lp := range lps {
		byID[lp.ID] = lp
	}

	// The seven user-facing rungs AS10-01 mandates, plus the debug 720p30.
	required := []string{"720p60", "1080p60", "1080p120", "1440p60", "1440p120", "4k60", "4k120"}
	for _, id := range required {
		lp, ok := byID[id]
		if !ok {
			t.Errorf("required launch profile %q missing", id)
			continue
		}
		if lp.Visibility != profile.VisibilityUser {
			t.Errorf("launch profile %q visibility = %q, want user", id, lp.Visibility)
		}
	}
	if lp, ok := byID["720p30"]; !ok {
		t.Error("720p30 must remain (debug/internal fallback)")
	} else if lp.Visibility == profile.VisibilityUser {
		t.Errorf("720p30 must not be user-facing, got %q", lp.Visibility)
	}

	// Ordering: highest quality first (sort_order), and every chain has at least
	// one rung with an h264 floor.
	prevHeight, prevFPS := int32(1<<30), int32(1<<30)
	for _, lp := range lps {
		if len(lp.Rungs) == 0 {
			t.Fatalf("launch profile %q has no rungs — nothing to dispatch", lp.ID)
		}
		hasH264 := false
		for _, r := range lp.Rungs {
			if r.Codec == profile.CodecH264 {
				hasH264 = true
			}
			if r.Visibility != profile.VisibilityInternal {
				t.Errorf("rung %q visibility = %q, want internal (a rung is never offered standalone)", r.ID, r.Visibility)
			}
			if r.Width <= 0 || r.Height <= 0 || r.FPS <= 0 {
				t.Errorf("rung %q non-positive geometry %dx%d@%d", r.ID, r.Width, r.Height, r.FPS)
			}
			if r.NominalBitrateKbps <= 0 || r.ABRFloorKbps <= 0 {
				t.Errorf("rung %q non-positive bitrate/floor (%d/%d)", r.ID, r.NominalBitrateKbps, r.ABRFloorKbps)
			}
			if r.ABRFloorKbps > r.NominalBitrateKbps {
				t.Errorf("rung %q ABR floor %d exceeds nominal %d", r.ID, r.ABRFloorKbps, r.NominalBitrateKbps)
			}
			if r.MinDecodeHeight != r.Height {
				t.Errorf("rung %q MinDecodeHeight %d != Height %d", r.ID, r.MinDecodeHeight, r.Height)
			}
			if r.RecommendedOfferBandwidthKbps < r.MinOfferBandwidthKbps {
				t.Errorf("rung %q recommended bandwidth %d below minimum %d", r.ID, r.RecommendedOfferBandwidthKbps, r.MinOfferBandwidthKbps)
			}
			if r.Playout0Ms <= 0 {
				t.Errorf("rung %q non-positive Playout0Ms %d", r.ID, r.Playout0Ms)
			}
			if r.H264Profile == "" {
				t.Errorf("rung %q has an empty h264_profile", r.ID)
			}
			if r.FPS >= 120 && r.HighRefreshDisplay != profile.DisplayRequired {
				t.Errorf("rung %q is %d fps but high_refresh_display = %q", r.ID, r.FPS, r.HighRefreshDisplay)
			}
		}
		if !hasH264 {
			t.Errorf("launch profile %q has no h264 rung — the resolve-time floor would have nothing to dispatch", lp.ID)
		}
		top := lp.Rungs[0]
		if top.Height > prevHeight || (top.Height == prevHeight && top.FPS > prevFPS) {
			t.Errorf("ladder not ordered highest-first at %q: (h=%d,fps=%d) ranks above previous (h=%d,fps=%d)",
				lp.ID, top.Height, top.FPS, prevHeight, prevFPS)
		}
		prevHeight, prevFPS = top.Height, top.FPS
	}
}

// TestFanOutIsSingleH264RungPerProfile asserts migration 0036's fan-out rule for
// the shipped state: every seeded row has codecs NULL, whose in-code default has
// exactly one LAUNCHABLE entry (h264), so every launch profile comes out
// single-rung, streaming exactly what it streams today.
func TestFanOutIsSingleH264RungPerProfile(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)

	lps, err := store.ListLaunchProfiles(context.Background(), false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(lps) != 8 {
		t.Fatalf("got %d launch profiles, want 8 (the migration-0015 seed)", len(lps))
	}
	for _, lp := range lps {
		if len(lp.Rungs) != 1 {
			t.Errorf("%s: %d rungs, want 1 (codecs NULL ⇒ one h264 rung)", lp.ID, len(lp.Rungs))
			continue
		}
		r := lp.Rungs[0]
		if r.ID != lp.ID+"-h264" || r.Codec != profile.CodecH264 {
			t.Errorf("%s: rung = %q/%q, want %q/h264", lp.ID, r.ID, r.Codec, lp.ID+"-h264")
		}
	}
}

// TestRungInheritsParentVerbatim asserts §2.3: the fan-out does NO per-codec
// tuning. Every inherited value must match the legacy parent row exactly — that
// is what makes the behaviour-neutrality diff a meaningful instrument.
func TestRungInheritsParentVerbatim(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT p.id,
		       (r.width = p.width AND r.height = p.height AND r.fps = p.fps
		        AND r.h264_profile = p.h264_profile
		        AND r.nominal_bitrate_kbps = p.nominal_bitrate_kbps
		        AND r.min_offer_bandwidth_kbps = p.min_offer_bandwidth_kbps
		        AND r.recommended_offer_bandwidth_kbps = p.recommended_offer_bandwidth_kbps
		        AND r.headroom_factor = p.headroom_factor
		        AND r.abr_floor_kbps = p.abr_floor_kbps
		        AND r.max_startup_rtt_ms = p.max_startup_rtt_ms
		        AND r.min_decode_height = p.min_decode_height
		        AND r.high_refresh_display = p.high_refresh_display
		        AND r.hardware_encoder_required = p.hardware_encoder_required
		        AND r.browser_client = p.browser_client
		        AND r.playout0_ms = p.playout0_ms)
		FROM stream_profiles p
		JOIN stream_profiles r ON r.id = p.id || '-h264'
		WHERE p.codec IS NULL
	`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id string
		var same bool
		if err := rows.Scan(&id, &same); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !same {
			t.Errorf("rung %s-h264 does not inherit its parent verbatim", id)
		}
		n++
	}
	if n != 8 {
		t.Fatalf("compared %d parent/rung pairs, want 8", n)
	}
}

// TestDeleteStreamProfileInUseIs409 — refuse-if-in-use is the SERVER rule; the
// admin UI's disabled Delete button is only the client half.
func TestDeleteStreamProfileInUseIs409(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	if err := store.DeleteStreamProfile(ctx, "1080p60-h264"); err != ErrProfileInUse {
		t.Fatalf("delete an in-use rung: got %v, want ErrProfileInUse", err)
	}

	// Detach it and the delete succeeds — proving the 409 was about the reference,
	// not about the row being undeletable.
	if _, err := pool.Exec(ctx, `DELETE FROM launch_profile_rungs WHERE stream_profile_id = '1080p60-h264'`); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if err := store.DeleteStreamProfile(ctx, "1080p60-h264"); err != nil {
		t.Fatalf("delete an unreferenced rung: %v", err)
	}
	if err := store.DeleteStreamProfile(ctx, "1080p60-h264"); err != ErrProfileUnknown {
		t.Fatalf("delete a missing rung: got %v, want ErrProfileUnknown", err)
	}
}

// TestDeleteStreamProfileReferencedBySessionIs409 covers the SECOND reference
// dimension, which the first cut of the gate missed entirely.
//
// `sessions.stream_profile_id` (migration 0036) is a plain FK with NO `ON DELETE`
// clause, i.e. NO ACTION. Counting only `launch_profile_rungs` therefore produced
// an empty `used_by`, an ENABLED Delete button, and — the moment the operator
// clicked it on a rung that any session had ever resolved to — a raw
// `sessions_stream_profile_id_fkey` violation mapped to `500 internal`. Tower
// carries 823 session rows, so this was reachable as soon as anyone did the
// per-rung tuning the restructure exists to enable.
//
// The FK is deliberately NOT `ON DELETE SET NULL`: that would erase which rung a
// historical session actually got, which is the whole reason for the column. So
// a rung named by session history is genuinely permanent, and the correct answer
// is an actionable 409 that says so — never a 500, and never a silent history
// wipe.
func TestDeleteStreamProfileReferencedBySessionIs409(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	// A rung that NO launch profile lists — so the old, launch-profile-only count
	// is zero and the row looks freely deletable.
	if _, err := pool.Exec(ctx, `
		INSERT INTO stream_profiles (
		    id, display_name, width, height, fps, h264_profile,
		    nominal_bitrate_kbps, min_offer_bandwidth_kbps, recommended_offer_bandwidth_kbps,
		    headroom_factor, abr_floor_kbps, max_startup_rtt_ms, min_decode_height,
		    high_refresh_display, hardware_encoder_required, browser_client, playout0_ms,
		    visibility, sort_order, codec)
		VALUES ('orphan-h264','H.264 · Orphan', 1920, 1080, 60, 'high',
		        12000, 15000, 18000, 1.5, 4000, 0, 1080,
		        'none', false, 'recommended', 50, 'internal', 5, 'h264')`); err != nil {
		t.Fatalf("seed orphan rung: %v", err)
	}
	usedBy, sessions, err := store.StreamProfileUsedBy(ctx, "orphan-h264")
	if err != nil {
		t.Fatalf("used_by: %v", err)
	}
	if len(usedBy) != 0 || sessions != 0 {
		t.Fatalf("fresh orphan rung used_by = %v / sessions = %d, want empty/0", usedBy, sessions)
	}

	// One ENDED session that resolved to it — history, not a live reference.
	if _, err := pool.Exec(ctx, `
		INSERT INTO sessions (user_id, app_id, host_id, gpu_id, state,
		    width, height, fps, bitrate_kbps, h264_profile,
		    profile_id, stream_profile_id, playout0_ms, reserved_encode_slots)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'stopped',
		        1920, 1080, 60, 12000, 'constrained-baseline',
		        '1080p60', 'orphan-h264', 50, 0)`,
		s.userID, s.appID, s.hostID, s.gpuID); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// (a) the gate refuses with a 409-shaped error, NOT an FK error mapped to 500.
	if err := store.DeleteStreamProfile(ctx, "orphan-h264"); err != ErrProfileInUse {
		t.Fatalf("delete a rung named by session history: got %v, want ErrProfileInUse "+
			"(anything else is the raw sessions_stream_profile_id_fkey violation → 500)", err)
	}

	// (b) used_by must SURFACE that dimension, or the admin UI keeps Delete enabled
	//     and the operator only discovers the refusal by clicking it.
	usedBy, sessions, err = store.StreamProfileUsedBy(ctx, "orphan-h264")
	if err != nil {
		t.Fatalf("used_by after seeding a session: %v", err)
	}
	if len(usedBy) != 0 {
		t.Errorf("used_by launch profiles = %v, want none — no chain lists this rung", usedBy)
	}
	if sessions != 1 {
		t.Errorf("used_by session count = %d, want 1", sessions)
	}

	// (c) the session history is the ONLY thing holding it: clear it and the delete
	//     succeeds, proving the 409 was about the reference and not an undeletable row.
	if _, err := pool.Exec(ctx, `DELETE FROM sessions WHERE stream_profile_id = 'orphan-h264'`); err != nil {
		t.Fatalf("clear session history: %v", err)
	}
	if err := store.DeleteStreamProfile(ctx, "orphan-h264"); err != nil {
		t.Fatalf("delete an unreferenced rung: %v", err)
	}
}

// TestDeleteLaunchProfileInUseIs409 covers all THREE reference dimensions: an
// app, the global policy, and a user preference.
func TestDeleteLaunchProfileInUseIs409(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	// Unreferenced to start with.
	seedChain(t, pool, "spare", []chainRung{{id: "spare-h264", codec: "h264", w: 1920, h: 1080}})
	if used, err := store.LaunchProfileUsedByFor(ctx, "spare"); err != nil || used.any() {
		t.Fatalf("fresh chain used_by = %+v (err %v), want empty", used, err)
	}

	// (a) an app.
	if _, err := pool.Exec(ctx, `UPDATE apps SET default_profile_id = 'spare' WHERE id::text = $1`, s.appID); err != nil {
		t.Fatalf("point app: %v", err)
	}
	if err := store.DeleteLaunchProfile(ctx, "spare"); err != ErrLaunchProfileInUse {
		t.Fatalf("delete with an app referencing it: got %v, want ErrLaunchProfileInUse", err)
	}
	used, _ := store.LaunchProfileUsedByFor(ctx, "spare")
	if len(used.Apps) != 1 {
		t.Errorf("used_by.apps = %+v, want one app", used.Apps)
	}
	if _, err := pool.Exec(ctx, `UPDATE apps SET default_profile_id = NULL WHERE id::text = $1`, s.appID); err != nil {
		t.Fatalf("unpoint app: %v", err)
	}

	// (b) the global policy.
	if _, err := pool.Exec(ctx, `UPDATE stream_profile_policy SET global_default_profile_id = 'spare' WHERE id = true`); err != nil {
		t.Fatalf("point policy: %v", err)
	}
	if err := store.DeleteLaunchProfile(ctx, "spare"); err != ErrLaunchProfileInUse {
		t.Fatalf("delete with the global policy referencing it: got %v, want ErrLaunchProfileInUse", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE stream_profile_policy SET global_default_profile_id = NULL WHERE id = true`); err != nil {
		t.Fatalf("unpoint policy: %v", err)
	}

	// (c) a user preference — the FK the original spec missed.
	if _, err := store.UpdateUserProfilePreferences(ctx, s.userID, strptr("spare")); err != nil {
		t.Fatalf("point user preference: %v", err)
	}
	if err := store.DeleteLaunchProfile(ctx, "spare"); err != ErrLaunchProfileInUse {
		t.Fatalf("delete with a user preference referencing it: got %v, want ErrLaunchProfileInUse", err)
	}
	if _, err := store.UpdateUserProfilePreferences(ctx, s.userID, strptr("")); err != nil {
		t.Fatalf("unpoint user preference: %v", err)
	}

	if err := store.DeleteLaunchProfile(ctx, "spare"); err != nil {
		t.Fatalf("delete an unreferenced launch profile: %v", err)
	}
}

// TestUserPreferenceAgainstNewLaunchProfile is gap 7 (re-spec §4.1) made
// executable.
//
// user_profile_preferences.default_profile_id is the THIRD foreign key migration
// 0036 repointed, and it is the most dangerous omission in the phase because of
// HOW it fails: the table has 0 rows on Tower, so a missed repoint leaves the
// migration green, the tests green and the deploy clean — and then the first
// user who sets a quality preference against a launch profile that has no legacy
// stream_profiles row gets a 500 from an FK violation.
//
// A test using only MIGRATED ids proves nothing, because the legacy row still
// exists under the expand migration and the write would succeed either way. So
// this writes against a NEWLY CREATED launch profile.
func TestUserPreferenceAgainstNewLaunchProfile(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	seedChain(t, pool, "brand-new-chain", []chainRung{{id: "brand-new-chain-h264", codec: "h264", w: 1920, h: 1080}})

	// It must NOT exist as a stream profile — otherwise the FK would be satisfied
	// by the wrong table and the test would be vacuous.
	var legacyExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM stream_profiles WHERE id = 'brand-new-chain')`).Scan(&legacyExists); err != nil {
		t.Fatalf("check legacy row: %v", err)
	}
	if legacyExists {
		t.Fatal("test setup is vacuous: a stream_profiles row shares the new launch profile's id")
	}

	prefs, err := store.UpdateUserProfilePreferences(ctx, s.userID, strptr("brand-new-chain"))
	if err != nil {
		t.Fatalf("write a user preference against a NEW launch profile: %v", err)
	}
	if prefs.DefaultProfileID == nil || *prefs.DefaultProfileID != "brand-new-chain" {
		t.Fatalf("preference = %v, want brand-new-chain", prefs.DefaultProfileID)
	}
	readBack, err := store.GetUserProfilePreferences(ctx, s.userID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if readBack.DefaultProfileID == nil || *readBack.DefaultProfileID != "brand-new-chain" {
		t.Fatalf("read back = %v, want brand-new-chain", readBack.DefaultProfileID)
	}

	// The same repoint applies to the global policy.
	if _, err := store.UpdateProfilePolicy(ctx, strptr("brand-new-chain"), true, s.userID); err != nil {
		t.Fatalf("write the global default against a NEW launch profile: %v", err)
	}

	// And a non-user-visible / unknown id is still rejected at validation, not by
	// the FK.
	if _, err := store.UpdateUserProfilePreferences(ctx, s.userID, strptr("1080p60-h264")); err != ErrProfileUnknown {
		t.Fatalf("a RUNG id as a user preference: got %v, want ErrProfileUnknown", err)
	}
}

// TestLaunchProfileRungOrderIsPreference asserts the write path assigns
// `position` from the ORDER of the id array — a client never sends positions —
// and that a re-write replaces the whole chain (that is how a drag-reorder
// lands).
func TestLaunchProfileRungOrderIsPreference(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	seedChain(t, pool, "order-src", []chainRung{
		{id: "order-av1", codec: "av1", w: 2560, h: 1440},
		{id: "order-hevc", codec: "hevc", w: 2560, h: 1440},
		{id: "order-h264", codec: "h264", w: 1920, h: 1080},
	})

	name := "Ordered"
	created, err := store.CreateLaunchProfile(ctx, LaunchProfileWrite{
		ID: "ordered", DisplayName: &name,
		Rungs: []string{"order-hevc", "order-av1", "order-h264"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := rungIDs(created.Rungs); got[0] != "order-hevc" || got[1] != "order-av1" || got[2] != "order-h264" {
		t.Fatalf("rung order = %v, want the written order", got)
	}

	updated, err := store.UpdateLaunchProfile(ctx, "ordered", LaunchProfileWrite{
		Rungs: []string{"order-av1", "order-hevc", "order-h264"},
	})
	if err != nil {
		t.Fatalf("reorder: %v", err)
	}
	if got := rungIDs(updated.Rungs); got[0] != "order-av1" || got[1] != "order-hevc" || got[2] != "order-h264" {
		t.Fatalf("reordered rungs = %v, want the new order", got)
	}
	if len(updated.Rungs) != 3 {
		t.Fatalf("reorder left %d rungs, want 3 (the chain is replaced, not appended to)", len(updated.Rungs))
	}
}

// TestRungABRFloorReadsTheRung is the AS10-06 health-evaluation fix: the floor
// comes from the RESOLVED RUNG in the database, so an admin tuning a per-rung
// floor — the entire point of the restructure — is actually honoured.
func TestRungABRFloorReadsTheRung(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	floor, err := store.RungABRFloor(ctx, "1080p60-h264")
	if err != nil {
		t.Fatalf("read floor: %v", err)
	}
	if floor != 4000 {
		t.Fatalf("1080p60-h264 floor = %d, want 4000 (inherited from the seed)", floor)
	}

	if _, err := pool.Exec(ctx, `UPDATE stream_profiles SET abr_floor_kbps = 1234 WHERE id = '1080p60-h264'`); err != nil {
		t.Fatalf("tune floor: %v", err)
	}
	floor, _ = store.RungABRFloor(ctx, "1080p60-h264")
	if floor != 1234 {
		t.Fatalf("tuned floor = %d, want 1234 — health evaluation must not read a compiled-in number", floor)
	}

	// An unknown rung yields (0, nil) so the caller skips evaluation.
	if floor, err := store.RungABRFloor(ctx, "no-such-rung"); err != nil || floor != 0 {
		t.Fatalf("unknown rung = (%d,%v), want (0,nil)", floor, err)
	}
}
