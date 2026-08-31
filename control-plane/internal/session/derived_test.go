package session

// steam-library-discovery Phase 3 — pure unit tests for the derived-tile
// primitives. No database: these are the pieces §10 calls the launch-time
// validation point, plus the env merge §3 specifies.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestComposeSteamFlagsIsAnInjectionBoundary is §10 point 4.
//
// STEAM_STARTUP_FLAGS replaces the Steam client's default flags wholesale and is
// word-split by `read -r -a` in the quasar-steam entrypoint, so every space in
// the returned string becomes an ARGUMENT to the client. The value it is built
// from was written by a background job that parsed a file on disk, so it is
// untrusted by construction.
//
// The test asserts the two properties that make this a boundary rather than a
// formatter: a fixed template with only a validated integer interpolated, and an
// ERROR — never a sanitized, truncated or empty flag string — for anything else.
// A silent fallback would produce "the user asked for a game and got the client",
// which §16.1 already records as unmeasurable.
func TestComposeSteamFlagsIsAnInjectionBoundary(t *testing.T) {
	t.Run("valid appids render the fixed template", func(t *testing.T) {
		for _, c := range []struct{ id, want string }{
			{"620", "-bigpicture -applaunch 620"},
			{"1145360", "-bigpicture -applaunch 1145360"},
			{"1", "-bigpicture -applaunch 1"},
			{"9999999999", "-bigpicture -applaunch 9999999999"}, // 10 digits, the bound
		} {
			got, err := composeSteamFlags(c.id)
			if err != nil {
				t.Fatalf("composeSteamFlags(%q): %v", c.id, err)
			}
			if got != c.want {
				t.Errorf("composeSteamFlags(%q) = %q, want %q", c.id, got, c.want)
			}
		}
	})

	t.Run("everything else is an error, not a fallback", func(t *testing.T) {
		for _, bad := range []string{
			"1; rm -rf /",    // shell-shaped
			"0",              // not a positive integer
			"99999999999",    // 11 digits, over the bound
			"",               // empty
			"480 -foo",       // the argument-injection case the grammar exists for
			"-applaunch 480", // an attempt to smuggle the flag itself
			"007",            // leading zero
			" 620",           // leading whitespace
			"620 ",           // trailing whitespace
			"620\n",          // trailing newline
			"620\n-novid",    // newline injection
			"62 0",           // internal space
			"6.20",           // separator
			"0x1f4",          // hex
			"abc",            // not a number at all
		} {
			got, err := composeSteamFlags(bad)
			if err == nil {
				t.Errorf("composeSteamFlags(%q) = %q, want an error", bad, got)
			}
			if got != "" {
				t.Errorf("composeSteamFlags(%q) returned %q alongside its error; it must return nothing", bad, got)
			}
			// The rejected value must not be echoed: it is either a hand-edited row
			// or a parser defect, and neither is worth putting arbitrary stored bytes
			// into an operator's log lines.
			if err != nil && strings.Contains(err.Error(), bad) && bad != "" {
				t.Errorf("composeSteamFlags(%q) echoed the value in its error: %v", bad, err)
			}
		}
	})
}

