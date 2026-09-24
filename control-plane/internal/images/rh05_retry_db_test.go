package images

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// Operator Retry is scoped to one selected host/image and repeated requests
// share one pending dispatch. The HTTP path carries the real admin gate.
func TestRH05OperatorRetryCoalescesAndRequiresCurrentFailure(t *testing.T) {
	env, hosts := newActionsEnv(t, "retry-selected", "retry-other")
	ctx := context.Background()
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	host := hosts[0]
	if _, err := env.pool.Exec(ctx, `UPDATE app_placement SET mode='fixed'`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `INSERT INTO app_placement_hosts(app_id,host_id)
		SELECT id,$1::uuid FROM apps`, host); err != nil {
		t.Fatal(err)
	}
	env.ens.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "failed", Error: "test failure"})
	route := "/v1/admin/hosts/" + host + "/images/" + imgID + "/retry"
	if code, body := env.do(t, http.MethodPost, route, ""); code != http.StatusAccepted || len(strings.TrimSpace(string(body))) != 0 {
		t.Fatalf("retry: %d %s", code, body)
	}
	if code, _ := env.do(t, http.MethodPost, route, ""); code != http.StatusAccepted {
		t.Fatalf("duplicate retry: %d", code)
	}
	if target, details := imageAuditRow(t, env.pool, "image.retry"); target != imgID || details["host_id"] != host || len(details) != 1 {
		t.Fatalf("retry audit leaked or lost identity: target=%q details=%v", target, details)
	}
	env.ens.Wait()
	if got := env.fleet.waitEnsure(t); got.HostID != host || got.ImageID != imgID {
		t.Fatalf("retry dispatched %+v", got)
	}
	// The agent accepted the command, but has not reported image_state yet.
	// A later operator request must still join this same physical attempt.
	if code, _ := env.do(t, http.MethodPost, route, ""); code != http.StatusAccepted {
		t.Fatalf("retry before image_state: %d", code)
	}
	env.ens.Wait()
	env.fleet.noMoreEnsures(t, 30*time.Millisecond)
	if code, body := env.do(t, http.MethodPost, "/v1/admin/hosts/"+hosts[1]+"/images/"+imgID+"/retry", ""); code != http.StatusConflict || !strings.Contains(string(body), "Image is not required on this host") {
		t.Fatalf("unselected retry: %d %s", code, body)
	}
	env.ens.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "ready"})
	if code, body := env.do(t, http.MethodPost, route, ""); code != http.StatusConflict || !strings.Contains(string(body), "Image is not failed for the adopted version") {
		t.Fatalf("ready retry: %d %s", code, body)
	}
	// Older agents report an empty version. That legacy failure still belongs
	// to the current adoption and only explicit Retry may re-arm it.
	env.ens.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, State: "failed", Error: "legacy failure"})
	if err := env.ens.EnsureAll(ctx); err != nil {
		t.Fatal(err)
	}
	env.ens.Wait()
	env.fleet.noMoreEnsures(t, 0)
	if code, body := env.do(t, http.MethodPost, route, ""); code != http.StatusAccepted {
		t.Fatalf("legacy failed retry: %d %s", code, body)
	}
	env.ens.Wait()
	if got := env.fleet.waitEnsure(t); got.Version != imgVer {
		t.Fatalf("legacy retry version: %+v", got)
	}
}

func TestRH05OperatorRetrySupersedesScheduledAutomaticBackoff(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	host := seedHost(t, pool, "retry-backoff-host")
	fleet := newFleet(host)
	e := NewEnsurer(pool, fleet, testLog(), WithRetry(1, 75*time.Millisecond))
	defer e.Close()
	ctx := context.Background()
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "failed", Error: "transient"})
	if err := e.RetryHostImage(ctx, host, imgID); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	fleet.waitEnsure(t)
	fleet.noMoreEnsures(t, 25*time.Millisecond)
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "failed", Error: "still failing"})
	e.Wait()
	if got := fleet.waitEnsure(t); got.HostID != host || got.ImageID != imgID {
		t.Fatalf("fresh bounded retry budget: %+v", got)
	}
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "failed", Error: "budget exhausted"})
	e.Wait()
	fleet.noMoreEnsures(t, 0)
}

func TestRH05BackoffOnUnsupportedHostLeavesOperatorRetryAvailable(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	host := seedHost(t, pool, "retry-unsupported-host")
	fleet := newFleet(host)
	e := NewEnsurer(pool, fleet, testLog(), WithRetry(1, time.Millisecond))
	defer e.Close()
	ctx := context.Background()
	e.markUnsupported(host)
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "failed", Error: "previous failure"})
	e.Wait()
	fleet.noMoreEnsures(t, 0)
	if err := e.RetryHostImage(ctx, host, imgID); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if got := fleet.waitEnsure(t); got.HostID != host || got.ImageID != imgID {
		t.Fatalf("operator retry after unsupported timeout: %+v", got)
	}
}

func TestRH05RetryCanBeRescheduledAfterAgentRejectsAcceptance(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	host := seedHost(t, pool, "retry-rejected-host")
	fleet := newFleet(host)
	fleet.reject = "rejected"
	e := NewEnsurer(pool, fleet, testLog(), WithRetry(0, time.Millisecond))
	defer e.Close()
	ctx := context.Background()
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "failed", Error: "initial failure"})
	for i := 0; i < 2; i++ {
		if err := e.RetryHostImage(ctx, host, imgID); err != nil {
			t.Fatal(err)
		}
		e.Wait()
		fleet.waitEnsure(t)
	}
}
