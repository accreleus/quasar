package hostcfg

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidateRejectsUnknownKey(t *testing.T) {
	if err := Validate(map[string]any{"nope": 1}); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestValidateRejectsOutOfRange(t *testing.T) {
	if err := Validate(map[string]any{"target_usage": float64(9)}); err == nil {
		t.Fatal("expected error: target_usage max is 7")
	}
	if err := Validate(map[string]any{"gop": float64(0)}); err == nil {
		t.Fatal("expected error: gop min is 1")
	}
}

func TestValidateRejectsWrongTypeAndEnum(t *testing.T) {
	if err := Validate(map[string]any{"abr_enabled": "yes"}); err == nil {
		t.Fatal("expected error: abr_enabled is bool")
	}
	if err := Validate(map[string]any{"encoder": "x265"}); err == nil {
		t.Fatal("expected error: encoder not in enum")
	}
}

func TestValidateAllowsNullForNullable(t *testing.T) {
	if err := Validate(map[string]any{"abr_floor_kbps": nil}); err != nil {
		t.Fatalf("null clear of nullable knob should be valid: %v", err)
	}
	// Validate (not ValidatePatch) still rejects null for non-nullable knobs.
	if err := Validate(map[string]any{"gop": nil}); err == nil {
		t.Fatal("null for a non-nullable knob must be rejected by Validate")
	}
}

// TestValidatePatch covers the PATCH-specific validation semantics: null always
// means "clear" (even for non-nullable knobs), but unknown keys are still rejected,
// and non-null values are still type/range/enum-checked.
func TestValidatePatch(t *testing.T) {
	// null-clear of a non-nullable enum knob → OK (was 400 before the fix)
	if err := ValidatePatch(map[string]any{"encoder": nil}); err != nil {
		t.Fatalf("null-clear of encoder (non-nullable) must be allowed: %v", err)
	}
	// null-clear of a non-nullable int knob → OK
	if err := ValidatePatch(map[string]any{"gop": nil}); err != nil {
		t.Fatalf("null-clear of gop (non-nullable) must be allowed: %v", err)
	}
	// null-clear of a nullable knob → still OK
	if err := ValidatePatch(map[string]any{"abr_floor_kbps": nil}); err != nil {
		t.Fatalf("null-clear of nullable knob must be allowed: %v", err)
	}
	// null for an unknown key → error (can't clear a key that isn't in the catalog)
	if err := ValidatePatch(map[string]any{"bogus": nil}); err == nil {
		t.Fatal("null for unknown key must be rejected")
	}
	// non-null value still range-checked
	if err := ValidatePatch(map[string]any{"target_usage": float64(9)}); err == nil {
		t.Fatal("out-of-range target_usage must be rejected")
	}
	// non-null value valid enum → OK
	if err := ValidatePatch(map[string]any{"encoder": "va"}); err != nil {
		t.Fatalf("valid encoder value must be accepted: %v", err)
	}
}

// TestValidateHomeRoot covers the storage-config home_root knob: a string knob that
// must be empty or an absolute path.
func TestValidateHomeRoot(t *testing.T) {
	if err := Validate(map[string]any{"home_root": "/data/quasar/homes"}); err != nil {
		t.Fatalf("absolute home_root must be valid: %v", err)
	}
	if err := Validate(map[string]any{"home_root": ""}); err != nil {
		t.Fatalf("empty home_root must be valid (disables local measurement): %v", err)
	}
	if err := Validate(map[string]any{"home_root": "relative/path"}); err == nil {
		t.Fatal("relative home_root must be rejected")
	}
	if err := Validate(map[string]any{"home_root": 42}); err == nil {
		t.Fatal("non-string home_root must be rejected")
	}
	// home_root is live-class (applies on next launch, no restart).
	if RestartChange(map[string]any{}, map[string]any{"home_root": "/data/homes"}) {
		t.Error("home_root change must NOT be restart-class")
	}
}

// TestValidateNvidiaLib32Path covers the #375 nvidia_lib32_path knob: a string
// knob that must be empty or an absolute path, applied live (no restart).
func TestValidateNvidiaLib32Path(t *testing.T) {
	if err := Validate(map[string]any{"nvidia_lib32_path": "/usr/lib"}); err != nil {
		t.Fatalf("absolute nvidia_lib32_path must be valid: %v", err)
	}
	if err := Validate(map[string]any{"nvidia_lib32_path": ""}); err != nil {
		t.Fatalf("empty nvidia_lib32_path must be valid (agent auto-detects): %v", err)
	}
	if err := Validate(map[string]any{"nvidia_lib32_path": "usr/lib"}); err == nil {
		t.Fatal("relative nvidia_lib32_path must be rejected")
	}
	if err := Validate(map[string]any{"nvidia_lib32_path": 42}); err == nil {
		t.Fatal("non-string nvidia_lib32_path must be rejected")
	}
	// nvidia_lib32_path is live-class (applies on next launch, no restart).
	if RestartChange(map[string]any{}, map[string]any{"nvidia_lib32_path": "/usr/lib"}) {
		t.Error("nvidia_lib32_path change must NOT be restart-class")
	}
}

// TestValidateHomeRootUnder covers the storage-root-constrained write-path
// check: home_root must be the host's agent-reported root or a subpath of it.
func TestValidateHomeRootUnder(t *testing.T) {
	const root = "/var/lib/quasar/homes"

	if err := ValidateHomeRootUnder(root, root); err != nil {
		t.Errorf("the exact reported root must be accepted: %v", err)
	}
	if err := ValidateHomeRootUnder(root+"/alice", root); err != nil {
		t.Errorf("a subpath of the reported root must be accepted: %v", err)
	}
	if err := ValidateHomeRootUnder(root+"/alice/saves", root); err != nil {
		t.Errorf("a nested subpath must be accepted: %v", err)
	}
	// the classic prefix bug: "<root>-evil" starts with "<root>" as a raw string
	// but is a SIBLING directory, not a subpath.
	if err := ValidateHomeRootUnder(root+"-evil", root); err == nil {
		t.Error("a sibling-prefix path like <root>-evil must be rejected")
	}
	if err := ValidateHomeRootUnder("/data/somewhere/else", root); err == nil {
		t.Error("an unrelated absolute path must be rejected")
	}
	// the agent has reported no root at all: nothing to be a subpath of, so any
	// non-empty candidate is refused.
	if err := ValidateHomeRootUnder("/anything", ""); err == nil {
		t.Error("a non-empty candidate must be rejected when the agent reported no root")
	}
	// clearing the override is always allowed, even with no reported root.
	if err := ValidateHomeRootUnder("", ""); err != nil {
		t.Errorf("clearing (empty candidate) must always be allowed: %v", err)
	}
	if err := ValidateHomeRootUnder("", root); err != nil {
		t.Errorf("clearing (empty candidate) must always be allowed: %v", err)
	}
	// trailing-slash / unclean input still resolves correctly.
	if err := ValidateHomeRootUnder(root+"/", root); err != nil {
		t.Errorf("a trailing slash on the exact root must still be accepted: %v", err)
	}
}

func TestResolveMergesOverrides(t *testing.T) {
	r := Resolve(map[string]any{"gop": float64(120), "abr_enabled": true})
	if r["gop"] != float64(120) {
		t.Errorf("gop = %v, want 120", r["gop"])
	}
	if r["abr_enabled"] != true {
		t.Errorf("abr_enabled = %v, want true", r["abr_enabled"])
	}
	if r["target_usage"] != float64(6) {
		t.Errorf("target_usage = %v, want default 6", r["target_usage"])
	}
}

func TestRestartChange(t *testing.T) {
	if !RestartChange(map[string]any{}, map[string]any{"encoder": "va"}) {
		t.Error("encoder change must be restart")
	}
	if RestartChange(map[string]any{}, map[string]any{"gop": float64(90)}) {
		t.Error("gop change must NOT be restart")
	}
	if RestartChange(map[string]any{"encoder": "va"}, map[string]any{"encoder": "va"}) {
		t.Error("unchanged encoder must NOT be restart")
	}
}

// TestAgentOverridesIsSparse pins the #194 fix: the agent receives ONLY the
// host's explicit overrides (never the catalog defaults), so an un-overridden
// knob keeps the agent's env value instead of being clobbered by the default.
func TestAgentOverridesIsSparse(t *testing.T) {
	// No overrides → empty map → agent keeps its full env baseline.
	if got := AgentOverrides(map[string]any{}); len(got) != 0 {
		t.Errorf("no overrides must yield empty map (agent keeps env), got %v", got)
	}
	// nil/cleared keys are stripped; set keys pass through.
	got := AgentOverrides(map[string]any{"encoder": "nvenc", "abr_floor_kbps": nil})
	if len(got) != 1 || got["encoder"] != "nvenc" {
		t.Errorf("AgentOverrides = %v, want {encoder:nvenc} (nil stripped)", got)
	}
	// The #194 bug: a host with only an unrelated override must NOT receive a
	// catalog-default encoder (which would override the agent's QUASAR_ENCODER=nvenc).
	if _, present := AgentOverrides(map[string]any{"gop": float64(90)})["encoder"]; present {
		t.Error("AgentOverrides must not include encoder when it is not overridden (the #194 regression)")
	}
	// Resolve (the display view) DOES include the default — confirm they differ.
	if _, present := Resolve(map[string]any{"gop": float64(90)})["encoder"]; !present {
		t.Error("Resolve (display) should still carry the default encoder")
	}
}

func TestCatalogHasEveryLadderKnob(t *testing.T) {
	want := map[string]struct {
		typ   Type
		def   any
		class Class
		env   string
	}{
		"abr_mode":                     {TypeEnum, "smooth", ClassLive, "QUASAR_ABR_MODE"},
		"abr_ladder":                   {TypeBool, true, ClassLive, "QUASAR_ABR_LADDER"},
		"abr_ladder_max_bias":          {TypeInt, float64(2), ClassLive, "QUASAR_ABR_LADDER_MAX_BIAS"},
		"abr_ladder_engage_dwell":      {TypeInt, float64(2), ClassLive, "QUASAR_ABR_LADDER_ENGAGE_DWELL"},
		"abr_ladder_recover_dwell":     {TypeInt, float64(2), ClassLive, "QUASAR_ABR_LADDER_RECOVER_DWELL"},
		"abr_ladder_resolution":        {TypeBool, false, ClassLive, "QUASAR_ABR_LADDER_RESOLUTION"},
		"abr_ladder_res_exponent":      {TypeFloat, 0.75, ClassLive, "QUASAR_ABR_LADDER_RES_EXPONENT"},
		"abr_ladder_res_engage_frac":   {TypeFloat, 0.6, ClassLive, "QUASAR_ABR_LADDER_RES_ENGAGE_FRAC"},
		"abr_ladder_res_recover_frac":  {TypeFloat, 0.8, ClassLive, "QUASAR_ABR_LADDER_RES_RECOVER_FRAC"},
		"abr_ladder_res_engage_dwell":  {TypeInt, float64(2), ClassLive, "QUASAR_ABR_LADDER_RES_ENGAGE_DWELL"},
		"abr_ladder_res_recover_dwell": {TypeInt, float64(2), ClassLive, "QUASAR_ABR_LADDER_RES_RECOVER_DWELL"},
		"abr_ladder_res_min_step_s":    {TypeInt, float64(10), ClassLive, "QUASAR_ABR_LADDER_RES_MIN_STEP_S"},
		"abr_ladder_res_min_height":    {TypeInt, float64(720), ClassLive, "QUASAR_ABR_LADDER_RES_MIN_HEIGHT"},
		"abr_ladder_fps":               {TypeBool, false, ClassLive, "QUASAR_ABR_LADDER_FPS"},
		// Amendment 5: default TRUE. It is inert unless a rung is actually engaged, and
		// both rungs ship dark — so this is dark too, and turning a rung on gets the
		// behaviour that makes the rung worth anything.
		"abr_ladder_floor_follows_rung": {TypeBool, true, ClassLive, "QUASAR_ABR_LADDER_FLOOR_FOLLOWS_RUNG"},
		"abr_ladder_order":              {TypeEnum, "hybrid", ClassLive, "QUASAR_ABR_LADDER_ORDER"},
	}
	got := map[string]Knob{}
	for _, k := range Catalog() {
		got[k.Key] = k
	}
	for key, w := range want {
		k, ok := got[key]
		if !ok {
			t.Fatalf("catalog is missing %q", key)
		}
		if k.Type != w.typ || k.Class != w.class || k.EnvVar != w.env {
			t.Errorf("%s: got {%v %v %q}, want {%v %v %q}", key, k.Type, k.Class, k.EnvVar, w.typ, w.class, w.env)
		}
		if !reflect.DeepEqual(k.Default, w.def) {
			t.Errorf("%s default: got %#v want %#v", key, k.Default, w.def)
		}
	}
	if got["abr_ladder_resolution"].Default != false || got["abr_ladder_fps"].Default != false {
		t.Fatal("the resolution and fps rungs must ship dark (default false)")
	}
}

// TestCatalogHasEveryGovernorKnob covers the SPT-04 governor hysteresis knobs
// (docs/reports/2026-08-18-overnight-optimisation/t3-ladder-step-recovery.md):
// documented in docs/configuration.md as Live-class/operator-settable, but
// absent from this catalog until fixed alongside deploy/docker-compose.yml's
// passthrough. Mirrors TestCatalogHasEveryLadderKnob.
func TestCatalogHasEveryGovernorKnob(t *testing.T) {
	want := map[string]struct {
		typ   Type
		def   any
		class Class
		env   string
	}{
		"abr_ewma_alpha":       {TypeFloat, 0.3, ClassLive, "QUASAR_ABR_EWMA_ALPHA"},
		"abr_deadband":         {TypeFloat, 0.15, ClassLive, "QUASAR_ABR_DEADBAND"},
		"abr_max_up_step":      {TypeFloat, 0.25, ClassLive, "QUASAR_ABR_MAX_UP_STEP"},
		"abr_min_interval_ms":  {TypeInt, float64(1000), ClassLive, "QUASAR_ABR_MIN_INTERVAL_MS"},
		"abr_max_down_step":    {TypeFloat, 0.125, ClassLive, "QUASAR_ABR_MAX_DOWN_STEP"},
		"abr_down_dwell_ms":    {TypeInt, float64(7000), ClassLive, "QUASAR_ABR_DOWN_DWELL_MS"},
		"abr_cliff_guard_frac": {TypeFloat, 0.50, ClassLive, "QUASAR_ABR_CLIFF_GUARD_FRAC"},
	}
	got := map[string]Knob{}
	for _, k := range Catalog() {
		got[k.Key] = k
	}
	for key, w := range want {
		k, ok := got[key]
		if !ok {
			t.Fatalf("catalog is missing %q", key)
		}
		if k.Type != w.typ || k.Class != w.class || k.EnvVar != w.env {
			t.Errorf("%s: got {%v %v %q}, want {%v %v %q}", key, k.Type, k.Class, k.EnvVar, w.typ, w.class, w.env)
		}
		if !reflect.DeepEqual(k.Default, w.def) {
			t.Errorf("%s default: got %#v want %#v", key, k.Default, w.def)
		}
	}
}

func TestLadderEnumsAreConstrained(t *testing.T) {
	cat := map[string]Knob{}
	for _, k := range Catalog() {
		cat[k.Key] = k
	}
	if !reflect.DeepEqual(cat["abr_mode"].Enum, []string{"off", "protective", "smooth"}) {
		t.Fatalf("abr_mode enum = %v", cat["abr_mode"].Enum)
	}
	if !reflect.DeepEqual(cat["abr_ladder_order"].Enum, []string{"res_first", "fps_first", "hybrid"}) {
		t.Fatalf("abr_ladder_order enum = %v", cat["abr_ladder_order"].Enum)
	}
	if err := Validate(map[string]any{"abr_mode": "aggressive"}); err == nil {
		t.Fatal("an unknown abr_mode must be rejected")
	}
	if err := Validate(map[string]any{"abr_ladder_res_exponent": 1.5}); err == nil {
		t.Fatal("res_exponent above 1.0 must be rejected")
	}
	if err := Validate(map[string]any{"abr_ladder_res_min_height": float64(240)}); err == nil {
		t.Fatal("min_height below 360 must be rejected")
	}
}

func TestValidateResolvedRejectsACollapsedHysteresisBand(t *testing.T) {
	ok := Resolve(map[string]any{"abr_ladder_res_engage_frac": 0.6, "abr_ladder_res_recover_frac": 0.8})
	if err := ValidateResolved(ok); err != nil {
		t.Fatalf("the defaults must validate: %v", err)
	}
	bad := Resolve(map[string]any{"abr_ladder_res_engage_frac": 0.85, "abr_ladder_res_recover_frac": 0.8})
	err := ValidateResolved(bad)
	if err == nil {
		t.Fatal("engage_frac >= recover_frac must be rejected")
	}
	for _, needle := range []string{"abr_ladder_res_engage_frac", "abr_ladder_res_recover_frac"} {
		if !strings.Contains(err.Error(), needle) {
			t.Errorf("error %q does not name %s", err, needle)
		}
	}
	if ValidateResolved(Resolve(map[string]any{"abr_ladder_res_engage_frac": 0.8, "abr_ladder_res_recover_frac": 0.8})) == nil {
		t.Fatal("engage_frac == recover_frac must be rejected")
	}
	if ValidateResolved(Resolve(map[string]any{"abr_ladder_res_engage_frac": 0.78, "abr_ladder_res_recover_frac": 0.80})) == nil {
		t.Fatal("a band narrower than 0.05 must be rejected")
	}
}

func TestAbrEnabledSurvivesAsDeprecated(t *testing.T) {
	cat := map[string]Knob{}
	for _, k := range Catalog() {
		cat[k.Key] = k
	}
	if _, ok := cat["abr_enabled"]; !ok {
		t.Fatal("abr_enabled must remain in the catalog (deprecated, not removed)")
	}
	if err := Validate(map[string]any{"abr_enabled": false}); err != nil {
		t.Fatalf("abr_enabled must still validate: %v", err)
	}
}
