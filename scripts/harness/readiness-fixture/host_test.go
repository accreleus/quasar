package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// recordingWriter captures the host's one stdout line for assertion.
type recordingWriter struct {
	mu    sync.Mutex
	lines []string
}

func (w *recordingWriter) Printf(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, fmt.Sprintf(format, args...))
}

func (w *recordingWriter) last() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.lines) == 0 {
		return ""
	}
	return w.lines[len(w.lines)-1]
}

// fakeCPForHost is a scriptable fake control plane driving the scripted host
// through register -> capacity -> assign/start/stop.
type fakeCPForHost struct {
	srv        *httptest.Server
	upgrader   websocket.Upgrader
	refuse     bool
	mu         sync.Mutex
	conn       *websocket.Conn
	capacities chan capacityMsg
	acks       chan ackMsg
	states     chan sessionStateMsg
	heartbeats chan heartbeatMsg
}

func newFakeCPForHost(t *testing.T) *fakeCPForHost {
	t.Helper()
	f := &fakeCPForHost{
		upgrader:   websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		capacities: make(chan capacityMsg, 8),
		acks:       make(chan ackMsg, 8),
		states:     make(chan sessionStateMsg, 8),
		heartbeats: make(chan heartbeatMsg, 8),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		conn, err := f.upgrader.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conn = conn
		f.mu.Unlock()

		var reg registerMsg
		if err := conn.ReadJSON(&reg); err != nil {
			return
		}
		if f.refuse {
			conn.WriteJSON(map[string]string{"type": "error", "message": "node_name already enrolled and live"})
			conn.Close()
			return
		}
		conn.WriteJSON(registeredMsg{Type: "registered", HostID: "host-123", NodeSecret: "s3cr3t", HeartbeatIntervalMs: 50})

		for {
			var raw json.RawMessage
			if err := conn.ReadJSON(&raw); err != nil {
				return
			}
			var env struct {
				Type string `json:"type"`
			}
			json.Unmarshal(raw, &env)
			switch env.Type {
			case "capacity":
				var c capacityMsg
				json.Unmarshal(raw, &c)
				f.capacities <- c
			case "heartbeat":
				var hb heartbeatMsg
				json.Unmarshal(raw, &hb)
				f.heartbeats <- hb
			case "ack":
				var a ackMsg
				json.Unmarshal(raw, &a)
				f.acks <- a
			case "session_state":
				var s sessionStateMsg
				json.Unmarshal(raw, &s)
				f.states <- s
			}
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCPForHost) wsURL() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") }

func (f *fakeCPForHost) send(v any) {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	conn.WriteJSON(v)
}

func recv[T any](t *testing.T, ch chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

func TestHostRegisterCapacityLifecycle(t *testing.T) {
	fcp := newFakeCPForHost(t)
	out := &recordingWriter{}
	h := &Host{cfg: HostConfig{
		ControlPlaneURL: fcp.wsURL(),
		NodeName:        "rh02h-test-host",
		EnrollmentToken: "tok",
		Slots:           1,
		VRAMMB:          8192,
		ReportInterval:  30 * time.Second,
	}, out: out}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()

	cap := recv(t, fcp.capacities, "initial capacity")
	if len(cap.GPUs) != 1 || cap.GPUs[0].EncodeSlotsTotal != 1 || cap.GPUs[0].VRAMMBTotal != 8192 {
		t.Fatalf("unexpected capacity: %+v", cap)
	}
	if cap.GPUs[0].Vendor == "" {
		t.Fatal("expected a vendor on the reported GPU")
	}
	var checks []map[string]json.RawMessage
	if err := json.Unmarshal(cap.Readiness, &checks); err != nil || len(checks) == 0 {
		t.Fatalf("expected a non-empty default readiness array, got %s (%v)", cap.Readiness, err)
	}

	if out.last() != "registered host_id=host-123\n" {
		t.Fatalf("unexpected stdout line: %q", out.last())
	}

	// heartbeat cadence
	recv(t, fcp.heartbeats, "heartbeat")

	// session lifecycle
	fcp.send(map[string]string{"type": "session_assign", "id": "cmd-1", "session_id": "sess-1"})
	ack1 := recv(t, fcp.acks, "assign ack")
	if ack1.ID != "cmd-1" || !ack1.OK {
		t.Fatalf("unexpected assign ack: %+v", ack1)
	}

	fcp.send(map[string]string{"type": "session_start", "id": "cmd-2", "session_id": "sess-1"})
	ack2 := recv(t, fcp.acks, "start ack")
	if ack2.ID != "cmd-2" || !ack2.OK {
		t.Fatalf("unexpected start ack: %+v", ack2)
	}
	st1 := recv(t, fcp.states, "starting")
	if st1.State != "starting" {
		t.Fatalf("expected starting, got %+v", st1)
	}
	st2 := recv(t, fcp.states, "running")
	if st2.State != "running" {
		t.Fatalf("expected running, got %+v", st2)
	}

	hb := recv(t, fcp.heartbeats, "heartbeat after start")
	if len(hb.RunningSessions) != 1 || hb.RunningSessions[0] != "sess-1" {
		t.Fatalf("expected running_sessions=[sess-1], got %+v", hb)
	}

	fcp.send(map[string]string{"type": "session_stop", "id": "cmd-3", "session_id": "sess-1"})
	ack3 := recv(t, fcp.acks, "stop ack")
	if ack3.ID != "cmd-3" || !ack3.OK {
		t.Fatalf("unexpected stop ack: %+v", ack3)
	}
	stopping := recv(t, fcp.states, "stopping")
	if stopping.State != "stopping" {
		t.Fatalf("expected stopping, got %+v", stopping)
	}
	stopped := recv(t, fcp.states, "stopped")
	if stopped.State != "stopped" {
		t.Fatalf("expected stopped, got %+v", stopped)
	}

	// unknown downstream type must never crash the host
	fcp.send(map[string]string{"type": "config_update"})
	fcp.send(map[string]string{"type": "image_ensure", "id": "cmd-4"})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error on clean shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after context cancellation")
	}
}

func TestHostEnrollmentRefusalIsNonZero(t *testing.T) {
	fcp := newFakeCPForHost(t)
	fcp.refuse = true
	h := &Host{cfg: HostConfig{
		ControlPlaneURL: fcp.wsURL(),
		NodeName:        "rh02h-test-host",
		EnrollmentToken: "tok",
		Slots:           1,
		VRAMMB:          8192,
	}, out: &recordingWriter{}}

	err := h.Run(context.Background())
	if err == nil {
		t.Fatal("expected an error when the control plane refuses enrollment")
	}
	var alreadyEnrolled *errNodeAlreadyEnrolled
	if !isAlreadyEnrolledErr(err, &alreadyEnrolled) {
		t.Fatalf("expected an errNodeAlreadyEnrolled, got %v (%T)", err, err)
	}
}

func isAlreadyEnrolledErr(err error, target **errNodeAlreadyEnrolled) bool {
	if e, ok := err.(*errNodeAlreadyEnrolled); ok {
		*target = e
		return true
	}
	return false
}

func TestHostReadinessFileReReadEachReport(t *testing.T) {
	fcp := newFakeCPForHost(t)
	dir := t.TempDir()
	path := dir + "/readiness.json"
	writeFile(t, path, `[{"id":"a","status":"pass"}]`)

	h := &Host{cfg: HostConfig{
		ControlPlaneURL: fcp.wsURL(),
		NodeName:        "rh02h-test-host",
		EnrollmentToken: "tok",
		Slots:           1,
		VRAMMB:          8192,
		ReadinessFile:   path,
		ReportInterval:  50 * time.Millisecond,
	}, out: &recordingWriter{}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)

	first := recv(t, fcp.capacities, "first capacity")
	var firstChecks []map[string]json.RawMessage
	json.Unmarshal(first.Readiness, &firstChecks)
	if len(firstChecks) != 1 {
		t.Fatalf("unexpected first readiness: %s", first.Readiness)
	}

	writeFile(t, path, `[{"id":"a","status":"fail"},{"id":"b","status":"pass"}]`)

	deadline := time.After(3 * time.Second)
	for {
		select {
		case c := <-fcp.capacities:
			var checks []map[string]json.RawMessage
			json.Unmarshal(c.Readiness, &checks)
			if len(checks) == 2 {
				return
			}
		case <-deadline:
			t.Fatal("readiness file change was never picked up by a later capacity report")
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
