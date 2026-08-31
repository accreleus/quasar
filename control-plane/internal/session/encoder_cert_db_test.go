package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// SPT-06 — DB integration tests for the encoder certification store.
// Tests exercise migration 0018 (host_encoder_certification table).
// Requires Postgres (run via scripts/dev/dev.sh go-test-db).

// certSeedRow returns a valid EncoderCertRow for testing. rungID must name a real
// stream_profiles row — migration 0041 made stream_profile_id the key AND an FK.
func certSeedRow(hostID string, gpuIndex int, encoder, profileID, rungID string, bitrateKbps int, verdict string) EncoderCertRow {
	return EncoderCertRow{
		HostID:          hostID,
		GPUIndex:        gpuIndex,
		Encoder:         encoder,
		ProfileID:       profileID,
		StreamProfileID: rungID,
		Width:           1920,
		Height:          1080,
		FPS:             60,
		BitrateKbps:     bitrateKbps,
		Verdict:         verdict,
		EncodeP50:       10.0,
		EncodeP95:       14.0,
		EncodeMax:       18.0,
		OutputFPS:       59.5,
		DropRate:        0.005,
		LiveWriteStable: true,
		SampleWindowMs:  25000,
		SampleCount:     250,
		MeasuredAt:      time.Now().UTC().Truncate(time.Millisecond),
	}
}

func TestUpsertAndQueryEncoderCert(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	row := certSeedRow(s.hostID, 0, "va", "1080p60", "1080p60-h264", 8000, VerdictOK)
	if err := store.UpsertEncoderCert(ctx, row); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	certs, err := store.GetEncoderCerts(ctx, s.hostID, CertFilter{})
	if err != nil {
		t.Fatalf("get certs: %v", err)
	}
	if len(certs) != 1 {
		t.Fatalf("expected 1 cert, got %d", len(certs))
	}
	got := certs[0]
	if got.HostID != s.hostID {
		t.Errorf("host_id: got %q want %q", got.HostID, s.hostID)
	}
	if got.Verdict != VerdictOK {
		t.Errorf("verdict: got %q want %q", got.Verdict, VerdictOK)
	}
	if got.EncodeP95 != row.EncodeP95 {
		t.Errorf("encode_ms_p95: got %v want %v", got.EncodeP95, row.EncodeP95)
	}
	if got.SampleCount != row.SampleCount {
		t.Errorf("sample_count: got %d want %d", got.SampleCount, row.SampleCount)
	}
	if got.LiveWriteStable != true {
		t.Errorf("live_write_stable: got false want true")
	}
}

// TestEncoderCertVulkanAccepted guards migration 0019_encoder_cert_vulkan: the
// widened encoder CHECK must admit encoder='vulkan'. Before 0019 this upsert
// fails with a check_violation on host_encoder_certification_encoder_check.
func TestEncoderCertVulkanAccepted(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	row := certSeedRow(s.hostID, 0, "vulkan", "1080p60", "1080p60-h264", 8000, VerdictOK)
	if err := store.UpsertEncoderCert(ctx, row); err != nil {
		t.Fatalf("upsert vulkan cert: %v", err)
	}

	enc := "vulkan"
	certs, err := store.GetEncoderCerts(ctx, s.hostID, CertFilter{Encoder: &enc})
	if err != nil {
		t.Fatalf("get vulkan certs: %v", err)
	}
	if len(certs) != 1 {
		t.Fatalf("expected 1 vulkan cert, got %d", len(certs))
	}
	if certs[0].Encoder != "vulkan" {
		t.Errorf("encoder: got %q want vulkan", certs[0].Encoder)
	}
}

