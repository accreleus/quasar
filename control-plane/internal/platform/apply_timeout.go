package platform

import (
	"context"
	"strings"
	"time"
)

// What a host apply that ended on its deadline tells the operator (#201).
//
// The updater writes the verdict and it travels home over the agent's socket.
// When the new agent fails its health wait and the ADR 0004 restore fails too,
// no agent comes back to carry it, so the attempt can only expire — and a bare
// `timeout` hides a diagnosis the host already holds. These strings say where
// it is. Not a second relay path: the updater is host-local by design.

// applyNotSentOutput is the timeout with no agent AND nothing sent: no updater
// has a result for this attempt, so naming the read command would send an
// operator after a 404.
const applyNotSentOutput = "This apply expired without ever being sent: the host's agent was not connected to " +
	"the control plane. Nothing on this host was pulled, recreated or changed, and its updater has no " +
	"result for this attempt.\n\n" +
	"Bring the agent back — Fleet ▸ Hosts shows when it was last seen — and apply again."

// applyTimeoutOutput composes the `output` of a host attempt failed as
// ReasonTimeout. "" means say nothing new: the agent is on the wire, so the
// missing verdict is not a relay failure.
//
// sent is whether a `release_apply` was accepted by the agent — only then can an
// updater have a result to read. requestID is "" when none was minted.
func applyTimeoutOutput(agentConnected, sent bool, requestID, socket string) string {
	if agentConnected {
		return ""
	}
	if !sent || requestID == "" {
		return applyNotSentOutput
	}
	if socket == "" {
		socket = UpdaterSocketPath
	}
	var b strings.Builder
	b.WriteString("This apply expired without a verdict and the host's agent has not come back, so the ")
	b.WriteString("updater's own result could not be relayed to the control plane.\n\n")
	b.WriteString("That most often means the new container failed its health wait and the updater's ")
	b.WriteString("automatic restore of the previous one (ADR 0004) failed too. It is not proof: a ")
	b.WriteString("restore still running when the deadline fell, a host that lost power part-way ")
	b.WriteString("through one, an agent stopped by hand and a host off the network all look the same ")
	b.WriteString("from here. If this host is back in Fleet ▸ Hosts, the restore finished after the ")
	b.WriteString("apply gave up.\n\n")
	b.WriteString("The verdict is on that host — the real reason, the failed container's last log lines, ")
	b.WriteString("and the `previous` digests to put back by hand. Read it in the stack directory there. ")
	b.WriteString("Ask the updater container, not the node agent: the node agent is the one that is down.\n\n")
	b.WriteString("  docker compose exec quasar-updater curl -s --unix-socket ")
	b.WriteString(socket)
	b.WriteString(" http://u/v1/results/")
	b.WriteString(requestID)
	b.WriteString("\n\n")
	b.WriteString("`docker compose logs quasar-updater` carries the same verdict, and says whether the ")
	b.WriteString("restore finished.")
	return b.String()
}

// timeoutOutput is this attempt's expiry, as prose. The connection is read here
// rather than remembered from the send: whether an agent came back is only
// knowable now. A nil Connected reads as connected, the rule waitConnected uses.
func (r *Runner) timeoutOutput(attemptID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a, err := r.store.Attempt(ctx, attemptID)
	if err != nil {
		r.log.Warn("apply: could not re-read the attempt for the timeout hint",
			"attempt_id", attemptID, "err", err)
		return ""
	}
	if a.HostID == nil {
		return ""
	}
	if r.deps.Connected == nil || r.deps.Connected(*a.HostID) {
		return ""
	}
	requestID, err := r.store.AttemptRequestID(ctx, attemptID)
	if err != nil {
		// A command with an empty id is worse than no command.
		r.log.Warn("apply: could not read the request id for the timeout hint",
			"attempt_id", attemptID, "err", err)
		return ""
	}
	// MintRequestID writes `pending` BEFORE the send, so a row still in it may
	// never have been handed over: a restart inside the connect/ack window is
	// re-adopted straight into watch. Any state the agent relayed has moved the
	// row off `pending`, so reading `pending` as unsent only ever errs towards
	// the text that promises nothing.
	sent := requestID != "" && a.State != AttemptPending
	// NOT ConfiguredUpdaterSocket: that is THIS container's override, and the
	// command runs in a different host's updater container, which compose
	// passes no QUASAR_UPDATER_SOCKET.
	return joinApplyOutput(a.Output, applyTimeoutOutput(false, sent, requestID, UpdaterSocketPath))
}

// applyOutputLimit is `platform_apply_attempts.output`'s CHECK (migration 0075).
// An oversized output is REFUSED, not truncated, and a refused FailAttempt
// leaves the attempt non-terminal for ever — so the cap is enforced here too.
const applyOutputLimit = 8192

const applyOutputElision = "\n\n[…]\n\n"

// joinApplyOutput adds the hint under whatever the agent already relayed. The
// hint is the new information, so an over-long relay is what gives way; the cut
// must land on a rune boundary, because Postgres rejects a partial one.
func joinApplyOutput(previous, hint string) string {
	if hint == "" || previous == "" {
		return hint
	}
	if len(previous)+2+len(hint) <= applyOutputLimit {
		return previous + "\n\n" + hint
	}
	keep := min(applyOutputLimit-len(hint)-len(applyOutputElision), len(previous))
	if keep <= 0 {
		return hint
	}
	return strings.ToValidUTF8(previous[:keep], "") + applyOutputElision + hint
}
