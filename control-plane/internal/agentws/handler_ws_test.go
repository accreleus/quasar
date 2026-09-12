package agentws

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func websocketTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(nil, "test-token", log, nil, nil, nil, nil, nil)
	// #401/#406: a Handler owns two drain goroutines. Without Close every test
	// that builds one leaks them for the life of the test binary, which is what
	// the package's goleak ignores were papering over.
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestAgentRejectsBinaryRegister(t *testing.T) {
	_, url := websocketTestServer(t)
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte(`{"type":"register"}`)); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseUnsupportedData {
		t.Fatalf("close = %v, want code %d", err, websocket.CloseUnsupportedData)
	}
}

func TestAgentRejectsOversizedRegister(t *testing.T) {
	_, url := websocketTestServer(t)
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, make([]byte, agentReadLimit+1)); err != nil {
		t.Fatalf("write oversized: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseMessageTooBig {
		t.Fatalf("close = %v, want code %d", err, websocket.CloseMessageTooBig)
	}
}

// FuzzAgentPeekType exercises the shared text-frame discriminator used during
// register, capacity, and the steady-state agent message loop. The WebSocket
// boundary itself enforces agentReadLimit (covered above); this target protects
// the untrusted JSON payload passed beyond that boundary without needing a DB
// or a live agent connection.
func FuzzAgentPeekType(f *testing.F) {
	for _, seed := range [][]byte{
		nil,
		[]byte(`{"type":"register"}`),
		[]byte(`{"type":`),
		[]byte(`null`),
		[]byte(`[]`),
		[]byte{'{', 0xff, '}'},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		// The invariant is deliberately narrow: arbitrary frame bytes must be
		// accepted or rejected without panicking before the protocol dispatcher
		// sees them. Semantic type validation remains the caller's contract.
		_, _ = peekType(raw)
	})
}

func failedAgentRegister(t *testing.T, url string, headers map[string]string) {
	t.Helper()
	h := make(http.Header)
	for k, v := range headers {
		h.Set(k, v)
	}
	conn, _, err := websocket.DefaultDialer.Dial(url, h)
	if err != nil {
		t.Fatalf("dial failed attempt: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{
		"type": "register", "node_name": "bad", "agent_version": "test",
		"auth": map[string]string{"enrollment_token": "wrong"},
	}); err != nil {
		t.Fatalf("write register: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func TestEnrollmentFailuresAreRateLimitedPreUpgradeAndIgnoreXFF(t *testing.T) {
	_, url := websocketTestServer(t)
	for i := 0; i < enrollmentFailureLimit; i++ {
		failedAgentRegister(t, url, map[string]string{"X-Forwarded-For": fmt.Sprintf("203.0.113.%d", i)})
	}
	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("blocked enrollment unexpectedly upgraded")
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%v, want 429", resp)
	}
}

func TestSuccessfulEnrollmentClearsPriorFailures(t *testing.T) {
	pool := testPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(pool, "test-token", log, nil, nil, nil, nil, nil)
	t.Cleanup(h.Close) // #401/#406: stop the drain goroutines with the Handler.
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	for i := 0; i < enrollmentFailureLimit-1; i++ {
		failedAgentRegister(t, url, nil)
	}
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("valid dial: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "register", "node_name": "valid-after-failures", "agent_version": "test",
		"auth": map[string]string{"enrollment_token": "test-token"},
	}); err != nil {
		t.Fatalf("valid register: %v", err)
	}
	var registered map[string]any
	if err := conn.ReadJSON(&registered); err != nil {
		t.Fatalf("read registered: %v", err)
	}
	conn.Close()
	for i := 0; i < enrollmentFailureLimit-1; i++ {
		failedAgentRegister(t, url, nil)
	}
	// The successful enrollment cleared the first batch, so one more upgrade is
	// still admitted (and becomes the new threshold failure).
	failedAgentRegister(t, url, nil)
}

