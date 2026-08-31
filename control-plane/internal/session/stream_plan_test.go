package session

// stream_plan_test.go — pure unit tests for the post-placement stream decision
// (stream_plan.go): planStream, pickCert, and StreamPlan.applyTo. No database
// required; these run on every `go test`. Sibling to rung_test.go — reuses its
// r/hwRung/probe/probeAt helpers where a case is really about rung resolution,
// and adds rung/chain builders with an EXPLICIT bitrate where a case is about
// the cert cap or the SPT-07 envelope, which rung_test.go's r() (bitrate
// derived from height) can't express.

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// fixedNow is the pinned clock for every case below. pickCert's staleness
// check and the cert rows' MeasuredAt both read wall time; pinning it means a
// case never depends on when the test happens to run.
var fixedNow = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

// rung builds a rung with an EXPLICIT bitrate — unlike rung_test.go's r(),
// which derives NominalBitrateKbps from height. The cert-cap and envelope
// cases below need to pick resolution and bitrate apart from each other.
func rung(id string, codec profile.Codec, w, h, fps, kbps int32) profile.Profile {
	return profile.Profile{
		ID: id, DisplayName: id, Codec: codec,
		Width: w, Height: h, FPS: fps,
		NominalBitrateKbps: kbps, MinDecodeHeight: h,
		ABRFloorKbps: kbps / 3, Playout0Ms: 50,
		H264Profile: "high", Visibility: profile.VisibilityInternal,
	}
}

// chain builds a launch profile ("chain") from an id and its rungs, in
// position order.
func chain(id string, rungs ...profile.Profile) profile.LaunchProfile {
	return profile.LaunchProfile{ID: id, DisplayName: id, Rungs: rungs, Visibility: profile.VisibilityInternal}
}

// certCapFixture is the top/lower chain pair shared by the SPT-06 cap tests:
// a 1080p60 h264 chain at 8000kbps, and its 720p60 h264 lower chain at
// 4000kbps — the shape planStream actually walks (resolve top, cap on ITS
// rung, hop to lower and re-resolve).
func certCapFixture() (top, lower profile.LaunchProfile) {
	top = chain("1080p60", rung("1080p60-h264", profile.CodecH264, 1920, 1080, 60, 8000))
	lower = chain("720p60", rung("720p60-h264", profile.CodecH264, 1280, 720, 60, 4000))
	return top, lower
}

// --- planStream --------------------------------------------------------------

