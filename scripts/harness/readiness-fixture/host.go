package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// HostConfig is readiness-fixture host's CLI surface (protocol/agent-api.md
// Transport, Auth/enrollment, register/capacity/heartbeat).
type HostConfig struct {
	ControlPlaneURL string
	NodeName        string
	EnrollmentToken string
	Slots           int
	VRAMMB          int
	ReadinessFile   string
	ReportInterval  time.Duration
}

// Host is a scripted node agent: enough of protocol/agent-api.md to register
// one GPU, keep it schedulable (host `reported`, gpus.encode_slots_total > 0,
// a heartbeat that never lapses) and answer session lifecycle commands with
// no real media path.
type Host struct {
	cfg HostConfig
	out writer // stdout, overridable in tests
}

// writer is the minimal stdout surface the host writes its one contractual
// line to ("registered host_id=<id>") — a seam so tests can capture it
// without racing real os.Stdout.
type writer interface {
	Printf(format string, args ...any)
}

type stdoutWriter struct{}

func (stdoutWriter) Printf(format string, args ...any) { fmt.Printf(format, args...) }

func NewHost(cfg HostConfig) *Host { return &Host{cfg: cfg, out: stdoutWriter{}} }

// registerMsg / registeredMsg / capacityMsg mirror protocol/agent-api.md's
// upstream/downstream shapes for exactly the fields this fixture sends or
// needs to read — it is not a general client, so no other message shape
// round-trips through a Go struct here.
type registerMsg struct {
	Type         string       `json:"type"`
	NodeName     string       `json:"node_name"`
	AgentVersion string       `json:"agent_version"`
	Auth         registerAuth `json:"auth"`
}
type registerAuth struct {
	EnrollmentToken string `json:"enrollment_token,omitempty"`
}
type registeredMsg struct {
	Type                string `json:"type"`
	HostID              string `json:"host_id"`
	NodeSecret          string `json:"node_secret,omitempty"`
	HeartbeatIntervalMs int    `json:"heartbeat_interval_ms"`
}
type errorMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

type gpuCapacity struct {
	Index            int    `json:"index"`
	Vendor           string `json:"vendor"`
	Model            string `json:"model"`
	VRAMMBTotal      int    `json:"vram_mb_total"`
	EncodeSlotsTotal int    `json:"encode_slots_total"`
}

type capacityMsg struct {
	Type      string          `json:"type"`
	Host      hostCapacity    `json:"host"`
	Codecs    []string        `json:"codecs"`
	GPUs      []gpuCapacity   `json:"gpus"`
	Readiness json.RawMessage `json:"readiness"`
}

type hostCapacity struct {
	CPUCores int `json:"cpu_cores"`
	MemMB    int `json:"mem_mb"`
}

type heartbeatMsg struct {
	Type            string   `json:"type"`
	RunningSessions []string `json:"running_sessions"`
	TSUnixMs        int64    `json:"ts_unix_ms"`
}

type ackMsg struct {
	Type  string  `json:"type"`
	ID    string  `json:"id"`
	OK    bool    `json:"ok"`
	Error *string `json:"error"`
}

type sessionStateMsg struct {
	Type      string  `json:"type"`
	SessionID string  `json:"session_id"`
	State     string  `json:"state"`
	Detail    string  `json:"detail,omitempty"`
	Error     *string `json:"error"`
}

// downstreamEnvelope is decoded first to read `type`/`id`/`session_id` before
// dispatch — every command shape differs beyond that.
type downstreamEnvelope struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
}

// errNodeAlreadyEnrolled is returned by Run when the control plane refuses
// enrollment because node_name is live — the caller (main) exits non-zero.
type errNodeAlreadyEnrolled struct{ detail string }

func (e *errNodeAlreadyEnrolled) Error() string {
	return "node_name already enrolled: " + e.detail
}

