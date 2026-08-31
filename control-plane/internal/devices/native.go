package devices

// AS10-12 — optional typed view of the native capability report
// (protocol/native-client.md): an additive superset of the web probe, same
// endpoint, same user_devices.capabilities column, no migration.
//
// Storage stays opaque: the store persists json.RawMessage verbatim and never
// routes through this struct. Unmarshalling into it drops unknown fields, so
// it is read-side only (tests, future mapping). Every native-only field is a
// pointer or omitempty so the web subset round-trips identically.

// NativeCapabilities is the optional typed view of a native capability report.
// Mirrors the TS NativeCapabilities (web/src/webrtc/capability.ts) and the schema
// in protocol/native-client.md.
type NativeCapabilities struct {
	// --- identity ---
	ReportVersion *int   `json:"report_version,omitempty"`
	ClientType    string `json:"client_type,omitempty"`
	ClientName    string `json:"client_name,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`

	// --- platform / os ---
	Platform string        `json:"platform,omitempty"`
	OS       *NativeOSInfo `json:"os,omitempty"`

	// --- display ---
	Display *NativeDisplayInfo `json:"display,omitempty"`

	// --- decode ---
	// Codecs is the flat eligibility surface (shared with the web probe). The rich
	// Decode matrix is forward-data only (NOT an eligibility gate).
	Codecs          map[string]bool     `json:"codecs,omitempty"`
	MaxDecodeHeight *int                `json:"max_decode_height,omitempty"`
	Decode          *NativeDecodeMatrix `json:"decode,omitempty"`

	// --- audio / input ---
	Audio *NativeAudioInfo `json:"audio,omitempty"`
	Input *NativeInputInfo `json:"input,omitempty"`

	// --- metrics / health ---
	Metrics *NativeMetrics `json:"metrics,omitempty"`
	Health  *NativeHealth  `json:"health,omitempty"`

	// --- per-profile certification map (same shape as the web probe) ---
	Profiles map[string]NativeProfileCert `json:"profiles,omitempty"`

	// --- network ---
	BandwidthKbps *float64 `json:"bandwidth_kbps,omitempty"`
	RTTMs         *float64 `json:"rtt_ms,omitempty"`

	// MeasuredAt is server-stamped at upsert (any client value is overwritten).
	MeasuredAt string `json:"measured_at,omitempty"`
}

// NativeOSInfo is the os{} sub-object.
type NativeOSInfo struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	Arch    string `json:"arch,omitempty"`
}

// NativeDisplayInfo is display{}: web geometry plus native HDR/VRR flags.
type NativeDisplayInfo struct {
	Width            *int     `json:"width,omitempty"`
	Height           *int     `json:"height,omitempty"`
	DevicePixelRatio *float64 `json:"device_pixel_ratio,omitempty"`
	RefreshHz        *float64 `json:"refresh_hz,omitempty"`
	HDR              *bool    `json:"hdr,omitempty"`
	VRR              *bool    `json:"vrr,omitempty"`
}

// CodecDecodeInfo is one decode.<codec> entry. Forward-data only.
type CodecDecodeInfo struct {
	HW        bool     `json:"hw"`
	Profiles  []string `json:"profiles,omitempty"`
	Levels    []string `json:"levels,omitempty"`
	MaxHeight *int     `json:"max_height,omitempty"`
}

// NativeDecodeMatrix is the rich per-codec decode object. HEVC/AV1 are placeholders.
type NativeDecodeMatrix struct {
	H264 *CodecDecodeInfo `json:"h264,omitempty"`
	HEVC *CodecDecodeInfo `json:"hevc,omitempty"`
	AV1  *CodecDecodeInfo `json:"av1,omitempty"`
}

// NativeAudioInfo is audio{}.
type NativeAudioInfo struct {
	Channels   *int     `json:"channels,omitempty"`
	SampleRate *int     `json:"sample_rate,omitempty"`
	Codecs     []string `json:"codecs,omitempty"`
}

// NativeController is one input.controllers[] entry.
type NativeController struct {
	Type    string `json:"type,omitempty"`
	Rumble  bool   `json:"rumble"`
	Haptics bool   `json:"haptics"`
	Player  int    `json:"player"`
}

// NativeInputInfo is input{}.
type NativeInputInfo struct {
	RawMouse      bool               `json:"raw_mouse"`
	Keyboard      bool               `json:"keyboard"`
	HighRateInput bool               `json:"high_rate_input"`
	Controllers   []NativeController `json:"controllers,omitempty"`
}

// NativeMetrics is metrics{}. Reuses the BrowserMetrics vocabulary. Forward-data.
type NativeMetrics struct {
	DecodeMs            *float64 `json:"decode_ms,omitempty"`
	PresentFPS          *float64 `json:"present_fps,omitempty"`
	PresentIntervalSDMs *float64 `json:"present_interval_sd_ms,omitempty"`
	GlassToGlassP50Ms   *float64 `json:"glass_to_glass_ms_p50,omitempty"`
	GlassToGlassP95Ms   *float64 `json:"glass_to_glass_ms_p95,omitempty"`
	InteractiveP50Ms    *float64 `json:"interactive_ms_p50,omitempty"`
	JitterBufferMs      *float64 `json:"jitter_buffer_ms,omitempty"`
	RenderPath          string   `json:"render_path,omitempty"`
}

// NativeHealth is health{} (reuses the AS10-11 ClientHealth vocabulary).
type NativeHealth struct {
	Class  string `json:"class,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// NativeProfileCert is one profiles.<id> certification entry (same shape as the
// web ProfileCertification). A native client can certify the higher H.264 profiles
// (main/high) the browser cannot decode.
type NativeProfileCert struct {
	H264ProfileDecoded string   `json:"h264_profile_decoded,omitempty"`
	DecodePass         *bool    `json:"decode_pass,omitempty"`
	PresentPass        *bool    `json:"present_pass,omitempty"`
	DecodeMs           *float64 `json:"decode_ms,omitempty"`
	PresentFPS         *float64 `json:"present_fps,omitempty"`
	DroppedRatio       *float64 `json:"dropped_ratio,omitempty"`
	MeasuredAt         string   `json:"measured_at,omitempty"`
}
