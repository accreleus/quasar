package session

// codec_decision_test.go — UI-P6 pure unit tests. No database.
//
// The resolver's own behaviour is covered by rung_test.go; what is tested HERE is
// the RECORD of that behaviour, because the record is what an operator reads. The
// three cases that must never blur into each other each get their own test:
//
//	a clamp rejection   — TestCodecDecisionRecordsEveryClampRejection
//	the terminal floor  — TestCodecDecisionFloorIsRecordedAsBypass
//	an operator override— TestCodecDecisionOverrideIsNotRecordedAsMerit
//
// The failure this guards against is a record that is technically populated but
// reads as a lie: a floor rung that looks like it passed the clamps, or an
// override that looks like it won the walk.

import (
	"encoding/json"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// decide runs the resolver and projects the decision, which is the pair the
// operator-facing behaviour actually depends on.
func decide(t *testing.T, rungs []profile.Profile, host []string, caps hostEncoderCaps,
	dp *DeviceProbe, failed map[string]bool, override string) codecDecisionDoc {
	t.Helper()
	_, dec, err := resolveRung(rungs, host, caps, dp, failed, codecOv(override))
	if err != nil {
		t.Fatalf("resolveRung: %v", err)
	}
	return newCodecDecisionDoc(dec)
}

// find returns the recorded verdict for a rung id.
func find(t *testing.T, doc codecDecisionDoc, rungID string) codecDecisionRung {
	t.Helper()
	for _, c := range doc.Considered {
		if c.RungID == rungID {
			return c
		}
	}
	t.Fatalf("rung %q missing from the record: %+v", rungID, doc.Considered)
	return codecDecisionRung{}
}

func rejectedBy(c codecDecisionRung) string {
	if c.RejectedBy == nil {
		return ""
	}
	return *c.RejectedBy
}

// TestCodecDecisionRecordsEveryClampRejection — one case per rejection reason.
//
// This is the acceptance criterion in its most literal form: a session that fell
// back must say WHICH clamp rejected the preferred rung. Before UI-P6 nothing
// recorded that at all, so "why did I get H.264 and not HEVC?" could only be
// re-derived by hand from the host codec set, the probe and the history.
func TestCodecDecisionRecordsEveryClampRejection(t *testing.T) {
	// AV1 1440p preferred, HEVC 1440p next, H.264 1080p floor.
	high := []profile.Profile{
		r("1440p60-av1", profile.CodecAV1, 1440),
		r("1440p60-hevc", profile.CodecHEVC, 1440),
		r("1080p60-h264", profile.CodecH264, 1080),
	}
	hwChain := []profile.Profile{
		hwRung("1440p60-av1", profile.CodecAV1, 1440),
		r("1080p60-h264", profile.CodecH264, 1080),
	}
	oddChain := []profile.Profile{
		r("weird", profile.Codec("vp9"), 1080),
		r("1080p60-h264", profile.CodecH264, 1080),
	}

	cases := []struct {
		name string
		doc  codecDecisionDoc
		// rejected is the rung expected to carry wantReason.
		rejected   string
		wantReason string
		wantResult string
	}{
		{
			name: "clamp 1 host_encoder",
			doc: decide(t, high, []string{"h264", "h265"}, hostEncoderCaps{},
				probeAt(true, true, 2160), nil, ""),
			rejected: "1440p60-av1", wantReason: rejectHostEncoder, wantResult: "1440p60-hevc",
		},
		{
			name: "clamp 2/3 client_decode",
			doc: decide(t, high, []string{"h264", "h265", "av1"}, hostEncoderCaps{},
				probeAt(true, false, 2160), nil, ""),
			rejected: "1440p60-av1", wantReason: rejectClientDecode, wantResult: "1440p60-hevc",
		},
		{
			name: "clamp 2/3 decode_height",
			doc: decide(t, high, []string{"h264", "h265", "av1"}, hostEncoderCaps{},
				probeAt(true, true, 1080), nil, ""),
			rejected: "1440p60-hevc", wantReason: rejectDecodeHeight, wantResult: "1080p60-h264",
		},
		{
			name: "clamp 4 decode_history",
			doc: decide(t, high, []string{"h264", "h265", "av1"}, hostEncoderCaps{},
				probeAt(true, true, 2160), map[string]bool{"1440p60-av1": true}, ""),
			rejected: "1440p60-av1", wantReason: rejectDecodeHistory, wantResult: "1440p60-hevc",
		},
		{
			name: "clamp 5 hardware_encoder",
			doc: decide(t, hwChain, []string{"h264", "av1"},
				hostEncoderCaps{Known: true, HardwareEncoder: false},
				probeAt(true, true, 2160), nil, ""),
			rejected: "1440p60-av1", wantReason: rejectHardwareEncoder, wantResult: "1080p60-h264",
		},
		{
			name: "unknown_codec (defensive, hand-edited data)",
			doc: decide(t, oddChain, []string{"h264"}, hostEncoderCaps{},
				probe(false, false), nil, ""),
			rejected: "weird", wantReason: rejectUnknownCodec, wantResult: "1080p60-h264",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := find(t, c.doc, c.rejected)
			if rejectedBy(got) != c.wantReason {
				t.Errorf("%s rejected_by = %q, want %q", c.rejected, rejectedBy(got), c.wantReason)
			}
			if got.Selected {
				t.Errorf("%s is marked selected, but it was rejected", c.rejected)
			}
			if c.doc.ResultRung != c.wantResult {
				t.Errorf("result_rung = %q, want %q", c.doc.ResultRung, c.wantResult)
			}
			// The rung that actually won did so ON MERIT: no reason, no bypass.
			sel := find(t, c.doc, c.wantResult)
			if !sel.Selected {
				t.Errorf("%s not marked selected", c.wantResult)
			}
			if rejectedBy(sel) != "" {
				t.Errorf("%s carries rejected_by %q; a rung that won the walk has none", c.wantResult, rejectedBy(sel))
			}
			if sel.ClampsBypassed {
				t.Errorf("%s marked clamps_bypassed; it survived every clamp", c.wantResult)
			}
			if c.doc.Floor {
				t.Error("floor = true, but a rung survived the walk")
			}
			if c.doc.Override != nil {
				t.Errorf("override = %v, want null", *c.doc.Override)
			}
		})
	}
}