// TestInjectSteamFlagsMergesEnvOnly is §3's merge rule: parent first, tile
// second, tile wins — the same rule mergeRuntimePreset already established for
// preset-vs-app — and it touches `env` and nothing else.
func TestInjectSteamFlagsMergesEnvOnly(t *testing.T) {
	t.Run("the tile overrides the parent's flags and leaves other keys alone", func(t *testing.T) {
		parent := []byte(`{"image":"steam:1","args":["--x"],"mounts":["/a:/a:ro"],
			"env":{"STEAM_STARTUP_FLAGS":"-bigpicture","DISPLAY":":0","LANG":"en_GB"}}`)

		out, err := injectSteamFlags(parent, "-bigpicture -applaunch 620")
		if err != nil {
			t.Fatalf("injectSteamFlags: %v", err)
		}
		var spec struct {
			Image  string            `json:"image"`
			Args   []string          `json:"args"`
			Mounts []string          `json:"mounts"`
			Env    map[string]string `json:"env"`
		}
		if err := json.Unmarshal(out, &spec); err != nil {
			t.Fatalf("parse merged spec: %v", err)
		}
		if spec.Env["STEAM_STARTUP_FLAGS"] != "-bigpicture -applaunch 620" {
			t.Errorf("STEAM_STARTUP_FLAGS = %q, want the TILE's value (tile wins)", spec.Env["STEAM_STARTUP_FLAGS"])
		}
		if spec.Env["DISPLAY"] != ":0" || spec.Env["LANG"] != "en_GB" {
			t.Errorf("other env keys were disturbed: %v", spec.Env)
		}
		if spec.Image != "steam:1" || len(spec.Args) != 1 || len(spec.Mounts) != 1 {
			t.Errorf("a non-env field was touched: image=%q args=%v mounts=%v", spec.Image, spec.Args, spec.Mounts)
		}
	})

	t.Run("a parent with no env at all", func(t *testing.T) {
		out, err := injectSteamFlags([]byte(`{"image":"steam:1"}`), "-bigpicture -applaunch 620")
		if err != nil {
			t.Fatalf("injectSteamFlags: %v", err)
		}
		var spec struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal(out, &spec); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if spec.Env["STEAM_STARTUP_FLAGS"] != "-bigpicture -applaunch 620" {
			t.Errorf("env = %v, want the flags created", spec.Env)
		}
	})

	t.Run("an env of the wrong JSON type is an error, not a silent overwrite", func(t *testing.T) {
		// A hand-edited row with `"env": []` must fail the launch loudly rather than
		// have its env replaced by ours: the parent's other variables would vanish
		// and the container would start subtly wrong.
		if _, err := injectSteamFlags([]byte(`{"env":[]}`), "-bigpicture -applaunch 620"); err == nil {
			t.Error("injectSteamFlags with a non-object env: want an error, got nil")
		}
	})
}

// TestHomeAppIDIsTheOnlyRule pins §2's function itself. It is trivial by design —
// the value is that it is NAMED, so a reviewer can grep it and find all seven
// sites instead of hunting for inlined `if x.ParentAppID != ""` checks.
func TestHomeAppIDIsTheOnlyRule(t *testing.T) {
	tile := LaunchApp{ID: "tile-1", ParentAppID: "parent-1"}
	if got := homeAppID(tile); got != "parent-1" {
		t.Errorf("homeAppID(derived tile) = %q, want the parent", got)
	}
	if !tile.IsDerived() {
		t.Error("IsDerived() = false for a tile with a parent")
	}

	ordinary := LaunchApp{ID: "app-1"}
	if got := homeAppID(ordinary); got != "app-1" {
		t.Errorf("homeAppID(ordinary app) = %q, want its own id", got)
	}
	if ordinary.IsDerived() {
		t.Error("IsDerived() = true for an app with no parent")
	}
}

// TestCreateParamsHomeAppIDDefaults pins the scheduler-side half of the same
// rule. The fallback is what lets every pre-Phase-3 caller and every test fixture
// keep today's behaviour with no edit.
func TestCreateParamsHomeAppIDDefaults(t *testing.T) {
	if got := (CreateParams{AppID: "app-1"}).homeAppID(); got != "app-1" {
		t.Errorf("homeAppID with no HomeAppID = %q, want the AppID", got)
	}
	if got := (CreateParams{AppID: "tile-1", HomeAppID: "parent-1"}).homeAppID(); got != "parent-1" {
		t.Errorf("homeAppID = %q, want parent-1", got)
	}
}

// TestHomeInUseErrorUnwrapsToTheSentinel is what keeps every existing
// `errors.Is(err, ErrHomeInUse)` check — and the HTTP status mapping — working
// unchanged while the concrete type carries the session id for the body.
func TestHomeInUseErrorUnwrapsToTheSentinel(t *testing.T) {
	err := error(&HomeInUseError{SessionID: "sess-1"})
	if !errors.Is(err, ErrHomeInUse) {
		t.Error("errors.Is(&HomeInUseError{}, ErrHomeInUse) = false; every existing check would break")
	}
	if got := conflictingSessionID(err); got != "sess-1" {
		t.Errorf("conflictingSessionID = %q, want sess-1", got)
	}
	// A bare sentinel names no session, and the body must then omit the field
	// rather than emit "" — a client branches on presence and must never render a
	// link to nowhere.
	if got := conflictingSessionID(ErrHomeInUse); got != "" {
		t.Errorf("conflictingSessionID(bare sentinel) = %q, want \"\"", got)
	}
}
