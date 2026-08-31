package signal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gorilla/websocket"

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
	srv := newTestServer(t, pool)
	store := session.NewStore(pool)

	sess, firstToken := seedAssignedSession(t, pool)

	// Tab 1 attaches and stays attached.
	first, _, err := websocket.DefaultDialer.Dial(signalURL(srv, firstToken), nil)
	if err != nil {
		t.Fatalf("dial first: %v", err)
	}
	defer first.Close()

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