// TestCodecDecisionFloorIsRecordedAsBypass — the honesty test.
//
// When NOTHING survives, the terminal h264 rung is dispatched bypassing every
// clamp, INCLUDING the ones that rejected it. The record must show that, not a
// rung that looks like it passed: `floor: true`, the selected rung marked
// `clamps_bypassed`, and — the load-bearing part — its `rejected_by` STILL SET.
// An operator reading "selected, rejected_by: decode_history" learns the real
// situation (we shipped a rung this device has already failed to decode); an
// operator reading "selected, rejected_by: null" would be misinformed.
func TestCodecDecisionFloorIsRecordedAsBypass(t *testing.T) {
	floorRung := hwRung("4k60-h264", profile.CodecH264, 2160) // hw-required AND 4K
	rungs := []profile.Profile{r("4k60-av1", profile.CodecAV1, 2160), floorRung}

	doc := decide(t, rungs,
		[]string{"h265"}, // clamp 1: the host cannot even encode h264
		hostEncoderCaps{Known: true, HardwareEncoder: false}, // clamp 5
		probeAt(false, false, 720),                           // clamp 2/3
		map[string]bool{"4k60-av1": true, "4k60-h264": true}, // clamp 4
		"")

	if !doc.Floor {
		t.Fatal("floor = false; nothing survived the walk, so the floor fired")
	}
	if doc.ResultRung != "4k60-h264" || doc.ResultCodec != wireCodecH264 {
		t.Fatalf("result = (%q,%q), want (4k60-h264,h264)", doc.ResultRung, doc.ResultCodec)
	}
	sel := find(t, doc, "4k60-h264")
	if !sel.Selected {
		t.Error("the floor rung is not marked selected")
	}
	if !sel.ClampsBypassed {
		t.Error("the floor rung is not marked clamps_bypassed; it survived nothing")
	}
	if rejectedBy(sel) == "" {
		t.Error("the floor rung lost its rejection reason — the record now reads as if it passed the clamps")
	}
	// Every OTHER rung stays a plain rejection.
	other := find(t, doc, "4k60-av1")
	if other.Selected || other.ClampsBypassed || rejectedBy(other) == "" {
		t.Errorf("non-selected rung recorded wrong: %+v", other)
	}
	// And the floor is NOT an override.
	if doc.Override != nil {
		t.Errorf("override = %q; the floor is not an override", *doc.Override)
	}
}