func TestEncoderCertUpsertReplaces(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	// Upsert same unique key twice with different verdicts.
	row1 := certSeedRow(s.hostID, 0, "va", "1080p60", "1080p60-h264", 8000, VerdictOK)
	if err := store.UpsertEncoderCert(ctx, row1); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}

	row2 := certSeedRow(s.hostID, 0, "va", "1080p60", "1080p60-h264", 8000, VerdictUnsafe)
	row2.EncodeP95 = 25.0
	row2.SampleCount = 300
	if err := store.UpsertEncoderCert(ctx, row2); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}

	certs, err := store.GetEncoderCerts(ctx, s.hostID, CertFilter{})
	if err != nil {
		t.Fatalf("get certs: %v", err)
	}
	if len(certs) != 1 {
		t.Fatalf("expected 1 cert after upsert-replace, got %d", len(certs))
	}
	// The second upsert must win.
	if certs[0].Verdict != VerdictUnsafe {
		t.Errorf("verdict after replace: got %q want %q", certs[0].Verdict, VerdictUnsafe)
	}
	if certs[0].EncodeP95 != 25.0 {
		t.Errorf("encode_ms_p95 after replace: got %v want 25.0", certs[0].EncodeP95)
	}
}

func TestGetEncoderCertsFilter(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	// Upsert rows with different profiles and GPU indices.
	rows := []EncoderCertRow{
		certSeedRow(s.hostID, 0, "va", "1080p60", "1080p60-h264", 8000, VerdictOK),
		certSeedRow(s.hostID, 0, "va", "720p60", "720p60-h264", 6000, VerdictCapped),
		certSeedRow(s.hostID, 1, "nvenc", "1080p60", "1080p60-h264", 8000, VerdictOK),
	}
	for _, r := range rows {
		if err := store.UpsertEncoderCert(ctx, r); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	// Filter by profile_id.
	pid := "1080p60"
	certs, err := store.GetEncoderCerts(ctx, s.hostID, CertFilter{ProfileID: &pid})
	if err != nil {
		t.Fatalf("get certs by profile: %v", err)
	}
	if len(certs) != 2 {
		t.Fatalf("expected 2 certs for 1080p60, got %d", len(certs))
	}

	// Filter by GPU index 0.
	gpu := 0
	certs, err = store.GetEncoderCerts(ctx, s.hostID, CertFilter{GPUIndex: &gpu})
	if err != nil {
		t.Fatalf("get certs by gpu: %v", err)
	}
	if len(certs) != 2 {
		t.Fatalf("expected 2 certs for gpu 0, got %d", len(certs))
	}

	// Filter by encoder.
	enc := "nvenc"
	certs, err = store.GetEncoderCerts(ctx, s.hostID, CertFilter{Encoder: &enc})
	if err != nil {
		t.Fatalf("get certs by encoder: %v", err)
	}
	if len(certs) != 1 {
		t.Fatalf("expected 1 cert for nvenc, got %d", len(certs))
	}
	if certs[0].Encoder != "nvenc" {
		t.Errorf("encoder: got %q want nvenc", certs[0].Encoder)
	}
}

// TestCertForRungMatchesPickCert is the ANTI-DRIFT GUARD between the two
// selectors that now exist for the same question.
//
// CertForRung answers "the row that applies to this rung at this bitrate" by
// pushing `ORDER BY ABS(bitrate_kbps - $n) ASC, measured_at DESC LIMIT 1` into
// SQL. The launch path cannot use it — it reads certs for rungs it has not
// resolved yet — so CertsForRungs returns the fresh set unranked and pickCert
// (stream_plan.go) restates that ORDER BY in Go, where it is unit-testable.
//
// Two implementations of one rule is a drift hazard, and stream_plan.go's
// comment asserts they must not drift. This test is what makes that assertion
// enforceable rather than aspirational: same rows, same question, same answer.
// If it fails, one of the two moved.
func TestCertForRungMatchesPickCert(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	// Rows spread either side of the asked-for bitrates, on two rungs, so the
	// distance ordering and the rung filter both have work to do.
	for _, bw := range []int{3000, 4000, 8000, 12000} {
		must(t, store.UpsertEncoderCert(ctx,
			certSeedRow(s.hostID, 0, "va", "1080p60", "1080p60-h264", bw, VerdictOK)))
	}
	must(t, store.UpsertEncoderCert(ctx,
		certSeedRow(s.hostID, 0, "va", "720p60", "720p60-h264", 4500, VerdictUnsafe)))

	rungIDs := []string{"1080p60-h264", "720p60-h264", "4k120-h264"}
	certs, err := store.CertsForRungs(ctx, s.hostID, 0, rungIDs, CertStaleness)
	must(t, err)

	// The bitrates probe exact hits, midpoints, and both extremes — including a
	// value below the lowest row and one above the highest.
	for _, rungID := range rungIDs {
		for _, bw := range []int32{0, 3000, 3400, 3500, 3600, 6000, 7000, 8000, 10000, 99000} {
			// encoder "" is the branch the launch path always took.
			want, err := store.CertForRung(ctx, s.hostID, 0, "", rungID, bw, CertStaleness)
			must(t, err)
			got := pickCert(certs, rungID, bw, time.Now(), CertStaleness)

			switch {
			case want == nil && got != nil:
				t.Errorf("rung=%s bw=%d: SQL found nothing, pickCert chose %d kbps", rungID, bw, got.BitrateKbps)
			case want != nil && got == nil:
				t.Errorf("rung=%s bw=%d: SQL chose %d kbps, pickCert found nothing", rungID, bw, want.BitrateKbps)
			case want != nil && got != nil && want.ID != got.ID:
				t.Errorf("rung=%s bw=%d: SQL chose id=%s (%d kbps), pickCert chose id=%s (%d kbps)",
					rungID, bw, want.ID, want.BitrateKbps, got.ID, got.BitrateKbps)
			}
		}
	}
}

func TestCertForRung(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	// Upsert rows at different bitrates for the same (host, gpu, encoder, rung).
	for _, bw := range []int{4000, 8000, 12000} {
		r := certSeedRow(s.hostID, 0, "va", "1080p60", "1080p60-h264", bw, VerdictOK)
		if err := store.UpsertEncoderCert(ctx, r); err != nil {
			t.Fatalf("upsert bw=%d: %v", bw, err)
		}
	}

	// Ask for the row closest to 7000 kbps — should return the 8000 row.
	cert, err := store.CertForRung(ctx, s.hostID, 0, "va", "1080p60-h264", 7000, CertStaleness)
	if err != nil {
		t.Fatalf("cert for rung: %v", err)
	}
	if cert == nil {
		t.Fatal("expected a cert row, got nil")
	}
	if cert.BitrateKbps != 8000 {
		t.Errorf("closest bitrate: got %d want 8000", cert.BitrateKbps)
	}

	// Ask for a non-existent rung — should return nil (no cap).
	cert, err = store.CertForRung(ctx, s.hostID, 0, "va", "4k120-h264", 8000, CertStaleness)
	if err != nil {
		t.Fatalf("cert for unknown rung: %v", err)
	}
	if cert != nil {
		t.Errorf("expected nil for unknown rung, got %+v", cert)
	}

	// THE POINT OF MIGRATION 0041. The LAUNCH PROFILE id is not the key: asking
	// for it as if it were a rung must find nothing, because a chain has no single
	// encode cost. Before 0041 this same lookup returned the h264 measurement and
	// the scheduler applied it to whatever codec the session resolved.
	cert, err = store.CertForRung(ctx, s.hostID, 0, "va", "1080p60", 8000, CertStaleness)
	if err != nil {
		t.Fatalf("cert for launch profile id: %v", err)
	}
	if cert != nil {
		t.Errorf("a launch-profile id must not resolve a rung cert, got %+v", cert)
	}
}

// TestCertIsCodecScoped is the direct assertion this whole re-key exists for: a
// cert measured for ONE codec must not apply to a DIFFERENT codec at the same
// launch profile and bitrate.
//
// Pre-0041 the two rows below could not even coexist — they collided on
// UNIQUE (host, gpu, encoder, profile_id, bitrate_kbps), so certifying the AV1
// rung silently OVERWROTE the H.264 verdict, and whichever ran last became the
// scheduler's opinion of both.
func TestCertIsCodecScoped(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	chain := seedMultiCodecChain(t, pool)

	// Same host, GPU, encoder, launch profile and bitrate; different codecs.
	h264Row := certSeedRow(s.hostID, 0, "nvenc", chain.id, chain.h264RungID, 8000, VerdictOK)
	av1Row := certSeedRow(s.hostID, 0, "nvenc", chain.id, chain.av1RungID, 8000, VerdictUnsafe)
	av1Row.EncodeP95 = 31.0
	if err := store.UpsertEncoderCert(ctx, h264Row); err != nil {
		t.Fatalf("upsert h264 cert: %v", err)
	}
	if err := store.UpsertEncoderCert(ctx, av1Row); err != nil {
		t.Fatalf("upsert av1 cert: %v", err)
	}

	// Both survive: the key now has a codec dimension.
	pid := chain.id
	certs, err := store.GetEncoderCerts(ctx, s.hostID, CertFilter{ProfileID: &pid})
	if err != nil {
		t.Fatalf("get certs: %v", err)
	}
	if len(certs) != 2 {
		t.Fatalf("expected 2 certs (one per rung) for %s, got %d", chain.id, len(certs))
	}

	// And each lookup returns ITS OWN verdict, not the other codec's.
	gotH264, err := store.CertForRung(ctx, s.hostID, 0, "nvenc", chain.h264RungID, 8000, CertStaleness)
	if err != nil || gotH264 == nil {
		t.Fatalf("cert for h264 rung: %v / %v", gotH264, err)
	}
	if gotH264.Verdict != VerdictOK {
		t.Errorf("h264 rung verdict: got %q want %q (the av1 measurement leaked across)",
			gotH264.Verdict, VerdictOK)
	}

	gotAV1, err := store.CertForRung(ctx, s.hostID, 0, "nvenc", chain.av1RungID, 8000, CertStaleness)
	if err != nil || gotAV1 == nil {
		t.Fatalf("cert for av1 rung: %v / %v", gotAV1, err)
	}
	if gotAV1.Verdict != VerdictUnsafe {
		t.Errorf("av1 rung verdict: got %q want %q (the h264 measurement leaked across)",
			gotAV1.Verdict, VerdictUnsafe)
	}
	if gotAV1.EncodeP95 != 31.0 {
		t.Errorf("av1 rung encode_ms_p95: got %v want 31.0", gotAV1.EncodeP95)
	}
}

// TestResolveCertTarget covers the three ways a bench cell names what to certify.
func TestResolveCertTarget(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	_ = seed(t, pool, 4)
	ctx := context.Background()

	chain := seedMultiCodecChain(t, pool)

	// A LAUNCH PROFILE resolves to its first h264 rung — what the bench has always
	// streamed, so an un-updated harness keeps measuring what it measured before.
	got, err := store.ResolveCertTarget(ctx, "1080p60", "")
	if err != nil {
		t.Fatalf("resolve launch profile: %v", err)
	}
	if got.Rung.ID != "1080p60-h264" || got.LaunchProfileID != "1080p60" {
		t.Errorf("launch profile target: got rung=%q lp=%q want 1080p60-h264 / 1080p60",
			got.Rung.ID, got.LaunchProfileID)
	}

	// Even when the chain's FIRST rung is not h264, the fallback still picks h264 —
	// it is preserving the old bench behaviour, not re-running the launch resolver.
	got, err = store.ResolveCertTarget(ctx, chain.id, "")
	if err != nil {
		t.Fatalf("resolve multi-codec chain: %v", err)
	}
	if got.Rung.ID != chain.h264RungID {
		t.Errorf("multi-codec chain default target: got %q want %q", got.Rung.ID, chain.h264RungID)
	}

	// An explicit rung wins and carries its chain as context.
	got, err = store.ResolveCertTarget(ctx, "", chain.av1RungID)
	if err != nil {
		t.Fatalf("resolve av1 rung: %v", err)
	}
	if got.Rung.ID != chain.av1RungID || got.LaunchProfileID != chain.id {
		t.Errorf("av1 rung target: got rung=%q lp=%q want %q / %q",
			got.Rung.ID, got.LaunchProfileID, chain.av1RungID, chain.id)
	}
	if got.Rung.Codec != "av1" {
		t.Errorf("av1 rung codec: got %q want av1", got.Rung.Codec)
	}

	// A pre-0036 legacy stream_profiles row is NOT a rung and cannot be certified.
	if _, err := store.ResolveCertTarget(ctx, "", "1080p60"); !errors.Is(err, ErrCertTargetNotARung) {
		t.Errorf("legacy row as rung: got %v want ErrCertTargetNotARung", err)
	}

	// A rung no launch profile lists can never be launched, so certifying it is
	// meaningless and is refused rather than silently recorded.
	orphan := seedOrphanRung(t, pool)
	if _, err := store.ResolveCertTarget(ctx, "", orphan); !errors.Is(err, ErrCertTargetOrphanRung) {
		t.Errorf("orphan rung: got %v want ErrCertTargetOrphanRung", err)
	}

	if _, err := store.ResolveCertTarget(ctx, "no-such-profile", ""); err == nil {
		t.Error("unknown id must not resolve a cert target")
	}
}

func TestCertStaleness(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	// Insert a row then backdate its measured_at to 8 days ago.
	row := certSeedRow(s.hostID, 0, "va", "1080p60", "1080p60-h264", 8000, VerdictOK)
	if err := store.UpsertEncoderCert(ctx, row); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	_, err := pool.Exec(ctx,
		`UPDATE host_encoder_certification SET measured_at = now() - interval '8 days'
		 WHERE host_id = $1::uuid AND profile_id = '1080p60'`, s.hostID)
	if err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Query with 7-day max age — the stale row must be excluded.
	maxAge := 7 * 24 * time.Hour
	certs, err := store.GetEncoderCerts(ctx, s.hostID, CertFilter{MaxAge: &maxAge})
	if err != nil {
		t.Fatalf("get certs: %v", err)
	}
	if len(certs) != 0 {
		t.Fatalf("expected 0 certs (stale), got %d", len(certs))
	}

	// CertForRung with CertStaleness (7 days) must also return nil.
	cert, err := store.CertForRung(ctx, s.hostID, 0, "va", "1080p60-h264", 8000, CertStaleness)
	if err != nil {
		t.Fatalf("cert for rung: %v", err)
	}
	if cert != nil {
		t.Errorf("expected nil for stale cert, got %+v", cert)
	}
}

// TestEnsureBenchUser verifies the system bench user is created and idempotent.
func TestEnsureBenchUser(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	id1, err := store.EnsureBenchUser(ctx)
	if err != nil {
		t.Fatalf("first EnsureBenchUser: %v", err)
	}
	if id1 == "" {
		t.Fatal("expected non-empty user id")
	}

	// Idempotent: second call returns the same id.
	id2, err := store.EnsureBenchUser(ctx)
	if err != nil {
		t.Fatalf("second EnsureBenchUser: %v", err)
	}
	if id1 != id2 {
		t.Errorf("idempotent upsert: first=%q second=%q", id1, id2)
	}
}

// TestMeasureToVerdictUpsert verifies the numeric measure→DeriveVerdict→upsert
// pipeline with real numbers for Renoir (unsafe) and RTX 5090 (ok) cases.
//
// Renoir VCN: p95 ~20ms @ 60fps budget 16.7ms → unsafe (p95 > budget).
// RTX 5090:   p95 ~1ms  @ 60fps budget 16.7ms → ok (p95 ≤ 0.70×16.7=11.7).
func TestMeasureToVerdictUpsert(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	const budget60fps = 1000.0 / 60.0

	type tc struct {
		name        string
		p95         float64
		outputFPS   float64
		dropRate    float64
		wantVerdict string
	}
	cases := []tc{
		// Renoir VCN at 1080p60: p95=20ms, budget=16.7ms → p95 > budget → unsafe.
		{"renoir-1080p60", 20.08, 45.3, 0.0, VerdictUnsafe},
		// RTX 5090 at 1080p60: p95=1ms, full FPS, no drops → ok.
		{"5090-1080p60", 1.0, 60.0, 0.0, VerdictOK},
		// Tight budget: p95=14ms, within budget but above 70% threshold → capped.
		{"capped-1080p60", 14.0, 60.0, 0.0, VerdictCapped},
	}

	for i, tc := range cases {
		verdict := DeriveVerdict(tc.p95, budget60fps, tc.outputFPS, 60.0, tc.dropRate)
		if verdict != tc.wantVerdict {
			t.Errorf("DeriveVerdict[%s]: got %q want %q", tc.name, verdict, tc.wantVerdict)
		}

		// Upsert the result and read it back to verify DB round-trip.
		row := EncoderCertRow{
			HostID:          s.hostID,
			GPUIndex:        i,
			Encoder:         "va",
			ProfileID:       "1080p60",
			StreamProfileID: "1080p60-h264",
			Width:           1920,
			Height:          1080,
			FPS:             60,
			BitrateKbps:     8000,
			Verdict:         verdict,
			EncodeP50:       tc.p95 * 0.95,
			EncodeP95:       tc.p95,
			EncodeMax:       tc.p95 * 1.2,
			OutputFPS:       tc.outputFPS,
			DropRate:        tc.dropRate,
			LiveWriteStable: true,
			SampleWindowMs:  25000,
			SampleCount:     250,
			MeasuredAt:      time.Now().UTC().Truncate(time.Millisecond),
		}
		if err := store.UpsertEncoderCert(ctx, row); err != nil {
			t.Fatalf("[%s] upsert: %v", tc.name, err)
		}
		gpu := i
		enc := "va"
		pid := "1080p60"
		certs, err := store.GetEncoderCerts(ctx, s.hostID, CertFilter{GPUIndex: &gpu, Encoder: &enc, ProfileID: &pid})
		if err != nil {
			t.Fatalf("[%s] get certs: %v", tc.name, err)
		}
		if len(certs) != 1 {
			t.Fatalf("[%s] expected 1 cert, got %d", tc.name, len(certs))
		}
		if certs[0].Verdict != verdict {
			t.Errorf("[%s] stored verdict: got %q want %q", tc.name, certs[0].Verdict, verdict)
		}
		if certs[0].EncodeP95 != tc.p95 {
			t.Errorf("[%s] stored encode_ms_p95: got %v want %v", tc.name, certs[0].EncodeP95, tc.p95)
		}
	}
}

// TestPinHostID verifies the scheduler's PinHostID constraint routes to the
// target host and rejects a pinned host that is full.
func TestPinHostID(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 2) // 2 encode slots on s.hostID
	ctx := context.Background()

	// Insert a second host (no GPUs → never a placement candidate).
	var host2ID string
	must(t, pool.QueryRow(ctx,
		`INSERT INTO hosts (node_name, status, capacity_detection) VALUES ('host-2','online','ok') RETURNING id::text`).
		Scan(&host2ID))

	// Pin to s.hostID — must succeed.
	p := launchParams(s)
	p.PinHostID = s.hostID
	sess, err := store.ScheduleAndCreate(ctx, p)
	if err != nil {
		t.Fatalf("pinned launch failed: %v", err)
	}
	if sess.HostID == nil || *sess.HostID != s.hostID {
		t.Errorf("session should be on pinned host %q, got %v", s.hostID, sess.HostID)
	}

	// Exhaust the host's capacity (1 more slot).
	p2 := launchParams(s)
	p2.PinHostID = s.hostID
	if _, err := store.ScheduleAndCreate(ctx, p2); err != nil {
		t.Fatalf("second pinned launch failed: %v", err)
	}

	// Third launch on pinned (full) host must fail — no other host has capacity.
	p3 := launchParams(s)
	p3.PinHostID = s.hostID
	_, err = store.ScheduleAndCreate(ctx, p3)
	if err == nil {
		t.Error("expected error when pinned host is full, got nil")
	}
}

