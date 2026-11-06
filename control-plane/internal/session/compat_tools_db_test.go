package session

// compat_tools_db_test.go — amendment 56 (#1066): the compat_tools field on session_assign, from
// the real heartbeat path to the dispatcher. Semantics: control-api.md §Compatibility tools.

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

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

func TestCompatToolsOnlyForTheSteamFamily(t *testing.T) {
	f := compatFixture(t, true)
	if got := f.coord.compatToolsFor(f.appID); got != nil {
		t.Errorf("a non-Steam app got compat_tools %+v", got)
	}
	if got := f.coord.compatToolsFor(f.steam); got == nil {
		t.Error("the Steam parent got no compat_tools")
	}
}
