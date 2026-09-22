package session

// codec_no_rung_400_http_test.go — #298: explicit codec override validation.
//
// An explicit stream.codec override that names a codec no rung of the chosen
// launch profile uses is refused with 400 validation_failed at the HTTP layer,
// distinct from 409 ErrCodecUnsupportedByHost (the host cannot encode a codec
// the profile offers).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestPostSessionsExplicitCodecNoRungReturns400 — #298: an explicit codec
// override naming a codec no rung of the chosen launch profile offers is
// refused with 400 validation_failed. The message names the codec and profile.
func TestPostSessionsExplicitCodecNoRungReturns400(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, _, _ := newStopServer(t, pool)
	ctx := context.Background()

	s := seed(t, pool, 4)
	_, err := authSvc.Register(ctx, "codec298@test.local", "codec298user", "quasar-fixture-pw-07")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	tok := loginTok(t, authSvc, "codec298@test.local", "quasar-fixture-pw-07")

	// Create a launch profile with only h264 codec.
	h264Only := chainRung{id: "1080p60-h264", codec: "h264", w: 1920, h: 1080, minBW: 10000}
	seedChain(t, pool, "h264-only", []chainRung{h264Only})
	allowLaunchProfiles(t, pool, s.appID, "h264-only")
	setAppProfilePolicy(t, pool, s.appID, "prefer", strPtr("h264-only"))

	// Try to launch with an explicit codec override for a codec not in the profile.
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(map[string]any{
		"app_id":     s.appID,
		"profile_id": "h264-only",
		"stream": map[string]any{
			"codec": "av1",
		},
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sessions", &buf)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/sessions: %v", err)
	}
	defer resp.Body.Close()

	// Should be 400, not 500.
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}

	// Parse the error response.
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("error object not found in response: %+v", body)
	}

	// Check the error code.
	if code, ok := errObj["code"].(string); !ok || code != "validation_failed" {
		t.Errorf("error.code: got %q, want validation_failed", code)
	}

	// Check that the message mentions the codec and profile.
	msg, ok := errObj["message"].(string)
	if !ok {
		t.Errorf("error.message missing or not a string: %+v", errObj)
		return
	}
	if msg == "" {
		t.Error("error.message is empty")
	}
	// Message should mention the codec override (av1) and the profile (h264-only).
	if !hasSubstring(msg, "av1") {
		t.Errorf("message does not mention codec av1: %q", msg)
	}
	if !hasSubstring(msg, "h264-only") {
		t.Errorf("message does not mention profile h264-only: %q", msg)
	}
}

// hasSubstring checks if a string contains a substring.
func hasSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