// TestCodecDecisionFloorKeepsTheClampThatRejectedIt pins the reason itself, not
// merely its presence: a floor rung rejected by the decode-height ceiling must
// say decode_height, so the operator knows the device could not have decoded what
// was shipped.
func TestCodecDecisionFloorKeepsTheClampThatRejectedIt(t *testing.T) {
	rungs := []profile.Profile{
		r("4k60-h264", profile.CodecH264, 2160),
		r("1080p60-h264", profile.CodecH264, 1080),
	}
	doc := decide(t, rungs, []string{"h264"}, hostEncoderCaps{}, probeAt(false, false, 480), nil, "")
	if !doc.Floor {
		t.Fatal("floor = false, want true")
	}
	sel := find(t, doc, "1080p60-h264")
	if rejectedBy(sel) != rejectDecodeHeight {
		t.Errorf("floor rung rejected_by = %q, want %q", rejectedBy(sel), rejectDecodeHeight)
	}
	if !sel.ClampsBypassed || !sel.Selected {
		t.Errorf("floor rung flags wrong: %+v", sel)
	}
}

// TestCodecDecisionOverrideIsNotRecordedAsMerit — an operator override must read
// as an override.
//
// Clamp 0 pre-empts the walk and skips clamps 2/3, 4 and 5, so the dispatched
// rung was never measured against them. Recording it with `rejected_by: null` and
// nothing else would be indistinguishable from a rung that beat the whole chain
// on merit — and here the rung had in fact already FAILED to decode on this
// device, which is precisely the fact an operator needs.
func TestCodecDecisionOverrideIsNotRecordedAsMerit(t *testing.T) {
	high := []profile.Profile{
		r("1440p60-av1", profile.CodecAV1, 1440),
		r("1440p60-hevc", profile.CodecHEVC, 1440),
		r("1080p60-h264", profile.CodecH264, 1080),
	}
	doc := decide(t, high, []string{"h264", "h265", "av1"}, hostEncoderCaps{},
		nil,                                   // no probe: clamp 2/3 would have rejected h265
		map[string]bool{"1440p60-hevc": true}, // clamp 4 would have rejected it too
		"h265")

	if doc.Override == nil || *doc.Override != "h265" {
		t.Fatalf("override = %v, want h265", doc.Override)
	}
	if doc.Floor {
		t.Error("floor = true; an override is not the floor")
	}
	if doc.ResultRung != "1440p60-hevc" || doc.ResultCodec != wireCodecH265 {
		t.Fatalf("result = (%q,%q), want (1440p60-hevc,h265)", doc.ResultRung, doc.ResultCodec)
	}
	sel := find(t, doc, "1440p60-hevc")
	if !sel.Selected {
		t.Error("overridden rung not marked selected")
	}
	if !sel.ClampsBypassed {
		t.Error("overridden rung not marked clamps_bypassed — it reads as if it won the walk")
	}
	if rejectedBy(sel) != "" {
		t.Errorf("overridden rung rejected_by = %q; clamp 0 evaluated no clamp on it", rejectedBy(sel))
	}
}

// TestCodecDecisionMeritSelectionHasNoBypass is the control case for the two
// above: when the top rung simply wins, nothing is flagged.
func TestCodecDecisionMeritSelectionHasNoBypass(t *testing.T) {
	rungs := []profile.Profile{
		r("1440p60-av1", profile.CodecAV1, 1440),
		r("1080p60-h264", profile.CodecH264, 1080),
	}
	doc := decide(t, rungs, []string{"h264", "av1"}, hostEncoderCaps{}, probeAt(true, true, 2160), nil, "")
	if doc.Floor || doc.Override != nil {
		t.Fatalf("floor/override set on a clean merit win: %+v", doc)
	}
	if len(doc.Considered) != 1 {
		t.Fatalf("considered = %d entries, want 1 (the walk stops at the first survivor)", len(doc.Considered))
	}
	sel := doc.Considered[0]
	if !sel.Selected || sel.ClampsBypassed || sel.RejectedBy != nil {
		t.Errorf("merit-selected rung recorded wrong: %+v", sel)
	}
}

