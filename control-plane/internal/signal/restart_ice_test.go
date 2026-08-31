package signal

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/origins"
	"github.com/accreleus/quasar/control-plane/internal/session"
)

// fakeAgentSender stands in for the node agent's end of the relay. The #505
// suppression is deliberately invisible from the browser side — the browser is
// told nothing and simply never gets a second offer — so proving it needs a
// view of what actually left for the agent.
type fakeAgentSender struct {
	frames chan []byte
}

func newFakeAgentSender() *fakeAgentSender {
	return &fakeAgentSender{frames: make(chan []byte, 16)}
}

func (f *fakeAgentSender) SendSignaling(_, _ string, innerMsg json.RawMessage) error {
	f.frames <- append([]byte(nil), innerMsg...)
	return nil
}

// next returns the next frame the agent received, or fails the test.
func (f *fakeAgentSender) next(t *testing.T) string {
	t.Helper()
	select {
	case frame := <-f.frames:
		return string(frame)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a frame to reach the agent")
		return ""
	}
}

// nothingElse asserts the agent received nothing beyond what was already read.
// Call it only after a later frame has arrived, so it is an ordering assertion
// rather than a sleep.
func (f *fakeAgentSender) nothingElse(t *testing.T) {
	t.Helper()
	select {
	case frame := <-f.frames:
		t.Fatalf("unexpected extra frame reached the agent: %s", frame)
	default:
	}
}

// newTestServerWithRelay builds a signaling server whose agent leg is a fake,
// and hands back the relay bus so a test can buffer agent frames the way a
// live pipeline does before the browser attaches.
func newTestServerWithRelay(t *testing.T, pool *pgxpool.Pool) (*httptest.Server, *agentws.RelayBus, *fakeAgentSender) {
	t.Helper()
	relay := agentws.NewRelayBus(discardLog())
	h := NewHandler(session.NewStore(pool), agentws.NewRegistry(discardLog()), relay, discardLog(),
		origins.NewResolver("", false, nil, discardLog()))
	agent := newFakeAgentSender()
	h.registry = agent
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, relay, agent
}

func readBrowserFrame(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, frame, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read from signaling socket: %v", err)
	}
	return string(frame)
}

func writeBrowserFrame(t *testing.T, conn *websocket.Conn, frame string) {
	t.Helper()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
		t.Fatalf("write %s: %v", frame, err)
	}
}

