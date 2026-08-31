package signal

import "testing"

// The relay's view of a frame is `type` + `pc`, and an absent `pc` means video
// (signaling.md #304 amendment). Everything else stays opaque.
func TestClassifySignalFrame(t *testing.T) {
	cases := []struct {
		name     string
		frame    string
		wantKind string
		wantPc   string
	}{
		{"offer with pc", `{"type":"offer","pc":"audio","sdp":"v=0"}`, "offer", "audio"},
		{"offer without pc defaults to video", `{"type":"offer","sdp":"v=0"}`, "offer", "video"},
		{"restart_ice", `{"type":"restart_ice","pc":"video"}`, "restart_ice", "video"},
		{"answer", `{"type":"answer","pc":"audio","sdp":"v=0"}`, "answer", "audio"},
		{"ice is transparent", `{"type":"ice","pc":"audio","candidate":{}}`, "ice", "audio"},
		// The wire permits any valid JSON; a non-object must not panic or be
		// mistaken for a signaling verb.
		{"scalar", `null`, "", "video"},
		{"array", `[1,2]`, "", "video"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, pc := classifySignalFrame([]byte(tc.frame))
			if kind != tc.wantKind || pc != tc.wantPc {
				t.Fatalf("classifySignalFrame(%s) = (%q, %q), want (%q, %q)",
					tc.frame, kind, pc, tc.wantKind, tc.wantPc)
			}
		})
	}
}

// #505 in miniature: an offer relayed to the browser makes an ICE restart for
// that PC redundant, and only for that PC.
func TestRestartIceIsDroppedOnlyWhileAnOfferIsOutstanding(t *testing.T) {
	n := newNegotiationState()

	// Nothing offered yet — a genuine reconnect's restart_ice must relay.
	if allow, _ := n.allowBrowserFrame([]byte(`{"type":"restart_ice","pc":"video"}`)); !allow {
		t.Fatal("restart_ice with no outstanding offer must relay")
	}

	n.noteAgentFrame([]byte(`{"type":"offer","pc":"video","sdp":"v=0"}`))
	if allow, pc := n.allowBrowserFrame([]byte(`{"type":"restart_ice","pc":"video"}`)); allow || pc != "video" {
		t.Fatalf("restart_ice for an outstanding video offer must be dropped, got allow=%v pc=%q", allow, pc)
	}
	// The audio PC was never offered — its restart_ice is unaffected.
	if allow, _ := n.allowBrowserFrame([]byte(`{"type":"restart_ice","pc":"audio"}`)); !allow {
		t.Fatal("suppression must be per-PC, not per-socket")
	}
}

// The suppression window must close, or the recovery controller's later ICE
// restarts (fired long after the answer, on the same socket) would be muted.
func TestAnswerReopensTheIceRestartPath(t *testing.T) {
	n := newNegotiationState()
	n.noteAgentFrame([]byte(`{"type":"offer","sdp":"v=0"}`)) // no pc ⇒ video
	if allow, _ := n.allowBrowserFrame([]byte(`{"type":"answer","sdp":"v=0"}`)); !allow {
		t.Fatal("an answer always relays")
	}
	if allow, _ := n.allowBrowserFrame([]byte(`{"type":"restart_ice","pc":"video"}`)); !allow {
		t.Fatal("once answered, a later ICE restart must reach the agent")
	}
}

// A client that never answers must not be muted for the life of the socket:
// one redundant request is dropped per outstanding offer, not all of them.
func TestOnlyOneRestartIsDroppedPerOutstandingOffer(t *testing.T) {
	n := newNegotiationState()
	n.noteAgentFrame([]byte(`{"type":"offer","pc":"video","sdp":"v=0"}`))
	if allow, _ := n.allowBrowserFrame([]byte(`{"type":"restart_ice","pc":"video"}`)); allow {
		t.Fatal("the first redundant restart must be dropped")
	}
	if allow, _ := n.allowBrowserFrame([]byte(`{"type":"restart_ice","pc":"video"}`)); !allow {
		t.Fatal("a second restart must relay — the client is asking again for a reason")
	}
}

// ICE trickle and bye must never touch the state machine.
func TestNonNegotiationFramesAreTransparent(t *testing.T) {
	n := newNegotiationState()
	n.noteAgentFrame([]byte(`{"type":"ice","pc":"video","candidate":{}}`))
	n.noteAgentFrame([]byte(`{"type":"bye"}`))
	if allow, _ := n.allowBrowserFrame([]byte(`{"type":"restart_ice","pc":"video"}`)); !allow {
		t.Fatal("only an offer opens a negotiation")
	}
	if allow, _ := n.allowBrowserFrame([]byte(`{"type":"ice","pc":"video","candidate":{}}`)); !allow {
		t.Fatal("ice must always relay")
	}
}