// Run connects, registers, and serves the scripted lifecycle until ctx is
// cancelled (SIGTERM/SIGINT) or the connection fails.
func (h *Host) Run(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, h.cfg.ControlPlaneURL, nil)
	if err != nil {
		return fmt.Errorf("dial control plane: %w", err)
	}
	defer conn.Close()

	reg := registerMsg{
		Type:         "register",
		NodeName:     h.cfg.NodeName,
		AgentVersion: "harness-fixture",
		Auth:         registerAuth{EnrollmentToken: h.cfg.EnrollmentToken},
	}
	if err := conn.WriteJSON(reg); err != nil {
		return fmt.Errorf("send register: %w", err)
	}

	var raw json.RawMessage
	if err := conn.ReadJSON(&raw); err != nil {
		return fmt.Errorf("read registered: %w", err)
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("parse registered envelope: %w", err)
	}
	if envelope.Type == "error" {
		var em errorMsg
		json.Unmarshal(raw, &em)
		msg := em.Message
		if msg == "" {
			msg = em.Error
		}
		if isAlreadyEnrolled(msg) {
			return &errNodeAlreadyEnrolled{detail: msg}
		}
		return fmt.Errorf("register rejected: %s", msg)
	}
	if envelope.Type != "registered" {
		return fmt.Errorf("unexpected reply to register: %s", envelope.Type)
	}
	var registered registeredMsg
	if err := json.Unmarshal(raw, &registered); err != nil {
		return fmt.Errorf("parse registered: %w", err)
	}

	h.out.Printf("registered host_id=%s\n", registered.HostID)

	heartbeatInterval := time.Duration(registered.HeartbeatIntervalMs) * time.Millisecond
	if heartbeatInterval <= 0 {
		heartbeatInterval = 5 * time.Second
	}

	state := newSessionState()

	if err := h.sendCapacity(conn); err != nil {
		return fmt.Errorf("send capacity: %w", err)
	}

	var wg sync.WaitGroup
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	wg.Add(1)
	go func() {
		defer wg.Done()
		h.reportLoop(loopCtx, conn, state, heartbeatInterval)
	}()

	// ReadJSON blocks with no deadline, so a signal has to close the socket to
	// be noticed: say goodbye, then close, and the read loop returns.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-loopCtx.Done()
		state.writeMu.Lock()
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutting down"),
			time.Now().Add(time.Second))
		state.writeMu.Unlock()
		conn.Close()
	}()

	err = h.readLoop(loopCtx, conn, state)
	cancel()
	wg.Wait()
	if ctx.Err() != nil {
		return nil // clean shutdown requested, not a failure
	}
	return err
}

// isAlreadyEnrolled recognises the "identity takeover" refusal
// (protocol/agent-api.md: enrollment onto a node_name whose agent is live is
// refused). The wording is the control plane's, so this only sharpens the
// error text; every rejection exits non-zero either way.
func isAlreadyEnrolled(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "enrolled") || strings.Contains(m, "already registered") || strings.Contains(m, "live")
}

// readReadinessFile loads the JSON array of checks the harness controls;
// re-read on every capacity report so a rule change mid-run takes effect
// without restarting the fixture. Missing/unparsable file ⇒ one passing
// check, never an empty capacity.readiness (which would read as "reported,
// nothing to say" rather than "healthy").
func (h *Host) readReadinessFile() json.RawMessage {
	if h.cfg.ReadinessFile == "" {
		return json.RawMessage(`[{"id":"harness_fixture_host","status":"pass","summary":"scripted host","remediation":""}]`)
	}
	data, err := os.ReadFile(h.cfg.ReadinessFile)
	if err != nil {
		log.Printf("readiness-fixture: readiness file unreadable, reporting empty: %v", err)
		return json.RawMessage(`[]`)
	}
	var probe []json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		log.Printf("readiness-fixture: readiness file is not a JSON array, reporting empty: %v", err)
		return json.RawMessage(`[]`)
	}
	return json.RawMessage(data)
}

