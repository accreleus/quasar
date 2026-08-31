package session

// migration_0041_db_test.go — the encoder-certification re-key.
//
// 0041 adds a key dimension (the rung) to host_encoder_certification and
// re-points the existing rows at the h264 rung of the launch profile they name.
// Two things have to be true and they are proved separately:
//
//   - TestMigration0041MigratesExistingRows: the DATA migration is right. Rows
//     land on the rung they were actually measured at, and a row that cannot be
//     interpreted at the new grain is dropped rather than guessed at.
//   - TestMigration0041RoundTrip: the down path WORKS and is lossless for data
//     0041 itself produced — including the rows it deleted, which come back.
//   - TestMigration0041DownCollapsesPerRungRows: the down path's ONE lossy case
//     (two rungs of a chain certified separately cannot both fit the 0018 key)
//     resolves deterministically instead of failing on the unique constraint.
//
// All three run in a SCRATCH database (scratchDB, migration_0036_db_test.go):
// they migrate DOWN, which must never happen to a database other tests share.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// version39 is the last migration before this one; version41 is this one.
// (0040 is deliberately skipped — it is taken by a separate in-flight change.)
const (
	version39 = 39
	version41 = 41
)

// certFixture is one pre-0041 certification row, in the shape the SPT-06 harness
// has always written: an H.264 measurement labelled with a launch profile.
type certFixture struct {
	profileID string
	bitrate   int
	verdict   string
	wantRung  string // "" ⇒ the row must not survive the migration
}

