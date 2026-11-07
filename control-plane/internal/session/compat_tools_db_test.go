package session

// compat_tools_db_test.go — amendment 56 (#1066): the compat_tools field on session_assign, from
// the real heartbeat path to the dispatcher. Semantics: control-api.md §Compatibility tools.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/compat"
)

// compatFixture is a warm-Steam fixture with the Valve catalogue, a default and one mapping.
func compatFixture(t *testing.T, supports bool) *warmFixture {
	t.Helper()
	f := newWarmFixture(t, 4)
	f.disp.compat = supports
	ctx := context.Background()
	must(t, compat.NewStore(f.pool).Reconcile(ctx))
	must(t, exec(t, f.pool, `DELETE FROM compat_tool_mappings`))
	must(t, exec(t, f.pool, `INSERT INTO instance_settings (id) VALUES (true) ON CONFLICT DO NOTHING`))
	must(t, exec(t, f.pool, `UPDATE instance_settings SET compat_tool_default_id =
		(SELECT id FROM compat_tools WHERE steam_name = 'proton_10') WHERE id = true`))
	must(t, exec(t, f.pool, `INSERT INTO compat_tool_mappings (provider, external_id, tool_id, forced)
		SELECT 'steam', '1145360', id, true FROM compat_tools WHERE steam_name = 'proton_hotfix'`))
	t.Cleanup(func() {
		_, _ = f.pool.Exec(ctx, `UPDATE instance_settings SET compat_tool_default_id = NULL`)
		_, _ = f.pool.Exec(ctx, `DELETE FROM compat_tool_mappings`)
		_, _ = f.pool.Exec(ctx, `UPDATE compat_tools SET enabled = true WHERE family = 'valve'`)
	})
	return f
}

func parkedAssign(t *testing.T, f *warmFixture) (agentws.SessionAssignCmd, string) {
	t.Helper()
	f.coord.SteamWarmHeartbeat(context.Background(), f.userID, false, f.deviceID)
	id := f.parkedID(t)
	if id == "" {
		t.Fatal("heartbeat created no parked session")
	}
	return f.disp.assignFor(t, id), id
}

func TestAssignCarriesTheEffectiveMappingToACapableAgent(t *testing.T) {
	f := compatFixture(t, true)
	a, id := parkedAssign(t, f)

	want := &agentws.CompatToolsSpec{Mount: []string{}, Entries: []agentws.CompatToolEntry{
		{AppID: "0", Name: "proton_10"},
		{AppID: "1145360", Name: "proton_hotfix", Forced: true},
	}}
	if !reflect.DeepEqual(a.CompatTools, want) {
		t.Errorf("assign compat_tools = %+v, want %+v", a.CompatTools, want)
	}
	raw, _ := json.Marshal(a)
	var wire struct {
		CompatTools struct {
			Mount json.RawMessage `json:"mount"`
		} `json:"compat_tools"`
	}
	must(t, json.Unmarshal(raw, &wire))
	if string(wire.CompatTools.Mount) != "[]" {
		t.Errorf("mount on the wire = %s, want []", wire.CompatTools.Mount)
	}
	var names []string
	var isNull bool
	must(t, f.pool.QueryRow(context.Background(),
		`SELECT compat_tool_names, compat_tool_names IS NULL FROM sessions WHERE id = $1::uuid`, id).Scan(&names, &isNull))
	if isNull || len(names) != 0 {
		t.Errorf("compat_tool_names = %v (null=%v), want an empty array", names, isNull)
	}
}

func TestAssignOmitsCompatToolsForAnAgentWithoutTheCapability(t *testing.T) {
	f := compatFixture(t, false)
	a, id := parkedAssign(t, f)
	if a.CompatTools != nil {
		t.Errorf("assign compat_tools = %+v for an agent that did not advertise compat_tools_v1", a.CompatTools)
	}
	var isNull bool
	must(t, f.pool.QueryRow(context.Background(),
		`SELECT compat_tool_names IS NULL FROM sessions WHERE id = $1::uuid`, id).Scan(&isNull))
	if !isNull {
		t.Error("compat_tool_names recorded for a session that was sent none")
	}
}

