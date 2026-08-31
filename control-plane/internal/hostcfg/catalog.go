// Package hostcfg is the single source of truth for the per-host node-agent
// runtime knobs surfaced in /admin. Each Knob mirrors one QUASAR_* env var the
// agent reads; the catalog declares its type, default (= the agent's env
// default), validation, and whether changing it needs an agent restart.
package hostcfg

// Type is a knob's value type as seen by the API/UI.
type Type string

const (
	TypeBool   Type = "bool"
	TypeInt    Type = "int"
	TypeFloat  Type = "float"
	TypeEnum   Type = "enum"
	TypeString Type = "string"
)

// Class is whether a change applies live (next session) or needs an agent restart.
type Class string

const (
	ClassLive    Class = "live"
	ClassRestart Class = "restart"
)

// Knob is one catalog entry.
type Knob struct {
	Key      string   `json:"key"`
	Type     Type     `json:"type"`
	Default  any      `json:"default"` // nil for a nullable knob with no default
	Min      *float64 `json:"min,omitempty"`
	Max      *float64 `json:"max,omitempty"`
	Enum     []string `json:"enum,omitempty"`
	Nullable bool     `json:"nullable"`
	Class    Class    `json:"class"`
	EnvVar   string   `json:"env_var"`
	// AbsPathOrEmpty constrains a string knob's value to "" or an absolute path
	// (storage-config: home_root). Ignored for non-string knobs.
	AbsPathOrEmpty bool `json:"-"`
}

func f(v float64) *float64 { return &v }

