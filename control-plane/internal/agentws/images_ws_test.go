package agentws

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Image-management P2 wire handling: the upstream image_state message, the
// optional register `images` array, and the robustness rule that neither may
// cost a host its agent connection.
//
// DB-backed (register writes a hosts row), so TEST_DATABASE_URL-gated.

// recordingImageEvents captures the ImageEvents callbacks the handler makes.
type recordingImageEvents struct {
	mu         sync.Mutex
	states     []ImageStateMsg
	registered []recordedRegister
	stateCh    chan ImageStateMsg
	regCh      chan recordedRegister
}

type recordedRegister struct {
	Images   []RegisterImage
	Reported bool
}

func newRecorder() *recordingImageEvents {
	return &recordingImageEvents{
		stateCh: make(chan ImageStateMsg, 8),
		regCh:   make(chan recordedRegister, 8),
	}
}

func (r *recordingImageEvents) AgentImageState(_ context.Context, _ string, m ImageStateMsg) {
	r.mu.Lock()
	r.states = append(r.states, m)
	r.mu.Unlock()
	r.stateCh <- m
}

func (r *recordingImageEvents) AgentImagesRegistered(_ context.Context, _ string, imgs []RegisterImage, reported bool) {
	rec := recordedRegister{Images: imgs, Reported: reported}
	r.mu.Lock()
	r.registered = append(r.registered, rec)
	r.mu.Unlock()
	r.regCh <- rec
}

func (r *recordingImageEvents) stateCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.states)
}

// imageWSAgent brings a fake agent all the way through register + capacity, so
// the connection is in the steady-state message loop where image_state lives.
func imageWSAgent(t *testing.T, nodeName string, register map[string]any) (*websocket.Conn, *recordingImageEvents) {
	t.Helper()
	pool := testPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(pool, "test-token", log, nil, nil, nil, nil, nil)
	rec := newRecorder()
	h.SetImageEvents(rec)
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	if register == nil {
		register = map[string]any{}
	}
	register["type"] = "register"
	register["node_name"] = nodeName
	register["agent_version"] = "test"
	register["auth"] = map[string]string{"enrollment_token": "test-token"}
	if err := conn.WriteJSON(register); err != nil {
		t.Fatalf("write register: %v", err)
	}
	var registered map[string]any
	if err := conn.ReadJSON(&registered); err != nil {
		t.Fatalf("read registered: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "capacity",
		"host": map[string]any{"cpu_cores": 8, "mem_mb": 32000},
		"gpus": []map[string]any{{"index": 0, "vendor": "nvidia", "model": "test",
			"vram_mb_total": 16384, "encode_slots_total": 2}},
	}); err != nil {
		t.Fatalf("write capacity: %v", err)
	}
	return conn, rec
}

func waitState(t *testing.T, rec *recordingImageEvents) ImageStateMsg {
	t.Helper()
	select {
	case m := <-rec.stateCh:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an image_state callback")
		return ImageStateMsg{}
	}
}

// TestImageStateDispatchedToImageEvents — a well-formed image_state reaches the
// image-events surface with every field intact.
func TestImageStateDispatchedToImageEvents(t *testing.T) {
	conn, rec := imageWSAgent(t, "img-state-node", nil)

	if err := conn.WriteJSON(map[string]any{
		"type": "image_state", "image_id": "steam", "version": "2026.08.07",
		"state": "pulling", "progress_pct": 42, "bytes": 1234567, "error": "",
	}); err != nil {
		t.Fatalf("write image_state: %v", err)
	}
	m := waitState(t, rec)
	if m.ImageID != "steam" || m.Version != "2026.08.07" || m.State != "pulling" ||
		m.ProgressPct != 42 || m.Bytes != 1234567 {
		t.Fatalf("image_state decoded as %+v", m)
	}
}

