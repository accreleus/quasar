package crud

// PUT / DELETE /v1/admin/hosts/{id}/readiness-overrides/{check_id}
// (control-api.md "Readiness override"). The lifecycle, the lapse and the race
// against a report are tested in internal/readinessgate; this file is the HTTP
// contract: statuses, the body, validation, and authorization before lookup.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
)

func overrideServer(t *testing.T, pool *pgxpool.Pool) (srv *httptest.Server, adminTok, adminID, userTok string) {
	t.Helper()
	ctx := context.Background()
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	NewHandler(pool, audit.NewStore(pool)).Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	admin, err := authSvc.Register(ctx, "ovr-admin@test.local", "ovradmin", "unrelated-pw-02")
	if err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE id::text=$1`, admin.ID); err != nil {
		t.Fatalf("promote: %v", err)
	}
	adminLogin, err := authSvc.Login(ctx, "ovr-admin@test.local", "unrelated-pw-02", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	if _, err := authSvc.Register(ctx, "ovr-user@test.local", "ovruser", "unrelated-pw-01"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	userLogin, err := authSvc.Login(ctx, "ovr-user@test.local", "unrelated-pw-01", "")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}
	return srv, adminLogin.Plaintext, admin.ID, userLogin.Plaintext
}

// blockedHost stores a fresh report with two failing evidence checks and one
// agent-enforced one, with the derived columns as the verdict writer leaves them.
func blockedHost(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var hostID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO hosts (node_name, status, capacity_detection, readiness, readiness_reported_at, readiness_block_host)
		VALUES ('ovr-host','online','ok', $1::jsonb, now(), true) RETURNING id::text`, `[
		{"id":"audio_probe","status":"fail","summary":"s","remediation":"","blocks":{"scope":"host","enforced_by":"control_plane"}},
		{"id":"runtime_endpoint","status":"fail","summary":"s","remediation":"","blocks":{"scope":"host","enforced_by":"agent"}},
		{"id":"render_node","status":"fail","summary":"s","remediation":""},
		{"id":"input_probe","status":"pass","summary":"s","remediation":"","blocks":{"scope":"host","enforced_by":"control_plane"}}
	]`).Scan(&hostID); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	return hostID
}

func do(t *testing.T, method, url, bearer string) (*http.Response, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func errCode(t *testing.T, body []byte) (code, message string) {
	t.Helper()
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return e.Error.Code, e.Error.Message
}

func TestReadinessOverridePutAndDelete(t *testing.T) {
	pool := testDB(t)
	srv, adminTok, adminID, _ := overrideServer(t, pool)
	hostID := blockedHost(t, pool)
	url := srv.URL + "/v1/admin/hosts/" + hostID + "/readiness-overrides/audio_probe"

	resp, body := do(t, http.MethodPut, url, adminTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: %d %s", resp.StatusCode, body)
	}
	var first map[string]any
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"check_id", "created_by", "created_by_username", "created_at", "inert"} {
		if _, ok := first[k]; !ok {
			t.Errorf("PUT body lacks %q: %s", k, body)
		}
	}
	if len(first) != 5 || first["check_id"] != "audio_probe" || first["created_by"] != adminID ||
		first["created_by_username"] != "ovradmin" || first["inert"] != false {
		t.Fatalf("PUT body = %s", body)
	}

	resp, body = do(t, http.MethodPut, url, adminTok)
	var second map[string]any
	_ = json.Unmarshal(body, &second)
	if resp.StatusCode != http.StatusOK || second["created_at"] != first["created_at"] {
		t.Fatalf("repeat PUT: %d %s, want 200 with the existing override", resp.StatusCode, body)
	}

	// The host body shows it, and never hides the failing check behind it.
	resp, body = do(t, http.MethodGet, srv.URL+"/v1/hosts/"+hostID, adminTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET host: %d", resp.StatusCode)
	}
	var wrapped struct {
		Host struct {
			Gate struct {
				Blocking []struct {
					CheckID    string `json:"check_id"`
					Overridden bool   `json:"overridden"`
				} `json:"blocking"`
			} `json:"readiness_gate"`
			Overrides []map[string]any `json:"readiness_overrides"`
		} `json:"host"`
	}
	if err := json.Unmarshal(body, &wrapped); err != nil {
		t.Fatal(err)
	}
	host := wrapped.Host
	if len(host.Overrides) != 1 || host.Overrides[0]["check_id"] != "audio_probe" ||
		host.Overrides[0]["created_by_username"] != "ovradmin" || host.Overrides[0]["inert"] != false {
		t.Fatalf("readiness_overrides = %+v", host.Overrides)
	}
	seen := map[string]bool{}
	for _, b := range host.Gate.Blocking {
		seen[b.CheckID] = true
		if want := b.CheckID == "audio_probe"; b.Overridden != want {
			t.Errorf("blocking %s overridden=%v, want %v", b.CheckID, b.Overridden, want)
		}
	}
	if !seen["audio_probe"] || !seen["runtime_endpoint"] || len(seen) != 2 {
		t.Fatalf("blocking = %+v, want both failing evidence checks, the overridden one included", host.Gate.Blocking)
	}

	for i := 0; i < 2; i++ {
		resp, body = do(t, http.MethodDelete, url, adminTok)
		if resp.StatusCode != http.StatusNoContent || len(body) != 0 {
			t.Fatalf("DELETE #%d: %d %q, want 204 and no body", i+1, resp.StatusCode, body)
		}
	}
}

