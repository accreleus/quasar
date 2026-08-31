// host_lifecycle.go — host drain/uncordon and agent connect/disconnect reaping.
package session

import (
	"context"
	"sync"
	"time"
)

// JobReclaimer closes the background-job runs a host's agent was executing when
// it died (#492), returning how many it closed. A func, not an interface,
// because the production implementation (*jobs.Dispatcher) is constructed long
// after the coordinator, so the wiring must capture the variable.
//
// nil is a valid quiet default: an unreclaimed run is bounded by the
// dispatcher's own claim-timeout reaper.
type JobReclaimer func(ctx context.Context, hostID, reason string) (int, error)

// jobReclaimReason is stored in job_runs.error and names the cause, not the
// mechanism. It must read identically to jobs.DefaultReclaimReason, since either
// path can write the row; copied rather than imported because the JobReclaimer
// seam exists to keep this package off internal/jobs. Pinned by
// TestReclaimReasonMatchesTheJobsFallback, which imports jobs in test code only.
const jobReclaimReason = "agent restarted"

// WithJobReclaimer wires the jobs-framework seam used on agent re-register.
func WithJobReclaimer(r JobReclaimer) CoordinatorOption {
	return func(c *Coordinator) { c.jobs = r }
}

// Force-drain fan-out bounds: per-session stops run concurrently under one
// shared deadline, or a wedged agent serializes stopAckTimeout per session and
// the handler blows the HTTP server's 10s WriteTimeout.
const (
	drainStopConcurrency = 8
	drainStopBudget      = 8 * time.Second
)

// DrainHost cordons a host: online → draining, so the scheduler (which places
// only on `online`) stops putting sessions on it. A stable administrative state —
// an admin uncordons it, or an agent disconnect flips it offline. Race-safe by
// construction: a launch that already read status='online' may complete, but any
// pick after the status commits excludes the host.
//
// force=true additionally session_stops every non-terminal session, best-effort
// with the reaper as backstop. Idempotent, still honouring force. Returns
// ErrNotFound or ErrHostNotDrainable (the host is offline).
func (c *Coordinator) DrainHost(ctx context.Context, hostID string, force bool) (Host, error) {
	h, err := c.store.GetHost(ctx, hostID)
	if err != nil {
		return Host{}, err
	}
	switch h.Status {
	case "offline":
		return Host{}, ErrHostNotDrainable
	case "online":
		if err := c.store.SetHostStatus(ctx, hostID, "draining"); err != nil {
			return Host{}, err
		}
		h.Status = "draining"
	case "draining":
		// already cordoned; fall through to honour force
	}

	if force {
		ids, err := c.store.NonTerminalSessionIDsOnHost(ctx, hostID)
		if err != nil {
			return Host{}, err
		}
		// Best-effort: a stop that fails to dispatch is reconciled by the reaper,
		// and the host is draining regardless.
		dctx, cancel := context.WithTimeout(ctx, drainStopBudget)
		defer cancel()
		var wg sync.WaitGroup
		sem := make(chan struct{}, drainStopConcurrency)
		for _, sid := range ids {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				if _, err := c.Stop(dctx, sid, "host_draining"); err != nil {
					c.log.Warn("force-drain stop failed", "host_id", hostID, "session_id", sid, "err", err)
				}
			}()
		}
		wg.Wait()
		c.log.Info("force-drained host", "host_id", hostID, "stopped", len(ids))
	} else {
		c.log.Info("drained host (graceful)", "host_id", hostID)
	}
	return h, nil
}

// UncordonHost returns a draining host to service. A `draining` host always has
// a connected agent (a disconnect flips it offline), so draining IS the
// agent-connected precondition. Idempotent. Returns ErrNotFound or
// ErrHostNotResumable (offline; it returns online on its agent's reconnect).
func (c *Coordinator) UncordonHost(ctx context.Context, hostID string) (Host, error) {
	h, err := c.store.GetHost(ctx, hostID)
	if err != nil {
		return Host{}, err
	}
	switch h.Status {
	case "offline":
		return Host{}, ErrHostNotResumable
	case "online":
	case "draining":
		if err := c.store.SetHostStatus(ctx, hostID, "online"); err != nil {
			return Host{}, err
		}
		h.Status = "online"
		c.log.Info("uncordoned host", "host_id", hostID)
	}
	return h, nil
}

