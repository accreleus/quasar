package session

import (
	"context"
	"time"
)

// DefaultSessionGrace is how long a host may be silent before its sessions are
// given up on (#128). It must exceed the agent's own grace plus its maximum
// reconnect backoff, or the control plane terminalises sessions the agent is
// still holding and would have re-reported.
const DefaultSessionGrace = 120 * time.Second

// sweepStaleHosts fails the sessions of hosts whose agent is neither connected
// nor recently heard from (#128).
//
// This is the backstop that lets HostDisconnected and AgentReconnected stop
// reaping `running` rows. Without it a host that never comes back would hold its
// sessions — and their GPU reservations — forever.
//
// The deadline is measured from max(last_heartbeat_at, bootedAt). The boot term
// is load-bearing: a control plane restarting after a quiet period would
// otherwise find every host "silent for hours" and reap every session in its
// first tick, before any agent's ~2 s reconnect could land. That is the very
// failure this issue is about, reintroduced from the other side.
//
// Everything non-terminal goes, not just `running`: on a host that is genuinely
// gone, an `assigned`/`starting` row's launch goroutine died with the old
// process and nothing else will ever finish it.
func (c *Coordinator) sweepStaleHosts(ctx context.Context, bootedAt time.Time, grace time.Duration) {
	if c.agents == nil {
		// Unwired connectivity would make every host look disconnected. A
		// backstop that cannot tell must never reap.
		c.log.Warn("stale sweep: agent connectivity not wired; skipping")
		return
	}
	hosts, err := c.store.HostsWithActiveSessions(ctx)
	if err != nil {
		c.log.Warn("stale sweep: host list failed", "err", err)
		return
	}
	for _, h := range hosts {
		if c.agents.IsConnected(h.ID) {
			continue
		}
		last := bootedAt
		if h.LastHeartbeat != nil && h.LastHeartbeat.After(bootedAt) {
			last = *h.LastHeartbeat
		}
		if time.Since(last) < grace {
			continue
		}
		ids, err := c.store.NonTerminalSessionIDsOnHost(ctx, h.ID)
		if err != nil {
			c.log.Warn("stale sweep: session list failed", "host_id", h.ID, "err", err)
			continue
		}
		if len(ids) == 0 {
			continue
		}
		c.log.Warn("stale sweep: host silent past the grace window; failing its sessions",
			"host_id", h.ID, "node_name", h.NodeName, "sessions", len(ids))
		for _, sid := range ids {
			detail := "host_lost"
			c.failSessionWithDetail(sid, "host agent did not return within the grace window", &detail)
		}
		// The scheduler filters on status, and a control-plane restart never
		// stamps a host offline (agentws/store.go), so a dead host would keep
		// attracting placements that fail at dispatch.
		if h.Status == "online" {
			if err := c.store.SetHostStatus(ctx, h.ID, "offline"); err != nil {
				c.log.Warn("stale sweep: mark offline failed", "host_id", h.ID, "err", err)
			}
		}
	}
}

// RunStaleSweep ticks sweepStaleHosts until ctx is cancelled. bootedAt is this
// process's start, captured by the caller before any agent could reconnect.
func (c *Coordinator) RunStaleSweep(ctx context.Context, bootedAt time.Time, grace time.Duration) {
	if grace <= 0 {
		grace = DefaultSessionGrace
	}
	tick := grace / 4
	if tick < 5*time.Second {
		tick = 5 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sweepStaleHosts(ctx, bootedAt, grace)
		}
	}
}
