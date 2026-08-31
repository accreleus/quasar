// coordinator_types.go — shared parameter/result types for the Coordinator lifecycle.
package session

import "time"

// StreamOverride carries optional per-launch stream parameters; nil fields fall
// back to the app's defaults. The handler validates H264Profile against the
// schema.md legal set; nil is the constrained-baseline floor.
type StreamOverride struct {
	Width       *int32
	Height      *int32
	FPS         *int32
	BitrateKbps *int32
	H264Profile *string
	// Codec overrides the resolved session codec (wire vocabulary), validated by
	// ValidCodec in the handler. It must stay OUT of any(), so a codec-only
	// override neither bypasses the eligibility gate nor perturbs the
	// h264_profile / envelope / cert-cap logic (§5.1).
	Codec *string
}

// any reports whether the caller supplied an explicit stream parameter. Such an
// override beats a selected profile field-by-field and bypasses the eligibility
// gate: the operator is forcing concrete values. Codec is excluded — see above.
func (o StreamOverride) any() bool {
	return o.Width != nil || o.Height != nil || o.FPS != nil ||
		o.BitrateKbps != nil || (o.H264Profile != nil && *o.H264Profile != "")
}

// codecOverride returns the explicit codec override, or "". The handler has
// already validated a non-empty value against ValidCodec.
func (o StreamOverride) codecOverride() string {
	if o.Codec != nil {
		return *o.Codec
	}
	return ""
}

// LaunchParams carries everything a launch needs beyond the user id. Either
// ProfileID is set (resolve from the catalog, gate eligibility unless IsAdmin or
// an Override) or empty (tier selection capped by app defaults). In both, an
// explicit Override beats the resolved base field-by-field.
type LaunchParams struct {
	AppID     string
	ProfileID string         // "" ⇒ legacy/tier path
	Override  StreamOverride // optional explicit stream params
	IsAdmin   bool           // bypasses the eligibility gate
	// ClientType is the launching client's own declaration ("native" ⇒ can decode
	// main/high H.264). The profile lift keys on THIS, not the stored probe's
	// client_type: a native probe becomes the account's latest, so keying on it
	// would let a native session poison a later browser launch.
	ClientType string
	// Mic is the launch REQUEST, not the granted state: the launcher ANDs it with
	// the instance setting to resolve what is dispatched and persisted
	// (Session.Mic). A request against a disabled instance proceeds with no mic.
	Mic bool
}

// isNativeClient is lenient: only the exact string "native" counts, anything
// else falls back to browser and the constrained-baseline floor.
func (lp LaunchParams) isNativeClient() bool { return lp.ClientType == "native" }

// legalH264Profiles is the schema.md `h264_profile` CHECK set
// (`CHECK (h264_profile IN ('constrained-baseline','main','high'))`).
var legalH264Profiles = map[string]bool{
	"constrained-baseline": true,
	"main":                 true,
	"high":                 true,
}

func ValidH264Profile(p string) bool { return legalH264Profiles[p] }

// validCodecs is the schema.md sessions.codec CHECK set: the WIRE vocabulary
// (h265), not the catalog's hevc. Gates the `stream.codec` launch override.
var validCodecs = map[string]bool{
	wireCodecH264: true,
	wireCodecH265: true,
	wireCodecAV1:  true,
}

func ValidCodec(c string) bool { return validCodecs[c] }

// LaunchResult is what a successful launch returns to the API layer.
type LaunchResult struct {
	Session        Session
	SignalingToken string
	TokenExpiresAt time.Time
}