func seedPre0041Certs(t *testing.T, pool *pgxpool.Pool, fixtures []certFixture) string {
	t.Helper()
	ctx := context.Background()

	var hostID string
	if err := pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, capacity_detection)
		VALUES ('host-0041','online','ok') RETURNING id::text`).Scan(&hostID); err != nil {
		t.Fatalf("seed host: %v", err)
	}

	for _, f := range fixtures {
		if _, err := pool.Exec(ctx, `
			INSERT INTO host_encoder_certification
			    (host_id, gpu_index, encoder, profile_id,
			     width, height, fps, bitrate_kbps,
			     verdict, encode_ms_p50, encode_ms_p95, encode_ms_max,
			     output_fps, drop_rate, live_write_stable,
			     sample_window_ms, sample_count, agent_version, measured_at, updated_at)
			VALUES ($1::uuid, 0, 'va', $2,
			        1920, 1080, 60, $3,
			        $4, 10.0, 14.0, 18.0,
			        59.5, 0.005, true,
			        25000, 250, '0.6.0', now(), now())
		`, hostID, f.profileID, f.bitrate, f.verdict); err != nil {
			t.Fatalf("seed cert row %s: %v", f.profileID, err)
		}
	}
	return hostID
}

// TestMigration0041MigratesExistingRows pins the honest mapping: the pre-0041
// rows are H.264 measurements (launchCertCell never set a codec, and the sessions
// INSERT coalesces "" to h264), so they belong to the h264 rung of their launch
// profile — the very rung 0036's fan-out cloned from the row's own resolution.
func TestMigration0041MigratesExistingRows(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, version39)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()

	fixtures := []certFixture{
		{profileID: "1080p60", bitrate: 8000, verdict: "unsafe", wantRung: "1080p60-h264"},
		{profileID: "720p60", bitrate: 6000, verdict: "capped", wantRung: "720p60-h264"},
		{profileID: "720p30", bitrate: 4000, verdict: "ok", wantRung: "720p30-h264"},
		// A row naming something that is not a launch profile: uninterpretable at
		// rung grain, so it must be dropped rather than mapped to a guess.
		{profileID: "retired-profile", bitrate: 8000, verdict: "unsafe", wantRung: ""},
	}
	hostID := seedPre0041Certs(t, pool, fixtures)

	// Sanity: at 39 the column does not exist yet.
	var exists bool
	must(t, pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'host_encoder_certification' AND column_name = 'stream_profile_id')`).Scan(&exists))
	if exists {
		t.Fatal("stream_profile_id must not exist before 0041")
	}

	migrateTo(t, m, version41)

	for _, f := range fixtures {
		var rung string
		err := pool.QueryRow(ctx, `
			SELECT stream_profile_id FROM host_encoder_certification
			WHERE host_id = $1::uuid AND profile_id = $2`, hostID, f.profileID).Scan(&rung)
		if f.wantRung == "" {
			if err == nil {
				t.Errorf("row %s survived with rung %q; an uninterpretable row must be dropped",
					f.profileID, rung)
			}
			continue
		}
		if err != nil {
			t.Errorf("row %s: %v", f.profileID, err)
			continue
		}
		if rung != f.wantRung {
			t.Errorf("row %s: stream_profile_id = %q, want %q", f.profileID, rung, f.wantRung)
		}
	}

	// The dropped row is recoverable — it is in the snapshot the down path reads.
	var backed int
	must(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM _backup_0041_host_encoder_certification WHERE profile_id = 'retired-profile'`).Scan(&backed))
	if backed != 1 {
		t.Errorf("snapshot rows for the dropped cert: got %d want 1", backed)
	}

	// The new key admits per-codec rows for ONE launch profile. Under the 0018 key
	// this INSERT is a unique violation — which is the whole defect.
	if _, err := pool.Exec(ctx, `
		INSERT INTO host_encoder_certification
		    (host_id, gpu_index, encoder, profile_id, stream_profile_id,
		     width, height, fps, bitrate_kbps,
		     verdict, encode_ms_p50, encode_ms_p95, encode_ms_max,
		     output_fps, drop_rate, live_write_stable,
		     sample_window_ms, sample_count, measured_at, updated_at)
		SELECT $1::uuid, 0, 'va', '1080p60', sp.id,
		       1920, 1080, 60, 8000,
		       'ok', 2.0, 3.0, 4.0,
		       60.0, 0.0, true,
		       25000, 250, now(), now()
		FROM stream_profiles sp WHERE sp.id = '720p60-h264'
	`, hostID); err != nil {
		t.Fatalf("a second rung of the same launch profile must be certifiable: %v", err)
	}

	// And the FK is real: an unknown rung is refused.
	if _, err := pool.Exec(ctx, `
		UPDATE host_encoder_certification SET stream_profile_id = 'no-such-rung'
		WHERE host_id = $1::uuid AND profile_id = '720p30'`, hostID); err == nil {
		t.Error("stream_profile_id must be FK-constrained to stream_profiles")
	}
}

// TestMigration0041RoundTrip proves the down path restores the pre-migration
// table exactly — including the row the up migration deleted as uninterpretable.
func TestMigration0041RoundTrip(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, version39)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()

	seedPre0041Certs(t, pool, []certFixture{
		{profileID: "1080p60", bitrate: 8000, verdict: "unsafe"},
		{profileID: "720p60", bitrate: 6000, verdict: "capped"},
		{profileID: "720p30", bitrate: 4000, verdict: "ok"},
		{profileID: "retired-profile", bitrate: 8000, verdict: "unsafe"},
	})

	before := dumpTables(t, pool, []string{"host_encoder_certification"})

	migrateTo(t, m, version41)
	migrateTo(t, m, version39)

	after := dumpTables(t, pool, []string{"host_encoder_certification"})
	if before != after {
		t.Errorf("0041 round trip is not lossless.\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	// The 0018 unique key is back and enforcing: a duplicate on it must be refused.
	var hostID string
	must(t, pool.QueryRow(ctx,
		`SELECT host_id::text FROM host_encoder_certification LIMIT 1`).Scan(&hostID))
	if _, err := pool.Exec(ctx, `
		INSERT INTO host_encoder_certification
		    (host_id, gpu_index, encoder, profile_id,
		     width, height, fps, bitrate_kbps,
		     verdict, encode_ms_p50, encode_ms_p95, encode_ms_max,
		     output_fps, drop_rate, live_write_stable,
		     sample_window_ms, sample_count, measured_at, updated_at)
		VALUES ($1::uuid, 0, 'va', '1080p60',
		        1920, 1080, 60, 8000,
		        'ok', 1.0, 2.0, 3.0, 60.0, 0.0, true, 25000, 250, now(), now())
	`, hostID); err == nil {
		t.Error("the restored 0018 unique key must refuse a duplicate (host, gpu, encoder, profile, bitrate)")
	}
}

// TestMigration0041DownCollapsesPerRungRows covers the down path's inherently
// lossy case. Two rungs of one chain certified at the same bitrate cannot both
// exist under the 0018 key; the newest measurement must win, deterministically,
// rather than the migration dying on a unique violation.
func TestMigration0041DownCollapsesPerRungRows(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, version41)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()

	var hostID string
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, capacity_detection)
		VALUES ('host-0041-collapse','online','ok') RETURNING id::text`).Scan(&hostID))

	// An AV1 rung on the 1080p60 chain, so the chain has two certifiable rungs.
	must(t, pool.QueryRow(ctx, `
		INSERT INTO stream_profiles (id, display_name, width, height, fps, h264_profile,
		    nominal_bitrate_kbps, min_offer_bandwidth_kbps, recommended_offer_bandwidth_kbps,
		    headroom_factor, abr_floor_kbps, max_startup_rtt_ms, min_decode_height,
		    high_refresh_display, hardware_encoder_required, browser_client, playout0_ms,
		    visibility, codec)
		SELECT '1080p60-av1', '1080p60 av1', width, height, fps, h264_profile,
		    nominal_bitrate_kbps, min_offer_bandwidth_kbps, recommended_offer_bandwidth_kbps,
		    headroom_factor, abr_floor_kbps, max_startup_rtt_ms, min_decode_height,
		    high_refresh_display, hardware_encoder_required, browser_client, playout0_ms,
		    'internal', 'av1'
		FROM stream_profiles WHERE id = '1080p60-h264'
		RETURNING id`).Scan(new(string)))
	if _, err := pool.Exec(ctx, `
		INSERT INTO launch_profile_rungs (launch_profile_id, stream_profile_id, position)
		VALUES ('1080p60', '1080p60-av1', 2)`); err != nil {
		t.Fatalf("attach av1 rung: %v", err)
	}

	older := time.Now().Add(-2 * time.Hour)
	newer := time.Now()
	for _, r := range []struct {
		rung       string
		verdict    string
		measuredAt time.Time
	}{
		{"1080p60-h264", "ok", older},
		{"1080p60-av1", "unsafe", newer},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO host_encoder_certification
			    (host_id, gpu_index, encoder, profile_id, stream_profile_id,
			     width, height, fps, bitrate_kbps,
			     verdict, encode_ms_p50, encode_ms_p95, encode_ms_max,
			     output_fps, drop_rate, live_write_stable,
			     sample_window_ms, sample_count, measured_at, updated_at)
			VALUES ($1::uuid, 0, 'nvenc', '1080p60', $2,
			        1920, 1080, 60, 8000,
			        $3, 10.0, 14.0, 18.0, 59.5, 0.005, true, 25000, 250, $4, now())
		`, hostID, r.rung, r.verdict, r.measuredAt); err != nil {
			t.Fatalf("seed cert for %s: %v", r.rung, err)
		}
	}

	migrateTo(t, m, version39)

	var n int
	var verdict string
	must(t, pool.QueryRow(ctx, `
		SELECT COUNT(*), MIN(verdict) FROM host_encoder_certification
		WHERE host_id = $1::uuid AND profile_id = '1080p60'`, hostID).Scan(&n, &verdict))
	if n != 1 {
		t.Fatalf("after down: got %d rows for the 1080p60 key, want 1 (collapsed)", n)
	}
	if verdict != "unsafe" {
		t.Errorf("collapsed verdict: got %q want %q (the NEWEST measurement must win)", verdict, "unsafe")
	}
}