// registerWithNodeSecret registers with a reconnect credential and returns the
// control plane's first reply (an `error` frame on every path this exercises).
func registerWithNodeSecret(t *testing.T, url, nodeName, secret string) map[string]any {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial: %v (status %d)", err, status)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{
		"type": "register", "node_name": nodeName, "agent_version": "test",
		"auth": map[string]string{"node_secret": secret},
	}); err != nil {
		t.Fatalf("write register: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var reply map[string]any
	if err := conn.ReadJSON(&reply); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	return reply
}

// #199: a node_secret this control plane never minted is NOT a bad-token
// enrollment attempt, and must not spend the enrollment-failure budget.
//
// The reported failure: a machine enrolled to another control plane keeps its
// agent data volume, so its agent presents the old secret on every reconnect.
// Ten rejects inside a minute used to exhaust the limiter, and the operator —
// who had already pasted a fresh enrollment token — got a 429 that looked like a
// second, unrelated fault on top of a `host_not_found` naming the wrong remedy.
func TestUnknownNodeSecretDoesNotSpendTheEnrollmentBudget(t *testing.T) {
	pool := testPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(pool, "test-token", log, nil, nil, nil, nil, nil)
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	// Comfortably past enrollmentFailureLimit: every one of these is refused, and
	// none of them may count.
	for i := 0; i < enrollmentFailureLimit+3; i++ {
		reply := registerWithNodeSecret(t, url, "never-enrolled-here", "deadbeef")
		if reply["code"] != "host_not_found" {
			t.Fatalf("attempt %d: code=%v message=%v, want host_not_found", i, reply["code"], reply["message"])
		}
		msg, _ := reply["message"].(string)
		if !strings.Contains(msg, "node_secret") {
			t.Errorf("attempt %d: message %q should name the credential that was refused", i, msg)
		}
	}

	// The budget is untouched, so a genuine enrollment still gets in.
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial after the stale-secret rejects: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{
		"type": "register", "node_name": "enrolls-after-stale", "agent_version": "test",
		"auth": map[string]string{"enrollment_token": "test-token"},
	}); err != nil {
		t.Fatalf("write register: %v", err)
	}
	var registered map[string]any
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := conn.ReadJSON(&registered); err != nil {
		t.Fatalf("read registered: %v", err)
	}
	if registered["type"] != "registered" {
		t.Fatalf("reply=%v, want registered", registered)
	}
}

// A node_secret that is WRONG for a node_name this control plane does know is a
// credential guess, and must keep spending the budget (the limiter's whole job).
func TestWrongNodeSecretForAKnownHostStillSpendsTheBudget(t *testing.T) {
	pool := testPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(pool, "test-token", log, nil, nil, nil, nil, nil)
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	// A genuinely enrolled host, so its node_secret_hash is set and a wrong secret
	// is a real mismatch rather than an absent column.
	enroll := func() {
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatalf("dial for enrollment: %v", err)
		}
		defer conn.Close()
		if err := conn.WriteJSON(map[string]any{
			"type": "register", "node_name": "known-host", "agent_version": "test",
			"auth": map[string]string{"enrollment_token": "test-token"},
		}); err != nil {
			t.Fatalf("enroll register: %v", err)
		}
		var registered map[string]any
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		if err := conn.ReadJSON(&registered); err != nil {
			t.Fatalf("read registered: %v", err)
		}
		if registered["type"] != "registered" {
			t.Fatalf("enroll reply=%v, want registered", registered)
		}
	}
	enroll()

	for i := 0; i < enrollmentFailureLimit; i++ {
		reply := registerWithNodeSecret(t, url, "known-host", fmt.Sprintf("guess-%d", i))
		if reply["code"] != "auth_failed" {
			t.Fatalf("attempt %d: code=%v, want auth_failed", i, reply["code"])
		}
	}
	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("guessing a known host's node_secret was not rate limited")
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%v, want 429", resp)
	}
}
