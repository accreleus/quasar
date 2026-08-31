package session

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Table-driven coverage of every UI-P3 merge rule. These run WITHOUT a database
// (the rules are a pure function) — the DB-backed end-to-end assertions live in
// runtime_preset_db_test.go.
//
// Each case names the rule it pins. The two that are easiest to get wrong on
// purpose — a key set on BOTH preset and app, and the same container path
// mounted twice — have dedicated cases and must never be "helpfully" resolved.
func TestMergeRuntimePresetRules(t *testing.T) {
	tests := []struct {
		name   string
		app    string // app runtime_spec JSON
		preset runtimePreset
		want   map[string]any
	}{
		{
			name:   "image: app overrides the preset when set",
			app:    `{"image":"app-image:1"}`,
			preset: runtimePreset{Image: "preset-image:1"},
			want:   map[string]any{"image": "app-image:1"},
		},
		{
			name:   "image: blank app image inherits from the preset",
			app:    `{"image":""}`,
			preset: runtimePreset{Image: "preset-image:1"},
			want:   map[string]any{"image": "preset-image:1"},
		},
		{
			name:   "image: absent app image inherits from the preset",
			app:    `{}`,
			preset: runtimePreset{Image: "preset-image:1"},
			want:   map[string]any{"image": "preset-image:1"},
		},
		{
			name:   "image: no preset image leaves the app's own",
			app:    `{"image":"app-image:1"}`,
			preset: runtimePreset{Image: ""},
			want:   map[string]any{"image": "app-image:1"},
		},
		{
			name:   "args: appended, preset first",
			app:    `{"args":["--app-a","--app-b"]}`,
			preset: runtimePreset{Args: json.RawMessage(`["--preset-a","--preset-b"]`)},
			want: map[string]any{
				"args": []any{"--preset-a", "--preset-b", "--app-a", "--app-b"},
			},
		},
		{
			name:   "args: app with none still gets the preset's",
			app:    `{}`,
			preset: runtimePreset{Args: json.RawMessage(`["--preset-only"]`)},
			want:   map[string]any{"args": []any{"--preset-only"}},
		},
		{
			name:   "env: preset first, app second, no conflicts",
			app:    `{"env":{"APP_KEY":"app"}}`,
			preset: runtimePreset{Env: json.RawMessage(`{"PRESET_KEY":"preset"}`)},
			want: map[string]any{
				"env": map[string]any{"PRESET_KEY": "preset", "APP_KEY": "app"},
			},
		},
		{
			// THE conflicting-env-key case. The app is the more specific object and
			// must win; a preset value must never override what the app states.
			name:   "env: a key set on BOTH takes the APP's value",
			app:    `{"env":{"LOG_LEVEL":"debug","APP_ONLY":"a"}}`,
			preset: runtimePreset{Env: json.RawMessage(`{"LOG_LEVEL":"info","PRESET_ONLY":"p"}`)},
			want: map[string]any{
				"env": map[string]any{
					"LOG_LEVEL":   "debug", // app wins, NOT "info"
					"APP_ONLY":    "a",
					"PRESET_ONLY": "p",
				},
			},
		},
		{
			name:   "mounts: appended, preset first",
			app:    `{"mounts":["/data/app:/app"]}`,
			preset: runtimePreset{Mounts: json.RawMessage(`["/data/cache:/cache"]`)},
			want: map[string]any{
				"mounts": []any{"/data/cache:/cache", "/data/app:/app"},
			},
		},
		{
			// THE duplicate-mounts case. Two mounts on one container path is a real
			// misconfiguration: BOTH must survive the merge so it surfaces at launch
			// instead of being silently resolved by us picking one.
			name:   "mounts: duplicate container paths are NOT deduplicated",
			app:    `{"mounts":["/host/app:/shared"]}`,
			preset: runtimePreset{Mounts: json.RawMessage(`["/host/preset:/shared"]`)},
			want: map[string]any{
				"mounts": []any{"/host/preset:/shared", "/host/app:/shared"},
			},
		},
		{
			name:   "mounts: byte-identical duplicates are also kept",
			app:    `{"mounts":["/same:/same"]}`,
			preset: runtimePreset{Mounts: json.RawMessage(`["/same:/same"]`)},
			want: map[string]any{
				"mounts": []any{"/same:/same", "/same:/same"},
			},
		},
		{
			name: "unrelated keys are carried through untouched",
			app:  `{"image":"app:1","gpu":true,"future_key":{"nested":1}}`,
			preset: runtimePreset{
				Image: "preset:1",
				Env:   json.RawMessage(`{"A":"1"}`),
			},
			want: map[string]any{
				"image":      "app:1",
				"gpu":        true,
				"future_key": map[string]any{"nested": float64(1)},
				"env":        map[string]any{"A": "1"},
			},
		},
		{
			name: "all four rules at once",
			app:  `{"image":"","args":["--app"],"env":{"K":"app"},"mounts":["/a:/a"],"gpu":true}`,
			preset: runtimePreset{
				Image:  "preset:1",
				Args:   json.RawMessage(`["--preset"]`),
				Env:    json.RawMessage(`{"K":"preset","P":"p"}`),
				Mounts: json.RawMessage(`["/p:/p"]`),
			},
			want: map[string]any{
				"image":  "preset:1",
				"args":   []any{"--preset", "--app"},
				"env":    map[string]any{"K": "app", "P": "p"},
				"mounts": []any{"/p:/p", "/a:/a"},
				"gpu":    true,
			},
		},
		{
			name:   "an empty preset adds nothing",
			app:    `{"image":"app:1","args":["--a"],"env":{"K":"v"},"mounts":["/a:/a"]}`,
			preset: runtimePreset{},
			want: map[string]any{
				"image":  "app:1",
				"args":   []any{"--a"},
				"env":    map[string]any{"K": "v"},
				"mounts": []any{"/a:/a"},
			},
		},
		{
			name: "an empty app spec becomes the preset verbatim",
			app:  `{}`,
			preset: runtimePreset{
				Image:  "preset:1",
				Args:   json.RawMessage(`["--p"]`),
				Env:    json.RawMessage(`{"K":"v"}`),
				Mounts: json.RawMessage(`["/p:/p"]`),
			},
			want: map[string]any{
				"image":  "preset:1",
				"args":   []any{"--p"},
				"env":    map[string]any{"K": "v"},
				"mounts": []any{"/p:/p"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := mergeRuntimePreset(json.RawMessage(tc.app), tc.preset)
			if err != nil {
				t.Fatalf("merge: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("merged spec is not valid JSON (%s): %v", out, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("merged spec mismatch\n got: %#v\nwant: %#v", got, tc.want)
			}
		})
	}
}

// mergeManagedHome is the storage half of the rules: the preset provides the
// default and the app may override.
func TestMergeManagedHomeRules(t *testing.T) {
	tests := []struct {
		name         string
		appManaged   bool
		appPath      string
		preset       runtimePreset
		wantManaged  bool
		wantPathIsIn string
	}{
		{
			name: "preset provides managed home when the app does not",
			preset: runtimePreset{
				ManagedHome: true, HomeContainerPath: "/home/preset",
			},
			appManaged: false, appPath: defaultHomeContainerPath,
			wantManaged: true, wantPathIsIn: "/home/preset",
		},
		{
			name:       "app turns managed home on with no preset home",
			preset:     runtimePreset{},
			appManaged: true, appPath: "/home/app",
			wantManaged: true, wantPathIsIn: "/home/app",
		},
		{
			name: "app's explicit non-default path overrides the preset's",
			preset: runtimePreset{
				ManagedHome: true, HomeContainerPath: "/home/preset",
			},
			appManaged: true, appPath: "/home/app",
			wantManaged: true, wantPathIsIn: "/home/app",
		},
		{
			name: "app left at the schema default inherits the preset's path",
			preset: runtimePreset{
				ManagedHome: true, HomeContainerPath: "/srv/home",
			},
			appManaged: true, appPath: defaultHomeContainerPath,
			wantManaged: true, wantPathIsIn: "/srv/home",
		},
		{
			name:       "no preset home and no app home stays off at the default path",
			preset:     runtimePreset{},
			appManaged: false, appPath: defaultHomeContainerPath,
			wantManaged: false, wantPathIsIn: defaultHomeContainerPath,
		},
		{
			name:       "an empty app path never escapes as empty",
			preset:     runtimePreset{},
			appManaged: true, appPath: "",
			wantManaged: true, wantPathIsIn: defaultHomeContainerPath,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			managed, path := mergeManagedHome(tc.appManaged, tc.appPath, tc.preset)
			if managed != tc.wantManaged {
				t.Fatalf("managed_home = %v, want %v", managed, tc.wantManaged)
			}
			if path != tc.wantPathIsIn {
				t.Fatalf("home_container_path = %q, want %q", path, tc.wantPathIsIn)
			}
		})
	}
}

// A malformed runtime_spec must be a real error, never a silently-dropped merge.
func TestMergeRuntimePresetRejectsMalformedSpec(t *testing.T) {
	for _, tc := range []struct {
		name string
		app  string
	}{
		{"not JSON", `{`},
		{"args is not an array", `{"args":"--nope"}`},
		{"mounts is not an array", `{"mounts":{"a":"b"}}`},
		{"env is not an object", `{"env":["A=1"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := mergeRuntimePreset(json.RawMessage(tc.app), runtimePreset{Image: "x"}); err == nil {
				t.Fatalf("expected an error for %s, got none", tc.name)
			}
		})
	}
}

// TestMergeRuntimePresetNetworkPrecedence pins the §S2 resolve chain at the
// control-plane layer: app runtime_spec.network beats the preset's, a preset
// network fills in when the app states none, and NEITHER stating one leaves the
// key ABSENT so the agent applies its own host fallback
// (QUASAR_CONTAINER_NETWORK, else `none`).
//
// The absent case is the one that matters most: emitting `"network": ""` would
// be an operator-looking value on the wire that the agent would then have to
// special-case, and it would break the "an app with no network requirement is
// byte-identical to before this feature" property.
func TestMergeRuntimePresetNetworkPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		app    string
		preset runtimePreset
		want   any // nil = the key must be absent
	}{
		{
			// Both directions matter, but this one is the security-relevant one:
			// an app pins itself to ISOLATED even though its preset opens a
			// bridge. (`host` is deliberately not used as an example anywhere on
			// the app side — validateRuntimeNetwork refuses it.)
			name:   "app spec wins over the preset",
			app:    `{"network":"none"}`,
			preset: runtimePreset{Network: "bridge"},
			want:   "none",
		},
		{
			name:   "preset applies when the app states none",
			app:    `{}`,
			preset: runtimePreset{Network: "bridge"},
			want:   "bridge",
		},
		{
			name:   "a blank app network inherits the preset",
			app:    `{"network":""}`,
			preset: runtimePreset{Network: "bridge"},
			want:   "bridge",
		},
		{
			name:   "a blank preset network leaves the key absent",
			app:    `{}`,
			preset: runtimePreset{Network: ""},
			want:   nil,
		},
		{
			name:   "a blank preset network does not clobber the app's",
			app:    `{"network":"none"}`,
			preset: runtimePreset{Network: ""},
			want:   "none",
		},
		{
			name:   "a blank app network with a blank preset stays absent, not empty",
			app:    `{"network":""}`,
			preset: runtimePreset{Network: ""},
			want:   "", // the app's own explicit "" is carried through untouched
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := mergeRuntimePreset(json.RawMessage(tc.app), tc.preset)
			if err != nil {
				t.Fatalf("merge: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("merged spec is not valid JSON (%s): %v", out, err)
			}
			v, present := got["network"]
			if tc.want == nil {
				if present {
					t.Fatalf("network should be absent, got %#v (spec %s)", v, out)
				}
				return
			}
			if !present {
				t.Fatalf("network absent, want %#v (spec %s)", tc.want, out)
			}
			if v != tc.want {
				t.Fatalf("network = %#v, want %#v", v, tc.want)
			}
		})
	}
}

// TestValidateRuntimeNetwork covers the app-side guard Alice's round-2 review
// asked for: an app's OWN runtime_spec.network must be a string in the app-facing
// set, checked on EVERY launch — with or without a preset — before anything can
// inherit over it or dispatch it.
//
// The two silent wrongs this closes, which the merge alone could not:
//   - a present-but-non-string value read as ABSENT, so a preset silently
//     OVERWROTE the operator's stated intent with no error at all;
//   - with no preset, that same value dispatched verbatim and failed LATE inside
//     the agent's session_assign deserialization — an opaque serde error against
//     a message no operator ever sees.
func TestValidateRuntimeNetwork(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spec    string
		wantErr string // "" = must be accepted
	}{
		{name: "absent is fine", spec: `{"image":"a:1"}`},
		{name: "empty bytes are fine", spec: ``},
		{name: "explicit null is not stated", spec: `{"network":null}`},
		{name: "blank means inherit", spec: `{"network":""}`},
		{name: "none is allowed", spec: `{"network":"none"}`},
		{name: "bridge is allowed", spec: `{"network":"bridge"}`},

		// The security boundary: `host` is a REAL docker mode and the agent's own
		// operator knob takes it, but an app may never ask for it — it removes the
		// container's network namespace, and an app's runtime_spec is portable.
		{name: "host is refused from an app", spec: `{"network":"host"}`, wantErr: "isolation"},

		{name: "an arbitrary network is refused", spec: `{"network":"container:quasar-control"}`, wantErr: "rejected"},
		{name: "case matters", spec: `{"network":"Bridge"}`, wantErr: "rejected"},

		// Non-strings: previously read as "absent" and silently dropped.
		{name: "a bool is a named error", spec: `{"network":true}`, wantErr: "must be a string"},
		{name: "a number is a named error", spec: `{"network":1}`, wantErr: "must be a string"},
		{name: "an array is a named error", spec: `{"network":["bridge"]}`, wantErr: "must be a string"},
		{name: "an object is a named error", spec: `{"network":{"mode":"bridge"}}`, wantErr: "must be a string"},

		{name: "malformed JSON is an error", spec: `{`, wantErr: "parse runtime_spec"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRuntimeNetwork(json.RawMessage(tc.spec))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want accepted, got error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error containing %q, got none", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// A non-string app network must NOT be silently overwritten by a preset. Before
// the guard this merged "successfully" and the preset's value won, so an
// operator's malformed entry vanished without a trace. The merge itself still
// exhibits that behaviour by design (it only reads well-formed strings) — which
// is exactly why validateRuntimeNetwork runs ahead of it at the launch path,
// asserted here as a pair so the two cannot drift apart.
func TestNonStringAppNetworkIsCaughtBeforeInheritance(t *testing.T) {
	const spec = `{"network":true}`
	if err := validateRuntimeNetwork(json.RawMessage(spec)); err == nil {
		t.Fatal("validateRuntimeNetwork must reject a non-string network")
	}
	out, err := mergeRuntimePreset(json.RawMessage(spec), runtimePreset{Network: "bridge"})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("merged spec: %v", err)
	}
	if got["network"] != "bridge" {
		t.Fatalf("merge behaviour changed (%#v) — the guard above is the only thing "+
			"stopping this silent overwrite; re-check the launch path ordering", got["network"])
	}
}
