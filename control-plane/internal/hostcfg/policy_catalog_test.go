package hostcfg

import (
	"errors"
	"sort"
	"strings"
	"testing"
)

// matrixRow is the frozen RH05 source/effect/evidence matrix
// (docs/design/rh05-contract-proposal.md), transcribed independently of
// policyMatrix so a drift in either shows here.
type matrixRow struct {
	key, modes, group, scope, link string
}

var frozenMatrix = []matrixRow{
	{"abr_enabled", "D/E", "abr_enabled", "next_session", "abr_mode"},
	{"abr_floor_kbps", "D/E", "abr_floor_kbps", "next_session", ""},
	{"abr_floor_ratio", "D/E", "abr_floor_ratio", "next_session", ""},
	{"abr_mode", "D/E", "abr_mode", "next_session", "abr_mode"},
	{"abr_ewma_alpha", "D/E", "abr_ewma_alpha", "next_session", ""},
	{"abr_deadband", "D/E", "abr_deadband", "next_session", ""},
	{"abr_max_up_step", "D/E", "abr_max_up_step", "next_session", ""},
	{"abr_min_interval_ms", "D/E", "abr_min_interval_ms", "next_session", ""},
	{"abr_max_down_step", "D/E", "abr_max_down_step", "next_session", ""},
	{"abr_down_dwell_ms", "D/E", "abr_down_dwell_ms", "next_session", ""},
	{"abr_cliff_guard_frac", "D/E", "abr_cliff_guard_frac", "next_session", ""},
	{"abr_ladder", "D/E", "abr_ladder", "next_session", ""},
	{"abr_ladder_max_bias", "D/E", "abr_ladder_max_bias", "next_session", ""},
	{"abr_ladder_engage_dwell", "D/E", "abr_ladder_engage_dwell", "next_session", ""},
	{"abr_ladder_recover_dwell", "D/E", "abr_ladder_recover_dwell", "next_session", ""},
	{"abr_ladder_resolution", "D/E", "abr_ladder_resolution", "next_session", ""},
	{"abr_ladder_res_exponent", "D/E", "abr_ladder_res_exponent", "next_session", ""},
	{"abr_ladder_res_engage_frac", "D/E", "abr_ladder_res_engage_frac", "next_session", "res_hysteresis"},
	{"abr_ladder_res_recover_frac", "D/E", "abr_ladder_res_recover_frac", "next_session", "res_hysteresis"},
	{"abr_ladder_res_engage_dwell", "D/E", "abr_ladder_res_engage_dwell", "next_session", ""},
	{"abr_ladder_res_recover_dwell", "D/E", "abr_ladder_res_recover_dwell", "next_session", ""},
	{"abr_ladder_res_min_step_s", "D/E", "abr_ladder_res_min_step_s", "next_session", ""},
	{"abr_ladder_res_min_height", "D/E", "abr_ladder_res_min_height", "next_session", ""},
	{"abr_ladder_fps", "D/E", "abr_ladder_fps", "next_session", ""},
	{"abr_ladder_floor_follows_rung", "D/E", "abr_ladder_floor_follows_rung", "next_session", ""},
	{"abr_ladder_order", "D/E", "abr_ladder_order", "next_session", ""},
	{"gop", "D/E", "gop", "next_session", ""},
	{"slices", "D/E", "slices", "next_session", ""},
	{"target_usage", "D/E", "target_usage", "next_session", ""},
	{"queue_buffers", "D/E", "queue_buffers", "next_session", ""},
	{"zerocopy", "D/E", "zerocopy", "next_session", ""},
	{"latency_probe", "D/E", "latency_probe", "next_session", ""},
	{"idle_timeout_secs", "D/E", "idle_timeout_secs", "next_session", ""},
	{"app_boot_timeout_secs", "D/E", "app_boot_timeout_secs", "next_session", ""},
	{"home_root", "D/E", "home_root", "next_session", "home_storage"},
	{"nvidia_lib32_path", "D/E", "nvidia_lib32_path", "next_session", ""},
	{"encoder", "A/D/E", "hardware", "restart", ""},
	{"render_node", "A/D/E", "hardware", "restart", ""},
	{"cuda_device", "D/E", "hardware", "restart", ""},
}

