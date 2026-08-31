package agentws

import "context"

// Events is the callback surface the agent WS handler invokes on upstream
// activity. The session coordinator (package session) implements it. Defining it
// here (rather than importing session) keeps the dependency one-way: session
// imports agentws to build commands and dispatch via the Registry; agentws never
// imports session.
type Events interface {
	// AgentState handles an agent's session_state callback (authoritative
	// lifecycle progress) for a session on the given host.
	AgentState(ctx context.Context, hostID string, m SessionStateMsg)
	// HostDisconnected fires when a host's agent connection is lost: the
	// coordinator reaps that host's non-terminal sessions (schema.md invariant #3).
	HostDisconnected(ctx context.Context, hostID string)
	// AgentReconnected fires when a host's agent (re)connects fresh: the
	// coordinator reconciles by failing the stale sessions the restarted agent is
	// no longer running, releasing their reservations (P2-06).
	AgentReconnected(ctx context.Context, hostID string)
	// AgentMetrics handles an agent's session_metrics telemetry sample (P4-01).
	// Fire-and-forget: the coordinator validates the session belongs to this host
	// (the same trust boundary as AgentState) and, on match, persists a
	// source='agent' row. It never affects session state, and a host mismatch or
	// malformed sample is dropped, never fataling the connection.
	AgentMetrics(ctx context.Context, hostID string, m SessionMetricsMsg)
	// AgentTraceEvent handles an agent-emitted session_trace_event (ST-03).
	// Fire-and-forget: the coordinator validates host ownership (the same
	// GetSessionHostState boundary as AgentMetrics) and writes a source='agent'
	// row via the trace store. A cross-host, not-running, or unknown session is
	// dropped; a failed insert is best-effort and never fatals the WS.
	AgentTraceEvent(ctx context.Context, hostID string, m SessionTraceEventMsg)
	// AgentSignalingFailure terminates only the affected session when its reliable
	// agent-to-browser relay cannot make progress. It must not disconnect the host.
	AgentSignalingFailure(ctx context.Context, hostID, sessionID, reason string)
	// LaunchConsoleSession launches a pinned console-mode session on hostID,
	// owned by userID, running appID (CM-06 auto-start on display hotplug).
	// Returns the launched session id.
	LaunchConsoleSession(ctx context.Context, hostID, userID, appID, videoTopology string, width, height, fps int32) (string, error)
	// StopConsoleSession stops a previously auto-started console session
	// (CM-06 auto-stop on display disconnect).
	StopConsoleSession(ctx context.Context, sessionID, reason string) error
	// ConsoleSessionActive reports whether a recorded auto-started console session
	// is still non-terminal (CM-06). The auto-start tracker uses it to detect a
	// self-terminated console session and relaunch it (level-triggered always-on).
	ConsoleSessionActive(ctx context.Context, sessionID string) bool
}

// noopEvents is used when no coordinator is wired (e.g. focused tests).
type noopEvents struct{}

func (noopEvents) AgentState(context.Context, string, SessionStateMsg)           {}
func (noopEvents) HostDisconnected(context.Context, string)                      {}
func (noopEvents) AgentReconnected(context.Context, string)                      {}
func (noopEvents) AgentMetrics(context.Context, string, SessionMetricsMsg)       {}
func (noopEvents) AgentTraceEvent(context.Context, string, SessionTraceEventMsg) {}
func (noopEvents) AgentSignalingFailure(context.Context, string, string, string) {}
func (noopEvents) LaunchConsoleSession(context.Context, string, string, string, string, int32, int32, int32) (string, error) {
	return "", nil
}
func (noopEvents) StopConsoleSession(context.Context, string, string) error { return nil }
func (noopEvents) ConsoleSessionActive(context.Context, string) bool        { return false }
