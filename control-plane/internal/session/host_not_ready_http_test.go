package session

// The wire shape of the readiness refusal (#262): 503 host_not_ready, no
// Retry-After (an admin clears it, not a timer), and a message that names no
// check, scope, GPU or host — readiness detail is admin-only and lives on the
// host body.

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestLaunchHostNotReady503(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newCapacityTestServer(t, pool)
	ctx := context.Background()

	s := seed(t, pool, 4)
	reportReadiness(t, pool, s.hostID, 5, true, false)
	token, _ := registerUser(t, ctx, authSvc, "notready262@test.local", "notready262")

	resp, body := launchHTTP(t, srv.URL, token, s.appID)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("launch onto a readiness-blocked host: want 503, got %d (%+v)", resp.StatusCode, body)
	}
	if body.Error.Code != "host_not_ready" {
		t.Fatalf("error.code = %q, want host_not_ready", body.Error.Code)
	}
	if got := resp.Header.Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want none: the condition clears when an admin acts", got)
	}
	msg := strings.ToLower(body.Error.Message)
	for _, leak := range []string{"input_probe", "probe", "gpu", "readiness", "host-1"} {
		if strings.Contains(msg, leak) {
			t.Errorf("the message leaks admin-only detail (%q): %q", leak, body.Error.Message)
		}
	}
	if !strings.Contains(msg, "administrator") {
		t.Errorf("the refusal must tell the user an administrator has to act: %q", body.Error.Message)
	}
}