// TestMalformedImageStateDoesNotDropConnection — the load-bearing robustness
// rule. An image report is not session authority; dropping the connection over
// one would reap the host's live sessions (schema.md invariant #3).
func TestMalformedImageStateDoesNotDropConnection(t *testing.T) {
	conn, rec := imageWSAgent(t, "img-bad-node", nil)

	// A type-correct discriminator with a garbage body: peekType succeeds, the
	// full decode does not.
	if err := conn.WriteMessage(websocket.TextMessage,
		[]byte(`{"type":"image_state","bytes":"not-a-number"}`)); err != nil {
		t.Fatalf("write malformed: %v", err)
	}
	// The connection must still be alive and still processing messages.
	if err := conn.WriteJSON(map[string]any{
		"type": "image_state", "image_id": "steam", "version": "1", "state": "ready",
	}); err != nil {
		t.Fatalf("write follow-up: %v", err)
	}
	if m := waitState(t, rec); m.State != "ready" {
		t.Fatalf("follow-up state = %q, want ready", m.State)
	}
	if got := rec.stateCount(); got != 1 {
		t.Fatalf("callbacks = %d, want 1 (the malformed frame must be dropped)", got)
	}
}

// TestRegisterImagesReportedAndAbsent — the `images` array is passed through as
// reported, and its ABSENCE is reported as absent rather than as an empty
// report. The two mean opposite things: absent keeps the stored rows, empty
// flips them off ready.
func TestRegisterImagesReportedAndAbsent(t *testing.T) {
	t.Run("reported", func(t *testing.T) {
		_, rec := imageWSAgent(t, "img-reg-node", map[string]any{
			"images": []map[string]any{{"image_id": "steam", "version": "2026.08.07", "state": "ready"}},
		})
		select {
		case got := <-rec.regCh:
			if !got.Reported || len(got.Images) != 1 || got.Images[0].ImageID != "steam" ||
				got.Images[0].State != "ready" || got.Images[0].Version != "2026.08.07" {
				t.Fatalf("register images = %+v", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for the register callback")
		}
	})

	t.Run("absent", func(t *testing.T) {
		_, rec := imageWSAgent(t, "img-reg-old-node", nil)
		select {
		case got := <-rec.regCh:
			if got.Reported || got.Images != nil {
				t.Fatalf("older agent reported %+v, want absent/nil", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for the register callback")
		}
	})

	t.Run("empty array is a real report", func(t *testing.T) {
		_, rec := imageWSAgent(t, "img-reg-empty-node", map[string]any{
			"images": []map[string]any{},
		})
		select {
		case got := <-rec.regCh:
			if !got.Reported || len(got.Images) != 0 {
				t.Fatalf("empty report = %+v, want reported with no images", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for the register callback")
		}
	})
}

// --- trust-boundary validation (review round #3) -----------------------------

// TestImageStateValidationDropsOversizedIDOrVersion — an image_id/version
// outside the documented bound drops the WHOLE message (there is nothing safe
// to store), without dropping the connection or affecting the next message.
func TestImageStateValidationDropsOversizedIDOrVersion(t *testing.T) {
	conn, rec := imageWSAgent(t, "img-oversized-node", nil)

	oversizedID := strings.Repeat("x", maxImageIDLen+1)
	if err := conn.WriteJSON(map[string]any{
		"type": "image_state", "image_id": oversizedID, "version": "1", "state": "ready",
	}); err != nil {
		t.Fatalf("write oversized image_id: %v", err)
	}
	oversizedVersion := strings.Repeat("v", maxVersionLen+1)
	if err := conn.WriteJSON(map[string]any{
		"type": "image_state", "image_id": "steam", "version": oversizedVersion, "state": "ready",
	}); err != nil {
		t.Fatalf("write oversized version: %v", err)
	}
	// A well-formed follow-up must still get through — the drop is per-message.
	if err := conn.WriteJSON(map[string]any{
		"type": "image_state", "image_id": "steam", "version": "1", "state": "ready",
	}); err != nil {
		t.Fatalf("write follow-up: %v", err)
	}
	m := waitState(t, rec)
	if m.State != "ready" || m.ImageID != "steam" {
		t.Fatalf("follow-up = %+v, want the well-formed message", m)
	}
	if got := rec.stateCount(); got != 1 {
		t.Fatalf("callbacks = %d, want 1 (both oversized messages must be dropped)", got)
	}
}

// TestImageStateValidationClampsErrorAndProgress — error is TRUNCATED rather
// than dropped (it is operator-facing context on an otherwise-valid state
// transition, unlike image_id/version where nothing safe remains to store);
// progress_pct and bytes are clamped into their documented range rather than
// rejecting the whole report.
func TestImageStateValidationClampsErrorAndProgress(t *testing.T) {
	conn, rec := imageWSAgent(t, "img-clamp-node", nil)

	longErr := strings.Repeat("e", maxImageErrLen+50)
	if err := conn.WriteJSON(map[string]any{
		"type": "image_state", "image_id": "steam", "version": "1", "state": "failed",
		"error": longErr, "progress_pct": 250, "bytes": -5,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	m := waitState(t, rec)
	if len(m.Error) != maxImageErrLen {
		t.Fatalf("error length = %d, want truncated to %d", len(m.Error), maxImageErrLen)
	}
	if m.ProgressPct != 100 {
		t.Fatalf("progress_pct = %d, want clamped to 100", m.ProgressPct)
	}
	if m.Bytes != 0 {
		t.Fatalf("bytes = %d, want clamped to 0", m.Bytes)
	}
}

// --- per-host rate limit (review round #3) ------------------------------------

// TestImageStateRateLimitDropsExcessNotConnection — a burst past the token
// bucket must drop the EXCESS messages without ever dropping the WebSocket
// connection: an image report is not session authority, and killing the
// connection over a noisy image_state stream would reap the host's live
// sessions (schema.md invariant #3).
func TestImageStateRateLimitDropsExcessNotConnection(t *testing.T) {
	conn, rec := imageWSAgent(t, "img-rl-node", nil)

	// Drain continuously so the recorder's small buffered channel never stalls
	// the server's read loop mid-burst — this test cares how many messages the
	// rate limiter LET THROUGH, not about consuming them in lockstep.
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-rec.stateCh:
			case <-done:
				return
			}
		}
	}()

	const sent = 40 // comfortably past the 30-token burst
	for i := 0; i < sent; i++ {
		if err := conn.WriteJSON(map[string]any{
			"type": "image_state", "image_id": "steam", "version": "1", "state": "pulling",
			"progress_pct": i % 100,
		}); err != nil {
			t.Fatalf("write image_state #%d: %v", i, err)
		}
	}

	// Let the read loop catch up on the burst.
	deadline := time.Now().Add(3 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		got = rec.stateCount()
		if got >= sent { // definitely not rate-limited — fail fast below
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got == 0 {
		t.Fatal("rate limiter dropped every message; want the burst allowance through")
	}
	if got >= sent {
		t.Fatalf("rate limiter let all %d messages through, want fewer than %d (the burst is 30)", got, sent)
	}

	// The connection must still be alive: after the refill window, a further
	// message still reaches the image-events surface.
	time.Sleep(1200 * time.Millisecond) // > 1 refill tick at 5 tokens/sec
	before := rec.stateCount()
	if err := conn.WriteJSON(map[string]any{
		"type": "image_state", "image_id": "steam", "version": "1", "state": "ready",
	}); err != nil {
		t.Fatalf("write follow-up after refill: %v", err)
	}
	waitDeadline := time.Now().Add(5 * time.Second)
	for rec.stateCount() == before && time.Now().Before(waitDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if rec.stateCount() <= before {
		t.Fatal("connection appears dropped: no follow-up callback after the rate-limit burst")
	}
}