func TestAssignLeavesOutADisabledTool(t *testing.T) {
	f := compatFixture(t, true)
	must(t, exec(t, f.pool, `UPDATE compat_tools SET enabled = false WHERE steam_name = 'proton_10'`))
	a, _ := parkedAssign(t, f)
	if a.CompatTools == nil || len(a.CompatTools.Entries) != 1 || a.CompatTools.Entries[0].AppID != "1145360" {
		t.Errorf("entries = %+v, want only the mapping: the disabled default must be left out", a.CompatTools)
	}
}

// A mapping set through the admin store reaches the launch as the game's appid entry, forced or
// not, whichever Steam-family app is launched: it is keyed by appid, not by a tile row.
func TestAdminMappingReachesTheLaunchForcedOrNot(t *testing.T) {
	f := compatFixture(t, true)
	ctx := context.Background()
	var admin, hotfix string
	must(t, f.pool.QueryRow(ctx, `SELECT id::text FROM users LIMIT 1`).Scan(&admin))
	must(t, f.pool.QueryRow(ctx, `SELECT id::text FROM compat_tools WHERE steam_name = 'proton_hotfix'`).Scan(&hotfix))
	entry := func(forced bool) agentws.CompatToolEntry {
		t.Helper()
		_, _, _, _, err := compat.NewStore(f.pool).SetMapping(ctx, "480", hotfix, forced, admin)
		must(t, err)
		spec := f.coord.compatToolsFor(f.steam, f.hostID)
		for _, e := range spec.Entries {
			if e.AppID == "480" {
				return e
			}
		}
		t.Fatalf("no entry for appid 480 in %+v", spec.Entries)
		return agentws.CompatToolEntry{}
	}
	if e := entry(false); e.Name != "proton_hotfix" || e.Forced {
		t.Errorf("unforced mapping sent as %+v", e)
	}
	if e := entry(true); e.Name != "proton_hotfix" || !e.Forced {
		t.Errorf("forced mapping sent as %+v", e)
	}
}

func TestCompatToolsOnlyForTheSteamFamily(t *testing.T) {
	f := compatFixture(t, true)
	if got := f.coord.compatToolsFor(f.appID, f.hostID); got != nil {
		t.Errorf("a non-Steam app got compat_tools %+v", got)
	}
	if got := f.coord.compatToolsFor(f.steam, f.hostID); got == nil {
		t.Error("the Steam parent got no compat_tools")
	}
}

// lockedFor holds an exclusive lock from another connection until the test ends.
func lockedFor(t *testing.T, f *warmFixture, sql string) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	must(t, err)
	if _, err := tx.Exec(ctx, sql); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
}

func shortCompatTimeout(t *testing.T) {
	t.Helper()
	old := compatReadTimeout
	compatReadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { compatReadTimeout = old })
}

func TestASlowMappingReadFailsOpenWithoutBlocking(t *testing.T) {
	f := compatFixture(t, true)
	shortCompatTimeout(t)
	lockedFor(t, f, `LOCK TABLE compat_tool_mappings IN ACCESS EXCLUSIVE MODE`)

	start := time.Now()
	got := f.coord.compatToolsFor(f.steam, f.hostID)
	if got != nil {
		t.Errorf("compat_tools = %+v after a read that timed out, want none", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the read blocked for %v, want it cut at the timeout", d)
	}
}

type compatEpoch struct{ agentws.HomeCommandEpoch }

func (compatEpoch) SupportsCompatTools() bool { return true }

func TestASlowRecordWriteDoesNotBlockTheAssign(t *testing.T) {
	f := compatFixture(t, true)
	_, id := parkedAssign(t, f)
	shortCompatTimeout(t)
	lockedFor(t, f, `SELECT 1 FROM sessions WHERE id = '`+id+`'::uuid FOR UPDATE`)

	in := agentws.SessionAssignCmd{SessionID: id, CompatTools: &agentws.CompatToolsSpec{Mount: []string{}}}
	start := time.Now()
	out, _ := f.coord.fitCompatTools(in, compatEpoch{}, f.hostID)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the record write blocked for %v, want it cut at the timeout", d)
	}
	if out.CompatTools != nil {
		t.Errorf("compat_tools = %+v after a record that timed out, want none: nothing is mounted unrecorded", out.CompatTools)
	}
}

