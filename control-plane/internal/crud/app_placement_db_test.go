package crud

import (
	"context"
	"net/http"
	"testing"
)

func TestAppPlacementParentTransitions(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	admin := adminBearer(t, ctx, pool, authSvc, "placement-transition@test.local", "placement-transition")
	var parent, tile, standalone string
	if err := pool.QueryRow(ctx, `INSERT INTO apps(name) VALUES ('placement parent') RETURNING id::text`).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO apps(name,parent_app_id,external_source,external_id)
		VALUES ('placement tile',$1::uuid,'steam','991') RETURNING id::text`, parent).Scan(&tile); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO apps(name) VALUES ('standalone placement') RETURNING id::text`).Scan(&standalone); err != nil {
		t.Fatal(err)
	}

	// The supported PATCH path may detach a derived tile. The new canonical
	// app must immediately have its own dynamic placement and accept edits.
	resp, body := patch(t, srv.URL+"/v1/apps/"+tile, map[string]any{"parent_app_id": nil}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detach = %d %+v", resp.StatusCode, body)
	}
	resp, body = getReq(t, srv.URL+"/v1/admin/apps/"+tile+"/placement", admin)
	if resp.StatusCode != http.StatusOK || body["app_id"] != tile || body["inherited_from"] != nil || body["mode"] != "all_eligible" {
		t.Fatalf("detached placement = %d %+v", resp.StatusCode, body)
	}
	resp, body = patch(t, srv.URL+"/v1/admin/apps/"+tile+"/placement",
		map[string]any{"expected_revision": "0", "mode": "fixed", "host_ids": []string{}}, admin)
	if resp.StatusCode != http.StatusOK || body["mode"] != "fixed" {
		t.Fatalf("detached edit = %d %+v", resp.StatusCode, body)
	}

	// Converting a canonical app to a derived tile removes its independent
	// policy and serves the parent's placement, including after another detach.
	resp, body = patch(t, srv.URL+"/v1/apps/"+standalone,
		map[string]any{"parent_app_id": parent, "external_source": "steam", "external_id": "992"}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("derive = %d %+v", resp.StatusCode, body)
	}
	resp, body = getReq(t, srv.URL+"/v1/admin/apps/"+standalone+"/placement", admin)
	if resp.StatusCode != http.StatusOK || body["app_id"] != parent || body["inherited_from"] != parent {
		t.Fatalf("inherited placement = %d %+v", resp.StatusCode, body)
	}
	var independent int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app_placement WHERE app_id=$1::uuid`, standalone).Scan(&independent); err != nil || independent != 0 {
		t.Fatalf("derived independent rows = %d, err=%v", independent, err)
	}
}

func TestAppPlacementReadinessRequiresFreshEvidence(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	admin := adminBearer(t, ctx, pool, authSvc, "placement-evidence@test.local", "placement-evidence")
	var appID, hostID string
	if err := pool.QueryRow(ctx, `INSERT INTO apps(name) VALUES ('placement readiness') RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO hosts(node_name,status,capacity_detection)
		VALUES ('placement-evidence-host','online','ok') RETURNING id::text`).Scan(&hostID); err != nil {
		t.Fatal(err)
	}
	read := func() any {
		t.Helper()
		resp, body := getReq(t, srv.URL+"/v1/admin/apps/"+appID+"/placement", admin)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("placement read = %d %+v", resp.StatusCode, body)
		}
		return body["hosts"].([]any)[0].(map[string]any)["ready"]
	}
	if got := read(); got != nil {
		t.Fatalf("unreported ready = %v, want null", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET readiness='[]'::jsonb, readiness_reported_at=now() WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != true {
		t.Fatalf("fresh ready = %v, want true", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET readiness_reported_at=now()-interval '10 minutes' WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != nil {
		t.Fatalf("stale ready = %v, want null", got)
	}
}

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
