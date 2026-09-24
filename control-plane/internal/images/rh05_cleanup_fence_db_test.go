package images

import (
	"context"
	"net/http"
	"strings"
	"testing"

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
