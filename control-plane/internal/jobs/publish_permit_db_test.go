package jobs

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/preparation"
)

// The permit is the externally visible final authority for a claimed run.
// Placement changes after dispatch must be observed by its database snapshot.
func TestSteamPublishPermitRechecksSelectionAndClaim(t *testing.T) {
	f := newAgentFixture(t, hostDef("template.warmup"))
	ctx := preparation.ConnectionContext(context.Background())
	ref := "ghcr.io/accreleus/quasar-steam@sha256:" + strings.Repeat("a", 64)
	for _, stmt := range []string{
		`INSERT INTO instance_settings(id) VALUES(true) ON CONFLICT (id) DO NOTHING`,
		`INSERT INTO image_catalog(id,manifest_version,display_name,kind,version,registry_ref,runtime,library_provider,raw) VALUES('steam',1,'Steam','prebuilt','v1','unused','{"managed_home":true}','steam','{}') ON CONFLICT (id) DO UPDATE SET kind='prebuilt',version='v1',runtime='{"managed_home":true}',library_provider='steam'`,
		`INSERT INTO installed_images(image_id,version,registry_ref) VALUES('steam','v1','` + ref + `') ON CONFLICT (image_id) DO UPDATE SET version='v1',registry_ref=EXCLUDED.registry_ref`,
		`UPDATE instance_settings SET steam_preparation_enabled=true,steam_preparation_revision=steam_preparation_revision+1,steam_preparation_image=jsonb_build_object('image_id','steam','registry_ref','` + ref + `'::text,'version','v1') WHERE id=true`,
		`UPDATE apps SET enabled=false WHERE runtime_spec->>'image'='` + ref + `'`,
		`INSERT INTO apps(name,runtime_spec) VALUES('permit selected Steam',jsonb_build_object('image','` + ref + `'::text))`,
	} {
		if _, err := f.store.pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE hosts SET status='online',agent_disconnected_at=NULL WHERE id=$1::uuid`, f.host); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO host_images(host_id,image_id,version,state) VALUES($1::uuid,'steam','v1','ready')`, f.host); err != nil {
		t.Fatal(err)
	}
	prep := preparation.New(f.store.pool)
	if err := prep.Register(ctx, f.host, map[string]int{"steam_preparation": 1, "template_publish_permit": 1}); err != nil {
		t.Fatal(err)
	}
	p, err := prep.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := prep.Report(ctx, f.host, &preparation.Reports{Steam: preparation.Report{PolicyRevision: p.Revision, Images: []preparation.ImageReport{{Image: p.Images[0], PreparationEnabled: true, ConsumptionEnabled: true, State: "waiting_image", Reason: "image_not_ready"}}}}); err != nil {
		t.Fatal(err)
	}
	params, err := prep.Params(ctx, f.host)
	if err != nil {
		t.Fatal(err)
	}
	f.materializeFor(t, "template.warmup", f.host, params)
	claimed := decodePending(t, agentReq(t, "GET", f.srv.URL+"/v1/agent/jobs/pending", "host-a", "secret-a", nil))
	if len(claimed.Runs) != 1 || claimed.Runs[0].PublishClaimToken == nil {
		t.Fatalf("claim missing current token: %+v", claimed)
	}
	run := claimed.Runs[0]
	permit := map[string]any{"publish_claim_token": *run.PublishClaimToken, "image_id": "steam", "registry_ref": ref, "version": "v1", "policy_revision": p.Revision}
	url := f.srv.URL + "/v1/agent/jobs/template.warmup/" + run.RunID + "/publish-permit"
	if resp := agentReq(t, "POST", url, "host-a", "secret-a", permit); resp.StatusCode != http.StatusOK {
		t.Fatalf("current permit: %d", resp.StatusCode)
	}
	if resp := agentReq(t, "POST", url, "host-b", "secret-b", permit); resp.StatusCode != http.StatusConflict {
		t.Fatalf("other host learned permit state: %d", resp.StatusCode)
	}
	staleIdentity := map[string]any{"publish_claim_token": *run.PublishClaimToken, "image_id": "steam", "registry_ref": "ghcr.io/accreleus/quasar-steam@sha256:" + strings.Repeat("b", 64), "version": "v1", "policy_revision": p.Revision}
	if resp := agentReq(t, "POST", url, "host-a", "secret-a", staleIdentity); resp.StatusCode != http.StatusConflict {
		t.Fatalf("different digest received permit: %d", resp.StatusCode)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE app_placement SET mode='fixed' WHERE app_id=(SELECT id FROM apps WHERE name='permit selected Steam')`); err != nil {
		t.Fatal(err)
	}
	if resp := agentReq(t, "POST", url, "host-a", "secret-a", permit); resp.StatusCode != http.StatusConflict {
		t.Fatalf("removed placement permit: %d", resp.StatusCode)
	}
	if resp := agentReq(t, "POST", f.srv.URL+"/v1/agent/jobs/report", "host-a", "secret-a", map[string]any{"run_id": run.RunID, "state": "succeeded", "publish_claim_token": "11111111-1111-1111-1111-111111111111"}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("stale report: %d", resp.StatusCode)
	}
	if stored, err := f.store.GetRun(ctx, run.RunID); err != nil || stored.State != StateRunning {
		t.Fatalf("stale report changed run: state=%s err=%v", stored.State, err)
	}
	if resp := agentReq(t, "POST", f.srv.URL+"/v1/agent/jobs/report", "host-a", "secret-a", map[string]any{"run_id": run.RunID, "state": "succeeded", "publish_claim_token": *run.PublishClaimToken}); resp.StatusCode != http.StatusOK {
		t.Fatalf("current report: %d", resp.StatusCode)
	}
	if stored, err := f.store.GetRun(ctx, run.RunID); err != nil || stored.State != StateSucceeded || stored.PublishPermitAcceptedAt == nil {
		t.Fatalf("successful claim lacks durable permit evidence: state=%s accepted=%v err=%v", stored.State, stored.PublishPermitAcceptedAt, err)
	}
	if resp := agentReq(t, "POST", f.srv.URL+"/v1/agent/jobs/report", "host-a", "secret-a", map[string]any{"run_id": run.RunID, "state": "succeeded", "publish_claim_token": "11111111-1111-1111-1111-111111111111"}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("terminal wrong-claim retry: %d", resp.StatusCode)
	}
	if resp := agentReq(t, "POST", f.srv.URL+"/v1/agent/jobs/report", "host-a", "secret-a", map[string]any{"run_id": run.RunID, "state": "succeeded", "publish_claim_token": *run.PublishClaimToken}); resp.StatusCode != http.StatusOK {
		t.Fatalf("terminal same-claim retry: %d", resp.StatusCode)
	}
	// A reconnect invalidates even a run that was claimed while selected. Its
	// old worker cannot publish against a newer authenticated policy epoch.
	if _, err := f.store.pool.Exec(ctx, `UPDATE app_placement SET mode='all_eligible' WHERE app_id=(SELECT id FROM apps WHERE name='permit selected Steam')`); err != nil {
		t.Fatal(err)
	}
	f.materializeFor(t, "template.warmup", f.host, params)
	second := decodePending(t, agentReq(t, "GET", f.srv.URL+"/v1/agent/jobs/pending", "host-a", "secret-a", nil))
	if len(second.Runs) != 1 || second.Runs[0].PublishClaimToken == nil {
		t.Fatalf("second claim missing token: %+v", second)
	}
	reconnected := preparation.ConnectionContext(context.Background())
	if err := prep.Register(reconnected, f.host, map[string]int{"steam_preparation": 1, "template_publish_permit": 1}); err != nil {
		t.Fatal(err)
	}
	if err := prep.Report(reconnected, f.host, &preparation.Reports{Steam: preparation.Report{PolicyRevision: p.Revision, Images: []preparation.ImageReport{{Image: p.Images[0], PreparationEnabled: true, ConsumptionEnabled: true, State: "waiting_image", Reason: "image_not_ready"}}}}); err != nil {
		t.Fatal(err)
	}
	permit["publish_claim_token"] = *second.Runs[0].PublishClaimToken
	url = f.srv.URL + "/v1/agent/jobs/template.warmup/" + second.Runs[0].RunID + "/publish-permit"
	if resp := agentReq(t, "POST", url, "host-a", "secret-a", permit); resp.StatusCode != http.StatusConflict {
		t.Fatalf("reconnected worker reused old claim: %d", resp.StatusCode)
	}
}
