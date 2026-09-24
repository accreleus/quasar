package crud

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// A selected app reference created while cleanup commits must be ordered
// behind its fence transition. This drives the actual authenticated app PATCH
// route against fresh Postgres, not a helper that merely mimics its SQL.
func TestRH05AppPatchWaitsForCleanupFenceTransaction(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	admin := adminBearer(t, ctx, pool, authSvc, "cleanup-writer@test.local", "cleanup-writer")
	var host, app string
	if err := pool.QueryRow(ctx, `INSERT INTO hosts(node_name,status,capacity_detection)
		VALUES('cleanup-writer-host','online','ok') RETURNING id::text`).Scan(&host); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO apps(name,runtime_spec)
		VALUES('cleanup-writer-app','{}'::jsonb) RETURNING id::text`).Scan(&app); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ref := "registry.example.test/retired@sha256:abc"
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(4,hashtext($1::text))`, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO host_image_operation_fences(host_id,image_id,generation,state)
		VALUES($1::uuid,'retired',1,'removing')`, host); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO host_image_operation_fences(host_id,image_id,generation,state)
		VALUES($1::uuid,'unrelated',5,'idle')`, host); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO host_image_cleanup_attempts
		(id,host_id,image_id,version,image_ref,runtime_image_id,generation,state,created_at,updated_at)
		VALUES('11111111-1111-4111-8111-111111111111',$1::uuid,'retired','v1',$2,'sha256:daemon',1,'removing',now(),now())`, host, ref); err != nil {
		t.Fatal(err)
	}
	type result struct {
		status int
		body   map[string]any
	}
	answer := make(chan result, 1)
	go func() {
		resp, body := patch(t, srv.URL+"/v1/apps/"+app, map[string]any{
			"runtime_spec": map[string]any{"image": ref},
		}, admin)
		answer <- result{resp.StatusCode, body}
	}()
	select {
	case got := <-answer:
		t.Fatalf("app writer escaped uncommitted cleanup fence: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-answer:
		if got.status != http.StatusOK {
			t.Fatalf("app patch after cleanup commit = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("app PATCH stayed blocked")
	}
	var generation int64
	var state string
	if err := pool.QueryRow(ctx, `SELECT generation,state FROM host_image_operation_fences
		WHERE host_id=$1::uuid AND image_id='retired'`, host).Scan(&generation, &state); err != nil {
		t.Fatal(err)
	}
	if generation != 2 || state != "removing" {
		t.Fatalf("writer failed to advance pending requirement fence: generation=%d state=%s", generation, state)
	}
	if err := pool.QueryRow(ctx, `SELECT generation FROM host_image_operation_fences
		WHERE host_id=$1::uuid AND image_id='unrelated'`, host).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != 5 {
		t.Fatalf("unrelated cleanup preview generation changed to %d", generation)
	}
}

// Two app swaps both touch the same two refs. The writer acquires the ref
// advisory locks and the union of fence rows in a stable order, so concurrent
// swaps cannot invert one another's lock order.
func TestRH05ConcurrentImageReferenceSwapsDoNotDeadlock(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	admin := adminBearer(t, ctx, pool, authSvc, "cleanup-swap@test.local", "cleanup-swap")
	refs := []string{"registry.example.test/a@sha256:111", "registry.example.test/b@sha256:222"}
	var host string
	if err := pool.QueryRow(ctx, `INSERT INTO hosts(node_name,status,capacity_detection)
		VALUES('cleanup-swap-host','online','ok') RETURNING id::text`).Scan(&host); err != nil {
		t.Fatal(err)
	}
	apps := make([]string, 2)
	for i, ref := range refs {
		id := "image-a"
		if i == 1 {
			id = "image-b"
		}
		if _, err := pool.Exec(ctx, `INSERT INTO image_catalog(id,manifest_version,display_name,kind,version,registry_ref,raw)
			VALUES($1,1,$1,'prebuilt','v1',$2,'{}'::jsonb)`, id, ref); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO installed_images(image_id,version,registry_ref,lazy)
			VALUES($1,'v1',$2,false)`, id, ref); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `INSERT INTO apps(name,runtime_spec)
			VALUES($1,jsonb_build_object('image',$2::text)) RETURNING id::text`, id, ref).Scan(&apps[i]); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO host_image_operation_fences(host_id,image_id,state)
			VALUES($1::uuid,$2,'idle')`, host, id); err != nil {
			t.Fatal(err)
		}
	}
	answer := make(chan int, 2)
	for i := range apps {
		go func(i int) {
			resp, _ := patch(t, srv.URL+"/v1/apps/"+apps[i], map[string]any{
				"runtime_spec": map[string]any{"image": refs[1-i]},
			}, admin)
			answer <- resp.StatusCode
		}(i)
	}
	for range 2 {
		select {
		case code := <-answer:
			if code != http.StatusOK {
				t.Fatalf("concurrent swap returned %d", code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent reference swaps deadlocked")
		}
	}
	var changed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM host_image_operation_fences
		WHERE host_id=$1::uuid AND image_id IN ('image-a','image-b') AND generation=2`, host).Scan(&changed); err != nil {
		t.Fatal(err)
	}
	if changed != 2 {
		t.Fatalf("only %d/2 matching cleanup generations advanced twice", changed)
	}
}