func TestPolicyMatrixCoversEveryCatalogKeyExactlyOnce(t *testing.T) {
	if len(frozenMatrix) != 39 {
		t.Fatalf("frozen matrix has %d rows, want 39", len(frozenMatrix))
	}
	catalog := map[string]bool{}
	for _, knob := range Catalog() {
		catalog[knob.Key] = true
	}
	if len(catalog) != 39 {
		t.Fatalf("catalog has %d keys, want 39", len(catalog))
	}
	for _, row := range frozenMatrix {
		spec, ok := PolicySpec(row.key)
		if !ok || !catalog[row.key] {
			t.Fatalf("%s: missing from policy matrix or catalog", row.key)
		}
		modes := "D/E"
		if spec.Automatic {
			modes = "A/D/E"
		}
		if modes != row.modes || spec.Group != row.group || spec.Scope != row.scope || spec.Link != row.link {
			t.Errorf("%s: spec %+v, frozen %+v", row.key, spec, row)
		}
		// Matrix scope must agree with the legacy catalog's restart class, or the
		// legacy PATCH restart guard and typed scope would disagree.
		if (spec.Scope == "restart") != (byKey()[row.key].Class == ClassRestart) {
			t.Errorf("%s: scope %s disagrees with catalog class %s", row.key, spec.Scope, byKey()[row.key].Class)
		}
	}
	if len(PolicyKeySpecs()) != 39 {
		t.Fatalf("policy matrix has %d rows", len(PolicyKeySpecs()))
	}
}

func TestNextSessionPolicyGroupsExcludeHardware(t *testing.T) {
	groups := NextSessionPolicyGroups()
	if len(groups) != 36 || !sort.StringsAreSorted(groups) {
		t.Fatalf("next-session groups = %v", groups)
	}
	for _, group := range groups {
		if group == "hardware" || !IsPolicyGroup(group) {
			t.Fatalf("unexpected group %q", group)
		}
		if keys := PolicyGroupKeys(group); len(keys) != 1 || keys[0] != group {
			t.Fatalf("%s keys = %v", group, keys)
		}
	}
	if got := PolicyGroupKeys("hardware"); strings.Join(got, ",") != "cuda_device,encoder,render_node" {
		t.Fatalf("hardware keys = %v", got)
	}
	if IsPolicyGroup("unknown") || IsPolicyGroup("encoder") {
		t.Fatal("a non-group name was accepted")
	}
}

// explicitSamples gives one valid and one invalid explicit value per
// next-session key, exercising each key's catalog type and inclusive bounds.
var explicitSamples = map[string]struct{ valid, invalid any }{
	"abr_enabled":                   {false, "false"},
	"abr_floor_kbps":                {float64(1), float64(0)},
	"abr_floor_ratio":               {float64(1), 1.01},
	"abr_mode":                      {"protective", "fast"},
	"abr_ewma_alpha":                {0.000001, float64(0)},
	"abr_deadband":                  {0.999999, float64(1)},
	"abr_max_up_step":               {0.000001, float64(0)},
	"abr_min_interval_ms":           {float64(1), 1.5},
	"abr_max_down_step":             {0.5, float64(1)},
	"abr_down_dwell_ms":             {float64(0), float64(-1)},
	"abr_cliff_guard_frac":          {0.5, float64(1)},
	"abr_ladder":                    {true, float64(1)},
	"abr_ladder_max_bias":           {float64(255), float64(256)},
	"abr_ladder_engage_dwell":       {float64(1), float64(0)},
	"abr_ladder_recover_dwell":      {float64(255), float64(256)},
	"abr_ladder_resolution":         {true, "yes"},
	"abr_ladder_res_exponent":       {0.5, 0.49},
	"abr_ladder_res_engage_frac":    {0.2, 0.19},
	"abr_ladder_res_recover_frac":   {float64(1), 1.1},
	"abr_ladder_res_engage_dwell":   {float64(60), float64(61)},
	"abr_ladder_res_recover_dwell":  {float64(1), float64(0)},
	"abr_ladder_res_min_step_s":     {float64(5), float64(4)},
	"abr_ladder_res_min_height":     {float64(2160), float64(2161)},
	"abr_ladder_fps":                {true, nil},
	"abr_ladder_floor_follows_rung": {false, float64(0)},
	"abr_ladder_order":              {"fps_first", "fastest"},
	"gop":                           {float64(1), float64(0)},
	"slices":                        {float64(8), 2.5},
	"target_usage":                  {float64(7), float64(8)},
	"queue_buffers":                 {float64(1), float64(0)},
	"zerocopy":                      {true, "true"},
	"latency_probe":                 {false, float64(0)},
	"idle_timeout_secs":             {float64(0), float64(-1)},
	"app_boot_timeout_secs":         {float64(0), 0.5},
	"home_root":                     {"/srv/homes/users", "relative/path"},
	"nvidia_lib32_path":             {"", "lib32"},
}