func TestUpdateSessionStream(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// UI-P4 WIDENED this write. Before, it carried only width/height/fps/bitrate/
	// profile_id, which sufficed ONLY because a certification cap could not change
	// playout0, the H.264 profile, or the codec. A RUNG can change all three, so
	// all of them must land in the same UPDATE or the persisted row disagrees with
	// what was dispatched to the agent.
	if err := store.UpdateSessionStream(ctx, sess.ID, SessionStreamUpdate{
		Width: 1280, Height: 720, FPS: 30, BitrateKbps: 3000,
		H264Profile: "main", Codec: "av1", Playout0Ms: 100,
		ProfileID: "720p30", StreamProfileID: "720p30-h264",
	}); err != nil {
		t.Fatalf("update session stream: %v", err)
	}

	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Width != 1280 || got.Height != 720 || got.FPS != 30 {
		t.Errorf("stream dims: got %dx%d@%d want 1280x720@30", got.Width, got.Height, got.FPS)
	}
	if got.BitrateKbps != 3000 {
		t.Errorf("bitrate: got %d want 3000", got.BitrateKbps)
	}
	if got.ProfileID == nil || *got.ProfileID != "720p30" {
		t.Errorf("profile_id: got %v want \"720p30\"", got.ProfileID)
	}
	if got.StreamProfileID == nil || *got.StreamProfileID != "720p30-h264" {
		t.Errorf("stream_profile_id: got %v want \"720p30-h264\"", got.StreamProfileID)
	}
	if got.H264Profile != "main" {
		t.Errorf("h264_profile: got %q want main", got.H264Profile)
	}
	if got.Codec != "av1" {
		t.Errorf("codec: got %q want av1", got.Codec)
	}
	if got.Playout0Ms != 100 {
		t.Errorf("playout0_ms: got %d want 100", got.Playout0Ms)
	}

	// An empty/zero field leaves its column ALONE — that is what lets a caller
	// write a subset without clobbering the rest.
	if err := store.UpdateSessionStream(ctx, sess.ID, SessionStreamUpdate{
		Width: 1920, Height: 1080, FPS: 60, BitrateKbps: 8000,
	}); err != nil {
		t.Fatalf("partial update: %v", err)
	}
	got, _ = store.Get(ctx, sess.ID)
	if got.H264Profile != "main" || got.Codec != "av1" || got.Playout0Ms != 100 {
		t.Errorf("partial update clobbered untouched columns: h264=%q codec=%q playout0=%d",
			got.H264Profile, got.Codec, got.Playout0Ms)
	}
	if got.ProfileID == nil || *got.ProfileID != "720p30" || got.StreamProfileID == nil || *got.StreamProfileID != "720p30-h264" {
		t.Errorf("partial update clobbered the profile ids: %v / %v", got.ProfileID, got.StreamProfileID)
	}
}

