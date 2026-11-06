package session

import (
	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/compat"
)

// compatToolsFor is the session_assign.compat_tools of a Steam-family session, nil for any other
// app. Fail-open: a failed read sends nothing and the session gets Steam's own default.
// semantics: control-api.md §Compatibility tools, "What the session receives".
func (c *Coordinator) compatToolsFor(appID string) *agentws.CompatToolsSpec {
	if c.store == nil || c.store.pool == nil {
		return nil
	}
	app, err := c.store.GetLaunchApp(c.ctx, appID)
	if err != nil || !app.SteamFamily {
		return nil
	}
	entries, err := compat.NewStore(c.store.pool).Entries(c.ctx)
	if err != nil {
		c.log.Warn("compat tools: reading the mapping failed; the session gets Steam's default", "app_id", appID, "err", err)
		return nil
	}
	spec := &agentws.CompatToolsSpec{Mount: []string{}, Entries: make([]agentws.CompatToolEntry, 0, len(entries))}
	for _, e := range entries {
		spec.Entries = append(spec.Entries, agentws.CompatToolEntry{AppID: e.AppID, Name: e.Name, Forced: e.Forced})
	}
	return spec
}

// fitCompatTools keeps compat_tools only on a connection that advertised compat_tools_v1 and
// records the mounted names on the session.
func (c *Coordinator) fitCompatTools(a agentws.SessionAssignCmd, epoch agentws.HomeCommandEpoch) agentws.SessionAssignCmd {
	if a.CompatTools == nil {
		return a
	}
	if e, ok := epoch.(interface{ SupportsCompatTools() bool }); !ok || !e.SupportsCompatTools() {
		a.CompatTools = nil
		return a
	}
	if _, err := c.store.pool.Exec(c.ctx, `UPDATE sessions SET compat_tool_names = $2::text[] WHERE id = $1::uuid`,
		a.SessionID, a.CompatTools.Mount); err != nil {
		c.log.Warn("compat tools: recording the mounted names failed", "session_id", a.SessionID, "err", err)
	}
	return a
}
