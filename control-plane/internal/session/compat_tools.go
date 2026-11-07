package session

import (
	"context"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/compat"
)

// compatReadTimeout bounds each launch-path compat query; slowness fails open like an error.
// A var so a test can shorten it.
var compatReadTimeout = 2 * time.Second

// compatToolsFor is the session_assign.compat_tools of a Steam-family session, nil for any other
// app. Fail-open: a failed read sends nothing and the session gets Steam's own default.
// semantics: control-api.md §Compatibility tools, "What the session receives".
func (c *Coordinator) compatToolsFor(appID, hostID string) *agentws.CompatToolsSpec {
	if c.store == nil || c.store.pool == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(c.ctx, compatReadTimeout)
	defer cancel()
	app, err := c.store.GetLaunchApp(ctx, appID)
	if err != nil || !app.SteamFamily {
		return nil
	}
	// Advisory: the spec that goes on the wire is read again under the tool rows' share locks
	// by fitCompatTools, once the connection is known to take it.
	entries, mount, err := compat.NewStore(c.store.pool).Entries(ctx, hostID)
	if err != nil {
		c.log.Warn("compat tools: reading the mapping failed; the session gets Steam's default", "app_id", appID, "err", err)
		return nil
	}
	return compatSpec(entries, mount)
}

func compatSpec(entries []compat.Entry, mount []string) *agentws.CompatToolsSpec {
	spec := &agentws.CompatToolsSpec{Mount: append([]string{}, mount...), Entries: make([]agentws.CompatToolEntry, 0, len(entries))}
	for _, e := range entries {
		spec.Entries = append(spec.Entries, agentws.CompatToolEntry{AppID: e.AppID, Name: e.Name, Forced: e.Forced})
	}
	return spec
}

// fitCompatTools keeps compat_tools only on a connection that advertised compat_tools_v1, re-reads
// it under the tool rows' share locks and records the mounted names on the session in the same
// transaction (compat.Store.Assign). Fail-open: a failed read sends nothing, records nothing, and
// the session gets Steam's own default.
func (c *Coordinator) fitCompatTools(a agentws.SessionAssignCmd, epoch agentws.HomeCommandEpoch, hostID string) agentws.SessionAssignCmd {
	if a.CompatTools == nil {
		return a
	}
	if e, ok := epoch.(interface{ SupportsCompatTools() bool }); !ok || !e.SupportsCompatTools() {
		a.CompatTools = nil
		return a
	}
	ctx, cancel := context.WithTimeout(c.ctx, compatReadTimeout)
	defer cancel()
	entries, mount, err := compat.NewStore(c.store.pool).Assign(ctx, a.SessionID, hostID)
	if err != nil {
		c.log.Warn("compat tools: reading the mapping under lock failed; the session gets Steam's default", "session_id", a.SessionID, "err", err)
		a.CompatTools = nil
		return a
	}
	a.CompatTools = compatSpec(entries, mount)
	return a
}
