package session

// compat_tools_db_test.go — amendment 56 (#1066): the compat_tools field on session_assign, from
// the real heartbeat path to the dispatcher. Semantics: control-api.md §Compatibility tools.

import (
	"context"
	"encoding/json"
	"reflect"
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
		spec := f.coord.compatToolsFor(f.steam)
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
	if got := f.coord.compatToolsFor(f.appID); got != nil {
		t.Errorf("a non-Steam app got compat_tools %+v", got)
	}
	if got := f.coord.compatToolsFor(f.steam); got == nil {
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
	got := f.coord.compatToolsFor(f.steam)
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
	out := f.coord.fitCompatTools(in, compatEpoch{})
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the record write blocked for %v, want it cut at the timeout", d)
	}
	if out.CompatTools == nil {
		t.Error("the assign lost compat_tools because the record write was slow")
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
