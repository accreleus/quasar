package platform

import (
	"context"
	"strings"
	"time"
)

// What a host apply that ended on its deadline tells the operator (#201).
//
// The recovery actor journals the verdict and it travels home over the agent's
// socket. When the new agent fails its health wait and the ADR 0004 restore
// fails too, no agent comes back to carry it, so the attempt can only expire,
// and a bare `timeout` hides a diagnosis the host already holds. These strings
// say where it is. Not a second relay path: the actor is host-local by design.

// applyReach is how far a release got, as far as the attempt row can say.
// `pending` is ambiguous in both directions: MintRequestID writes it before the
// send, and the actor's own first result state is `pending` too, relayed and
// written straight back. No column records the ack, so that row must claim
// neither history.
type applyReach int

const (
	reachNotSent applyReach = iota // no request id was minted, or none sent yet
	reachUnknown                   // a request id exists; `pending` cannot say if the actor has it
	reachSent                      // a state past `pending` was relayed, so the actor has a result
)

func attemptReach(state, requestID string) applyReach {
	if requestID == "" {
		return reachNotSent
	}
	switch state {
	case AttemptQueued, AttemptWaitingSessions:
		return reachNotSent
	case AttemptPending:
		return reachUnknown
	}
	return reachSent
}

// applyNotSentOutput is the timeout with no agent and no request id: the actor
// has no journal for this attempt, so naming the read command would send an
// operator after a missing file.
const applyNotSentOutput = "This apply expired without ever being sent: the host's agent was not connected to " +
	"the control plane. Nothing on this host was pulled, replaced or changed, and its recovery actor has no " +
	"record of this attempt.\n\n" +
	"Bring the agent back (Fleet ▸ Hosts shows when it was last seen) and apply again."

// actorJournalDir is where the recovery actor keeps one journal per request id,
// inside its own container (quasar-recovery journal.rs, machine state).
const actorJournalDir = "/var/lib/quasar-machine/journal/"

// readResultCommand reads the recovery actor's journal for the request, on the
// host. The actor's container, never the node agent: every shape that reaches
// here has the node agent down.
func readResultCommand(requestID string) string {
	return "  docker exec quasar-recovery cat " + actorJournalDir + requestID + ".json\n\n"
}

// applyTimeoutOutput composes the `output` of a host attempt failed as
// ReasonTimeout. "" means say nothing new: the agent is on the wire, so the
// missing verdict is not a relay failure.
func applyTimeoutOutput(agentConnected bool, reach applyReach, requestID string) string {
	if agentConnected {
		return ""
	}
	if reach == reachNotSent || requestID == "" {
		return applyNotSentOutput
	}
	var b strings.Builder
	if reach == reachUnknown {
		// One read on the host resolves both halves, so the missing-file case is spelt out.
		b.WriteString("This apply expired without a verdict and the host's agent has not come back.\n\n")
		b.WriteString("The last this host reported was its recovery actor holding the request, unstarted, but an ")
		b.WriteString("apply the actor never received looks exactly the same from here, and nothing recorded ")
		b.WriteString("which this was. So whether anything on this host was pulled or replaced cannot be ")
		b.WriteString("settled from the control plane.\n\n")
		b.WriteString("It can be settled on the host. Ask the recovery actor, not the node agent: the ")
		b.WriteString("node agent is the one that is down.\n\n")
		b.WriteString(readResultCommand(requestID))
		b.WriteString("A journal there is the verdict: the real reason, the failed container's last log ")
		b.WriteString("lines, and the `previous` digests to put back by hand. No such file means the actor never ")
		b.WriteString("admitted the request and nothing on this host was changed.\n\n")
		b.WriteString("`docker exec quasar-recovery quasar-recovery status` shows the actor's last attempt, and ")
		b.WriteString("`docker logs quasar-recovery` carries the same verdict.")
		return b.String()
	}
	b.WriteString("This apply expired without a verdict and the host's agent has not come back, so the ")
	b.WriteString("recovery actor's own result could not be relayed to the control plane.\n\n")
	b.WriteString("That most often means the new container failed its health wait and the actor's ")
	b.WriteString("automatic restore of the previous one (ADR 0004) failed too. It is not proof: a ")
	b.WriteString("restore still running when the deadline fell, a pull slower than the deadline, a host ")
	b.WriteString("that lost power part-way through, an agent stopped by hand and a host off the network ")
	b.WriteString("all look the same from here. If this host is back in Fleet ▸ Hosts, the actor ")
	b.WriteString("finished after the apply gave up; the version shown there says which build it came ")
	b.WriteString("back on.\n\n")
	b.WriteString("The verdict is on that host: the real reason, the failed container's last log lines, ")
	b.WriteString("and the `previous` digests to put back by hand. Ask the recovery actor, not the node ")
	b.WriteString("agent: the node agent is the one that is down.\n\n")
	b.WriteString(readResultCommand(requestID))
	b.WriteString("`docker exec quasar-recovery quasar-recovery status` shows the actor's last attempt, and ")
	b.WriteString("`docker logs quasar-recovery` says whether the restore finished.")
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
	hint := applyTimeoutOutput(false, attemptReach(a.State, requestID), requestID)
	return joinApplyOutput(a.Output, hint)
}

// applyOutputLimit is the column's CHECK; it lives beside the writers that must
// respect it, in apply_store.go.
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
