package crud

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func doBody(t *testing.T, method, url, bearer, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestHostWebTransportEndpoint(t *testing.T) {
	pool := testDB(t)
	srv, adminTok, _, userTok := overrideServer(t, pool)
	ctx := context.Background()
	var hostID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO hosts (node_name, status, capacity_detection, transports, wt_port, wt_addresses, wt_certificates)
		VALUES ('wt-host','online','ok', '["webrtc","webtransport"]', 4433, '["192.0.2.10"]',
		        jsonb_build_array(jsonb_build_object('sha256', 'AAAA', 'not_before', now() - interval '1 day',
		                                             'not_after', now() + interval '10 days')))
		RETURNING id::text`).Scan(&hostID); err != nil {
		t.Fatal(err)
	}
	url := srv.URL + "/v1/admin/hosts/" + hostID + "/webtransport"
	get := func() map[string]any {
		t.Helper()
		resp, body := doBody(t, http.MethodGet, url, adminTok, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET: %d %s", resp.StatusCode, body)
		}
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	// Direct, from the agent's report.
	got := get()
	ep := got["endpoint"].(map[string]any)
	if got["usable"] != true || got["reason"] != nil || got["override"] != nil ||
		ep["url"] != "https://192.0.2.10:4433/wt" || ep["mode"] != "direct" {
		t.Fatalf("direct: %v", got)
	}
	if hashes := ep["cert_hashes"].([]any); len(hashes) != 1 || hashes[0].(map[string]any)["value"] != "AAAA" {
		t.Errorf("cert_hashes = %v", hashes)
	}

	// An edge override replaces the URL and drops the hashes.
	resp, body := doBody(t, http.MethodPatch, url, adminTok, `{"public_url":"https://play.example.com/wt/a","certificate":"public"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH: %d %s", resp.StatusCode, body)
	}
	got = get()
	ep = got["endpoint"].(map[string]any)
	if ep["url"] != "https://play.example.com/wt/a" || ep["mode"] != "edge" || len(ep["cert_hashes"].([]any)) != 0 {
		t.Errorf("edge: %v", got)
	}
	if ov := got["override"].(map[string]any); ov["public_url"] != "https://play.example.com/wt/a" || ov["certificate"] != "public" {
		t.Errorf("override = %v", ov)
	}

	// Clearing goes back to direct.
	if resp, body := doBody(t, http.MethodPatch, url, adminTok, `{"public_url":null}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("clear: %d %s", resp.StatusCode, body)
	}
	if got = get(); got["override"] != nil || got["endpoint"].(map[string]any)["mode"] != "direct" {
		t.Errorf("after clear: %v", got)
	}

	// Offline: not usable, endpoint null, reason says why.
	if _, err := pool.Exec(ctx, `UPDATE hosts SET status = 'offline' WHERE id = $1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	if got = get(); got["usable"] != false || got["endpoint"] != nil || got["reason"] != "host_offline" {
		t.Errorf("offline: %v", got)
	}

	// Validation.
	for name, payload := range map[string]string{
		"http":          `{"public_url":"http://play.example.com/wt"}`,
		"user info":     `{"public_url":"https://u@play.example.com/wt"}`,
		"fragment":      `{"public_url":"https://play.example.com/wt#x"}`,
		"bad cert":      `{"public_url":"https://play.example.com/wt","certificate":"self"}`,
		"unknown key":   `{"url":"https://play.example.com/wt"}`,
		"not an object": `[]`,
	} {
		if resp, body := doBody(t, http.MethodPatch, url, adminTok, payload); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, resp.StatusCode, body)
		}
	}

	// Admin only, enforced before the lookup; unknown hosts are 404.
	if resp, _ := doBody(t, http.MethodGet, url, userTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("user GET: %d, want 403", resp.StatusCode)
	}
	if resp, _ := doBody(t, http.MethodPatch, url, userTok, `{"public_url":"https://x.example.com/"}`); resp.StatusCode != http.StatusForbidden {
		t.Errorf("user PATCH: %d, want 403", resp.StatusCode)
	}
	missing := srv.URL + "/v1/admin/hosts/00000000-0000-0000-0000-000000000000/webtransport"
	if resp, _ := doBody(t, http.MethodGet, missing, adminTok, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown host GET: %d, want 404", resp.StatusCode)
	}
	if resp, _ := doBody(t, http.MethodPatch, srv.URL+"/v1/admin/hosts/nope/webtransport", adminTok, `{"public_url":null}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("malformed id PATCH: %d, want 404", resp.StatusCode)
	}

	// The change is in the audit trail.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_activity WHERE action = 'host.webtransport.update' AND target_id = $1`, hostID).Scan(&n); err != nil || n != 2 {
		t.Errorf("audit rows = %d (%v), want 2 (the set and the clear)", n, err)
	}
}

// A report that never mentioned WebTransport reads back as not reported.
func TestHostWebTransportNeverReported(t *testing.T) {
	pool := testDB(t)
	srv, adminTok, _, _ := overrideServer(t, pool)
	var hostID string
	if err := pool.QueryRow(context.Background(), `INSERT INTO hosts (node_name, status, capacity_detection)
		VALUES ('wt-none','online','ok') RETURNING id::text`).Scan(&hostID); err != nil {
		t.Fatal(err)
	}
	resp, body := doBody(t, http.MethodGet, srv.URL+"/v1/admin/hosts/"+hostID+"/webtransport", adminTok, "")
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil || resp.StatusCode != 200 {
		t.Fatalf("%d %s %v", resp.StatusCode, body, err)
	}
	if m["reported"] != nil || m["usable"] != false || m["reason"] != "not_reported" {
		t.Errorf("never reported: %v", m)
	}
}

// transports without webtransport is "reported" only to the agent; the admin read says null.
func TestHostWebTransportWebRTCOnlyReadsAsNotReported(t *testing.T) {
	pool := testDB(t)
	srv, adminTok, _, _ := overrideServer(t, pool)
	var hostID string
	if err := pool.QueryRow(context.Background(), `INSERT INTO hosts (node_name, status, capacity_detection, transports)
		VALUES ('wt-rtc','online','ok','["webrtc"]') RETURNING id::text`).Scan(&hostID); err != nil {
		t.Fatal(err)
	}
	_, body := doBody(t, http.MethodGet, srv.URL+"/v1/admin/hosts/"+hostID+"/webtransport", adminTok, "")
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	if m["reported"] != nil || m["reason"] != "not_reported" {
		t.Errorf("webrtc-only: %v", m)
	}
}
