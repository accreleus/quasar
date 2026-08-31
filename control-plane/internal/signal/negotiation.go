package signal

import "encoding/json"

// defaultPc is the PeerConnection a signaling message belongs to when it
// carries no `pc` field. signaling.md (#304 amendment): "`pc` is optional. When
// absent, the message belongs to the `"video"` PeerConnection."
const defaultPc = "video"

// signalFrameView is the minimal, read-only view of a Phase 0 signaling message
// that the relay needs. The relay is a bridge, not a participant: it does not
// parse SDP, does not rewrite anything, and must never grow a third field here
// without a reason as concrete as the one below.
type signalFrameView struct {
	Type string `json:"type"`
	Pc   string `json:"pc"`
}

// classifySignalFrame reads a frame's `type` and `pc`. A frame that is not a
// JSON object (the wire permits any valid JSON — see validateBrowserFrame)
// classifies as the empty kind, which every caller treats as "not interesting".
func classifySignalFrame(frame []byte) (kind, pc string) {
	var v signalFrameView
	if err := json.Unmarshal(frame, &v); err != nil {
		return "", defaultPc
	}
	if v.Pc == "" {
		return v.Type, defaultPc
	}
	return v.Type, v.Pc
}

// negotiationState is the per-socket record of which PeerConnections have an
// unanswered offer outstanding (#505). The bus buffers agent frames until a
// browser attaches, and a reconnecting client fires `restart_ice` per PC on
// socket open — so a client registering onto a buffered offer got two offers
// and answered both, and webrtcbin applies whichever answer lands first
// (kind=duplicate_answer for the other), silently losing the ICE restart.
// A `restart_ice` whose PC already has an unanswered offer outstanding is
// therefore redundant and dropped — exactly signaling.md's "Duplicate requests
// are idempotent while negotiation is in progress", so no wire shape changes.
// A buffered offer is never stale: RelayBus buffers only frames that reached
// no browser, so the offerer is still waiting on its answer — that is what
// makes suppression safe.
//
// Not safe for concurrent use: one instance per signaling socket, touched only
// by that socket's pump loop.
type negotiationState struct {
	awaitingAnswer map[string]bool
}

func newNegotiationState() *negotiationState {
	return &negotiationState{awaitingAnswer: make(map[string]bool, 2)}
}

// noteAgentFrame records an agent→browser frame. An `offer` opens a negotiation
// for its PC; everything else (ice/bye/error) is transparent to this state.
func (n *negotiationState) noteAgentFrame(frame []byte) {
	if kind, pc := classifySignalFrame(frame); kind == "offer" {
		n.awaitingAnswer[pc] = true
	}
}

// allowBrowserFrame decides whether a browser→agent frame is relayed, and keeps
// the per-PC state current.
//
// It returns false ONLY for a `restart_ice` whose PC already has an unanswered
// offer in flight. An `answer` closes that PC's negotiation, so the client's
// later recovery-driven ICE restarts (RecoveryController.onRetry, fired long
// after the answer) always relay — the suppression is scoped to the open burst
// it exists for, not to the life of the socket.
func (n *negotiationState) allowBrowserFrame(frame []byte) (allow bool, pc string) {
	kind, pc := classifySignalFrame(frame)
	switch kind {
	case "answer":
		delete(n.awaitingAnswer, pc)
		return true, pc
	case "restart_ice":
		if n.awaitingAnswer[pc] {
			// One-shot per outstanding offer, fail-open: clear the flag so a
			// client that never answers is not muted for the socket's life —
			// a lost restart is worse than a duplicate offer. Known bound: two
			// restart_ice for one PC before answering re-opens #505's race on
			// the second (today's SPA sends exactly one); a bursting client
			// needs the generation tagging #505 deferred, not a longer-lived
			// flag here.
			delete(n.awaitingAnswer, pc)
			return false, pc
		}
		return true, pc
	default:
		return true, pc
	}
}
