// The platform-release channel and edge branch ride the existing settings
// envelope (control-api.md §"Channel and edge branch"), so they are exercised
// through the same real RequireAuth→RequireAdmin chain every other field is.
package settings

import (
	"strings"
	"testing"
)

func TestReleaseChannelDefaultsToStableOnDevelop(t *testing.T) {
	pool := testDB(t)
	_, get := newSettingsHarness(t, pool)

	st := get(t).Settings
	if st.ReleaseChannel != ReleaseChannelStable {
		t.Errorf("release_channel = %q, want stable — an unconfigured instance is not shown branch builds", st.ReleaseChannel)
	}
	if st.ReleaseEdgeBranch != DefaultReleaseEdgeBranch {
		t.Errorf("release_edge_branch = %q, want %q", st.ReleaseEdgeBranch, DefaultReleaseEdgeBranch)
	}
}

func TestPatchReleaseChannelAndBranch(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)

	code, env := patch(t, `{"release_channel":"edge","release_edge_branch":"feat/x"}`)
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if env.Settings.ReleaseChannel != ReleaseChannelEdge || env.Settings.ReleaseEdgeBranch != "feat/x" {
		t.Fatalf("settings = %+v, want edge / feat/x", env.Settings)
	}

	// Absent means unchanged: a PATCH of an unrelated field must not reset the
	// channel, which a plain non-pointer decode would.
	code, env = patch(t, `{"registration_mode":"open"}`)
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if env.Settings.ReleaseChannel != ReleaseChannelEdge || env.Settings.ReleaseEdgeBranch != "feat/x" {
		t.Fatalf("an unrelated PATCH reset the release settings: %+v", env.Settings)
	}

	// Switching back to stable never clears the branch.
	code, env = patch(t, `{"release_channel":"stable"}`)
	if code != 200 || env.Settings.ReleaseEdgeBranch != "feat/x" {
		t.Fatalf("a channel switch cleared the branch: %d %+v", code, env.Settings)
	}
	if get(t).Settings.ReleaseChannel != ReleaseChannelStable {
		t.Fatal("the switch did not persist")
	}
}

func TestPatchReleaseValidationFailures(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)

	bad := []string{
		`{"release_channel":"nightly"}`,
		`{"release_edge_branch":""}`,
		`{"release_edge_branch":"has space"}`,
		`{"release_edge_branch":"a..b"}`,
		`{"release_edge_branch":"-leading"}`,
		`{"release_edge_branch":"` + strings.Repeat("b", 256) + `"}`,
	}
	for _, body := range bad {
		code, _ := patch(t, body)
		if code != 400 {
			t.Errorf("PATCH %s = %d, want 400 validation_failed", body, code)
		}
	}
	// Nothing was written by any of them.
	st := get(t).Settings
	if st.ReleaseChannel != ReleaseChannelStable || st.ReleaseEdgeBranch != DefaultReleaseEdgeBranch {
		t.Fatalf("a rejected PATCH still wrote: %+v", st)
	}
}

func TestReleaseKeysAreAudited(t *testing.T) {
	p := Patch{}
	channel, branch := ReleaseChannelEdge, "develop"
	p.ReleaseChannel, p.ReleaseEdgeBranch = &channel, &branch
	keys := strings.Join(p.ChangedKeys(), ",")
	if !strings.Contains(keys, "release_channel") || !strings.Contains(keys, "release_edge_branch") {
		t.Fatalf("changed keys = %q, want both release keys", keys)
	}
	if p.Empty() {
		t.Fatal("a patch naming only the release keys must not read as empty")
	}
}