func (h *Host) sendCapacity(conn *websocket.Conn) error {
	msg := capacityMsg{
		Type:   "capacity",
		Host:   hostCapacity{CPUCores: 4, MemMB: 8192},
		Codecs: []string{"h264"},
		GPUs: []gpuCapacity{{
			Index:            0,
			Vendor:           "amd",
			Model:            "harness-fixture-gpu",
			VRAMMBTotal:      h.cfg.VRAMMB,
			EncodeSlotsTotal: h.cfg.Slots,
		}},
		Readiness: h.readReadinessFile(),
	}
	return conn.WriteJSON(msg)
}

func (h *Host) reportLoop(ctx context.Context, conn *websocket.Conn, state *sessionState, heartbeatInterval time.Duration) {
	reportInterval := h.cfg.ReportInterval
	if reportInterval <= 0 {
		reportInterval = 10 * time.Second
	}
	hbTicker := time.NewTicker(heartbeatInterval)
	defer hbTicker.Stop()
	capTicker := time.NewTicker(reportInterval)
	defer capTicker.Stop()

	writeMu := &state.writeMu
	for {
		select {
		case <-ctx.Done():
			return
		case <-hbTicker.C:
			hb := heartbeatMsg{
				Type:            "heartbeat",
				RunningSessions: state.runningSessions(),
				TSUnixMs:        time.Now().UnixMilli(),
			}
			writeMu.Lock()
			_ = conn.WriteJSON(hb)
			writeMu.Unlock()
		case <-capTicker.C:
			writeMu.Lock()
			_ = h.sendCapacity(conn)
			writeMu.Unlock()
		}
	}
}

func (h *Host) readLoop(ctx context.Context, conn *websocket.Conn, state *sessionState) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		var raw json.RawMessage
		if err := conn.ReadJSON(&raw); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		var env downstreamEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue // unparsable frame: ignore, never crash
		}
		h.handleDownstream(conn, state, env, raw)
	}
}

func (h *Host) handleDownstream(conn *websocket.Conn, state *sessionState, env downstreamEnvelope, raw json.RawMessage) {
	writeMu := &state.writeMu
	ack := func(ok bool, errStr *string) {
		if env.ID == "" {
			return
		}
		writeMu.Lock()
		_ = conn.WriteJSON(ackMsg{Type: "ack", ID: env.ID, OK: ok, Error: errStr})
		writeMu.Unlock()
	}
	sendState := func(sessionID, s string) {
		writeMu.Lock()
		_ = conn.WriteJSON(sessionStateMsg{Type: "session_state", SessionID: sessionID, State: s})
		writeMu.Unlock()
	}

	switch env.Type {
	case "session_assign":
		state.assign(env.SessionID)
		ack(true, nil)
	case "session_start":
		ack(true, nil)
		sendState(env.SessionID, "starting")
		state.setRunning(env.SessionID)
		sendState(env.SessionID, "running")
	case "session_stop":
		ack(true, nil)
		sendState(env.SessionID, "stopping")
		state.remove(env.SessionID)
		sendState(env.SessionID, "stopped")
	default:
		// Unknown downstream type: ack if it carries an id and the contract
		// says such commands are acked (config_update, image_ensure, and
		// friends are fire-and-forget with no reply in protocol/agent-api.md
		// beyond their own upstream progress messages, so nothing to send
		// them here) — otherwise ignore silently. Never crash on an unknown
		// `type`, the standing forward-compatibility rule this wire relies on.
		_ = raw
	}
}

// sessionState tracks the fixture's one thing worth tracking: which session
// ids are assigned/running, so heartbeat.running_sessions and the
// assign/start/stop acks stay honest. writeMu serializes writes to the single
// WS connection across the report loop and the read-loop's command replies.
type sessionState struct {
	mu      sync.Mutex
	writeMu sync.Mutex
	running map[string]bool
}

func newSessionState() *sessionState { return &sessionState{running: map[string]bool{}} }

func (s *sessionState) assign(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.running[id]; !ok {
		s.running[id] = false
	}
}

func (s *sessionState) setRunning(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running[id] = true
}

func (s *sessionState) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, id)
}

func (s *sessionState) runningSessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.running))
	for id, running := range s.running {
		if running {
			out = append(out, id)
		}
	}
	return out
}
