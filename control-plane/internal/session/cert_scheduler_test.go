package session

import (
	"context"
	"testing"
	"time"
)

// SPT-06 — unit tests for DeriveVerdict, certCapProfile, and the in-memory
// certRunManager. DeriveVerdict tests are pure (no DB); certCapProfile tests
// are DB-backed (need Postgres via scripts/dev/dev.sh go-test-db).

// budget for 60 fps = 1000/60 ≈ 16.667 ms
const budget60fps = 1000.0 / 60.0

// --- DeriveVerdict unit tests (pure, always run) --------------------------------

func TestDeriveVerdict_ok(t *testing.T) {
	// p95 within 70% of budget, FPS on target, drops clean.
	verdict := DeriveVerdict(10.0, budget60fps, 60.0, 60.0, 0.0)
	if verdict != VerdictOK {
		t.Errorf("got %q want %q", verdict, VerdictOK)
	}
}

func TestDeriveVerdict_capped(t *testing.T) {
	// p95 above 70% threshold but still below budget → capped.
	// certThreshold * 16.667 ≈ 11.667 ms; p95=14 is above threshold, below budget.
	verdict := DeriveVerdict(14.0, budget60fps, 60.0, 60.0, 0.0)
	if verdict != VerdictCapped {
		t.Errorf("got %q want %q", verdict, VerdictCapped)
	}
}

func TestDeriveVerdict_unsafe_p95(t *testing.T) {
	// p95 over the frame budget → unsafe.
	verdict := DeriveVerdict(20.0, budget60fps, 60.0, 60.0, 0.0)
	if verdict != VerdictUnsafe {
		t.Errorf("got %q want %q", verdict, VerdictUnsafe)
	}
}

func TestDeriveVerdict_unsafe_byFPS(t *testing.T) {
	// p95 fine but FPS materially below target with high drops → unsafe.
	verdict := DeriveVerdict(10.0, budget60fps, 40.0, 60.0, 0.10)
	if verdict != VerdictUnsafe {
		t.Errorf("got %q want %q", verdict, VerdictUnsafe)
	}
}

func TestDeriveVerdict_unsafe_highDrops(t *testing.T) {
	// Drop rate alone above 5% → unsafe even if FPS looks ok.
	verdict := DeriveVerdict(10.0, budget60fps, 59.0, 60.0, 0.06)
	if verdict != VerdictUnsafe {
		t.Errorf("got %q want %q", verdict, VerdictUnsafe)
	}
}

func TestDeriveVerdict_ok_lowDrops(t *testing.T) {
	// Drop rate at exactly 1% should still be ok (boundary).
	verdict := DeriveVerdict(10.0, budget60fps, 60.0, 60.0, 0.01)
	if verdict != VerdictOK {
		t.Errorf("got %q want %q", verdict, VerdictOK)
	}
}

// --- lowerProfileRung unit tests (pure, always run) -----------------------------