func TestValidatePolicyEditPerKeyTypedSources(t *testing.T) {
	ctx := PolicyEditContext{MountedHomeRoot: "/srv/homes"}
	for _, group := range NextSessionPolicyGroups() {
		sample, ok := explicitSamples[group]
		if !ok {
			t.Fatalf("%s has no behavior sample", group)
		}
		t.Run(group, func(t *testing.T) {
			if err := ValidatePolicyEdit(map[string]PolicyChoice{group: {Source: "explicit", Value: sample.valid}}, ctx); err != nil {
				t.Fatalf("valid explicit: %v", err)
			}
			if err := ValidatePolicyEdit(map[string]PolicyChoice{group: {Source: "deployment"}}, ctx); err != nil {
				t.Fatalf("deployment: %v", err)
			}
			assertPolicyCode(t, ValidatePolicyEdit(map[string]PolicyChoice{group: {Source: "explicit", Value: sample.invalid}}, ctx), "validation_failed")
			assertPolicyCode(t, ValidatePolicyEdit(map[string]PolicyChoice{group: {Source: "explicit"}}, ctx), "validation_failed")
			assertPolicyCode(t, ValidatePolicyEdit(map[string]PolicyChoice{group: {Source: "deployment", Value: sample.valid}}, ctx), "validation_failed")
			// No Automatic on numeric, ABR or storage keys: only the hardware
			// group has a supported resolver.
			assertPolicyCode(t, ValidatePolicyEdit(map[string]PolicyChoice{group: {Source: "automatic"}}, ctx), "unsupported_source")
		})
	}
}

func TestValidatePolicyEditAutomaticOnlyForHardwareResolvers(t *testing.T) {
	for key, want := range map[string]bool{"encoder": true, "render_node": true, "cuda_device": false} {
		err := ValidatePolicyEdit(map[string]PolicyChoice{key: {Source: "automatic"}}, PolicyEditContext{})
		if want && err != nil {
			t.Fatalf("%s automatic: %v", key, err)
		}
		if !want {
			assertPolicyCode(t, err, "unsupported_source")
		}
	}
	assertPolicyCode(t, ValidatePolicyEdit(map[string]PolicyChoice{"encoder": {Source: "automatic", Value: "va"}}, PolicyEditContext{}), "validation_failed")
	assertPolicyCode(t, ValidatePolicyEdit(map[string]PolicyChoice{"gop": {Source: "guess"}}, PolicyEditContext{}), "validation_failed")
}

func TestValidatePolicyEditRejectsMalformedMultiKeyEditAsAWhole(t *testing.T) {
	err := ValidatePolicyEdit(map[string]PolicyChoice{
		"gop":           {Source: "explicit", Value: float64(90)},
		"slices":        {Source: "explicit", Value: float64(4)},
		"not_a_setting": {Source: "deployment"},
	}, PolicyEditContext{})
	assertPolicyCode(t, err, "validation_failed")
	assertPolicyCode(t, ValidatePolicyEdit(map[string]PolicyChoice{}, PolicyEditContext{}), "validation_failed")
}

