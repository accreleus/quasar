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

// applyReach is how far a release got, as far as the attempt row can say.
// `pending` is ambiguous in both directions: MintRequestID writes it before the
// send, and the updater's own first result state is `pending` too, relayed and
// written straight back. No column records the ack, so that row must claim
// neither history.
type applyReach int

const (
	reachNotSent applyReach = iota // no request id was minted, or none sent yet
	reachUnknown                   // a request id exists; `pending` cannot say if an updater has it
	reachSent                      // a state past `pending` was relayed, so an updater has a result
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

// applyNotSentOutput is the timeout with no agent and no request id: no updater
// has a result for this attempt, so naming the read command would send an
// operator after a 404.
const applyNotSentOutput = "This apply expired without ever being sent: the host's agent was not connected to " +
	"the control plane. Nothing on this host was pulled, recreated or changed, and its updater has no " +
	"result for this attempt.\n\n" +
	"Bring the agent back — Fleet ▸ Hosts shows when it was last seen — and apply again."

// readResultCommand reads the updater's own result, on the host. The updater
// container, never the node agent: every shape that reaches here has the node
// agent down.
func readResultCommand(requestID, socket string) string {
	return "  docker compose exec quasar-updater curl -s --unix-socket " + socket +
		" http://u/v1/results/" + requestID + "\n\n"
}

// applyTimeoutOutput composes the `output` of a host attempt failed as
// ReasonTimeout. "" means say nothing new: the agent is on the wire, so the
// missing verdict is not a relay failure.
func applyTimeoutOutput(agentConnected bool, reach applyReach, requestID, socket string) string {
	if agentConnected {
		return ""
	}
	if reach == reachNotSent || requestID == "" {
		return applyNotSentOutput
	}
	if socket == "" {
		socket = UpdaterSocketPath
	}
	var b strings.Builder
	if reach == reachUnknown {
		// One read on the host resolves both halves, so the 404 case is spelt out.
		b.WriteString("This apply expired without a verdict and the host's agent has not come back.\n\n")
		b.WriteString("The last this host reported was its updater holding the request, unstarted — but an ")
		b.WriteString("apply no updater ever received looks exactly the same from here, and nothing recorded ")
		b.WriteString("which this was. So whether anything on this host was pulled or recreated cannot be ")
		b.WriteString("settled from the control plane.\n\n")
		b.WriteString("It can be settled on the host. Ask the updater container, not the node agent: the ")
		b.WriteString("node agent is the one that is down.\n\n")
		b.WriteString(readResultCommand(requestID, socket))
		b.WriteString("A result there is the verdict — the real reason, the failed container's last log ")
		b.WriteString("lines, and the `previous` digests to put back by hand. A 404 means this updater never ")
		b.WriteString("received the request and nothing on this host was changed.\n\n")
		b.WriteString("`docker compose logs quasar-updater` carries the same verdict.")
		return b.String()
	}
	b.WriteString("This apply expired without a verdict and the host's agent has not come back, so the ")
	b.WriteString("updater's own result could not be relayed to the control plane.\n\n")
	b.WriteString("That most often means the new container failed its health wait and the updater's ")
	b.WriteString("automatic restore of the previous one (ADR 0004) failed too. It is not proof: a ")
	b.WriteString("restore still running when the deadline fell, a pull slower than the deadline, a host ")
	b.WriteString("that lost power part-way through, an agent stopped by hand and a host off the network ")
	b.WriteString("all look the same from here. If this host is back in Fleet ▸ Hosts, the updater ")
	b.WriteString("finished after the apply gave up — the version shown there says which build it came ")
	b.WriteString("back on.\n\n")
	b.WriteString("The verdict is on that host — the real reason, the failed container's last log lines, ")
	b.WriteString("and the `previous` digests to put back by hand. Read it in the stack directory there. ")
	b.WriteString("Ask the updater container, not the node agent: the node agent is the one that is down.\n\n")
	b.WriteString(readResultCommand(requestID, socket))
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
	// NOT ConfiguredUpdaterSocket: that is THIS container's override, and the
	// command runs in a different host's updater container, which compose
	// passes no QUASAR_UPDATER_SOCKET.
	hint := applyTimeoutOutput(false, attemptReach(a.State, requestID), requestID, UpdaterSocketPath)
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