// --- multi-codec chain fixtures (0041) -------------------------------------------
//
// The migration-0015/0036 catalogue is single-rung h264 on every chain, which is
// exactly the shape under which the wrong-grained key was harmless. These helpers
// build the shape that exposes it. They are torn down per test because
// stream_profiles / launch_profiles are NOT truncated between tests in this
// package — a leaked extra rung would silently change what other launch-path
// tests resolve.

type certTestChain struct {
	id         string
	h264RungID string
	av1RungID  string
}

// seedMultiCodecChain creates an INTERNAL launch profile with an AV1 rung first
// and an H.264 rung second, both cloned from 1080p60. Internal visibility keeps it
// out of every user-facing catalogue read, so eligibility/recommendation tests are
// untouched. AV1 is deliberately position 1: it makes "the chain's first rung" and
// "the chain's h264 rung" different objects, so a test cannot pass by accident.
func seedMultiCodecChain(t *testing.T, pool *pgxpool.Pool) certTestChain {
	t.Helper()
	ctx := context.Background()
	store := NewStore(pool)

	chain := certTestChain{
		id:         "certtest-mc",
		h264RungID: "certtest-mc-h264",
		av1RungID:  "certtest-mc-av1",
	}

	base, err := store.GetStreamProfile(ctx, "1080p60")
	if err != nil {
		t.Fatalf("load base stream profile: %v", err)
	}

	for _, r := range []struct {
		id    string
		codec profile.Codec
	}{{chain.av1RungID, profile.CodecAV1}, {chain.h264RungID, profile.CodecH264}} {
		p := base
		p.ID = r.id
		p.Codec = r.codec
		p.DisplayName = r.id
		p.Visibility = profile.VisibilityInternal
		if _, err := store.CreateStreamProfile(ctx, p); err != nil {
			t.Fatalf("create rung %s: %v", r.id, err)
		}
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO launch_profiles (id, display_name, visibility, sort_order)
		VALUES ($1, 'cert test multi-codec', 'internal', 9999)
	`, chain.id); err != nil {
		t.Fatalf("create launch profile: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO launch_profile_rungs (launch_profile_id, stream_profile_id, position)
		VALUES ($1, $2, 1), ($1, $3, 2)
	`, chain.id, chain.av1RungID, chain.h264RungID); err != nil {
		t.Fatalf("create launch profile rungs: %v", err)
	}

	t.Cleanup(func() {
		cctx := context.Background()
		// host_encoder_certification.stream_profile_id is ON DELETE CASCADE, so any
		// cert rows the test wrote go with the rungs.
		if _, err := pool.Exec(cctx, `DELETE FROM launch_profiles WHERE id = $1`, chain.id); err != nil {
			t.Logf("cleanup launch profile: %v", err)
		}
		if _, err := pool.Exec(cctx, `DELETE FROM stream_profiles WHERE id = ANY($1)`,
			[]string{chain.av1RungID, chain.h264RungID}); err != nil {
			t.Logf("cleanup rungs: %v", err)
		}
	})
	return chain
}

