package crud

import (
	"context"
	"net/http"
	"testing"
)

// Operator reads and edits are the contract seam: authorization, inherited
// placement and optimistic conflicts must hold for custom and managed apps.
func TestAppPlacementOperatorContract(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	_, err := authSvc.Register(ctx, "placement-admin@test.local", "placement-admin", "fixture-password-01")
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `UPDATE users SET role='admin' WHERE email='placement-admin@test.local'`)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := authSvc.Login(ctx, "placement-admin@test.local", "fixture-password-01", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = authSvc.Register(ctx, "placement-user@test.local", "placement-user", "fixture-password-02")
	if err != nil {
		t.Fatal(err)
	}
	user, err := authSvc.Login(ctx, "placement-user@test.local", "fixture-password-02", "")
	if err != nil {
		t.Fatal(err)
	}
	var appID, tileID, hostID string
	if err := pool.QueryRow(ctx, `INSERT INTO apps(name) VALUES ('custom placement') RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO apps(name,parent_app_id,external_source,external_id)
		VALUES ('derived placement',$1::uuid,'steam','42') RETURNING id::text`, appID).Scan(&tileID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO hosts(node_name,status,capacity_detection)
		VALUES ('placement-host','online','ok') RETURNING id::text`).Scan(&hostID); err != nil {
		t.Fatal(err)
	}
	url := srv.URL + "/v1/admin/apps/" + appID + "/placement"
	if resp, _ := getReq(t, url, user.Plaintext); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("regular user placement read = %d", resp.StatusCode)
	}
	resp, view := getReq(t, url, admin.Plaintext)
	if resp.StatusCode != http.StatusOK || view["mode"] != "all_eligible" || view["revision"] != "0" {
		t.Fatalf("default placement = %d %+v", resp.StatusCode, view)
	}
	hosts := view["hosts"].([]any)
	if len(hosts) != 1 || hosts[0].(map[string]any)["selected"] != true {
		t.Fatalf("new host absent from dynamic selection: %+v", view)
	}
	if resp, _ := patch(t, url, map[string]any{"expected_revision": "0", "mode": "fixed", "host_ids": []string{hostID}}, user.Plaintext); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("regular user placement patch = %d", resp.StatusCode)
	}
	resp, view = patch(t, url, map[string]any{"expected_revision": "0", "mode": "fixed", "host_ids": []string{hostID}}, admin.Plaintext)
	if resp.StatusCode != http.StatusOK || view["mode"] != "fixed" || view["revision"] != "1" {
		t.Fatalf("fixed edit = %d %+v", resp.StatusCode, view)
	}
	resp, stale := patch(t, url, map[string]any{"expected_revision": "0", "mode": "fixed", "host_ids": []string{}}, admin.Plaintext)
	if resp.StatusCode != http.StatusConflict || stale["error"].(map[string]any)["code"] != "stale_revision" || stale["current"].(map[string]any)["revision"] != "1" {
		t.Fatalf("stale edit = %d %+v", resp.StatusCode, stale)
	}
	tileURL := srv.URL + "/v1/admin/apps/" + tileID + "/placement"
	resp, inherited := getReq(t, tileURL, admin.Plaintext)
	if resp.StatusCode != http.StatusOK || inherited["inherited_from"] != appID || inherited["app_id"] != appID {
		t.Fatalf("derived read = %d %+v", resp.StatusCode, inherited)
	}
	resp, rejected := patch(t, tileURL, map[string]any{"expected_revision": "1", "mode": "fixed", "host_ids": []string{}}, admin.Plaintext)
	if resp.StatusCode != http.StatusConflict || rejected["error"].(map[string]any)["code"] != "inherited_placement" || rejected["parent_app_id"] != appID {
		t.Fatalf("derived edit = %d %+v", resp.StatusCode, rejected)
	}
	if resp, _ := patch(t, url, map[string]any{"expected_revision": "1", "mode": "fixed", "host_ids": []string{"00000000-0000-4000-8000-000000000099"}}, admin.Plaintext); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown host edit = %d", resp.StatusCode)
	}
}
