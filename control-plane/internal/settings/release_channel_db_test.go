// The platform-release channel and edge branch ride the existing settings
// envelope (control-api.md §"Channel and edge branch"), so they are exercised
// through the same real RequireAuth→RequireAdmin chain every other field is.
package settings

import (
	"context"
	"strings"
	"testing"
	"time"
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

// Beta is a third value the migration-0079 CHECK must accept, and the branch it
// never uses must survive the round trip.
func TestPatchReleaseChannelBeta(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)

	code, env := patch(t, `{"release_edge_branch":"feat/x"}`)
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	code, env = patch(t, `{"release_channel":"beta"}`)
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if env.Settings.ReleaseChannel != ReleaseChannelBeta || env.Settings.ReleaseEdgeBranch != "feat/x" {
		t.Fatalf("settings = %+v, want beta / feat/x", env.Settings)
	}
	if get(t).Settings.ReleaseChannel != ReleaseChannelBeta {
		t.Fatal("the switch to beta did not persist")
	}

	code, _ = patch(t, `{"release_channel":"stable"}`)
	if code != 200 || get(t).Settings.ReleaseChannel != ReleaseChannelStable {
		t.Fatalf("the switch back to stable did not persist: %d", code)
	}
}

// Preview (amendment 48) is a fourth value the migration-0107 CHECK must accept;
// the branch it never uses survives, and a switch away and back round-trips.
func TestPatchReleaseChannelPreview(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)

	if code, _ := patch(t, `{"release_edge_branch":"feat/x"}`); code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	for _, channel := range []string{ReleaseChannelPreview, ReleaseChannelEdge, ReleaseChannelPreview, ReleaseChannelStable} {
		code, env := patch(t, `{"release_channel":"`+channel+`"}`)
		if code != 200 {
			t.Fatalf("switch to %s: status = %d, want 200", channel, code)
		}
		if env.Settings.ReleaseChannel != channel || env.Settings.ReleaseEdgeBranch != "feat/x" {
			t.Fatalf("settings = %+v, want %s / feat/x", env.Settings, channel)
		}
		if got := get(t).Settings.ReleaseChannel; got != channel {
			t.Fatalf("the switch to %s did not persist: %s", channel, got)
		}
	}
}

// release_check NULL follows the channel; a stored value wins over it, and a
// channel switch never changes the stored value.
func TestReleaseCheckPeriodFollowsTheChannel(t *testing.T) {
	pool := testDB(t)
	patch, _ := newSettingsHarness(t, pool)
	store := NewStore(pool)
	ctx := context.Background()
	const day, week = 24 * time.Hour, 7 * 24 * time.Hour

	cases := []struct {
		channel string
		check   *string
		want    time.Duration
	}{
		{ReleaseChannelStable, nil, week},
		{ReleaseChannelBeta, nil, week},
		{ReleaseChannelEdge, nil, week},
		{ReleaseChannelPreview, nil, day},
		{ReleaseChannelPreview, strPtr(ReleaseCheckWeekly), week},
		{ReleaseChannelStable, strPtr(ReleaseCheckDaily), day},
	}
	for _, c := range cases {
		if _, err := pool.Exec(ctx, `UPDATE instance_settings SET release_check = $1`, c.check); err != nil {
			t.Fatalf("set release_check: %v", err)
		}
		if code, _ := patch(t, `{"release_channel":"`+c.channel+`"}`); code != 200 {
			t.Fatalf("switch to %s: %d", c.channel, code)
		}
		got, err := store.ReleaseCheckPeriod(ctx)
		if err != nil {
			t.Fatalf("read period: %v", err)
		}
		if got != c.want {
			t.Errorf("%s with release_check %v: period %v, want %v", c.channel, deref(c.check), got, c.want)
		}
		st, _ := store.Get(ctx)
		if deref(st.ReleaseCheck) != deref(c.check) {
			t.Errorf("the switch to %s changed release_check to %v", c.channel, deref(st.ReleaseCheck))
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE instance_settings SET release_check = 'hourly'`); err == nil {
		t.Error("the release_check CHECK accepted 'hourly'")
	}
}

func strPtr(s string) *string { return &s }

func deref(s *string) string {
	if s == nil {
		return "<null>"
	}
	return *s
}

func TestPatchReleaseValidationFailures(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)

	bad := []string{
		`{"release_channel":"nightly"}`,
		`{"release_channel":"Preview"}`,
		`{"release_channel":"preview "}`,
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
	// An explicit null (Set with no value) is a change, and is audited by name.
	null := Patch{ReleaseCheckSet: true}
	if null.Empty() || strings.Join(null.ChangedKeys(), ",") != "release_check" {
		t.Fatalf("a null release_check: empty=%v keys=%v", null.Empty(), null.ChangedKeys())
	}
}

// release_check (amendment 48): absent leaves it alone, null follows the channel
// again, "daily"/"weekly" store, anything else is 400, and a channel switch
// never touches it. Every change is audited under instance.settings.updated.
func TestPatchReleaseCheck(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)

	if st := get(t).Settings; st.ReleaseCheck != nil {
		t.Fatalf("release_check = %v on a fresh instance, want null", *st.ReleaseCheck)
	}
	code, env := patch(t, `{"release_check":"weekly"}`)
	if code != 200 || deref(env.Settings.ReleaseCheck) != ReleaseCheckWeekly {
		t.Fatalf("set weekly: %d %v", code, deref(env.Settings.ReleaseCheck))
	}

	// Absent is unchanged, and so is a channel switch to the channel whose
	// default is the other period.
	for _, body := range []string{`{"registration_mode":"open"}`, `{"release_channel":"preview"}`} {
		if code, env = patch(t, body); code != 200 || deref(env.Settings.ReleaseCheck) != ReleaseCheckWeekly {
			t.Fatalf("%s changed release_check: %d %v", body, code, deref(env.Settings.ReleaseCheck))
		}
	}
	if period, err := NewStore(pool).ReleaseCheckPeriod(context.Background()); err != nil || period != 7*24*time.Hour {
		t.Fatalf("weekly on preview: period %v, %v", period, err)
	}

	// Explicit null follows the channel again: daily on preview.
	if code, env = patch(t, `{"release_check":null}`); code != 200 || env.Settings.ReleaseCheck != nil {
		t.Fatalf("explicit null: %d %v", code, deref(env.Settings.ReleaseCheck))
	}
	if period, _ := NewStore(pool).ReleaseCheckPeriod(context.Background()); period != 24*time.Hour {
		t.Fatalf("null on preview: period %v, want a day", period)
	}

	// A bad value is refused whole: nothing else in the body applies.
	for _, bad := range []string{`"hourly"`, `"Daily"`, `""`, `7`, `true`} {
		code, _ := patch(t, `{"release_check":`+bad+`,"registration_mode":"closed"}`)
		if code != 400 {
			t.Errorf("release_check %s: status %d, want 400", bad, code)
		}
	}
	if st := get(t).Settings; st.RegistrationMode == "closed" || st.ReleaseCheck != nil {
		t.Fatalf("a refused PATCH applied part of its body: %+v", st)
	}
}