// TestBufferedOfferSuppressesTheClientsRedundantIceRestart is #505's regression.
//
// A client reconnecting onto a session whose offers are still buffered used to
// get TWO offers per PC: the buffered original, plus the one its own
// `restart_ice`-on-open provoked. It answered both, webrtcbin kept whichever
// answer landed first, and when the stale one won the ICE restart was silently
// lost (the agent logs the loser as kind=duplicate_answer).
//
// The relay now drops an ICE restart for a PC that already has an unanswered
// offer in flight, so the agent is never asked for the second offer.
func TestBufferedOfferSuppressesTheClientsRedundantIceRestart(t *testing.T) {
	pool := testDB(t)
	sess, token := seedAssignedSession(t, pool)
	srv, relay, agent := newTestServerWithRelay(t, pool)

	// The pipeline is live and has offered both PCs before any browser exists —
	// exactly the state RelayBus buffers for.
	if err := relay.Deliver(sess.ID, []byte(`{"type":"offer","pc":"video","sdp":"v=0 video"}`)); err != nil {
		t.Fatalf("buffer video offer: %v", err)
	}
	if err := relay.Deliver(sess.ID, []byte(`{"type":"offer","pc":"audio","sdp":"v=0 audio"}`)); err != nil {
		t.Fatalf("buffer audio offer: %v", err)
	}

	conn, _, err := websocket.DefaultDialer.Dial(signalURL(srv, token), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Exactly one offer per PC reaches the browser.
	offers := []string{readBrowserFrame(t, conn), readBrowserFrame(t, conn)}
	for _, want := range []string{`"pc":"video"`, `"pc":"audio"`} {
		found := false
		for _, got := range offers {
			if strings.Contains(got, want) && strings.Contains(got, `"type":"offer"`) {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected one offer for %s, got %v", want, offers)
		}
	}

	// The SPA's websocket-open burst, then the answers it produces.
	writeBrowserFrame(t, conn, `{"type":"restart_ice","pc":"video"}`)
	writeBrowserFrame(t, conn, `{"type":"restart_ice","pc":"audio"}`)
	writeBrowserFrame(t, conn, `{"type":"answer","pc":"video","sdp":"v=0 answer video"}`)
	writeBrowserFrame(t, conn, `{"type":"answer","pc":"audio","sdp":"v=0 answer audio"}`)

	// Frames are relayed in order, so the two answers arriving proves the two
	// restarts ahead of them were already decided — and dropped.
	for _, wantPc := range []string{`"pc":"video"`, `"pc":"audio"`} {
		got := agent.next(t)
		if !strings.Contains(got, `"type":"answer"`) || !strings.Contains(got, wantPc) {
			t.Fatalf("agent received %s, want the %s answer (an ICE restart must not have been relayed)", got, wantPc)
		}
	}
	agent.nothingElse(t)

	// The window closes with the answer: a later restart — the recovery
	// controller's, after the media path degrades — must still reach the agent.
	writeBrowserFrame(t, conn, `{"type":"restart_ice","pc":"video"}`)
	if got := agent.next(t); !strings.Contains(got, `"type":"restart_ice"`) {
		t.Fatalf("post-answer ICE restart did not reach the agent: %s", got)
	}
}

// TestRestartIceLosesToTheBufferedOfferWithoutReadingItFirst is the test that
// actually exercises the pre-reader flush in handler.go, and the reason that
// block exists.
//
// The sibling test above reads both offers before writing anything, which
// hand-serializes the interleaving: the handler has necessarily processed the
// offers by then, so it passes even with the flush block deleted. The real
// client does no such thing — the SPA's `ws.onopen` fires `restart_ice` for
// both PCs immediately, long before the offer messages have been parsed. This
// test reproduces that: dial, then write the restart burst WITHOUT reading, so
// the buffered offers and the browser's first frames are genuinely in flight
// together.
//
// The guarantee under test is structural, not probabilistic: Register drains
// the pending buffer synchronously, and the handler flushes those frames before
// starting the browser reader goroutine, so an offer is always accounted for
// before any browser frame can be read. Delete the flush and the pump loop
// decides between two ready select cases by Go's uniform choice — which is
// precisely #505's coin flip.
//
// The burst shape matters, and it is the agent's real one. RelayBus sizes its
// buffer for "one video offer + one audio offer plus ICE candidates for every
// host/bridge address" — an observed 68 frames — so the AUDIO offer sits behind
// dozens of video ICE candidates. Without the flush, the pump dequeues those
// one at a time while the browser's restart burst lands in browserIn, and by
// the time the audio offer surfaces the select has long had two ready cases.
// That is where the coin flip becomes observable; two offers back-to-back are
// drained before the browser's first frame has even crossed the wire, which is
// why the naive shape passes either way and proves nothing.
//
// Repeated, because a coin flip has to be flipped more than once to be caught:
// with the flush this is deterministic across every trial; without it, the run
// fails with near certainty.
func TestRestartIceLosesToTheBufferedOfferWithoutReadingItFirst(t *testing.T) {
	pool := testDB(t)
	srv, relay, agent := newTestServerWithRelay(t, pool)

	const trials = 8
	for trial := 0; trial < trials; trial++ {
		sess, token := seedAssignedSession(t, pool)
		if err := relay.Deliver(sess.ID, []byte(`{"type":"offer","pc":"video","sdp":"v=0 video"}`)); err != nil {
			t.Fatalf("trial %d: buffer video offer: %v", trial, err)
		}
		// The video PC's trickle burst, ahead of the audio offer — as the agent
		// actually emits it.
		for i := 0; i < 60; i++ {
			cand := fmt.Sprintf(`{"type":"ice","pc":"video","candidate":{"candidate":"candidate:%d 1 udp 1 192.0.2.%d 40000 typ host","sdpMid":"0","sdpMLineIndex":0}}`, i, i)
			if err := relay.Deliver(sess.ID, []byte(cand)); err != nil {
				t.Fatalf("trial %d: buffer ice %d: %v", trial, i, err)
			}
		}
		if err := relay.Deliver(sess.ID, []byte(`{"type":"offer","pc":"audio","sdp":"v=0 audio"}`)); err != nil {
			t.Fatalf("trial %d: buffer audio offer: %v", trial, err)
		}

		conn, _, err := websocket.DefaultDialer.Dial(signalURL(srv, token), nil)
		if err != nil {
			t.Fatalf("trial %d: dial: %v", trial, err)
		}

		// The websocket-open burst, written WITHOUT first draining the offers
		// the server has already queued for us.
		writeBrowserFrame(t, conn, `{"type":"restart_ice","pc":"video"}`)
		writeBrowserFrame(t, conn, `{"type":"restart_ice","pc":"audio"}`)
		writeBrowserFrame(t, conn, `{"type":"answer","pc":"video","sdp":"v=0 answer video"}`)
		writeBrowserFrame(t, conn, `{"type":"answer","pc":"audio","sdp":"v=0 answer audio"}`)

		// In-order relaying makes the answers a barrier: once both have reached
		// the agent, the two restarts ahead of them were already decided.
		for _, wantPc := range []string{`"pc":"video"`, `"pc":"audio"`} {
			got := agent.next(t)
			if !strings.Contains(got, `"type":"answer"`) || !strings.Contains(got, wantPc) {
				t.Fatalf("trial %d: agent received %s, want the %s answer — a buffered offer lost the race to the browser's restart_ice",
					trial, got, wantPc)
			}
		}
		agent.nothingElse(t)
		conn.Close()
	}
}

// TestReconnectToEstablishedSessionStillRelaysIceRestart is the other half of
// #505: on a genuine reconnect the host pipeline is long past its one offer per
// webrtcbin and nothing is buffered, so `restart_ice` is the ONLY thing that
// makes media flow again. It must always relay.
func TestReconnectToEstablishedSessionStillRelaysIceRestart(t *testing.T) {
	pool := testDB(t)
	_, token := seedAssignedSession(t, pool)
	srv, _, agent := newTestServerWithRelay(t, pool)

	conn, _, err := websocket.DefaultDialer.Dial(signalURL(srv, token), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	writeBrowserFrame(t, conn, `{"type":"restart_ice","pc":"video"}`)
	writeBrowserFrame(t, conn, `{"type":"restart_ice","pc":"audio"}`)

	for _, wantPc := range []string{`"pc":"video"`, `"pc":"audio"`} {
		got := agent.next(t)
		if !strings.Contains(got, `"type":"restart_ice"`) || !strings.Contains(got, wantPc) {
			t.Fatalf("agent received %s, want the %s ICE restart", got, wantPc)
		}
	}
	agent.nothingElse(t)
}

// A buffered offer for one PC must not mute the other PC's ICE restart. The
// suppression is keyed on the PC that was actually offered, nothing wider.
//
// NOT a claim that audio-disabled sessions are handled. A session started with
// QUASAR_AUDIO_DISABLED buffers a video offer only, which is the shape below —
// but the surviving `restart_ice{audio}` then hits the agent's webrtc_for_pc,
// which falls back to the VIDEO webrtcbin when there is no audio one, so the
// audio restart retargets video and can still produce a second video offer.
// That fallback is a separate, pre-existing node-agent defect (being filed);
// this test pins the relay's per-PC scoping, and the relay cannot see it.
func TestSuppressionIsScopedToTheOfferedPeerConnection(t *testing.T) {
	pool := testDB(t)
	sess, token := seedAssignedSession(t, pool)
	srv, relay, agent := newTestServerWithRelay(t, pool)

	if err := relay.Deliver(sess.ID, []byte(`{"type":"offer","pc":"video","sdp":"v=0 video"}`)); err != nil {
		t.Fatalf("buffer video offer: %v", err)
	}
	conn, _, err := websocket.DefaultDialer.Dial(signalURL(srv, token), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if got := readBrowserFrame(t, conn); !strings.Contains(got, `"type":"offer"`) {
		t.Fatalf("first frame = %s, want the buffered offer", got)
	}

	writeBrowserFrame(t, conn, `{"type":"restart_ice","pc":"video"}`)
	writeBrowserFrame(t, conn, `{"type":"restart_ice","pc":"audio"}`)

	got := agent.next(t)
	if !strings.Contains(got, `"type":"restart_ice"`) || !strings.Contains(got, `"pc":"audio"`) {
		t.Fatalf("agent received %s, want only the audio ICE restart to survive", got)
	}
	agent.nothingElse(t)
}
