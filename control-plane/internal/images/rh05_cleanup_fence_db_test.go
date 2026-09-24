package images

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// Retry is an operator interface, and its delayed dispatcher must obey the
// durable cleanup fence even after the HTTP request has returned.
func TestRH05RemovingFenceRejectsOperatorRetryAndDelayedEnsure(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-fence-host")
	ctx := context.Background()
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	host := hosts[0]
	env.ens.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "failed"})
	if _, err := env.pool.Exec(ctx, `INSERT INTO host_image_operation_fences(host_id,image_id,state)
		VALUES($1::uuid,$2,'removing')`, host, imgID); err != nil {
		t.Fatal(err)
	}
	route := "/v1/admin/hosts/" + host + "/images/" + imgID + "/retry"
	if code, body := env.do(t, http.MethodPost, route, ""); code != http.StatusConflict ||
		!strings.Contains(string(body), "Image cleanup is in progress") {
		t.Fatalf("retry during cleanup: %d %s", code, body)
	}
	// Absent inventory would normally dispatch an ensure. The persisted fence
	// must suppress that delayed work independently of Retry.
	env.ens.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "absent"})
	if err := env.ens.EnsureHost(ctx, host); err != nil {
		t.Fatal(err)
	}
	env.ens.Wait()
	env.fleet.noMoreEnsures(t, 0)
}

func TestRH05RetryWaitsForConcurrentRemovalFence(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-race-host")
	ctx := context.Background()
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	host := hosts[0]
	env.ens.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "failed"})
	if _, err := env.pool.Exec(ctx, `INSERT INTO host_image_operation_fences(host_id,image_id,state)
		VALUES($1::uuid,$2,'idle')`, host, imgID); err != nil {
		t.Fatal(err)
	}
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE host_image_operation_fences SET state='removing'
		WHERE host_id=$1::uuid AND image_id=$2`, host, imgID); err != nil {
		t.Fatal(err)
	}
	type response struct {
		code int
		body []byte
	}
	result := make(chan response, 1)
	go func() {
		code, body := env.do(t, http.MethodPost,
			"/v1/admin/hosts/"+host+"/images/"+imgID+"/retry", "")
		result <- response{code, body}
	}()
	select {
	case got := <-result:
		t.Fatalf("retry escaped an uncommitted cleanup fence: %d %s", got.code, got.body)
	case <-time.After(50 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.code != http.StatusConflict || !strings.Contains(string(got.body), "Image cleanup is in progress") {
			t.Fatalf("retry after committed cleanup fence: %d %s", got.code, got.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not finish after cleanup fence committed")
	}
	env.fleet.noMoreEnsures(t, 0)
}

// A catalog sync can retire an image ID and later reintroduce it. Cleanup
// must still remember the prior verified version; otherwise re-adoption would
// silently make that recovery image eligible for deletion.
func TestRH05PriorSuccessSurvivesCatalogPruneAndReadd(t *testing.T) {
	pool := ensureDB(t)
	ctx := context.Background()
	seedCatalog(t, pool)
	install(t, pool, false)
	host := seedHost(t, pool, "history-prune-host")
	e := NewEnsurer(pool, nil, testLog())
	defer e.Close()
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "ready"})
	if _, err := pool.Exec(ctx, `UPDATE installed_images SET version=$2,registry_ref=$3
		WHERE image_id=$1`, imgID, imgVer2, imgDigest2); err != nil {
		t.Fatal(err)
	}
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer2, State: "ready"})
	if _, err := pool.Exec(ctx, `DELETE FROM image_catalog WHERE id=$1`, imgID); err != nil {
		t.Fatal(err)
	}
	seedCatalog(t, pool)
	var previous string
	if err := pool.QueryRow(ctx, `SELECT previous_version FROM host_image_success_history
		WHERE host_id=$1::uuid AND image_id=$2`, host, imgID).Scan(&previous); err != nil {
		t.Fatalf("retained history lost during catalog prune/re-add: %v", err)
	}
	if previous != imgVer {
		t.Fatalf("previous successful version = %q, want %q", previous, imgVer)
	}
}
