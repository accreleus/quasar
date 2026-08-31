// codec_decision.go — the durable form of a rungDecision: the JSON projection
// persisted to sessions.codec_decision and echoed on the session object as
// `codec_decision` (control-api.md). It also normalises the browser-reported
// negotiated codec, so the drill-down can flag disagreement with the
// server-resolved one.
//
// The three outcomes must stay distinguishable:
//
//	a clamp rejection — `considered[i].rejected_by` names the clamp; the selected
//	                    rung has `rejected_by: null`, `clamps_bypassed: false`.
//	the floor         — `floor: true`, and the selected rung STILL carries the
//	                    `rejected_by` that killed it, with `clamps_bypassed: true`.
//	an override       — `override` names the forced codec; the selected rung has
//	                    `clamps_bypassed: true`, `rejected_by: null`.
//
// Never collapse `floor` and `clamps_bypassed` into one flag: they answer "did
// anything survive?" and "was this rung measured?", and an override sets the
// second without the first.
package session

import (
	"encoding/json"
	"strings"
)

// codecDecisionRung is one rung's verdict in the persisted decision
// (control-api.md `codec_decision.considered[]`).
type codecDecisionRung struct {
	RungID string `json:"rung_id"`
	// Wire vocabulary, except for a rung whose catalog codec does not map
	// (rejected_by = unknown_codec), where it is the raw catalog value.
	Codec string `json:"codec"`
	// RejectedBy is the clamp that rejected this rung, null when it passed. A
	// floor-selected rung keeps its reason.
	RejectedBy *string `json:"rejected_by"`
	// Selected marks the dispatched rung: exactly one entry, except on the error
	// paths where nothing is dispatched or persisted.
	Selected bool `json:"selected"`
	// ClampsBypassed marks a rung dispatched unmeasured (override or floor).
	ClampsBypassed bool `json:"clamps_bypassed"`
}

// codecDecisionDoc is the persisted/exposed shape of a rungDecision.
type codecDecisionDoc struct {
	ResultRung  string `json:"result_rung"`
	ResultCodec string `json:"result_codec"`
	// Override is the stream.codec that pre-empted the walk, null on the normal path.
	Override *string `json:"override"`
	// Floor is true when no rung survived and the unconditional h264 floor fired.
	Floor      bool                `json:"floor"`
	Considered []codecDecisionRung `json:"considered"`
}

// newCodecDecisionDoc projects a resolver decision into its wire/storage shape.
func newCodecDecisionDoc(dec rungDecision) codecDecisionDoc {
	doc := codecDecisionDoc{
		ResultRung:  dec.ResultRung,
		ResultCodec: dec.Result,
		Floor:       dec.Floor,
		// Never nil: an empty walk must serialize as [], not null, so a consumer
		// can iterate unconditionally.
		Considered: make([]codecDecisionRung, 0, len(dec.Considered)),
	}
	if dec.Override != "" {
		ov := dec.Override
		doc.Override = &ov
	}
	for _, v := range dec.Considered {
		entry := codecDecisionRung{
			RungID:         v.ID,
			Codec:          v.Codec,
			Selected:       v.Selected,
			ClampsBypassed: v.Bypassed,
		}
		if v.Reject != "" {
			reason := v.Reject
			entry.RejectedBy = &reason
		}
		doc.Considered = append(doc.Considered, entry)
	}
	return doc
}

// marshalCodecDecision renders a decision for the sessions.codec_decision column.
// A marshal failure yields nil (the column stays NULL) rather than an error: the
// decision is diagnostics, and losing it must never fail a launch that has
// already resolved.
func marshalCodecDecision(dec rungDecision) json.RawMessage {
	b, err := json.Marshal(newCodecDecisionDoc(dec))
	if err != nil {
		return nil
	}
	return b
}

// maxNegotiatedCodecLen bounds what an untrusted browser can write into the
// session row. Every real value is <= 5 characters.
const maxNegotiatedCodecLen = 16

// normaliseNegotiatedCodec maps a browser getStats() mimeType ("video/H264")
// onto the wire vocabulary, so it is comparable with the server-resolved codec.
// It mirrors web/src/lib/codecDisplay.ts normaliseCodec for well-formed values
// and additionally bounds and rejects junk, because this output is written to a
// session row rather than only displayed — not a mirroring gap.
//
// An unrecognised but well-formed tail (vp8, vp9) is kept: storing this exists
// to surface disagreement, and dropping "the browser decoded something we have
// no name for" hides the loudest case. ok=false only for junk — empty, too long,
// or anything outside [a-z0-9._-]. Untrusted input, bounded here at ingest.
func normaliseNegotiatedCodec(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "h264", "avc", "avc1":
		return wireCodecH264, true
	case "h265", "hevc", "hvc1":
		return wireCodecH265, true
	case "av1", "av01":
		return wireCodecAV1, true
	}
	if s == "" || len(s) > maxNegotiatedCodecLen {
		return "", false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return "", false
		}
	}
	return s, true
}
