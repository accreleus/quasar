// Package profile defines the stream-profile / launch-profile model and the
// pure eligibility logic over it (AS10-01/AS10-02, restructured by UI-P4).
//
// A stream profile (Profile) is one encode rung — one codec, resolution, frame
// rate, bitrate, ABR floor, playout0, plus eligibility thresholds; never
// user-facing. A launch profile (launch.go) is an ordered list of rungs, best
// first — what a user picks, the default points at, and an app pins. Falling
// through a rung can change resolution as well as codec, and fall-through
// happens at launch only, never mid-session.
//
// There is no in-code catalog: profiles live in `stream_profiles` (seeded by
// migration 0015). What stays here is the types and eligibility.go — pure
// logic over supplied data. `conservativeDefaultID = "1080p60"` is an id
// constant resolving against launch profiles, not a catalog row.
package profile

// Codec identifies a video codec in a profile's preference list. Codec-agnostic:
// new codecs are added here without changing the Profile shape.
type Codec string

const (
	CodecH264 Codec = "h264"
	CodecHEVC Codec = "hevc"
	CodecAV1  Codec = "av1"
)

// CodecStatus describes how far along a codec is for a given profile.
//
//   - Launchable: a session can be launched with this codec today.
//   - Future:     planned; metadata only, not launchable yet.
//   - Unsupported: not applicable to this profile.
type CodecStatus string

const (
	CodecLaunchable  CodecStatus = "launchable"
	CodecFuture      CodecStatus = "future"
	CodecUnsupported CodecStatus = "unsupported"
)

// CodecPref is one entry in a profile's ordered codec-preference list. The JSON
// tags are the stable serialization used by the stream_profiles.codecs JSONB
// column and mirror the control-api codec object shape ({codec,status}).
type CodecPref struct {
	Codec  Codec       `json:"codec"`
	Status CodecStatus `json:"status"`
}

// Visibility controls whether a profile is offered to users or kept for
// debug/internal use only.
type Visibility string

const (
	// VisibilityUser: a user-facing game streaming profile.
	VisibilityUser Visibility = "user"
	// VisibilityDebug: available for diagnostics/acceptance harnesses, not the picker.
	VisibilityDebug Visibility = "debug"
	// VisibilityInternal: fallback/plumbing only, never surfaced.
	VisibilityInternal Visibility = "internal"
)

// BrowserSupport rates how well the browser (WebRTC) client is expected to run a
// profile. The native client (AS10-12) is expected to lift the "risky" rungs.
type BrowserSupport string

const (
	BrowserRecommended BrowserSupport = "recommended"
	BrowserSupported   BrowserSupport = "supported"
	BrowserRisky       BrowserSupport = "risky"
)

// DisplayReq expresses whether a high-refresh (>60 Hz) client display is needed
// to make a profile worthwhile.
type DisplayReq string

const (
	DisplayNone        DisplayReq = "none"
	DisplayRecommended DisplayReq = "recommended"
	DisplayRequired    DisplayReq = "required"
)

// Profile is one rung of the stream profile catalog. All bitrate/bandwidth
// fields are in kbps; all latency fields are in milliseconds.
type Profile struct {
	// ID is the canonical, stable identifier (e.g. "1080p60"). Used in APIs,
	// logs, and as the launch selector in AS10-03.
	ID string
	// DisplayName is the human-facing label for the picker (AS10-09).
	DisplayName string

	// Width, Height, FPS are the compositor/encoder output dimensions and rate.
	Width  int32
	Height int32
	FPS    int32

	// The rung's single codec (catalog vocabulary: h264|hevc|av1) — a rung IS
	// a codec; a codec is withdrawn by removing its rung. Empty only on a
	// legacy pre-0036 row, which nothing on the launch path reads.
	Codec Codec
	// Legacy ordered preference list (stream_profiles.codecs), retired by
	// UI-P4: never written by the admin path, never read on the launch path
	// after migration 0036 — Codec above is authoritative.
	Codecs []CodecPref
	// H264Profile is the preferred H.264 profile/constraint for this rung. The
	// launch path may negotiate a more constrained profile for browser receivers.
	H264Profile string

	// NominalBitrateKbps is the target CBR encoder bitrate for the profile.
	NominalBitrateKbps int32
	// MinOfferBandwidthKbps is the minimum measured downstream bandwidth a client
	// must have for the profile to be offered at all (AS10-02 eligibility).
	MinOfferBandwidthKbps int32
	// RecommendedOfferBandwidthKbps is the bandwidth at/above which the profile is
	// recommended (nominal × HeadroomFactor) — congestion control needs headroom
	// above the nominal bitrate to probe upward.
	RecommendedOfferBandwidthKbps int32
	// HeadroomFactor is the multiplier on NominalBitrateKbps used to derive the
	// recommended bandwidth. See AS-00 §Component 1 headroom rule (1.5×).
	HeadroomFactor float64
	// ABRFloorKbps is the lowest bitrate the in-session ABR governor (AS10-04)
	// will descend to within this profile. Resolution/FPS never change; only
	// bitrate adapts.
	ABRFloorKbps int32

	// MaxStartupRTTMs is the maximum measured RTT at which the profile is offered.
	// Zero means "no RTT constraint".
	MaxStartupRTTMs int32
	// MinDecodeHeight is the minimum decode resolution height the client must
	// support (AS10-02 eligibility). Equals Height for these profiles.
	MinDecodeHeight int32

	// HighRefreshDisplay states whether a >60 Hz client display is recommended or
	// required for the profile to be worthwhile.
	HighRefreshDisplay DisplayReq
	// HardwareEncoderRequired is true when the host must have a hardware encoder
	// (software encode cannot sustain the profile).
	HardwareEncoderRequired bool
	// BrowserClient rates browser (WebRTC) client suitability.
	BrowserClient BrowserSupport

	// Playout0Ms is the initial receiver jitter-buffer playout target applied at
	// session start (drives the AS-05 adaptive-playout controller).
	Playout0Ms int32

	// Visibility controls whether the profile is user-facing or debug/internal.
	Visibility Visibility
}

// DefaultCodecs returns the standard codec-preference list: H.264 launchable
// today, HEVC and AV1 as future placeholders. It is the single source of the
// ship-dark default, shared by the in-code catalog and the control plane's
// stream_profiles NULL-column fallback. Each call returns a fresh slice so
// callers cannot alias and mutate a shared backing array.
func DefaultCodecs() []CodecPref {
	return []CodecPref{
		{Codec: CodecH264, Status: CodecLaunchable},
		{Codec: CodecHEVC, Status: CodecFuture},
		{Codec: CodecAV1, Status: CodecFuture},
	}
}
