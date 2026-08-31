package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// The ack→status table (session-capture). captureErrorFor is pure and total, so
// the whole contract an operator sees is checkable without a live agent — which
// matters most for the row that cannot be produced on demand at all: an agent
// build old enough to ignore the message.
func TestCaptureErrorFor(t *testing.T) {
	cases := []struct {
		name    string
		res     agentws.AckResult
		sendErr error
		want    error
		status  int
		code    string
	}{
		{"accepted", agentws.AckResult{OK: true}, nil, nil, http.StatusAccepted, ""},
		{"busy", agentws.AckResult{Error: "busy"}, nil, ErrCaptureBusy,
			http.StatusConflict, httpx.CodeCaptureBusy},
		{"unknown kind", agentws.AckResult{Error: "unknown_kind"}, nil, ErrCaptureKindUnsupported,
			http.StatusUnprocessableEntity, httpx.CodeCaptureKindUnsupported},
		{"unsupported here", agentws.AckResult{Error: "unsupported"}, nil, ErrCaptureKindUnsupported,
			http.StatusUnprocessableEntity, httpx.CodeCaptureKindUnsupported},
		{"no such session", agentws.AckResult{Error: "no_such_session"}, nil, ErrCaptureNotRunning,
			http.StatusConflict, httpx.CodeSessionNotRunning},
		{"agent gone", agentws.AckResult{}, agentws.ErrAgentNotConnected, ErrCaptureAgentNotConnected,
			http.StatusServiceUnavailable, httpx.CodeAgentNotConnected},
		{"send queue full", agentws.AckResult{}, agentws.ErrSendQueueFull, ErrCaptureAgentNotConnected,
			http.StatusServiceUnavailable, httpx.CodeAgentNotConnected},
		// THE load-bearing row. An agent that does not know session_capture is
		// wire-silent (agent-api.md), so the ack times out and there is no nack to
		// read. Every other command in this codebase calls that a rejection; a
		// capture mutates nothing, so it is safe — and far more useful — to call it
		// "this agent predates captures" and name the rebuild.
		{"no ack at all", agentws.AckResult{}, fmt.Errorf("ack wait: %w", context.DeadlineExceeded),
			ErrCaptureUnsupported, http.StatusNotImplemented, httpx.CodeCaptureUnsupported},
		// An agent may grow its nack vocabulary. An unrecognised reason is reported
		// verbatim under a generic conflict, never mapped onto a code that would
		// misdescribe it.
		{"unknown nack", agentws.AckResult{Error: "gpu_on_fire"}, nil, ErrCaptureRejected,
			http.StatusConflict, httpx.CodeConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := captureErrorFor(tc.res, tc.sendErr)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("want nil, got %v", got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}

// An unknown nack must carry the agent's own words: an operator staring at a 409
// needs to know WHAT the host said, since the control plane evidently has no
// opinion about it.
func TestCaptureErrorForKeepsTheAgentsReason(t *testing.T) {
	err := captureErrorFor(agentws.AckResult{Error: "gpu_on_fire"}, nil)
	if got := err.Error(); got == "" || !errors.Is(err, ErrCaptureRejected) ||
		!strings.Contains(got, "gpu_on_fire") {
		t.Fatalf("agent reason lost: %q", got)
	}
}

// Params are burst_stats' alone, defaulted, clamped in both directions, and
// bounded by the wall-clock budget — a legal 40×1000 plan would otherwise ask for
// 40 s inside a 10 s budget and be doomed to report an error instead of data.
func TestClampCaptureParams(t *testing.T) {
	if p := clampCaptureParams("pipeline_dot", &agentws.CaptureParams{Windows: 5, WindowMs: 200}); p != nil {
		t.Fatalf("pipeline_dot must carry no params, got %+v", p)
	}
	if p := clampCaptureParams("burst_stats", nil); p == nil || p.Windows != 20 || p.WindowMs != 250 {
		t.Fatalf("burst_stats default: %+v", p)
	}
	if p := clampCaptureParams("burst_stats", &agentws.CaptureParams{Windows: 999, WindowMs: 9999}); p == nil ||
		p.WindowMs != captureMaxWindowMs || p.Windows*p.WindowMs > captureMaxMs {
		t.Fatalf("over-large plan not clamped into the budget: %+v", p)
	}
	if p := clampCaptureParams("burst_stats", &agentws.CaptureParams{Windows: -3, WindowMs: 1}); p == nil ||
		p.Windows != captureMinWindows || p.WindowMs != captureMinWindowMs {
		t.Fatalf("under-small plan not clamped: %+v", p)
	}
}

// A kind the control plane does not know is refused HERE, before dispatch: an
// operator's typo should not cost a round trip to the host and back.
func TestArmCaptureRefusesUnknownKindWithoutDispatching(t *testing.T) {
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, nil, disp, testLogger())
	_, err := coord.ArmCapture(context.Background(), "s-1", CaptureRequest{Kind: "pipeline_dump"})
	if !errors.Is(err, ErrCaptureKindUnsupported) {
		t.Fatalf("want ErrCaptureKindUnsupported, got %v", err)
	}
	if len(disp.types()) != 0 {
		t.Fatalf("an unknown kind was dispatched: %v", disp.types())
	}
}

// The minted id is a uuid, and it is fresh every time — it is a URL path segment
// and the join key between a 202 and the event that answers it.
func TestNewCaptureIDShapeAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newCaptureID()
		if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[14] != '4' || id[18] != '-' || id[23] != '-' {
			t.Fatalf("not a v4 uuid: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate capture id: %q", id)
		}
		seen[id] = true
	}
}