// TestPlanStreamShippedSingleRungChain is the production data shape after
// migration 0036: every launch profile fans out to a single h264 rung.
// Treat this as the NO-REGRESSION case — if this breaks, every launch does.
func TestPlanStreamShippedSingleRungChain(t *testing.T) {
	in := StreamInputs{
		SessionID:   "sess-1",
		HostID:      strptr("host-1"),
		GPUIndex:    i32(0),
		H264Profile: "high",
		Codec:       "h264",
		Chain:       chain("1080p60", rung("1080p60-h264", profile.CodecH264, 1920, 1080, 60, 8000)),
		HostCodecs:  []string{"h264"},
		Now:         fixedNow,
		CertMaxAge:  CertStaleness,
	}
	plan, err := planStream(in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if plan.ChainID != "1080p60" || plan.RungID != "1080p60-h264" {
		t.Errorf("chain/rung = %q/%q, want 1080p60/1080p60-h264", plan.ChainID, plan.RungID)
	}
	if plan.Capped {
		t.Error("Capped = true, want false (no certs)")
	}
	if plan.CapOutcome != capNoCert {
		t.Errorf("CapOutcome = %q, want %q", plan.CapOutcome, capNoCert)
	}
	if len(plan.Walks) != 1 {
		t.Fatalf("len(Walks) = %d, want 1", len(plan.Walks))
	}
	want := SessionStreamUpdate{
		Width: 1920, Height: 1080, FPS: 60, BitrateKbps: 8000,
		H264Profile: "high", Codec: "h264", Playout0Ms: 50,
		ProfileID: "1080p60", StreamProfileID: "1080p60-h264",
	}
	got := plan.Update
	got.CodecDecision = nil // compared separately below; it's a marshalled blob
	// SessionStreamUpdate carries a json.RawMessage (a slice), so the struct
	// isn't comparable with == — compare field by field instead.
	if got.Width != want.Width || got.Height != want.Height || got.FPS != want.FPS ||
		got.BitrateKbps != want.BitrateKbps || got.H264Profile != want.H264Profile ||
		got.Codec != want.Codec || got.Playout0Ms != want.Playout0Ms ||
		got.ProfileID != want.ProfileID || got.StreamProfileID != want.StreamProfileID {
		t.Errorf("Update = %+v, want %+v", got, want)
	}
	if len(plan.Update.CodecDecision) == 0 {
		t.Error("Update.CodecDecision is empty, want the marshalled rung decision")
	}
}

// TestPlanStreamMultiCodecRejectsUnprovenAV1: an av1 rung at position 1 loses
// to the h264 rung at position 2 because the client's decode probe says no
// av1 — and the rejection reason is recorded, not just the outcome.
func TestPlanStreamMultiCodecRejectsUnprovenAV1(t *testing.T) {
	in := StreamInputs{
		HostID:     strptr("host-1"),
		GPUIndex:   i32(0),
		Chain:      chain("high", r("2160p60-av1", profile.CodecAV1, 2160), r("1080p60-h264", profile.CodecH264, 1080)),
		HostCodecs: []string{"h264", "av1"},
		Probe:      probe(true, false), // hevc irrelevant here; av1 unproven
		Now:        fixedNow,
		CertMaxAge: CertStaleness,
	}
	plan, err := planStream(in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if plan.ChainID != "high" || plan.RungID != "1080p60-h264" {
		t.Errorf("chain/rung = %q/%q, want high/1080p60-h264", plan.ChainID, plan.RungID)
	}
	considered := plan.Decision.Considered
	if len(considered) != 2 {
		t.Fatalf("len(Considered) = %d, want 2", len(considered))
	}
	if considered[0].ID != "2160p60-av1" || considered[0].Selected || considered[0].Reject != rejectClientDecode {
		t.Errorf("av1 verdict = %+v, want rejected by %q, not selected", considered[0], rejectClientDecode)
	}
	if considered[1].ID != "1080p60-h264" || !considered[1].Selected {
		t.Errorf("h264 verdict = %+v, want selected", considered[1])
	}
}

// TestPlanStreamCertCapHopsToLowerChain: an unsafe cert on the resolved rung
// replaces the whole chain with the ladder's lower one, re-resolved.
func TestPlanStreamCertCapHopsToLowerChain(t *testing.T) {
	top, lower := certCapFixture()
	in := StreamInputs{
		HostID: strptr("host-1"), GPUIndex: i32(0),
		Chain: top, LowerChain: lower, LowerChainID: lower.ID,
		HostCodecs: []string{"h264"},
		Certs: []EncoderCertRow{
			{StreamProfileID: "1080p60-h264", BitrateKbps: 8000, Verdict: VerdictUnsafe, MeasuredAt: fixedNow},
		},
		// Distinct per-chain failure sets, neither of which rejects anything here:
		// they exist so the walk records can be told apart. See the Failed
		// assertion below.
		FailedRungs: map[string]bool{"top-only": true},
		LowerFailed: map[string]bool{"lower-only": true},
		Now:         fixedNow, CertMaxAge: CertStaleness,
	}
	plan, err := planStream(in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !plan.Capped {
		t.Error("Capped = false, want true")
	}
	if plan.CapOutcome != capApplied {
		t.Errorf("CapOutcome = %q, want %q", plan.CapOutcome, capApplied)
	}
	if plan.ChainID != "720p60" || plan.RungID != "720p60-h264" {
		t.Errorf("chain/rung = %q/%q, want the LOWER chain's 720p60/720p60-h264", plan.ChainID, plan.RungID)
	}
	if len(plan.Walks) != 2 {
		t.Fatalf("len(Walks) = %d, want 2 (the top attempt AND the lower hop)", len(plan.Walks))
	}
	if plan.Walks[0].ChainID != "1080p60" || plan.Walks[1].ChainID != "720p60" {
		t.Errorf("walk chain ids = %q, %q, want 1080p60, 720p60", plan.Walks[0].ChainID, plan.Walks[1].ChainID)
	}
	if plan.Update.Width != 1280 || plan.Update.Height != 720 || plan.Update.BitrateKbps != 4000 {
		t.Errorf("Update = %+v, want the LOWER rung's numbers (1280x720 @ 4000kbps)", plan.Update)
	}
	// Each walk must carry the failure set IT was resolved against. The launch
	// log prints this per walk, and pairing the second walk's verdicts with the
	// first chain's history yields a line that contradicts itself.
	if !plan.Walks[0].Failed["top-only"] {
		t.Error("walk 0 must carry the original chain's failure set")
	}
	if !plan.Walks[1].Failed["lower-only"] {
		t.Error("walk 1 must carry the cap target's failure set, not the original's")
	}
}

// TestPlanStreamCertCappedButLiveWriteStableIsNotCapped: a "capped" verdict
// with live_write_stable=true means the encoder CAN do live bitrate updates,
// so ABR can keep the stream in budget even though the bench measured it
// above the "ok" threshold — SPT-06 lets it through rather than hopping a
// whole chain down for something ABR can manage live.
func TestPlanStreamCertCappedButLiveWriteStableIsNotCapped(t *testing.T) {
	top, lower := certCapFixture()
	in := StreamInputs{
		HostID: strptr("host-1"), GPUIndex: i32(0),
		Chain: top, LowerChain: lower, LowerChainID: lower.ID,
		HostCodecs: []string{"h264"},
		Certs: []EncoderCertRow{
			{StreamProfileID: "1080p60-h264", BitrateKbps: 8000, Verdict: VerdictCapped, LiveWriteStable: true, MeasuredAt: fixedNow},
		},
		Now: fixedNow, CertMaxAge: CertStaleness,
	}
	plan, err := planStream(in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if plan.Capped {
		t.Error("Capped = true, want false")
	}
	if plan.CapOutcome != capCertOK {
		t.Errorf("CapOutcome = %q, want %q", plan.CapOutcome, capCertOK)
	}
	if plan.ChainID != "1080p60" {
		t.Errorf("ChainID = %q, want the original 1080p60", plan.ChainID)
	}
}

// TestPlanStreamCertCappedWithoutLiveWriteIsCapped is the mirror of the case
// above: live_write_stable=false means the encoder CANNOT do live bitrate
// updates, so ABR has no way to keep a "capped" rung in budget once it drifts
// — treated the same as unsafe.
func TestPlanStreamCertCappedWithoutLiveWriteIsCapped(t *testing.T) {
	top, lower := certCapFixture()
	in := StreamInputs{
		HostID: strptr("host-1"), GPUIndex: i32(0),
		Chain: top, LowerChain: lower, LowerChainID: lower.ID,
		HostCodecs: []string{"h264"},
		Certs: []EncoderCertRow{
			{StreamProfileID: "1080p60-h264", BitrateKbps: 8000, Verdict: VerdictCapped, LiveWriteStable: false, MeasuredAt: fixedNow},
		},
		Now: fixedNow, CertMaxAge: CertStaleness,
	}
	plan, err := planStream(in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !plan.Capped {
		t.Error("Capped = false, want true")
	}
	if plan.CapOutcome != capApplied {
		t.Errorf("CapOutcome = %q, want %q", plan.CapOutcome, capApplied)
	}
	if plan.ChainID != "720p60" {
		t.Errorf("ChainID = %q, want the lower chain 720p60", plan.ChainID)
	}
}

// TestPlanStreamCertUnsafeNoLowerChainNamed: the ladder names no lower chain
// (already at the bottom) — the cap has nowhere to hop to, so it allows the
// launch through uncapped rather than refusing it.
func TestPlanStreamCertUnsafeNoLowerChainNamed(t *testing.T) {
	top, _ := certCapFixture()
	in := StreamInputs{
		HostID: strptr("host-1"), GPUIndex: i32(0),
		Chain:      top, // LowerChain/LowerChainID left zero.
		HostCodecs: []string{"h264"},
		Certs: []EncoderCertRow{
			{StreamProfileID: "1080p60-h264", BitrateKbps: 8000, Verdict: VerdictUnsafe, MeasuredAt: fixedNow},
		},
		Now: fixedNow, CertMaxAge: CertStaleness,
	}
	plan, err := planStream(in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if plan.Capped {
		t.Error("Capped = true, want false")
	}
	if plan.CapOutcome != capNoLower {
		t.Errorf("CapOutcome = %q, want %q", plan.CapOutcome, capNoLower)
	}
	if plan.ChainID != "1080p60" {
		t.Errorf("ChainID = %q, want the original 1080p60 kept", plan.ChainID)
	}
	if plan.CapTarget != "" {
		t.Errorf("CapTarget = %q, want empty — nothing was named", plan.CapTarget)
	}
}

// TestPlanStreamCertUnsafeLowerChainFailedToLoad: the ladder names a lower
// chain, but its read came back the zero value (LowerChain.ID == "") — treat
// a load failure the same as "no lower", not as a hard failure of the launch.
func TestPlanStreamCertUnsafeLowerChainFailedToLoad(t *testing.T) {
	top, _ := certCapFixture()
	in := StreamInputs{
		HostID: strptr("host-1"), GPUIndex: i32(0),
		Chain: top, LowerChainID: "720p60", // named, but LowerChain never loaded.
		HostCodecs: []string{"h264"},
		Certs: []EncoderCertRow{
			{StreamProfileID: "1080p60-h264", BitrateKbps: 8000, Verdict: VerdictUnsafe, MeasuredAt: fixedNow},
		},
		Now: fixedNow, CertMaxAge: CertStaleness,
	}
	plan, err := planStream(in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if plan.Capped {
		t.Error("Capped = true, want false")
	}
	if plan.CapOutcome != capLowerUnreadable {
		t.Errorf("CapOutcome = %q, want %q", plan.CapOutcome, capLowerUnreadable)
	}
	if plan.ChainID != "1080p60" {
		t.Errorf("ChainID = %q, want the original 1080p60 kept", plan.ChainID)
	}
	if plan.CapTarget != "720p60" {
		t.Errorf("CapTarget = %q, want %q — it records what was attempted", plan.CapTarget, "720p60")
	}
}

// TestPlanStreamCertUnsafeLowerChainHasNoRungs: the lower chain loaded (a
// real row, non-empty ID) but holds no rungs — same treatment as a failed
// load, and for the same reason: there is nothing to resolve over.
func TestPlanStreamCertUnsafeLowerChainHasNoRungs(t *testing.T) {
	top, _ := certCapFixture()
	in := StreamInputs{
		HostID: strptr("host-1"), GPUIndex: i32(0),
		Chain: top, LowerChainID: "720p60",
		LowerChain: profile.LaunchProfile{ID: "720p60", DisplayName: "720p60"}, // no Rungs
		HostCodecs: []string{"h264"},
		Certs: []EncoderCertRow{
			{StreamProfileID: "1080p60-h264", BitrateKbps: 8000, Verdict: VerdictUnsafe, MeasuredAt: fixedNow},
		},
		Now: fixedNow, CertMaxAge: CertStaleness,
	}
	plan, err := planStream(in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if plan.Capped {
		t.Error("Capped = true, want false")
	}
	if plan.CapOutcome != capLowerEmpty {
		t.Errorf("CapOutcome = %q, want %q", plan.CapOutcome, capLowerEmpty)
	}
	if plan.CapTarget != "720p60" {
		t.Errorf("CapTarget = %q, want %q", plan.CapTarget, "720p60")
	}
}

// TestPlanStreamAdminBypassesCap: an admin launch is the operator forcing
// values — the cap must never even consult the cert table.
func TestPlanStreamAdminBypassesCap(t *testing.T) {
	top, lower := certCapFixture()
	in := StreamInputs{
		HostID: strptr("host-1"), GPUIndex: i32(0),
		Chain: top, LowerChain: lower, LowerChainID: lower.ID,
		HostCodecs: []string{"h264"},
		Params:     LaunchParams{IsAdmin: true},
		Certs: []EncoderCertRow{
			{StreamProfileID: "1080p60-h264", BitrateKbps: 8000, Verdict: VerdictUnsafe, MeasuredAt: fixedNow},
		},
		Now: fixedNow, CertMaxAge: CertStaleness,
	}
	plan, err := planStream(in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if plan.Capped {
		t.Error("Capped = true, want false — admin launches are never capped")
	}
	if plan.CapOutcome != capNotEligible {
		t.Errorf("CapOutcome = %q, want %q", plan.CapOutcome, capNotEligible)
	}
	if plan.ChainID != "1080p60" {
		t.Errorf("ChainID = %q, want original 1080p60", plan.ChainID)
	}
}

// TestPlanStreamExplicitOverrideBypassesCap: an explicit stream override is
// also the operator forcing values, orthogonal to admin — same treatment.
func TestPlanStreamExplicitOverrideBypassesCap(t *testing.T) {
	top, lower := certCapFixture()
	in := StreamInputs{
		HostID: strptr("host-1"), GPUIndex: i32(0),
		Chain: top, LowerChain: lower, LowerChainID: lower.ID,
		HostCodecs: []string{"h264"},
		Override:   StreamOverride{BitrateKbps: i32(6000)},
		Certs: []EncoderCertRow{
			{StreamProfileID: "1080p60-h264", BitrateKbps: 8000, Verdict: VerdictUnsafe, MeasuredAt: fixedNow},
		},
		Now: fixedNow, CertMaxAge: CertStaleness,
	}
	plan, err := planStream(in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if plan.Capped {
		t.Error("Capped = true, want false — an explicit override bypasses the cap")
	}
	if plan.CapOutcome != capNotEligible {
		t.Errorf("CapOutcome = %q, want %q", plan.CapOutcome, capNotEligible)
	}
}

// TestPlanStreamNoHostBypassesCapButStillResolves: placement produced no host
// (nil HostID/GPUIndex) — the cap has nothing to consult, but the walk must
// still run: an unplaced session is not a broken one. Also exercises clamp 1's
// other edge: nil HostCodecs is an h264-only host, not a host that accepts
// nothing.
func TestPlanStreamNoHostBypassesCapButStillResolves(t *testing.T) {
	in := StreamInputs{
		Chain: chain("high", r("2160p60-av1", profile.CodecAV1, 2160), r("1080p60-h264", profile.CodecH264, 1080)),
		// HostID/GPUIndex/HostCodecs all left zero.
		Probe:      probe(true, true),
		Now:        fixedNow,
		CertMaxAge: CertStaleness,
	}
	plan, err := planStream(in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if plan.CapOutcome != capNotEligible {
		t.Errorf("CapOutcome = %q, want %q", plan.CapOutcome, capNotEligible)
	}
	if plan.RungID != "1080p60-h264" {
		t.Errorf("RungID = %q, want the h264 rung — nil HostCodecs must clamp av1 like an h264-only host", plan.RungID)
	}
}

// TestPlanStreamEnvelopeReappliedAfterCapHop guards the bug rung.go and
// rungStream's docs warn about explicitly: the SPT-07 envelope must be
// RE-APPLIED to the LOWER rung's OWN bitrate after a cap hop, not carried over
// from the original rung's already-clamped number and not left unclamped.
//
// The top rung nominally dispatches at 8000kbps, the lower rung at 4000kbps,
// and the envelope's safe ceiling is 2500kbps — BELOW BOTH. A skipped
// re-apply would leave Update.BitrateKbps at the lower rung's raw 4000 (an
// unclamped value slipping through the fall-through, exactly what the comment
// warns about). RungBitrateKbps is asserted too because it is computed AFTER
// the hop from the WINNING rung: it must show the lower rung's pre-envelope
// number (4000), which is the only signal that distinguishes "recomputed for
// the final rung" from "reused the original rung's cached result" — both
// happen to saturate at the same clamped bitrate here, but only one is
// actually deriving it from the rung that got dispatched.
func TestPlanStreamEnvelopeReappliedAfterCapHop(t *testing.T) {
	top, lower := certCapFixture()
	in := StreamInputs{
		HostID: strptr("host-1"), GPUIndex: i32(0),
		Chain: top, LowerChain: lower, LowerChainID: lower.ID,
		HostCodecs: []string{"h264"},
		Envelope:   ProbeEnvelope{SafeCeilingKbps: 2500},
		Certs: []EncoderCertRow{
			// Matched against the TOP rung's POST-envelope dispatch bitrate
			// (2500, not its raw 8000) — that's what applyCertCap's pickCert
			// lookup actually reads.
			{StreamProfileID: "1080p60-h264", BitrateKbps: 2500, Verdict: VerdictUnsafe, MeasuredAt: fixedNow},
		},
		Now: fixedNow, CertMaxAge: CertStaleness,
	}
	plan, err := planStream(in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !plan.Capped || plan.ChainID != "720p60" {
		t.Fatalf("expected the cap to hop to 720p60, got Capped=%v ChainID=%q", plan.Capped, plan.ChainID)
	}
	if plan.Update.BitrateKbps != 2500 {
		t.Errorf("Update.BitrateKbps = %d, want 2500 (envelope ceiling applied to the FINAL rung)", plan.Update.BitrateKbps)
	}
	if plan.RungBitrateKbps != 4000 {
		t.Errorf("RungBitrateKbps = %d, want 4000 (the lower rung's PRE-envelope number, for logging the delta)", plan.RungBitrateKbps)
	}
}

// TestPlanStreamExplicitCodecOverrideNotInChain: an explicit admin/diagnostic
// codec override naming a codec no rung of the selected chain uses is a 400,
// and the failed walk still rides on the returned plan so the caller can log
// what was attempted.
func TestPlanStreamExplicitCodecOverrideNotInChain(t *testing.T) {
	single := chain("1080p60", rung("1080p60-h264", profile.CodecH264, 1920, 1080, 60, 8000))
	in := StreamInputs{
		Chain:      single,
		HostCodecs: []string{"h264", "av1"},
		Override:   StreamOverride{Codec: strptr("av1")},
		Now:        fixedNow, CertMaxAge: CertStaleness,
	}
	plan, err := planStream(in)
	if !errors.Is(err, ErrRungCodecNotAvailable) {
		t.Fatalf("err = %v, want ErrRungCodecNotAvailable", err)
	}
	if len(plan.Walks) != 1 || plan.Walks[0].ChainID != "1080p60" {
		t.Errorf("Walks = %+v, want the one failed walk recorded against 1080p60", plan.Walks)
	}
	if plan.Walks[0].Err == nil {
		t.Error("Walks[0].Err = nil, want the walk's own failure recorded")
	}
}

// TestPlanStreamExplicitCodecOverrideHostCannotEncode: the same override
// mechanism, but the requested codec exists in the chain and the placed host
// simply cannot encode it — a 409 (host encoder capability is physics, never
// overridable), distinct from the 400 above.
func TestPlanStreamExplicitCodecOverrideHostCannotEncode(t *testing.T) {
	high := chain("high", r("2160p60-av1", profile.CodecAV1, 2160), r("1080p60-h264", profile.CodecH264, 1080))
	in := StreamInputs{
		Chain:      high,
		HostCodecs: []string{"h264"}, // no av1
		Override:   StreamOverride{Codec: strptr("av1")},
		Now:        fixedNow, CertMaxAge: CertStaleness,
	}
	_, err := planStream(in)
	if !errors.Is(err, ErrCodecUnsupportedByHost) {
		t.Fatalf("err = %v, want ErrCodecUnsupportedByHost", err)
	}
}

// TestPlanStreamFloorKeepsSelectedAndRejectReason mirrors rung_test.go's
// TestTerminalRungBypassesEveryClamp through the FULL planStream path: when no
// rung survives the walk, the last h264 rung dispatches bypassing every clamp
// — but its verdict keeps BOTH Selected=true AND its ORIGINAL Reject reason.
// rung.go's rungVerdict doc is explicit about why that pairing matters: an
// unqualified pass would claim the rung survived clamps it was never actually
// measured against.
func TestPlanStreamFloorKeepsSelectedAndRejectReason(t *testing.T) {
	floor := hwRung("4k60-h264", profile.CodecH264, 2160)
	in := StreamInputs{
		Chain:       chain("4k-chain", r("4k60-av1", profile.CodecAV1, 2160), floor),
		HostCodecs:  []string{"h265"},                                     // clamp 1: can't even encode h264
		HostEncoder: hostEncoderCaps{Known: true, HardwareEncoder: false}, // clamp 5
		Probe:       probeAt(false, false, 720),                           // clamp 2/3
		FailedRungs: map[string]bool{"4k60-av1": true, "4k60-h264": true}, // clamp 4
		Now:         fixedNow, CertMaxAge: CertStaleness,
	}
	plan, err := planStream(in)
	if err != nil {
		t.Fatalf("err = %v; the floor must never fail to resolve", err)
	}
	if !plan.Decision.Floor {
		t.Error("Decision.Floor = false, want true")
	}
	if plan.RungID != "4k60-h264" {
		t.Fatalf("RungID = %q, want the floor 4k60-h264", plan.RungID)
	}

	var got *rungVerdict
	for i := range plan.Decision.Considered {
		if plan.Decision.Considered[i].ID == plan.RungID {
			got = &plan.Decision.Considered[i]
		}
	}
	if got == nil {
		t.Fatalf("no verdict recorded for the dispatched rung %q", plan.RungID)
	}
	if !got.Selected || !got.Bypassed || got.Reject == "" {
		t.Errorf("floor verdict = %+v, want Selected=true Bypassed=true and a NON-EMPTY Reject (kept, not cleared)", *got)
	}
}

// --- pickCert ------------------------------------------------------------

func TestPickCert(t *testing.T) {
	cases := []struct {
		name    string
		certs   []EncoderCertRow
		rungID  string
		bitrate int32
		maxAge  time.Duration
		want    string // cert ID; "" means nil
	}{
		{
			name: "closest bitrate wins",
			certs: []EncoderCertRow{
				{ID: "c4000", StreamProfileID: "r1", BitrateKbps: 4000, MeasuredAt: fixedNow},
				{ID: "c8000", StreamProfileID: "r1", BitrateKbps: 8000, MeasuredAt: fixedNow},
			},
			rungID: "r1", bitrate: 7000, maxAge: CertStaleness,
			want: "c8000", // |8000-7000|=1000 < |4000-7000|=3000
		},
		{
			name: "tie on distance: the more recent MeasuredAt wins",
			certs: []EncoderCertRow{
				{ID: "old", StreamProfileID: "r1", BitrateKbps: 6000, MeasuredAt: fixedNow.Add(-2 * time.Hour)},
				{ID: "new", StreamProfileID: "r1", BitrateKbps: 8000, MeasuredAt: fixedNow.Add(-1 * time.Hour)},
			},
			// Both are exactly 1000 away from 7000; "new" was measured later.
			rungID: "r1", bitrate: 7000, maxAge: CertStaleness,
			want: "new",
		},
		{
			name: "a row for a different StreamProfileID is never selected",
			certs: []EncoderCertRow{
				{ID: "wrong-rung", StreamProfileID: "r2", BitrateKbps: 7000, MeasuredAt: fixedNow},
			},
			rungID: "r1", bitrate: 7000, maxAge: CertStaleness,
			want: "",
		},
		{
			name: "a row older than maxAge relative to Now is excluded",
			certs: []EncoderCertRow{
				{ID: "stale", StreamProfileID: "r1", BitrateKbps: 7000, MeasuredAt: fixedNow.Add(-8 * 24 * time.Hour)},
			},
			rungID: "r1", bitrate: 7000, maxAge: CertStaleness, // 7 days
			want: "",
		},
		{
			name:   "empty input",
			rungID: "r1", bitrate: 7000, maxAge: CertStaleness,
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pickCert(c.certs, c.rungID, c.bitrate, fixedNow, c.maxAge)
			gotID := ""
			if got != nil {
				gotID = got.ID
			}
			if gotID != c.want {
				t.Errorf("pickCert() = %q, want %q", gotID, c.want)
			}
		})
	}
}

// --- StreamPlan.applyTo ---------------------------------------------------

// TestStreamPlanApplyTo copies stream fields onto a session and sets
// ProfileID/StreamProfileID to POINTERS at the plan's chain/rung. It applies
// the SAME plan value to two sessions across a loop, mutating only ChainID/
// RungID between iterations — the shape that would break a naive applyTo
// taking the address of the receiver's own field (&p.ChainID) instead of a
// fresh local: every session's ProfileID would then alias the SAME address,
// and both would read back whichever iteration ran last.
func TestStreamPlanApplyTo(t *testing.T) {
	cases := []struct {
		chainID, rungID, codec                string
		width, height, fps, bitrate, playout0 int32
	}{
		{"1080p60", "1080p60-h264", "h264", 1920, 1080, 60, 8000, 50},
		{"720p60", "720p60-h264", "h264", 1280, 720, 60, 4000, 75},
	}

	var plan StreamPlan
	sessions := make([]*Session, len(cases))
	for i, c := range cases {
		plan.ChainID, plan.RungID = c.chainID, c.rungID
		plan.Update = SessionStreamUpdate{
			Width: c.width, Height: c.height, FPS: c.fps, BitrateKbps: c.bitrate,
			Playout0Ms: c.playout0, Codec: c.codec,
			CodecDecision: json.RawMessage(`{"result":"` + c.codec + `"}`),
		}
		sess := &Session{}
		plan.applyTo(sess)
		sessions[i] = sess
	}

	for i, c := range cases {
		sess := sessions[i]
		if sess.Width != c.width || sess.Height != c.height || sess.FPS != c.fps ||
			sess.BitrateKbps != c.bitrate || sess.Playout0Ms != c.playout0 || sess.Codec != c.codec {
			t.Errorf("case %d: stream fields = %+v, want the %+v shape", i, sess, c)
		}
		if sess.ProfileID == nil || *sess.ProfileID != c.chainID {
			t.Errorf("case %d: sess.ProfileID = %v, want a pointer to %q", i, sess.ProfileID, c.chainID)
		}
		if sess.StreamProfileID == nil || *sess.StreamProfileID != c.rungID {
			t.Errorf("case %d: sess.StreamProfileID = %v, want a pointer to %q", i, sess.StreamProfileID, c.rungID)
		}
		wantDecision := `{"result":"` + c.codec + `"}`
		if string(sess.CodecDecision) != wantDecision {
			t.Errorf("case %d: sess.CodecDecision = %s, want %s", i, sess.CodecDecision, wantDecision)
		}
	}
	if sessions[0].ProfileID == sessions[1].ProfileID {
		t.Error("both sessions share the same ProfileID pointer — applyTo is aliasing instead of copying")
	}
	if sessions[0].StreamProfileID == sessions[1].StreamProfileID {
		t.Error("both sessions share the same StreamProfileID pointer — applyTo is aliasing instead of copying")
	}
}

// --- clamp 6: encoder throughput (#506) --------------------------------------

// issue506Rates is the measured RTX-5090 hint the node agent reports for a
// Vulkan-encoder host (issue #506's table). Named so the cases below read as
// "this host, that tier" rather than as three magic numbers.
var issue506Rates = map[string]float64{
	"h264": 1400,
	"h265": 395,
	"av1":  1215,
}

// codecChain is a chain holding one rung per codec at one tier, in the SHIP-DARK
// preference order an admin who enabled everything would get: h265, av1, h264. The
// h264 rung is last, which is where the floor rule wants it.
func codecChain(id string, w, h, fps int32) profile.LaunchProfile {
	return chain(id,
		rung(id+"-hevc", profile.CodecHEVC, w, h, fps, 20000),
		rung(id+"-av1", profile.CodecAV1, w, h, fps, 16000),
		rung(id+"-h264", profile.CodecH264, w, h, fps, 20000),
	)
}

// TestPlanStreamThroughputGateUsesTheLaunchEffectiveSize: an operator who sizes a
// 1440p120 chain down with an explicit stream override is launching 720p60 — a
// 55 Mpix/s demand, comfortably inside h265's 395. Judging the clamp on the rung's
// nominal 442 would substitute the codec for a session that was never going to run
// at that size, which is a silent downgrade of exactly the kind #506 is about.
//
// The size override does NOT bypass the clamp (unlike a codec override, which is a
// deliberate "force this codec"); it changes what the clamp measures.
func TestPlanStreamThroughputGateUsesTheLaunchEffectiveSize(t *testing.T) {
	in := throughputInputs(codecChain("1440p120", 2560, 1440, 120), issue506Rates)
	in.Override = StreamOverride{Width: i32(1280), Height: i32(720), FPS: i32(60)}
	plan, err := planStream(in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if plan.RungID != "1440p120-hevc" || plan.Update.Codec != "h265" {
		t.Fatalf("rung/codec = %q/%q, want 1440p120-hevc/h265 — the override shrank the "+
			"launch to 1280x720@60 (55 Mpix/s), which h265's 395 Mpix/s carries easily",
			plan.RungID, plan.Update.Codec)
	}
	if v := plan.Decision.Considered[0]; v.Reject != "" {
		t.Errorf("h265 rejected_by = %q, want \"\" (clamp 6 must judge the dispatched size)", v.Reject)
	}
	// And the inverse: an override that GROWS the launch past the hint must clamp,
	// so the fix is a real dependency on the override rather than a disabled clamp.
	in = throughputInputs(codecChain("1080p60", 1920, 1080, 60), issue506Rates)
	in.Override = StreamOverride{Width: i32(3840), Height: i32(2160), FPS: i32(60)}
	plan, err = planStream(in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if plan.Update.Codec != "av1" {
		t.Errorf("codec = %q, want av1 — an override to 2160p60 (498 Mpix/s) exceeds "+
			"h265's 395 even though the rung is nominally 1080p60", plan.Update.Codec)
	}
}

// throughputInputs builds the launch #506 describes: a Vulkan host advertising all
// three codecs, a client that can decode all three, and a chain at one tier.
func throughputInputs(ch profile.LaunchProfile, rates map[string]float64) StreamInputs {
	return StreamInputs{
		SessionID:   "sess-506",
		HostID:      strptr("host-1"),
		GPUIndex:    i32(0),
		H264Profile: "high",
		Codec:       "h264",
		Chain:       ch,
		HostCodecs:  []string{"h264", "h265", "av1"},
		HostEncoder: hostEncoderCaps{Known: true, HardwareEncoder: true, PixelRates: rates},
		// Proven decode for both gated codecs, and a 4K decode ceiling, so nothing
		// but clamp 6 can move these cases.
		Probe:      probeAt(true, true, 2160),
		Now:        fixedNow,
		CertMaxAge: CertStaleness,
	}
}

// TestPlanStreamThroughputGate is the #506 table. Each case names the tier, the
// pixel rate it demands, and which codec the host can actually sustain there.
func TestPlanStreamThroughputGate(t *testing.T) {
	cases := []struct {
		name string
		// Tier.
		id        string
		w, h, fps int32
		rates     map[string]float64
		// Expectations.
		wantRung   string
		wantCodec  string
		wantReject map[string]string // rung id → rejected_by ("" = passed)
	}{
		{
			// 2560*1440*120 = 442.4 Mpix/s. h265's 395 cannot carry it; av1's 1215
			// can, so the walk falls through exactly one rung. THIS IS THE ISSUE.
			name: "1440p120 rejects h265 and selects av1",
			id:   "1440p120",
			w:    2560, h: 1440, fps: 120, rates: issue506Rates,
			wantRung: "1440p120-av1", wantCodec: "av1",
			wantReject: map[string]string{"1440p120-hevc": rejectEncoderThroughput},
		},
		{
			// 3840*2160*60 = 497.7 Mpix/s — the issue's second cell, same verdict.
			name: "2160p60 rejects h265 and selects av1",
			id:   "2160p60",
			w:    3840, h: 2160, fps: 60, rates: issue506Rates,
			wantRung: "2160p60-av1", wantCodec: "av1",
			wantReject: map[string]string{"2160p60-hevc": rejectEncoderThroughput},
		},
		{
			// 1920*1080*60 = 124.4 Mpix/s, well inside h265's 395. The clamp must
			// not disturb the tier that was always fine — this is the regression
			// guard on every h265 session shipping today.
			name: "1080p60 keeps h265",
			id:   "1080p60",
			w:    1920, h: 1080, fps: 60, rates: issue506Rates,
			wantRung: "1080p60-hevc", wantCodec: "h265",
		},
		{
			// The same 1440p120 launch against a host that reported no hint at all
			// (a pre-amendment agent). Unknown gates nothing, so this is the
			// pre-#506 behaviour, unchanged.
			name: "1440p120 with no hint is unchanged",
			id:   "1440p120",
			w:    2560, h: 1440, fps: 120, rates: nil,
			wantRung: "1440p120-hevc", wantCodec: "h265",
		},
		{
			// A hint that covers h264 but says nothing about h265. Per-codec
			// absence is per-codec unknown, not "assume the h264 number".
			name: "a hint missing this codec does not clamp it",
			id:   "1440p120",
			w:    2560, h: 1440, fps: 120, rates: map[string]float64{"h264": 1400},
			wantRung: "1440p120-hevc", wantCodec: "h265",
		},
		{
			// Every codec short of the tier. Nothing survives, so the floor fires
			// and dispatches the LAST h264 rung bypassing every clamp — including
			// this one. The h264 floor rule is untouched by #506.
			name: "all codecs too slow falls to the h264 floor",
			id:   "2160p120",
			w:    3840, h: 2160, fps: 120,
			rates:    map[string]float64{"h264": 100, "h265": 100, "av1": 100},
			wantRung: "2160p120-h264", wantCodec: "h264",
			wantReject: map[string]string{
				"2160p120-hevc": rejectEncoderThroughput,
				"2160p120-av1":  rejectEncoderThroughput,
				// The floor KEEPS the reason that killed it — a floor-dispatched
				// rung must never read as an unqualified pass.
				"2160p120-h264": rejectEncoderThroughput,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planStream(throughputInputs(codecChain(tc.id, tc.w, tc.h, tc.fps), tc.rates))
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if plan.RungID != tc.wantRung {
				t.Errorf("RungID = %q, want %q", plan.RungID, tc.wantRung)
			}
			if plan.Update.Codec != tc.wantCodec {
				t.Errorf("Update.Codec = %q, want %q", plan.Update.Codec, tc.wantCodec)
			}
			for _, v := range plan.Decision.Considered {
				if got, want := v.Reject, tc.wantReject[v.ID]; got != want {
					t.Errorf("rung %s rejected_by = %q, want %q", v.ID, got, want)
				}
			}
		})
	}
}

// TestPlanStreamThroughputGateIsBypassedByAnExplicitOverride: clamp 0 honours
// only clamp 1. An operator forcing h265 at a tier the encoder cannot carry gets
// h265 — the whole point of the override is to reach a configuration the clamp
// chain would refuse, and the record says the clamps were skipped rather than
// survived.
func TestPlanStreamThroughputGateIsBypassedByAnExplicitOverride(t *testing.T) {
	in := throughputInputs(codecChain("1440p120", 2560, 1440, 120), issue506Rates)
	in.Override = StreamOverride{Codec: strptr("h265")}
	plan, err := planStream(in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if plan.RungID != "1440p120-hevc" || plan.Update.Codec != "h265" {
		t.Fatalf("rung/codec = %q/%q, want 1440p120-hevc/h265", plan.RungID, plan.Update.Codec)
	}
	v := plan.Decision.Considered[0]
	if !v.Selected || !v.Bypassed || v.Reject != "" {
		t.Errorf("override verdict = %+v, want selected+bypassed with no reject", v)
	}
}

// TestPlanStreamThroughputGateSurvivesSerialisation: the new reason has to reach
// the persisted `codec_decision.considered[].rejected_by`, or the operator
// drill-down cannot answer "why did I get av1 instead of h265?" — which is the
// only reason the clamp is worth recording separately from a log line.
func TestPlanStreamThroughputGateSurvivesSerialisation(t *testing.T) {
	plan, err := planStream(throughputInputs(codecChain("1440p120", 2560, 1440, 120), issue506Rates))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	var doc codecDecisionDoc
	if err := json.Unmarshal(plan.Update.CodecDecision, &doc); err != nil {
		t.Fatalf("unmarshal codec_decision: %v", err)
	}
	if doc.Considered[0].RejectedBy == nil || *doc.Considered[0].RejectedBy != "encoder_throughput" {
		t.Errorf("considered[0].rejected_by = %v, want \"encoder_throughput\"", doc.Considered[0].RejectedBy)
	}
	if doc.ResultCodec != "av1" {
		t.Errorf("result_codec = %q, want av1", doc.ResultCodec)
	}
}

// TestEncoderTooSlowAbstainsOnEveryUnknown pins the clamp's fail-open surface. It
// is a unit test rather than a planStream case because the point is the ENUMERATION
// — every shape of "we do not know" must abstain, and a reader should be able to
// see the whole list in one place.
func TestEncoderTooSlowAbstainsOnEveryUnknown(t *testing.T) {
	// 2560x1440@120 = 442.4 Mpix/s, against a host reporting 395 for h265.
	tier := rung("1440p120-hevc", profile.CodecHEVC, 2560, 1440, 120, 20000)
	slow := hostEncoderCaps{PixelRates: map[string]float64{"h265": 395}}
	if !encoderTooSlow("h265", tier, slow, StreamOverride{}) {
		t.Fatal("encoderTooSlow = false for 395 Mpix/s against a 442 Mpix/s rung, want true")
	}
	// A NON-POSITIVE override field is "unset", not "zero pixels" — pick() ignores
	// it — so the clamp still measures the rung. Pinned because the opposite
	// reading (0 fps ⇒ 0 demand ⇒ always fits) would silently disable clamp 6 for
	// any caller that sends an explicit zero.
	if !encoderTooSlow("h265", tier, slow, StreamOverride{FPS: i32(0)}) {
		t.Error("a zero FPS override disabled the clamp; pick() treats non-positive as unset, " +
			"so the rung's own 120 fps must still be measured")
	}

	cases := []struct {
		name string
		wire string
		rung profile.Profile
		host hostEncoderCaps
		ov   StreamOverride
	}{
		{name: "nil map", wire: "h265", rung: tier, host: hostEncoderCaps{}},
		{name: "codec absent from the map", wire: "h265", rung: tier,
			host: hostEncoderCaps{PixelRates: map[string]float64{"h264": 1400}}},
		{name: "zero rate", wire: "h265", rung: tier,
			host: hostEncoderCaps{PixelRates: map[string]float64{"h265": 0}}},
		{name: "negative rate", wire: "h265", rung: tier,
			host: hostEncoderCaps{PixelRates: map[string]float64{"h265": -1}}},
		{name: "rung with no fps", wire: "h265",
			rung: rung("no-fps", profile.CodecHEVC, 2560, 1440, 0, 20000), host: slow},
		{name: "rung with no resolution", wire: "h265",
			rung: rung("no-res", profile.CodecHEVC, 0, 0, 120, 20000), host: slow},
		{name: "rate exactly equal to the demand", wire: "h265", rung: tier,
			host: hostEncoderCaps{PixelRates: map[string]float64{"h265": 442.368}}},
		// The override is part of the abstain surface too: one that shrinks the
		// launch below the hint means there is nothing to clamp.
		{name: "override shrinks the launch under the hint", wire: "h265", rung: tier,
			host: slow, ov: StreamOverride{Width: i32(1280), Height: i32(720), FPS: i32(60)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if encoderTooSlow(tc.wire, tc.rung, tc.host, tc.ov) {
				t.Errorf("encoderTooSlow = true, want false (unknown/sufficient must never clamp)")
			}
		})
	}
}