func TestLowerProfileRung(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"1080p60", "720p60"},
		{"720p60", "720p30"},
		{"720p30", ""},  // no lower rung
		{"unknown", ""}, // not in the list
		{"1080p120", "1080p60"},
		{"1440p120", "1440p60"},
		{"1440p60", "1080p120"},
	}
	for _, tc := range cases {
		got := lowerProfileRung(tc.in)
		if got != tc.want {
			t.Errorf("lowerProfileRung(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}

// --- cert cap DB tests (need Postgres) ------------------------------------------
//
// The cap is no longer one method. gatherStreamInputs performs the batch cert
// read and planStream applies the verdict rule; certCap below composes exactly
// those two halves, so these cases still exercise the SQL that selects the row
// AND the Go that judges it.

// certCap is the cap decision as the launch path composes it: the batch read
// (SQL) plus pickCert / certShouldCap / lowerProfileRung (pure).
func certCap(t *testing.T, store *Store, hostID string, gpu int, chainID, rungID string, bitrate int32) (bool, string) {
	t.Helper()
	certs, err := store.CertsForRungs(context.Background(), hostID, gpu, []string{rungID}, CertStaleness)
	must(t, err)
	cert := pickCert(certs, rungID, bitrate, time.Now(), CertStaleness)
	if cert == nil || !certShouldCap(*cert) {
		return false, ""
	}
	lower := lowerProfileRung(chainID)
	if lower == "" {
		return false, ""
	}
	return true, lower
}

func TestCertCapRung_noCert(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)

	// No certs in DB — optimistic, no cap applied.
	capped, lower := certCap(t, store, s.hostID, 0, "1080p60", "1080p60-h264", 8000)
	if capped || lower != "" {
		t.Errorf("expected no cap when cert absent, got capped=%v lower=%q", capped, lower)
	}
}

func TestCertCapRung_okNoCap(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	row := certSeedRow(s.hostID, 0, "va", "1080p60", "1080p60-h264", 8000, VerdictOK)
	must(t, store.UpsertEncoderCert(ctx, row))

	capped, lower := certCap(t, store, s.hostID, 0, "1080p60", "1080p60-h264", 8000)
	if capped || lower != "" {
		t.Errorf("ok cert must not cap: capped=%v lower=%q", capped, lower)
	}
}

// TestCertCapRung_unsafeCaps is the SPT-06 no-regression case: the h264 behaviour
// the cap shipped with still fires, unchanged, on today's single-rung data.
func TestCertCapRung_unsafeCaps(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	row := certSeedRow(s.hostID, 0, "va", "1080p60", "1080p60-h264", 8000, VerdictUnsafe)
	must(t, store.UpsertEncoderCert(ctx, row))

	capped, lower := certCap(t, store, s.hostID, 0, "1080p60", "1080p60-h264", 8000)
	if !capped {
		t.Error("unsafe cert must cap")
	}
	// The next launch profile below 1080p60 is 720p60.
	if lower != "720p60" {
		t.Errorf("lower profile: got %q want \"720p60\"", lower)
	}
}

// TestCertCapRung_isCodecScoped pins the defect this re-key closes AT THE CAP:
// an `unsafe` verdict measured on one codec's rung must not cap a launch that
// resolved a DIFFERENT codec's rung of the same launch profile at the same
// bitrate — and vice versa, a clean verdict on one codec must not clear another.
//
// Pre-0041 both directions were wrong, because certCapProfile looked the verdict
// up by launch-profile id and got whichever row the bench happened to write.
func TestCertCapRung_isCodecScoped(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	// A REAL chain from the ladder, so the cap has a lower launch profile to hop
	// to, given a second (AV1) rung.
	const chainID = "1080p60"
	const h264Rung = "1080p60-h264"
	av1Rung := seedExtraRung(t, pool, chainID, "av1")

	// Only the AV1 rung is certified, and it is unsafe.
	must(t, store.UpsertEncoderCert(ctx,
		certSeedRow(s.hostID, 0, "nvenc", chainID, av1Rung, 8000, VerdictUnsafe)))

	// A session that resolved the AV1 rung is capped.
	capped, lower := certCap(t, store, s.hostID, 0, chainID, av1Rung, 8000)
	if !capped || lower != "720p60" {
		t.Errorf("an unsafe av1 cert must cap an av1 launch: capped=%v lower=%q", capped, lower)
	}

	// A session that resolved the H.264 rung of the SAME chain at the SAME bitrate
	// is NOT capped: nothing has been measured about it. Uncertified is optimistic.
	capped, lower = certCap(t, store, s.hostID, 0, chainID, h264Rung, 8000)
	if capped {
		t.Errorf("an av1 verdict must not cap an h264 launch (got lower=%q)", lower)
	}

	// Now certify H.264 as unsafe too and AV1 as ok, and check the mapping did not
	// simply invert: each rung must read its own row.
	must(t, store.UpsertEncoderCert(ctx,
		certSeedRow(s.hostID, 0, "nvenc", chainID, h264Rung, 8000, VerdictUnsafe)))
	must(t, store.UpsertEncoderCert(ctx,
		certSeedRow(s.hostID, 0, "nvenc", chainID, av1Rung, 8000, VerdictOK)))

	if capped, _ := certCap(t, store, s.hostID, 0, chainID, h264Rung, 8000); !capped {
		t.Error("an unsafe h264 cert must cap an h264 launch")
	}
	if capped, _ := certCap(t, store, s.hostID, 0, chainID, av1Rung, 8000); capped {
		t.Error("an ok av1 cert must not cap an av1 launch")
	}
}

// TestCertsForRungs_batchAcrossChains covers what the launch path actually reads:
// one query spanning both the selected chain's rungs and the cap target's, with
// the ranking left to pickCert.
func TestCertsForRungs_batchAcrossChains(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	must(t, store.UpsertEncoderCert(ctx,
		certSeedRow(s.hostID, 0, "va", "1080p60", "1080p60-h264", 8000, VerdictUnsafe)))
	must(t, store.UpsertEncoderCert(ctx,
		certSeedRow(s.hostID, 0, "va", "720p60", "720p60-h264", 4000, VerdictOK)))

	certs, err := store.CertsForRungs(ctx, s.hostID, 0,
		[]string{"1080p60-h264", "720p60-h264"}, CertStaleness)
	must(t, err)
	if len(certs) != 2 {
		t.Fatalf("expected both chains' certs in one read, got %d", len(certs))
	}

	if c := pickCert(certs, "1080p60-h264", 8000, time.Now(), CertStaleness); c == nil || !certShouldCap(*c) {
		t.Error("the 1080p60 rung's own unsafe row must be the one selected")
	}
	if c := pickCert(certs, "720p60-h264", 4000, time.Now(), CertStaleness); c == nil || certShouldCap(*c) {
		t.Error("the 720p60 rung's own ok row must be the one selected")
	}
	if c := pickCert(certs, "1440p60-h264", 8000, time.Now(), CertStaleness); c != nil {
		t.Error("a rung with no row must select nothing")
	}
}

// --- certRunManager unit tests (pure, always run) --------------------------------

func TestCertRunManager_startAndGet(t *testing.T) {
	m := newCertRunManager()

	// Start a run.
	run, ok := m.Start("host-1", 12)
	if !ok || run == nil {
		t.Fatal("Start returned ok=false on first call")
	}
	if run.Status != CertRunRunning {
		t.Errorf("status: got %q want running", run.Status)
	}
	if run.TotalPts != 12 {
		t.Errorf("total_pts: got %d want 12", run.TotalPts)
	}

	// Second Start for the same host must be rejected.
	_, ok2 := m.Start("host-1", 12)
	if ok2 {
		t.Error("second Start for same host must return ok=false")
	}

	// Get by ID.
	got, ok := m.Get(run.ID, "host-1")
	if !ok || got == nil {
		t.Fatal("Get returned ok=false")
	}
	if got.ID != run.ID {
		t.Errorf("run ID mismatch: got %q want %q", got.ID, run.ID)
	}

	// Get with wrong hostID must be rejected.
	_, ok = m.Get(run.ID, "host-2")
	if ok {
		t.Error("Get with wrong hostID must return false")
	}
}

func TestCertRunManager_completeAndFail(t *testing.T) {
	m := newCertRunManager()

	run, _ := m.Start("host-a", 4)
	m.Increment(run.ID, VerdictOK)
	m.Increment(run.ID, VerdictOK)
	m.Increment(run.ID, VerdictCapped)
	m.Increment(run.ID, VerdictUnsafe)

	m.Complete(run.ID, 2, 1, 1)
	got, _ := m.Get(run.ID, "host-a")
	if got.Status != CertRunCompleted {
		t.Errorf("status: got %q want completed", got.Status)
	}
	if got.SummaryOK != 2 || got.SummaryCapped != 1 || got.SummaryUnsafe != 1 {
		t.Errorf("summary: ok=%d capped=%d unsafe=%d", got.SummaryOK, got.SummaryCapped, got.SummaryUnsafe)
	}
	if got.EndedAt == nil {
		t.Error("EndedAt must be set on complete")
	}

	// Fail path on a separate run.
	run2, _ := m.Start("host-b", 2)
	m.Fail(run2.ID, "host went offline")
	got2, _ := m.Get(run2.ID, "host-b")
	if got2.Status != CertRunFailed {
		t.Errorf("status: got %q want failed", got2.Status)
	}
	if got2.ErrorMessage == nil || *got2.ErrorMessage != "host went offline" {
		t.Errorf("error_message: got %v", got2.ErrorMessage)
	}
}

// TestCertRunManager_StartEvictsPriorTerminalRunForHost pins the leak fix: once a
// host's run reaches a terminal state, starting a new run for that SAME host must
// evict the old terminal run from `runs` rather than let it accumulate forever.
func TestCertRunManager_StartEvictsPriorTerminalRunForHost(t *testing.T) {
	m := newCertRunManager()

	run1, ok := m.Start("host-1", 4)
	if !ok {
		t.Fatal("first Start failed")
	}
	m.Complete(run1.ID, 4, 0, 0)

	// The completed run is still reachable right after Complete (callers re-Get
	// immediately, e.g. handleCompleteCertRun).
	if _, ok := m.Get(run1.ID, "host-1"); !ok {
		t.Fatal("expected completed run to still be gettable immediately after Complete")
	}

	// Starting a new run for the same host must succeed (the prior run is
	// terminal) and evict the old run's entry.
	run2, ok := m.Start("host-1", 4)
	if !ok {
		t.Fatal("second Start for same host (after terminal) must succeed")
	}
	if run2.ID == run1.ID {
		t.Fatal("second run must have a distinct ID")
	}
	if _, ok := m.Get(run1.ID, "host-1"); ok {
		t.Error("prior terminal run must be evicted once a new run starts for the same host")
	}
	if got, ok := m.Get(run2.ID, "host-1"); !ok || got.Status != CertRunRunning {
		t.Fatalf("new run: ok=%v status=%v", ok, got)
	}

	m.mu.RLock()
	n := len(m.runs)
	m.mu.RUnlock()
	if n != 1 {
		t.Errorf("runs map size: got %d want 1 (old terminal run evicted)", n)
	}
}

// TestCertRunManager_StartSweepsExpiredTerminalRuns pins the TTL backstop: a
// terminal run older than certRunTTL is pruned by Start's sweep even for a
// DIFFERENT host (the case the byHost-supersede path alone would not catch —
// a host that never starts another run would otherwise leak its last run
// forever).
func TestCertRunManager_StartSweepsExpiredTerminalRuns(t *testing.T) {
	m := newCertRunManager()

	stale, ok := m.Start("host-stale", 2)
	if !ok {
		t.Fatal("Start(host-stale) failed")
	}
	m.Complete(stale.ID, 2, 0, 0)

	// Force the run to look older than the TTL.
	m.mu.Lock()
	old := time.Now().Add(-certRunTTL - time.Minute)
	m.runs[stale.ID].EndedAt = &old
	m.mu.Unlock()

	// Starting a run on an unrelated host triggers the sweep.
	if _, ok := m.Start("host-other", 2); !ok {
		t.Fatal("Start(host-other) failed")
	}

	if _, ok := m.Get(stale.ID, "host-stale"); ok {
		t.Error("expired terminal run must be swept by Start's sweep")
	}
}

// TestCertRunManager_RunningNeverPruned confirms the sweep never touches an
// in-flight run regardless of age (only terminal runs with EndedAt set qualify).
func TestCertRunManager_RunningNeverPruned(t *testing.T) {
	m := newCertRunManager()

	running, ok := m.Start("host-running", 2)
	if !ok {
		t.Fatal("Start(host-running) failed")
	}
	// Backdate StartedAt to look old; it has no EndedAt (still running) so the
	// sweep must never evict it.
	m.mu.Lock()
	m.runs[running.ID].StartedAt = time.Now().Add(-certRunTTL - time.Hour)
	m.mu.Unlock()

	if _, ok := m.Start("host-another", 1); !ok {
		t.Fatal("Start(host-another) failed")
	}

	if _, ok := m.Get(running.ID, "host-running"); !ok {
		t.Error("a still-running run must never be pruned by the sweep")
	}
}