// TestMarshalCodecDecisionShape pins the JSON an operator's UI actually parses:
// snake_case keys, an explicit null rejected_by rather than an omitted key, and
// `considered` as an array even when empty.
func TestMarshalCodecDecisionShape(t *testing.T) {
	rungs := []profile.Profile{
		r("1440p60-av1", profile.CodecAV1, 1440),
		r("1080p60-h264", profile.CodecH264, 1080),
	}
	_, dec, err := resolveRung(rungs, []string{"h264"}, hostEncoderCaps{}, nil, nil, StreamOverride{})
	if err != nil {
		t.Fatalf("resolveRung: %v", err)
	}
	raw := marshalCodecDecision(dec)
	if len(raw) == 0 {
		t.Fatal("marshalCodecDecision returned nothing")
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"result_rung", "result_codec", "override", "floor", "considered"} {
		if _, ok := m[k]; !ok {
			t.Errorf("key %q missing from %s", k, raw)
		}
	}
	if m["override"] != nil {
		t.Errorf("override = %v, want null", m["override"])
	}
	considered, ok := m["considered"].([]any)
	if !ok || len(considered) != 2 {
		t.Fatalf("considered = %v, want 2 entries", m["considered"])
	}
	first, _ := considered[0].(map[string]any)
	for _, k := range []string{"rung_id", "codec", "rejected_by", "selected", "clamps_bypassed"} {
		if _, ok := first[k]; !ok {
			t.Errorf("considered[0] key %q missing: %v", k, first)
		}
	}
	if first["rejected_by"] != rejectHostEncoder {
		t.Errorf("considered[0].rejected_by = %v, want %q", first["rejected_by"], rejectHostEncoder)
	}

	// An empty walk still serializes `considered: []`, never null — a consumer
	// must be able to iterate unconditionally.
	empty, err := json.Marshal(newCodecDecisionDoc(rungDecision{}))
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	var em map[string]any
	_ = json.Unmarshal(empty, &em)
	if arr, ok := em["considered"].([]any); !ok || len(arr) != 0 {
		t.Errorf("empty considered = %v, want []", em["considered"])
	}
}

// TestNormaliseNegotiatedCodec covers the server side of the browser/server
// comparison. It must agree with web/src/lib/codecDisplay.ts's normaliseCodec, or
// the admin drill-down and the in-session HUD would flag disagreement
// differently for the same session.
func TestNormaliseNegotiatedCodec(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		// The real browser shape.
		{"video/H264", "h264", true},
		{"video/H265", "h265", true},
		{"video/AV1", "av1", true},
		// Alternative spellings seen across implementations.
		{"H264", "h264", true},
		{"hevc", "h265", true},
		{"hvc1", "h265", true},
		{"avc1", "h264", true},
		{"av01", "av1", true},
		{" video/h264 ", "h264", true},
		// An unrecognised BUT well-formed codec is KEPT, not dropped: the server
		// never resolves vp9, so seeing it is the loudest possible disagreement
		// signal and discarding it would hide exactly the case worth surfacing.
		{"video/VP9", "vp9", true},
		{"video/VP8", "vp8", true},
		// Junk is refused — this is untrusted client input written to a row.
		{"", "", false},
		{"   ", "", false},
		{"video/", "", false},
		{"video/h264; profile-level-id=42e01f", "", false},
		{"video/<script>", "", false},
		{"video/aaaaaaaaaaaaaaaaaaaaa", "", false}, // over maxNegotiatedCodecLen
	}
	for _, c := range cases {
		got, ok := normaliseNegotiatedCodec(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("normaliseNegotiatedCodec(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestLatestNegotiatedCodec: a batch is scanned BACKWARDS for the newest usable
// value, not blindly read at its tail. getStats resolves the codec some way into
// a session, so a batch legitimately ends with samples that carry none, and
// reading only the last one would keep throwing away what the client did report.
func TestLatestNegotiatedCodec(t *testing.T) {
	t.Run("newest wins", func(t *testing.T) {
		got, ok := latestNegotiatedCodec([]statSample{
			{CodecMimeType: "video/H264"},
			{CodecMimeType: "video/H265"},
		})
		if !ok || got != "h265" {
			t.Errorf("got (%q,%v), want (h265,true)", got, ok)
		}
	})
	t.Run("trailing samples without a codec do not erase an earlier one", func(t *testing.T) {
		got, ok := latestNegotiatedCodec([]statSample{
			{CodecMimeType: "video/AV1"},
			{}, {},
		})
		if !ok || got != "av1" {
			t.Errorf("got (%q,%v), want (av1,true)", got, ok)
		}
	})
	t.Run("a junk value does not mask an earlier good one", func(t *testing.T) {
		got, ok := latestNegotiatedCodec([]statSample{
			{CodecMimeType: "video/H264"},
			{CodecMimeType: "video/<script>"},
		})
		if !ok || got != "h264" {
			t.Errorf("got (%q,%v), want (h264,true)", got, ok)
		}
	})
	t.Run("nothing to report", func(t *testing.T) {
		if _, ok := latestNegotiatedCodec(nil); ok {
			t.Error("empty batch reported a codec")
		}
		if _, ok := latestNegotiatedCodec([]statSample{{}, {}}); ok {
			t.Error("codec-less batch reported a codec")
		}
	})
}