func TestValidatePolicyEditResolutionHysteresisUsesKnownResolvedValuesOnly(t *testing.T) {
	engage := func(v float64) map[string]PolicyChoice {
		return map[string]PolicyChoice{"abr_ladder_res_engage_frac": {Source: "explicit", Value: v}}
	}
	// Both sides known: explicit engage against an explicit saved recover.
	saved := map[string]PolicyChoice{"abr_ladder_res_recover_frac": {Source: "explicit", Value: 0.8}}
	err := ValidatePolicyEdit(engage(0.78), PolicyEditContext{Current: saved})
	assertPolicyCode(t, err, "validation_failed")
	var typed *PolicyValidationError
	if !errors.As(err, &typed) || strings.Join(typed.Keys, ",") != "abr_ladder_res_engage_frac,abr_ladder_res_recover_frac" {
		t.Fatalf("hysteresis error must name both keys: %#v", err)
	}
	if err := ValidatePolicyEdit(engage(0.75), PolicyEditContext{Current: saved}); err != nil {
		t.Fatalf("0.05 band is inclusive: %v", err)
	}
	// Deployment side resolves from the reported baseline, never the catalog default.
	baseline := map[string]any{"abr_ladder_res_recover_frac": 0.7}
	assertPolicyCode(t, ValidatePolicyEdit(engage(0.7), PolicyEditContext{Baseline: baseline}), "validation_failed")
	// With no baseline the deployment side is unknown; the agent validates the pair.
	if err := ValidatePolicyEdit(engage(0.9), PolicyEditContext{}); err != nil {
		t.Fatalf("unknown deployment side must not use the catalog default: %v", err)
	}
	// An edit that fixes both sides in one request is judged on the new pair.
	both := map[string]PolicyChoice{
		"abr_ladder_res_engage_frac":  {Source: "explicit", Value: 0.9},
		"abr_ladder_res_recover_frac": {Source: "explicit", Value: float64(1)},
	}
	if err := ValidatePolicyEdit(both, PolicyEditContext{Current: saved}); err != nil {
		t.Fatalf("paired edit: %v", err)
	}
}

func TestValidatePolicyEditHomeRootStaysInsideMountAndExistingHomes(t *testing.T) {
	edit := func(v string) map[string]PolicyChoice {
		return map[string]PolicyChoice{"home_root": {Source: "explicit", Value: v}}
	}
	mounted := PolicyEditContext{MountedHomeRoot: "/srv/homes"}
	if err := ValidatePolicyEdit(edit("/srv/homes/a"), mounted); err != nil {
		t.Fatal(err)
	}
	assertPolicyCode(t, ValidatePolicyEdit(edit("/srv/homes-evil"), mounted), "validation_failed")
	assertPolicyCode(t, ValidatePolicyEdit(edit("/other"), mounted), "validation_failed")
	// No implicit mount creation: without a reported mount no explicit root is valid.
	assertPolicyCode(t, ValidatePolicyEdit(edit("/srv/homes"), PolicyEditContext{}), "validation_failed")

	withHomes := PolicyEditContext{MountedHomeRoot: "/srv/homes", ExistingHomeRefs: []string{"/srv/homes/u1/app", "/srv/homes/u2/app"}}
	if err := ValidatePolicyEdit(edit("/srv/homes"), withHomes); err != nil {
		t.Fatalf("root still containing every home: %v", err)
	}
	assertPolicyCode(t, ValidatePolicyEdit(edit("/srv/homes/u1"), withHomes), "home_conflict")
	assertPolicyCode(t, ValidatePolicyEdit(edit(""), withHomes), "home_conflict")
	if err := ValidatePolicyEdit(edit(""), mounted); err != nil {
		t.Fatalf("empty root with no homes: %v", err)
	}
	// Returning to the deployment mount can never strand a home created under it.
	if err := ValidatePolicyEdit(map[string]PolicyChoice{"home_root": {Source: "deployment"}}, withHomes); err != nil {
		t.Fatal(err)
	}
}

func assertPolicyCode(t *testing.T, err error, code string) {
	t.Helper()
	var typed *PolicyValidationError
	if !errors.As(err, &typed) || typed.Code != code {
		t.Fatalf("error = %#v, want code %s", err, code)
	}
}
