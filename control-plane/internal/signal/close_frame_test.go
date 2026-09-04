package signal

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/origins"
	"github.com/accreleus/quasar/control-plane/internal/session"
)

// failingAgentSender relays nothing: every browser frame fails the way a
// saturated agent outbound queue does (agentws.ErrSendQueueFull).
type failingAgentSender struct{ err error }

func (f *failingAgentSender) SendSignaling(_, _ string, _ json.RawMessage) error { return f.err }

func newTestServerWithFailingAgent(t *testing.T, pool *pgxpool.Pool, sendErr error) (*httptest.Server, *agentws.RelayBus) {
	t.Helper()
	relay := agentws.NewRelayBus(discardLog())
	h := NewHandler(session.NewStore(pool), agentws.NewRegistry(discardLog()), relay, discardLog(),
		origins.NewResolver("", false, nil, discardLog()))
	h.registry = &failingAgentSender{err: sendErr}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, relay
}

// awaitClose reads until the socket ends and returns the close code, failing
// the test if it ended any way other than with a close frame — which is exactly
// what #93's client reported ("connection reset without closing handshake").
func awaitClose(t *testing.T, conn *websocket.Conn) int {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		var ce *websocket.CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("socket ended without a close frame: %v", err)
		}
		return ce.Code
	}
}

// attach dials the signaling socket and returns only once the relay has
// registered it — a frame delivered on the agent leg reaches the browser only
// after Register, so reading one back is proof (same technique as #526's
// takeover test, and for the same reason: Dial returning proves only that the
// upgrade finished, not that the handler got past its DB round trips).
func attach(t *testing.T, srv *httptest.Server, relay *agentws.RelayBus, sess session.Session, token string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(signalURL(srv, token), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	// One probe is enough: delivered before Register it is buffered and flushed
	// on Register, delivered after it goes straight to the socket.
	if err := relay.Deliver(sess.ID, []byte(`{"type":"ice","pc":"video","candidate":"registration probe"}`)); err != nil {
		t.Fatalf("deliver registration probe: %v", err)
	}
	if readBrowserFrame(t, conn) == "" {
		t.Fatal("empty registration probe frame")
	}
	return conn
}

// #93 — a relay failure on the browser→agent leg must close with a code, not
// drop the socket.
//
// The relay used to `return` on any SendSignaling error other than
// ErrAgentNotConnected, so the deferred conn.Close() ended the connection with
// no close frame, one frame after the client's own write. The native client
// reported it as "connection reset without closing handshake right after the
// audio answer", with the answer silently never reaching the agent and no
// tracks ever arriving — and the only host-side trace was a Debug line under
// the default LOG_LEVEL=info. ErrSendQueueFull is signaling.md's 4500 ("relay
// to node agent unavailable"), so this needs no new close code.
func TestSaturatedAgentRelayClosesWithCode(t *testing.T) {
	pool := testDB(t)
	srv, relay := newTestServerWithFailingAgent(t, pool, agentws.ErrSendQueueFull)
	sess, token := seedAssignedSession(t, pool)

	conn := attach(t, srv, relay, sess, token)
	writeBrowserFrame(t, conn, `{"type":"answer","pc":"audio","sdp":"v=0"}`)

	if code := awaitClose(t, conn); code != wsCloseRelayUnavail {
		t.Fatalf("close code = %d, want %d (relay unavailable)", code, wsCloseRelayUnavail)
	}
}

// #93 — a session going terminal must close its live signaling socket.
//
// RelayBus.Forget (the coordinator's terminal hook, #402) deleted the browser
// registration and told the socket nothing, so after a DELETE the client held a
// socket that would never receive another agent frame and never be closed. The
// teardown reached the client only when its own transport died — again with no
// closing handshake. signaling.md already defines 4404 as "session not found or
// terminal"; this is that code, mid-session.
//
// It also asserts the frame the bus queued before Forget still arrives: the
// agent's `bye` is typically the last frame in flight, and closing on the
// terminal signal without draining would swallow it.
func TestTerminalSessionClosesLiveSignalingSocket(t *testing.T) {
	pool := testDB(t)
	srv, relay := newTestServerWithFailingAgent(t, pool, agentws.ErrSendQueueFull)
	sess, token := seedAssignedSession(t, pool)

	conn := attach(t, srv, relay, sess, token)
	if err := relay.Deliver(sess.ID, []byte(`{"type":"bye"}`)); err != nil {
		t.Fatalf("deliver bye: %v", err)
	}
	relay.Forget(sess.ID)

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, frame, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("the bye queued before Forget never arrived: %v", err)
	}
	if string(frame) != `{"type":"bye"}` {
		t.Fatalf("frame before close = %s, want the bye", frame)
	}
	if code := awaitClose(t, conn); code != wsCloseNotFound {
		t.Fatalf("close code = %d, want %d (session terminal)", code, wsCloseNotFound)
	}
}