// installGE enables the shipped GE tool and installs it on f's host; it returns the tool's Steam name.
func installGE(t *testing.T, f *warmFixture) string {
	t.Helper()
	var name, tag, sha string
	must(t, f.pool.QueryRow(context.Background(), `SELECT steam_name, tag, asset_sha512 FROM compat_tools WHERE family = 'ge' LIMIT 1`).
		Scan(&name, &tag, &sha))
	must(t, exec(t, f.pool, `UPDATE compat_tools SET enabled = true, first_enabled_at = now() WHERE steam_name = '`+name+`'`))
	must(t, exec(t, f.pool, `DELETE FROM host_compat_tools`))
	must(t, exec(t, f.pool, `INSERT INTO host_compat_tools (host_id, name, tag, sha512, bytes) VALUES ('`+f.hostID+`'::uuid, '`+name+`', '`+tag+`', '`+sha+`', 5)`))
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM host_compat_tools; UPDATE compat_tools SET enabled = false WHERE family = 'ge'`)
	})
	return name
}

func recordedNames(t *testing.T, f *warmFixture, id string) []string {
	t.Helper()
	var names []string
	must(t, f.pool.QueryRow(context.Background(), `SELECT compat_tool_names FROM sessions WHERE id = $1::uuid`, id).Scan(&names))
	return names
}

// The mount is decided and recorded in one transaction that holds the tool rows' share locks
// (control-api.md §Usable): a force-remove that locked the row first leaves the tool out, and one
// that comes after finds the name recorded.
func TestAForceRemoveLockedFirstLeavesTheToolOutOfTheMount(t *testing.T) {
	f := compatFixture(t, true)
	name := installGE(t, f)
	_, id := parkedAssign(t, f)
	if got := recordedNames(t, f, id); len(got) != 1 || got[0] != name {
		t.Fatalf("recorded names = %v, want [%s] on an installed, enabled tool", got, name)
	}
	spec := f.coord.compatToolsFor(f.steam, f.hostID) // the advisory read still sees the tool
	if spec == nil || len(spec.Mount) != 1 {
		t.Fatalf("advisory spec = %+v, want the tool mounted", spec)
	}

	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	must(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `SELECT 1 FROM compat_tools WHERE steam_name = $1 FOR UPDATE`, name)
	must(t, err)

	done := make(chan agentws.SessionAssignCmd, 1)
	go func() {
		out, _ := f.coord.fitCompatTools(agentws.SessionAssignCmd{SessionID: id, CompatTools: spec}, compatEpoch{}, f.hostID)
		done <- out
	}()
	select {
	case <-done:
		t.Fatal("the launch read the mount without waiting for the row lock held by the disable")
	case <-time.After(300 * time.Millisecond):
	}
	_, err = tx.Exec(ctx, `UPDATE compat_tools SET enabled = false WHERE steam_name = $1`, name)
	must(t, err)
	must(t, tx.Commit(ctx))

	out := <-done
	if out.CompatTools == nil || len(out.CompatTools.Mount) != 0 {
		t.Errorf("mount = %+v, want the force-removed tool left out", out.CompatTools)
	}
	if got := recordedNames(t, f, id); len(got) != 0 {
		t.Errorf("recorded names = %v, want none: the tool was never mounted", got)
	}
}

// A failing compat read leaves the launch going through, with no compat_tools.
func TestAFailingCompatReadLeavesTheLaunchGoingThrough(t *testing.T) {
	f := compatFixture(t, true)
	must(t, exec(t, f.pool, `ALTER TABLE compat_tool_mappings RENAME TO compat_tool_mappings_gone`))
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `ALTER TABLE compat_tool_mappings_gone RENAME TO compat_tool_mappings`)
	})

	a, id := parkedAssign(t, f)
	if a.CompatTools != nil {
		t.Errorf("compat_tools = %+v although the read failed", a.CompatTools)
	}
	f.disp.waitFor(t, "start")
	var state string
	must(t, f.pool.QueryRow(context.Background(), `SELECT state FROM sessions WHERE id = $1::uuid`, id).Scan(&state))
	if state == "failed" || state == "stopped" {
		t.Errorf("the session is %s, want the launch to go on", state)
	}
}

// An adoption keeps what the parked Steam mounted when it started: the claim copies the names, and
// the fit neither re-reads nor overwrites them (control-api.md §Warm Steam, claim).
func TestAnAdoptionKeepsTheParkedSessionsMountedNames(t *testing.T) {
	f := newSupersedeFixture(t)
	ctx := context.Background()
	must(t, exec(t, f.pool, `UPDATE sessions SET compat_tool_names = ARRAY['GE-Proton9-1'] WHERE id = $1::uuid`, f.parkedA))
	tx, err := f.pool.Begin(ctx)
	must(t, err)
	must(t, adoptRuntime(ctx, tx, f.parkedA, f.play))
	must(t, tx.Commit(ctx))

	spec := f.coord.compatToolsFor(f.steam, f.hostID)
	out, err := f.coord.fitCompatTools(agentws.SessionAssignCmd{SessionID: f.play, AdoptSessionID: f.parkedA, CompatTools: spec}, compatEpoch{}, f.hostID)
	must(t, err)
	if out.CompatTools == nil {
		t.Error("the adopting assign lost compat_tools")
	}
	if got := recordedNames(t, f.warmFixture, f.play); len(got) != 1 || got[0] != "GE-Proton9-1" {
		t.Errorf("adopting session names = %v, want the parked session's [GE-Proton9-1], though that tool is not mounted by a fresh read", got)
	}
}

// The other order: the launch has recorded the tool, so force-remove's stop step finds the session.
func TestAForceRemoveAfterTheLaunchRecordedStopsThatSession(t *testing.T) {
	f := newSupersedeFixture(t)
	ctx := context.Background()
	name := installGE(t, f.warmFixture)
	spec := f.coord.compatToolsFor(f.steam, f.hostID)
	_, err := f.coord.fitCompatTools(agentws.SessionAssignCmd{SessionID: f.play, CompatTools: spec}, compatEpoch{}, f.hostID)
	must(t, err)
	if got := recordedNames(t, f.warmFixture, f.play); len(got) != 1 || got[0] != name {
		t.Fatalf("recorded names = %v, want [%s]", got, name)
	}
	var geID string
	must(t, f.pool.QueryRow(ctx, `SELECT id::text FROM compat_tools WHERE steam_name = $1`, name).Scan(&geID))
	var stopped []string
	f.cs.Stopper = func(_ context.Context, id string) error { stopped = append(stopped, id); return nil }
	res, err := f.cs.ForceRemove(ctx, geID)
	must(t, err)
	if len(stopped) != 1 || stopped[0] != f.play || len(res.StoppedIDs) != 1 || res.StoppedIDs[0] != f.play {
		t.Errorf("stopped %v, result %v, want exactly the launch that recorded the tool (%s)", stopped, res.StoppedIDs, f.play)
	}
}

// The game's mapped GE tool is disabled between placement and the assign: the launch is refused
// (control-api.md §Placement: never placed for a tool it will not be given), not started without it.
func TestALaunchWhoseMappedToolWasDisabledAfterPlacementIsRefused(t *testing.T) {
	f := newSupersedeFixture(t)
	name := installGE(t, f.warmFixture)
	must(t, exec(t, f.pool, `UPDATE apps SET external_source = 'steam', external_id = '1145360' WHERE id = (SELECT app_id FROM sessions WHERE id = $1::uuid)`, f.play))
	must(t, exec(t, f.pool, `DELETE FROM compat_tool_mappings`))
	must(t, exec(t, f.pool, `INSERT INTO compat_tool_mappings (provider, external_id, tool_id)
		SELECT 'steam', '1145360', id FROM compat_tools WHERE steam_name = $1`, name))
	spec := f.coord.compatToolsFor(f.steam, f.hostID)
	if spec == nil || len(spec.Mount) != 1 {
		t.Fatalf("advisory spec = %+v, want the tool mounted (it was usable at placement)", spec)
	}
	// Disabled after placement; a mapping may name a disabled tool only by this race.
	must(t, exec(t, f.pool, `UPDATE compat_tools SET enabled = false WHERE steam_name = $1`, name))

	in := agentws.SessionAssignCmd{SessionID: f.play, CompatTools: spec}
	var missing *compat.ToolMissingError
	if _, err := f.coord.fitCompatTools(in, compatEpoch{}, f.hostID); !errors.As(err, &missing) {
		t.Fatalf("fit = %v, want a ToolMissingError", err)
	}
	if got := recordedNames(t, f.warmFixture, f.play); len(got) != 0 {
		t.Errorf("recorded names = %v for a refused launch", got)
	}
}

// Through the dispatch: the launch was placed while the game's mapped GE tool was usable, then the
// tool was disabled before the assign was built. The session fails before the agent is told
// anything; it never runs without the tool.
func TestALaunchMappedToAnUnusableToolFailsBeforeTheAgent(t *testing.T) {
	f := compatFixture(t, true)
	name := installGE(t, f)
	must(t, exec(t, f.pool, `UPDATE apps SET external_source = 'steam', external_id = '1145360' WHERE id = $1::uuid`, f.steam))
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `UPDATE apps SET external_source = '', external_id = '' WHERE id = $1::uuid`, f.steam) })
	must(t, exec(t, f.pool, `DELETE FROM compat_tool_mappings`))
	must(t, exec(t, f.pool, `INSERT INTO compat_tool_mappings (provider, external_id, tool_id)
		SELECT 'steam', '1145360', id FROM compat_tools WHERE steam_name = $1`, name))
	p := compatLaunch(f.seedIDs, "1145360")
	p.AppID = f.steam
	sess, err := f.store.ScheduleAndCreate(context.Background(), p)
	must(t, err) // placed: the host holds the tool
	must(t, exec(t, f.pool, `UPDATE compat_tools SET enabled = false WHERE steam_name = $1`, name))
	f.coord.dispatchAssignStart(sess, []byte(`{}`), nil)
	res := LaunchResult{Session: sess}
	deadline := time.Now().Add(5 * time.Second)
	var row rowState
	for time.Now().Before(deadline) {
		if row = readRow(t, f.pool, res.Session.ID); row.State == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var msg *string
	must(t, f.pool.QueryRow(context.Background(), `SELECT error_message FROM sessions WHERE id = $1::uuid`, res.Session.ID).Scan(&msg))
	if row.State != "failed" || msg == nil || !strings.Contains(*msg, "compat_tool_missing") {
		t.Fatalf("session = %+v, error %v, want failed with compat_tool_missing", row, msg)
	}
	f.disp.mu.Lock()
	defer f.disp.mu.Unlock()
	for _, a := range f.disp.assigns {
		if a.SessionID == res.Session.ID {
			t.Error("the agent was sent an assign for a launch whose tool is missing")
		}
	}
}