// seedOrphanRung creates a real rung that no launch profile lists.
func seedOrphanRung(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	store := NewStore(pool)
	const id = "certtest-orphan-hevc"

	base, err := store.GetStreamProfile(ctx, "1080p60")
	if err != nil {
		t.Fatalf("load base stream profile: %v", err)
	}
	p := base
	p.ID = id
	p.DisplayName = id
	p.Codec = profile.CodecHEVC
	p.Visibility = profile.VisibilityInternal
	if _, err := store.CreateStreamProfile(ctx, p); err != nil {
		t.Fatalf("create orphan rung: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM stream_profiles WHERE id = $1`, id); err != nil {
			t.Logf("cleanup orphan rung: %v", err)
		}
	})
	return id
}

// seedExtraRung appends a rung of the given catalog codec to an EXISTING launch
// profile, cloned from that chain's top rung, and removes it again on cleanup.
// Used where the test needs a real ladder id (lowerProfileRung only knows the
// shipped launch profiles) AND a second codec in the chain.
func seedExtraRung(t *testing.T, pool *pgxpool.Pool, launchProfileID string, codec profile.Codec) string {
	t.Helper()
	ctx := context.Background()
	store := NewStore(pool)

	lp, err := store.GetLaunchProfile(ctx, launchProfileID)
	if err != nil {
		t.Fatalf("load launch profile %s: %v", launchProfileID, err)
	}
	top, ok := lp.TopRung()
	if !ok {
		t.Fatalf("launch profile %s has no rungs", launchProfileID)
	}

	id := launchProfileID + "-certtest-" + string(codec)
	p := top
	p.ID = id
	p.DisplayName = id
	p.Codec = codec
	p.Visibility = profile.VisibilityInternal
	if _, err := store.CreateStreamProfile(ctx, p); err != nil {
		t.Fatalf("create extra rung %s: %v", id, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO launch_profile_rungs (launch_profile_id, stream_profile_id, position)
		VALUES ($1, $2, (SELECT COALESCE(MAX(position), 0) + 1
		                   FROM launch_profile_rungs WHERE launch_profile_id = $1))
	`, launchProfileID, id); err != nil {
		t.Fatalf("attach extra rung: %v", err)
	}

	t.Cleanup(func() {
		cctx := context.Background()
		if _, err := pool.Exec(cctx,
			`DELETE FROM launch_profile_rungs WHERE stream_profile_id = $1`, id); err != nil {
			t.Logf("cleanup extra rung link: %v", err)
		}
		if _, err := pool.Exec(cctx, `DELETE FROM stream_profiles WHERE id = $1`, id); err != nil {
			t.Logf("cleanup extra rung: %v", err)
		}
	})
	return id
}