// Catalog returns the v1 knob set. Order is the UI display order.
func Catalog() []Knob {
	return []Knob{
		{Key: "abr_enabled", Type: TypeBool, Default: true, Class: ClassLive, EnvVar: "QUASAR_ABR"}, // DEPRECATED by abr_mode (2026-08-16): false ⇒ off, true ⇒ defer to abr_mode. Kept so existing overrides and scripted PATCHes keep working.
		{Key: "abr_floor_kbps", Type: TypeInt, Default: nil, Min: f(1), Nullable: true, Class: ClassLive, EnvVar: "QUASAR_ABR_FLOOR_KBPS"},
		{Key: "abr_floor_ratio", Type: TypeFloat, Default: 0.3, Min: f(0), Max: f(1), Class: ClassLive, EnvVar: "QUASAR_ABR_FLOOR_RATIO"},
		{Key: "abr_mode", Type: TypeEnum, Default: "smooth", Enum: []string{"off", "protective", "smooth"}, Class: ClassLive, EnvVar: "QUASAR_ABR_MODE"},
		// SPT-04 governor hysteresis knobs. Ranges must mirror
		// node-agent/src/session/abr.rs `AbrGovernorSettings::apply_json`;
		// defaults mirror `AbrConfig::DEFAULT_*`.
		{Key: "abr_ewma_alpha", Type: TypeFloat, Default: 0.3, Min: f(0.000001), Max: f(1.0), Class: ClassLive, EnvVar: "QUASAR_ABR_EWMA_ALPHA"},
		{Key: "abr_deadband", Type: TypeFloat, Default: 0.15, Min: f(0.000001), Max: f(0.999999), Class: ClassLive, EnvVar: "QUASAR_ABR_DEADBAND"},
		{Key: "abr_max_up_step", Type: TypeFloat, Default: 0.25, Min: f(0.000001), Class: ClassLive, EnvVar: "QUASAR_ABR_MAX_UP_STEP"},
		{Key: "abr_min_interval_ms", Type: TypeInt, Default: float64(1000), Min: f(1), Class: ClassLive, EnvVar: "QUASAR_ABR_MIN_INTERVAL_MS"},
		{Key: "abr_max_down_step", Type: TypeFloat, Default: 0.125, Min: f(0.000001), Max: f(0.999999), Class: ClassLive, EnvVar: "QUASAR_ABR_MAX_DOWN_STEP"},
		{Key: "abr_down_dwell_ms", Type: TypeInt, Default: float64(7000), Min: f(0), Class: ClassLive, EnvVar: "QUASAR_ABR_DOWN_DWELL_MS"},
		{Key: "abr_cliff_guard_frac", Type: TypeFloat, Default: 0.50, Min: f(0.000001), Max: f(0.999999), Class: ClassLive, EnvVar: "QUASAR_ABR_CLIFF_GUARD_FRAC"},
		// #502: this is the ENCODER-SPEED-BIAS rung's switch, not a master switch for
		// the ladder. abr_ladder_resolution / abr_ladder_fps arm their rungs independently
		// (the agent expresses abr_ladder=false as max_bias=0); the only gate all three
		// share is abr_mode=smooth.
		{Key: "abr_ladder", Type: TypeBool, Default: true, Class: ClassLive, EnvVar: "QUASAR_ABR_LADDER"},
		{Key: "abr_ladder_max_bias", Type: TypeInt, Default: float64(2), Min: f(0), Max: f(255), Class: ClassLive, EnvVar: "QUASAR_ABR_LADDER_MAX_BIAS"},
		{Key: "abr_ladder_engage_dwell", Type: TypeInt, Default: float64(2), Min: f(1), Max: f(255), Class: ClassLive, EnvVar: "QUASAR_ABR_LADDER_ENGAGE_DWELL"},
		{Key: "abr_ladder_recover_dwell", Type: TypeInt, Default: float64(2), Min: f(1), Max: f(255), Class: ClassLive, EnvVar: "QUASAR_ABR_LADDER_RECOVER_DWELL"},
		{Key: "abr_ladder_resolution", Type: TypeBool, Default: false, Class: ClassLive, EnvVar: "QUASAR_ABR_LADDER_RESOLUTION"},
		{Key: "abr_ladder_res_exponent", Type: TypeFloat, Default: 0.75, Min: f(0.5), Max: f(1.0), Class: ClassLive, EnvVar: "QUASAR_ABR_LADDER_RES_EXPONENT"},
		{Key: "abr_ladder_res_engage_frac", Type: TypeFloat, Default: 0.6, Min: f(0.2), Max: f(0.95), Class: ClassLive, EnvVar: "QUASAR_ABR_LADDER_RES_ENGAGE_FRAC"},
		{Key: "abr_ladder_res_recover_frac", Type: TypeFloat, Default: 0.8, Min: f(0.3), Max: f(1.0), Class: ClassLive, EnvVar: "QUASAR_ABR_LADDER_RES_RECOVER_FRAC"},
		{Key: "abr_ladder_res_engage_dwell", Type: TypeInt, Default: float64(2), Min: f(1), Max: f(60), Class: ClassLive, EnvVar: "QUASAR_ABR_LADDER_RES_ENGAGE_DWELL"},
		// Dwell 2: -32% time-to-launch-rung (55.2s -> 37.6s), 0 oscillations
		// (docs/reports/2026-08-18-overnight-optimisation/t3-ladder-step-recovery.md).
		{Key: "abr_ladder_res_recover_dwell", Type: TypeInt, Default: float64(2), Min: f(1), Max: f(60), Class: ClassLive, EnvVar: "QUASAR_ABR_LADDER_RES_RECOVER_DWELL"},
		{Key: "abr_ladder_res_min_step_s", Type: TypeInt, Default: float64(10), Min: f(5), Max: f(120), Class: ClassLive, EnvVar: "QUASAR_ABR_LADDER_RES_MIN_STEP_S"},
		{Key: "abr_ladder_res_min_height", Type: TypeInt, Default: float64(720), Min: f(360), Max: f(2160), Class: ClassLive, EnvVar: "QUASAR_ABR_LADDER_RES_MIN_HEIGHT"},
		{Key: "abr_ladder_fps", Type: TypeBool, Default: false, Class: ClassLive, EnvVar: "QUASAR_ABR_LADDER_FPS"},
		{Key: "abr_ladder_floor_follows_rung", Type: TypeBool, Default: true, Class: ClassLive, EnvVar: "QUASAR_ABR_LADDER_FLOOR_FOLLOWS_RUNG"},
		{Key: "abr_ladder_order", Type: TypeEnum, Default: "hybrid", Enum: []string{"res_first", "fps_first", "hybrid"}, Class: ClassLive, EnvVar: "QUASAR_ABR_LADDER_ORDER"},
		{Key: "gop", Type: TypeInt, Default: float64(60), Min: f(1), Class: ClassLive, EnvVar: "QUASAR_GOP"},
		{Key: "slices", Type: TypeInt, Default: float64(8), Min: f(1), Class: ClassLive, EnvVar: "QUASAR_SLICES"},
		{Key: "target_usage", Type: TypeInt, Default: float64(6), Min: f(1), Max: f(7), Class: ClassLive, EnvVar: "QUASAR_TARGET_USAGE"},
		{Key: "queue_buffers", Type: TypeInt, Default: float64(3), Min: f(1), Class: ClassLive, EnvVar: "QUASAR_QUEUE_BUFFERS"},
		{Key: "zerocopy", Type: TypeBool, Default: false, Class: ClassLive, EnvVar: "QUASAR_ZEROCOPY"},
		// Host-stage latency probe (additive `probe_*` keys on session_metrics).
		// A measurement instrument, not a production setting; Live because probes
		// attach at pipeline build. Design:
		// docs/superpowers/specs/2026-08-18-latency-probe-design.md.
		{Key: "latency_probe", Type: TypeBool, Default: false, Class: ClassLive, EnvVar: "QUASAR_LATENCY_PROBE"},
		{Key: "idle_timeout_secs", Type: TypeInt, Default: float64(120), Min: f(0), Class: ClassLive, EnvVar: "QUASAR_IDLE_TIMEOUT_SECS"},
		{Key: "app_boot_timeout_secs", Type: TypeInt, Default: float64(300), Min: f(0), Class: ClassLive, EnvVar: "QUASAR_APP_BOOT_TIMEOUT_SECS"}, // #484: how long a launched app container may take to present its first frame before the session fails app_never_presented; 0 disables the watchdog
		{Key: "home_root", Type: TypeString, Default: "", AbsPathOrEmpty: true, Class: ClassLive, EnvVar: "QUASAR_HOME_ROOT"},                     // storage-config: absolute host dir holding managed homes (P5); empty disables local-driver measurement
		{Key: "nvidia_lib32_path", Type: TypeString, Default: "", AbsPathOrEmpty: true, Class: ClassLive, EnvVar: "QUASAR_NV_LIB32_PATH"},         // #375: absolute host dir holding 32-bit NVIDIA driver libs, mounted RO into NVIDIA app containers; empty = agent auto-detect at startup
		{Key: "encoder", Type: TypeEnum, Default: "openh264", Enum: []string{"openh264", "va", "nvenc", "vulkan"}, Class: ClassRestart, EnvVar: "QUASAR_ENCODER"},
		{Key: "render_node", Type: TypeString, Default: "software", Class: ClassRestart, EnvVar: "QUASAR_RENDER_NODE"},
		{Key: "cuda_device", Type: TypeInt, Default: float64(0), Min: f(0), Class: ClassRestart, EnvVar: "QUASAR_CUDA_DEVICE"},
	}
}

// byKey indexes the catalog by key name.
func byKey() map[string]Knob {
	m := make(map[string]Knob, 30)
	for _, k := range Catalog() {
		m[k.Key] = k
	}
	return m
}

// Defaults returns the non-null default value for every knob (nullable-without-default
// knobs are omitted, matching how the agent treats an unset env var).
func Defaults() map[string]any {
	out := map[string]any{}
	for _, k := range Catalog() {
		if k.Default != nil {
			out[k.Key] = k.Default
		}
	}
	return out
}