// HostDisconnected reaps a disconnected host's non-terminal sessions to failed,
// releasing their reservations (schema.md invariant #3 — the authority of last
// resort that does not depend on a callback a dead agent can't send).
func (c *Coordinator) HostDisconnected(ctx context.Context, hostID string) {
	// Capture the ids before the reap: ReapHost is a bulk UPDATE with no
	// per-session hook, so this is the only chance to drop their in-memory state.
	ids, idsErr := c.store.NonTerminalSessionIDsOnHost(ctx, hostID)
	if idsErr != nil {
		c.log.Warn("list host sessions before reap failed", "host_id", hostID, "err", idsErr)
	}

	n, err := c.store.ReapHost(ctx, hostID, "host agent connection lost")
	if err != nil {
		c.log.Error("reap host sessions failed", "host_id", hostID, "err", err)
		return
	}
	if n > 0 {
		c.log.Warn("reaped sessions on host disconnect", "host_id", hostID, "count", n)
	}
	for _, sid := range ids {
		c.health.forget(sid)
		c.display.forget(sid)
		c.swapper.forget(sid)        // #405: any orphaned pending swap
		c.forgetTerminalSession(sid) // #402: the relay's buffered frames
	}
}

// AgentReconnected reconciles a host whose agent connected fresh (P2-06). The
// node-agent rebuilds its session state from nothing every connection, so a new
// connection means it is running NONE of the sessions the control plane believes
// are here. Failing them releases each reservation in the same transaction
// rather than leaking capacity to rows stuck at `running`. A no-op on first
// enrollment; the agent sweeps its own orphaned containers on startup.
//
// The same argument closes the host's job runs (#492): left open they hold the
// job_runs_open_per_target single-flight slot until the claim-timeout reaper
// fires (an hour by default), 409ing every "Run now" in the meantime.
func (c *Coordinator) AgentReconnected(ctx context.Context, hostID string) {
	// Capture ids before the bulk reap; see HostDisconnected.
	ids, idsErr := c.store.NonTerminalSessionIDsOnHost(ctx, hostID)
	if idsErr != nil {
		c.log.Warn("list host sessions before reconcile failed", "host_id", hostID, "err", idsErr)
	}

	n, err := c.store.ReapHost(ctx, hostID, "agent reconnected; prior sessions not recovered")
	if err != nil {
		c.log.Error("reconcile host sessions failed", "host_id", hostID, "err", err)
		// The reconcile is the load-bearing half (it releases GPU reservations),
		// but the job reclaim still runs: a wedged job on a host whose reap failed
		// is a second problem, not a consequence.
		c.reclaimHostJobRuns(ctx, hostID)
		return
	}
	if n > 0 {
		c.log.Warn("reconciled stale sessions on agent reconnect", "host_id", hostID, "count", n)
	}
	for _, sid := range ids {
		c.health.forget(sid)
		c.display.forget(sid)
		c.swapper.forget(sid)        // #405: any orphaned pending swap
		c.forgetTerminalSession(sid) // #402: the relay's buffered frames
	}
	c.reclaimHostJobRuns(ctx, hostID)
}

// reclaimHostJobRuns closes this host's orphaned job runs (#492). Best-effort
// and always LAST: it returns no error upward and runs after the session
// reconcile, so a jobs framework that is absent, disabled or briefly unreachable
// cannot delay a single session transition. The jobs side owns the per-run log.
func (c *Coordinator) reclaimHostJobRuns(ctx context.Context, hostID string) {
	if c.jobs == nil {
		return
	}
	n, err := c.jobs(ctx, hostID, jobReclaimReason)
	if err != nil {
		c.log.Warn("reclaim host job runs failed", "host_id", hostID, "err", err)
		return
	}
	if n > 0 {
		c.log.Warn("reclaimed orphaned job runs on agent reconnect", "host_id", hostID, "count", n)
	}
}
