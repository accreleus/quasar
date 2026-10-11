package signal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/origins"
	"github.com/accreleus/quasar/control-plane/internal/session"
)

// #526 — attach is explicit takeover, and the loser is TOLD.
//
// Two tabs on one session used to ping-pong forever: the relay displaces the
// incumbent browser channel, the displaced socket closed with
// CloseNormalClosure (1000), and the client could not tell that apart from an
// ordinary hang-up — so it escalated, minted a fresh single-use token,
// re-attached, and displaced the tab that had just displaced it. The fix is a
// close code that only ever means "someone else attached": 4410.
//
// This test is the red/green boundary for the SERVER half. It is DB-gated like
// the rest of this package (TEST_DATABASE_URL); `make test-db` runs it.
func TestDisplacedBrowserGetsTakeoverCloseCode(t *testing.T) {
	pool := testDB(t)
	srv, relay, _ := newTestServerWithRelay(t, pool)
	store := session.NewStore(pool)

	sess, firstToken := seedAssignedSession(t, pool)

	// Tab 1 attaches and stays attached.
	first, _, err := websocket.DefaultDialer.Dial(signalURL(srv, firstToken), nil)
	if err != nil {
		t.Fatalf("dial first: %v", err)
	}
	defer first.Close()

	// Dial returning only means the upgrade finished — the handler still has
	// the token consume (DB roundtrips) ahead of it before it reaches
	// relay.Register. If tab 2 dials now, a loaded runner can register tab 2
	// FIRST, so tab 1 displaces tab 2 and then waits out its read deadline for
	// a close that never comes (the 5.07s i/o-timeout flake, issue #36).
	// "Last attach wins" is defined by Register order, not dial order, so the
	// test must observe tab 1's registration before creating tab 2: a frame
	// delivered on the agent leg reaches the browser socket only after
	// Register (buffered frames flush immediately after it), so reading it
	// back is proof of registration.
	if err := relay.Deliver(sess.ID, []byte(`{"type":"ice","pc":"video","candidate":"registration probe"}`)); err != nil {
		t.Fatalf("deliver registration probe: %v", err)
	}
	if frame := readBrowserFrame(t, first); frame == "" {
		t.Fatalf("empty registration probe frame")
	}

	// Tab 2 mints its own (independently single-use) token and attaches. This is
	// exactly the #524 resume path: the mint succeeds, so the second attach is
	// legitimate as far as the token store is concerned.
	second, err := store.MintSignalingToken(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("mint second token: %v", err)
	}
	secondConn, _, err := websocket.DefaultDialer.Dial(signalURL(srv, second.Plaintext), nil)
	if err != nil {
		t.Fatalf("dial second: %v", err)
	}
	defer secondConn.Close()

	// Tab 1 must be closed with the takeover code — not 1000, which is what let
	// it mistake a takeover for a blip worth recovering from.
	first.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := first.ReadMessage(); err != nil {
			var ce *websocket.CloseError
			if !errors.As(err, &ce) {
				t.Fatalf("first socket ended with a non-close error: %v", err)
			}
			if ce.Code != wsCloseTakenOver {
				t.Fatalf("displaced socket close code = %d, want %d (taken over)", ce.Code, wsCloseTakenOver)
			}
			return
		}
	}
}

// The takeover code must stay distinct from every other code the handler can
// send: the client's whole no-escalate decision keys on it, so a collision
// would silently re-enable the loop for one of the other outcomes.
func TestTakeoverCloseCodeIsDistinct(t *testing.T) {
	codes := map[int]string{
		wsCloseTokenInvalid: "token invalid",
		wsCloseNotFound:     "not found",
		wsCloseNotAssigned:  "not assigned",
		wsCloseRelayUnavail: "relay unavailable",
	}
	if name, clash := codes[wsCloseTakenOver]; clash {
		t.Fatalf("wsCloseTakenOver (%d) collides with %s", wsCloseTakenOver, name)
	}
	if wsCloseTakenOver < 4000 || wsCloseTakenOver > 4999 {
		t.Fatalf("wsCloseTakenOver (%d) is outside the application close-code range", wsCloseTakenOver)
	}
}

// gatedAgentSender holds its first send until released, so a test can pile a
// second frame and a takeover up behind the handler's pump.
type gatedAgentSender struct {
	frames  chan []byte
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedAgentSender) SendSignaling(_, _ string, innerMsg json.RawMessage) error {
	g.frames <- append([]byte(nil), innerMsg...)
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return nil
}

// #535 — a displaced socket forwards nothing it had already read.
//
// The pump selects on the browser's next frame and on the takeover together,
// and Go picks at random when both are ready. A frame from the displaced tab
// that wins that draw reaches the agent after the new tab attached: an answer
// there is applied to the transport the agent built for the new tab.
//
// Each round parks the pump inside a send, lets a second frame and a takeover
// arrive behind it, and releases. Unfixed, the second frame is forwarded in
// about half the rounds.
func TestDisplacedSocketForwardsNothingItHadAlreadyRead(t *testing.T) {
	pool := testDB(t)
	store := session.NewStore(pool)

	for round := 0; round < 12; round++ {
		sess, firstToken := seedAssignedSession(t, pool)
		relay := agentws.NewRelayBus(discardLog())
		h := NewHandler(store, agentws.NewRegistry(discardLog()), relay, discardLog(),
			origins.NewResolver("", false, nil, discardLog()))
		agent := &gatedAgentSender{
			frames:  make(chan []byte, 4),
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
		h.registry = agent
		srv := httptest.NewServer(h)

		first, _, err := websocket.DefaultDialer.Dial(signalURL(srv, firstToken), nil)
		if err != nil {
			t.Fatalf("round %d: dial first: %v", round, err)
		}
		writeBrowserFrame(t, first, `{"type":"ice","pc":"video","candidate":{"candidate":"first"}}`)
		select {
		case <-agent.entered:
		case <-time.After(3 * time.Second):
			t.Fatalf("round %d: the first frame never reached the agent", round)
		}
		// Read by the handler's reader while the pump is parked in the send.
		writeBrowserFrame(t, first, `{"type":"answer","pc":"video","sdp":"v=0 stale"}`)
		time.Sleep(50 * time.Millisecond)

		second, err := store.MintSignalingToken(context.Background(), sess.ID)
		if err != nil {
			t.Fatalf("round %d: mint second token: %v", round, err)
		}
		secondConn, _, err := websocket.DefaultDialer.Dial(signalURL(srv, second.Plaintext), nil)
		if err != nil {
			t.Fatalf("round %d: dial second: %v", round, err)
		}
		// The probe comes back only once the second socket is registered, which
		// is what displaces the first.
		if err := relay.Deliver(sess.ID, []byte(`{"type":"ice","pc":"video","candidate":"registration probe"}`)); err != nil {
			t.Fatalf("round %d: deliver registration probe: %v", round, err)
		}
		readBrowserFrame(t, secondConn)

		close(agent.release)
		first.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			if _, _, err := first.ReadMessage(); err != nil {
				var ce *websocket.CloseError
				if !errors.As(err, &ce) || ce.Code != wsCloseTakenOver {
					t.Fatalf("round %d: displaced socket ended with %v, want close %d", round, err, wsCloseTakenOver)
				}
				break
			}
		}
		if got := len(agent.frames); got != 1 {
			t.Fatalf("round %d: %d frames reached the agent, want only the one sent before the takeover", round, got)
		}
		first.Close()
		secondConn.Close()
		srv.Close()
	}
}