func TestReadinessOverrideRefusals(t *testing.T) {
	pool := testDB(t)
	srv, adminTok, _, _ := overrideServer(t, pool)
	hostID := blockedHost(t, pool)
	base := srv.URL + "/v1/admin/hosts/" + hostID + "/readiness-overrides/"
	const nobody = "00000000-0000-4000-8000-000000000000"

	conflicts := map[string]string{
		"runtime_endpoint": "agent", // enforced by the agent: no override lifts it
		"render_node":      "",      // a proxy: carries no blocks
		"input_probe":      "",      // passing
		"no_such_check":    "",
	}
	for id, mustMention := range conflicts {
		resp, body := do(t, http.MethodPut, base+id, adminTok)
		code, msg := errCode(t, body)
		if resp.StatusCode != http.StatusConflict || code != "conflict" || msg == "" {
			t.Errorf("PUT %s: %d %s, want 409 conflict with a message", id, resp.StatusCode, body)
		}
		if mustMention != "" && !strings.Contains(strings.ToLower(msg), mustMention) {
			t.Errorf("PUT %s: message %q must say which precondition failed (%q)", id, msg, mustMention)
		}
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM host_readiness_overrides`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused overrides stored %d rows (err %v)", n, err)
	}

	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		resp, body := do(t, method, srv.URL+"/v1/admin/hosts/"+nobody+"/readiness-overrides/audio_probe", adminTok)
		if code, _ := errCode(t, body); resp.StatusCode != http.StatusNotFound || code != "not_found" {
			t.Errorf("%s on an unknown host: %d %s, want 404 not_found", method, resp.StatusCode, body)
		}
	}

	// One path segment, percent-decoded once, 1–128 bytes, [A-Za-z0-9._:-].
	malformed := []string{
		strings.Repeat("a", 129),
		"audio%20probe", // a space
		"audio%2Fprobe", // a decoded slash
		"audio;probe",
		"audio_probe%00",
		"%C3%A9tat", // non-ASCII
		"a%25b",     // a literal percent after one decode
	}
	for _, id := range malformed {
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			resp, body := do(t, method, base+id, adminTok)
			if code, _ := errCode(t, body); resp.StatusCode != http.StatusBadRequest || code != "validation_failed" {
				t.Errorf("%s %q: %d %s, want 400 validation_failed", method, id, resp.StatusCode, body)
			}
		}
	}
	for _, id := range []string{strings.Repeat("a", 128), "A.b_c:d-9", "media_probe_gpu1"} {
		resp, _ := do(t, http.MethodDelete, base+id, adminTok)
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("DELETE %q: %d, want 204: a well-formed id is never a validation error", id, resp.StatusCode)
		}
	}
}

// TestReadinessOverrideIsAdminOnlyBeforeAnyLookup: authorization is decided at
// route registration, so a non-admin learns nothing about hosts or checks.
func TestReadinessOverrideIsAdminOnlyBeforeAnyLookup(t *testing.T) {
	pool := testDB(t)
	srv, _, _, userTok := overrideServer(t, pool)
	hostID := blockedHost(t, pool)
	const nobody = "00000000-0000-4000-8000-000000000000"

	paths := []string{
		"/v1/admin/hosts/" + hostID + "/readiness-overrides/audio_probe",
		"/v1/admin/hosts/" + nobody + "/readiness-overrides/audio_probe",
		"/v1/admin/hosts/" + hostID + "/readiness-overrides/audio;probe",
		"/v1/admin/hosts/not-a-uuid/readiness-overrides/audio_probe",
	}
	for _, p := range paths {
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			resp, body := do(t, method, srv.URL+p, userTok)
			if code, _ := errCode(t, body); resp.StatusCode != http.StatusForbidden || code != "forbidden" {
				t.Errorf("%s %s as a non-admin: %d %s, want 403 before any lookup or validation", method, p, resp.StatusCode, body)
			}
			resp, _ = do(t, method, srv.URL+p, "")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with no token: %d, want 401", method, p, resp.StatusCode)
			}
		}
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM host_readiness_overrides`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a non-admin stored %d override rows (err %v)", n, err)
	}
}
