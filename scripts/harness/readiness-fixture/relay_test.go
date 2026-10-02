package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeControlPlane is a minimal WS echo/record server standing in for the
// control plane's /agent/ws.
type fakeControlPlane struct {
	srv      *httptest.Server
	upgrader websocket.Upgrader
	received chan []byte
	send     chan []byte
}

func newFakeControlPlane(t *testing.T) *fakeControlPlane {
	t.Helper()
	f := &fakeControlPlane{
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		received: make(chan []byte, 32),
		send:     make(chan []byte, 32),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		conn, err := f.upgrader.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		go func() {
			for msg := range f.send {
				if conn.WriteMessage(websocket.TextMessage, msg) != nil {
					return
				}
			}
		}()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			f.received <- data
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeControlPlane) wsURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http")
}

func startRelay(t *testing.T, upstream string) (agentWSURL, controlHTTPURL string, relay *Relay) {
	t.Helper()
	relay = NewRelay("", upstream, "")
	mux := http.NewServeMux()
	mux.HandleFunc("/", relay.AgentHandler)
	agentTS := httptest.NewServer(mux)
	t.Cleanup(agentTS.Close)

	controlTS := httptest.NewServer(relay.ControlMux())
	t.Cleanup(controlTS.Close)

	return "ws" + strings.TrimPrefix(agentTS.URL, "http"), controlTS.URL, relay
}

func dialAgentSide(t *testing.T, wsURL string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial relay as agent: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestRelayForwardsVerbatimBothDirections(t *testing.T) {
	fcp := newFakeControlPlane(t)
	wsURL, _, _ := startRelay(t, fcp.wsURL())

	agent := dialAgentSide(t, wsURL)

	if err := agent.WriteMessage(websocket.TextMessage, []byte(`{"type":"register","node_name":"x"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-fcp.received:
		if string(got) != `{"type":"register","node_name":"x"}` {
			t.Fatalf("upstream got mutated frame: %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for upstream to receive frame")
	}

	// binary frame, order preserved
	if err := agent.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-fcp.received:
		if string(got) != string([]byte{1, 2, 3}) {
			t.Fatalf("binary frame mutated: %v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for binary frame")
	}

	// downstream direction
	fcp.send <- []byte(`{"type":"session_assign","id":"1"}`)
	agent.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := agent.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"type":"session_assign","id":"1"}` {
		t.Fatalf("downstream frame mutated: %s", data)
	}
}

func TestRelayReconnectsRepeatedly(t *testing.T) {
	fcp := newFakeControlPlane(t)
	wsURL, _, _ := startRelay(t, fcp.wsURL())

	for i := 0; i < 3; i++ {
		agent := dialAgentSide(t, wsURL)
		if err := agent.WriteMessage(websocket.TextMessage, []byte(`{"type":"heartbeat"}`)); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		select {
		case <-fcp.received:
		case <-time.After(3 * time.Second):
			t.Fatalf("iteration %d: upstream never saw the frame", i)
		}
		agent.Close()
	}
}

func TestRelayCapacityRewriteEndToEnd(t *testing.T) {
	fcp := newFakeControlPlane(t)
	wsURL, controlURL, _ := startRelay(t, fcp.wsURL())

	ruleBody := `{"mode":"inject","check":{"id":"harness_synthetic_gate","status":"fail","summary":"s","remediation":"r","source":"host_probe","blocks":{"scope":"host","enforced_by":"control_plane"}}}`
	req, _ := http.NewRequest(http.MethodPut, controlURL+"/rule", strings.NewReader(ruleBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /rule status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	agent := dialAgentSide(t, wsURL)
	capacity := `{"type":"capacity","readiness":[{"id":"input_probe","status":"pass"}]}`
	if err := agent.WriteMessage(websocket.TextMessage, []byte(capacity)); err != nil {
		t.Fatal(err)
	}

	var got []byte
	select {
	case got = <-fcp.received:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out")
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(got, &top); err != nil {
		t.Fatal(err)
	}
	var checks []map[string]json.RawMessage
	json.Unmarshal(top["readiness"], &checks)
	if len(checks) != 2 {
		t.Fatalf("want 2 checks, got %d: %s", len(checks), got)
	}

	statsResp, err := http.Get(controlURL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer statsResp.Body.Close()
	var stats Stats
	json.NewDecoder(statsResp.Body).Decode(&stats)
	if stats.CapacitySeen != 1 || stats.CapacityRewritten != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if stats.LastCapacityAt == nil {
		t.Fatal("expected last_capacity_at to be set")
	}
}

func TestRelayModeOffForwardsCapacityUntouched(t *testing.T) {
	fcp := newFakeControlPlane(t)
	wsURL, _, _ := startRelay(t, fcp.wsURL())

	agent := dialAgentSide(t, wsURL)
	capacity := `{"type":"capacity","readiness":[{"id":"input_probe","status":"pass"}]}`
	if err := agent.WriteMessage(websocket.TextMessage, []byte(capacity)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-fcp.received:
		if string(got) != capacity {
			t.Fatalf("mode off must forward untouched, got %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out")
	}
}

func TestControlRuleValidation(t *testing.T) {
	relay := NewRelay("", "ws://unused", "")
	ts := httptest.NewServer(relay.ControlMux())
	defer ts.Close()

	bad := `{"mode":"inject","check":{"id":"not_synthetic","status":"fail"}}`
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/rule", strings.NewReader(bad))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for non-synthetic id, got %d", resp.StatusCode)
	}
}

func TestControlHealthz(t *testing.T) {
	relay := NewRelay("", "ws://unused", "")
	ts := httptest.NewServer(relay.ControlMux())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
}

// The agent makes plain HTTP calls to the same address it dials the WebSocket
// on. The relay passes them through untouched.
func TestRelayPassesPlainHTTPThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Path", r.URL.Path+"?"+r.URL.RawQuery)
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("from-control-plane"))
	}))
	defer upstream.Close()

	relay := NewRelay("", "ws"+strings.TrimPrefix(upstream.URL, "http")+"/agent/ws", "")
	front := httptest.NewServer(http.HandlerFunc(relay.AgentHandler))
	defer front.Close()

	resp, err := http.Get(front.URL + "/v1/agent/source-policy?x=1")
	if err != nil {
		t.Fatalf("GET through relay: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTeapot || string(body) != "from-control-plane" {
		t.Fatalf("got %d %q, want 418 from-control-plane", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Seen-Path"); got != "/v1/agent/source-policy?x=1" {
		t.Fatalf("upstream saw %q, want the original path and query", got)
	}
}
