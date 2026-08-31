package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServerWithUser builds an auth test server and returns it alongside a
// bearer token for a freshly registered+logged-in user — the common setup for
// the self-only /v1/me/* endpoints.
func newTestServerWithUser(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := newTestServer(t)

	reg := map[string]string{"email": "prefs@example.com", "username": "prefsuser", "password": "overlay-strip-99"}
	if resp, body := post(t, srv.URL+"/v1/auth/register", reg, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: want 201, got %d (%v)", resp.StatusCode, body)
	}
	resp, body := post(t, srv.URL+"/v1/auth/login",
		map[string]string{"email": "prefs@example.com", "password": "overlay-strip-99"}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: want 200, got %d (%v)", resp.StatusCode, body)
	}
	token, _ := body["access_token"].(string)
	if token == "" {
		t.Fatalf("login: missing access_token in %v", body)
	}
	return srv, token
}

// patch mirrors post/get in this package (modelled exactly on the crud
// package's helper of the same name — internal/crud/handler_test.go).
func patch(t *testing.T, url string, body any, bearer string) (*http.Response, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(http.MethodPatch, url, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return do(t, req)
}

// TestHTTPUIPreferences drives both endpoints end-to-end and pins the
// behaviours that matter: unauthenticated is 401, a bad enum is 400 (not a
// silent clamp), a PATCH round-trips through a subsequent GET, and an
// unrecognised top-level key survives at the JSON top level (not nested under
// a synthetic wrapper) — proving Task 2's Extra flattening actually reaches
// the wire, not just the store.
func TestHTTPUIPreferences(t *testing.T) {
	srv, token := newTestServerWithUser(t)

	// No token → 401.
	resp, _ := get(t, srv.URL+"/v1/me/ui-preferences", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET = %d, want 401", resp.StatusCode)
	}

	// No token on PATCH → 401 too.
	resp, _ = patch(t, srv.URL+"/v1/me/ui-preferences", map[string]any{
		"session_overlay": map[string]any{"strip_position": "top"},
	}, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated PATCH = %d, want 401", resp.StatusCode)
	}

	// Fresh user → 200 with an empty overlay.
	resp, body := get(t, srv.URL+"/v1/me/ui-preferences", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d, want 200 (body %v)", resp.StatusCode, body)
	}
	overlay, _ := body["session_overlay"].(map[string]any)
	if len(overlay) != 0 {
		t.Fatalf("fresh user session_overlay = %v, want empty", overlay)
	}

	// Bad enum → 400 validation_failed, and nothing is persisted. ("left" is a
	// valid dock since the UI v3 amendment; "middle" is the value that is not.)
	resp, body = patch(t, srv.URL+"/v1/me/ui-preferences", map[string]any{
		"session_overlay": map[string]any{"strip_position": "middle"},
	}, token)
	if resp.StatusCode != http.StatusBadRequest || errCode(body) != "validation_failed" {
		t.Fatalf("bad enum PATCH = %d %v, want 400 validation_failed", resp.StatusCode, body)
	}
	resp, body = get(t, srv.URL+"/v1/me/ui-preferences", token)
	overlay, _ = body["session_overlay"].(map[string]any)
	if len(overlay) != 0 {
		t.Fatalf("rejected PATCH persisted state: %v", overlay)
	}

	// Unknown key inside session_overlay → 400 validation_failed too.
	resp, body = patch(t, srv.URL+"/v1/me/ui-preferences", map[string]any{
		"session_overlay": map[string]any{"strip_postion": "top"},
	}, token)
	if resp.StatusCode != http.StatusBadRequest || errCode(body) != "validation_failed" {
		t.Fatalf("unknown session_overlay key PATCH = %d %v, want 400 validation_failed", resp.StatusCode, body)
	}

	// Good PATCH → 200 and the value survives a GET.
	resp, body = patch(t, srv.URL+"/v1/me/ui-preferences", map[string]any{
		"session_overlay": map[string]any{"strip_position": "top", "strip_auto_hide": "never_visible"},
	}, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200 (body %v)", resp.StatusCode, body)
	}
	resp, body = get(t, srv.URL+"/v1/me/ui-preferences", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET after PATCH = %d, want 200", resp.StatusCode)
	}
	overlay, _ = body["session_overlay"].(map[string]any)
	if overlay["strip_position"] != "top" {
		t.Fatalf("strip_position = %v, want top", overlay["strip_position"])
	}
	if overlay["strip_auto_hide"] != "never_visible" {
		t.Fatalf("strip_auto_hide = %v, want never_visible", overlay["strip_auto_hide"])
	}

	// An unrecognised top-level key must round-trip through the HTTP surface
	// flattened at the document's top level, not nested or dropped — this is
	// where Task 2's Extra/MarshalJSON behaviour would silently regress if the
	// handler wrapped the store's response in another object.
	resp, body = patch(t, srv.URL+"/v1/me/ui-preferences", map[string]any{
		"library_density": "compact",
	}, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH with unknown top-level key = %d, want 200 (body %v)", resp.StatusCode, body)
	}
	if body["library_density"] != "compact" {
		t.Fatalf("PATCH response: library_density = %v, want compact (flattened at top level): %v", body["library_density"], body)
	}
	if _, nested := body["extra"]; nested {
		t.Fatalf("unknown key must not be nested under an \"extra\" wrapper: %v", body)
	}
	resp, body = get(t, srv.URL+"/v1/me/ui-preferences", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET after unknown-key PATCH = %d, want 200", resp.StatusCode)
	}
	if body["library_density"] != "compact" {
		t.Fatalf("GET response: library_density = %v, want compact (flattened at top level): %v", body["library_density"], body)
	}
	// The session_overlay set earlier must still be intact alongside the new key.
	overlay, _ = body["session_overlay"].(map[string]any)
	if overlay["strip_position"] != "top" {
		t.Fatalf("unknown-key PATCH clobbered session_overlay: %v", body)
	}

	// Malformed JSON body → 400 validation_failed.
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/v1/me/ui-preferences", strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, body = do(t, req)
	if resp.StatusCode != http.StatusBadRequest || errCode(body) != "validation_failed" {
		t.Fatalf("malformed JSON PATCH = %d %v, want 400 validation_failed", resp.StatusCode, body)
	}
}
