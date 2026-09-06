package preparation

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func dbTest(t *testing.T) (*Store, string, Policy) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	if err := migrate.Run(migrations.FS, url); err != nil {
		t.Fatal(err)
	}
	ctx := ConnectionContext(context.Background())
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`TRUNCATE hosts,image_catalog,instance_settings CASCADE`)
	exec(`INSERT INTO instance_settings(id) VALUES(true)`)
	exec(`INSERT INTO image_catalog(id,manifest_version,display_name,kind,version,registry_ref,runtime,library_provider,raw) VALUES('steam',1,'Steam','prebuilt','v1','unused','{"managed_home":true}','steam','{}')`)
	ref := "ghcr.io/accreleus/quasar-steam@sha256:" + strings.Repeat("a", 64)
	exec(`INSERT INTO installed_images(image_id,version,registry_ref) VALUES('steam','v1',$1)`, ref)
	host := "14500000-0000-0000-0000-000000000001"
	exec(`INSERT INTO hosts(id,node_name,node_secret_hash,status) VALUES($1,'prep-host','hash','online')`, host)
	exec(`INSERT INTO host_images(host_id,image_id,version,state) VALUES($1,'steam','v1','ready')`, host)
	store := New(pool)
	p, err := store.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return store, host, p
}
func TestAdoptionRevisionIsFrozenUntilActualSteamChange(t *testing.T) {
	s, _, p := dbTest(t)
	ctx := ConnectionContext(context.Background())
	if p.Revision != "2" || len(p.Images) != 1 {
		t.Fatalf("adoption: %+v", p)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE image_catalog SET library_provider=NULL,runtime='{}',version='future' WHERE id='steam'`)
	exec(`INSERT INTO image_catalog(id,manifest_version,display_name,kind,version,raw) VALUES('other',1,'Other','prebuilt','v1','{}')`)
	exec(`INSERT INTO installed_images(image_id,version,registry_ref) VALUES('other','v1','other')`)
	exec(`UPDATE installed_images SET version=version,registry_ref=registry_ref WHERE image_id='steam'`)
	after, err := s.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != p.Revision || len(after.Images) != 1 || after.Images[0] != p.Images[0] {
		t.Fatalf("catalog/unrelated/no-op changed adoption: %+v", after)
	}
	exec(`UPDATE image_catalog SET library_provider='steam',runtime='{"managed_home":true}' WHERE id='steam'`)
	exec(`UPDATE installed_images SET version='v2',registry_ref=$1 WHERE image_id='steam'`, "ghcr.io/accreleus/quasar-steam@sha256:"+strings.Repeat("b", 64))
	after, err = s.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != "3" || after.Images[0].Version != "v2" {
		t.Fatalf("new adoption not revised: %+v", after)
	}
	exec(`DELETE FROM installed_images WHERE image_id='steam'`)
	after, err = s.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != "4" || len(after.Images) != 0 {
		t.Fatalf("removal not revised: %+v", after)
	}
}
func TestRegistrationAndCurrentAcknowledgementGateJobs(t *testing.T) {
	s, host, p := dbTest(t)
	ctx := ConnectionContext(context.Background())
	if _, err := s.Params(ctx, host); err == nil {
		t.Fatal("legacy host admitted")
	}
	if err := s.Register(ctx, host, map[string]int{"steam_preparation": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Params(ctx, host); err == nil {
		t.Fatal("unacknowledged host admitted")
	}
	r := &Reports{Steam: Report{PolicyRevision: p.Revision, Images: []ImageReport{{Image: p.Images[0], PreparationEnabled: true, ConsumptionEnabled: true, State: "waiting_image", Reason: "image_not_ready"}}}}
	if err := s.Report(ctx, host, r); err != nil {
		t.Fatal(err)
	}
	params, err := s.Params(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(params)
	if err = s.AllowJob(ctx, host, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE instance_settings SET steam_preparation_enabled=false,steam_preparation_revision=steam_preparation_revision+1`); err != nil {
		t.Fatal(err)
	}
	if err = s.AllowJob(ctx, host, raw); err == nil {
		t.Fatal("already queued job admitted after disable")
	}
	if err = s.Register(ctx, host, nil); err != nil {
		t.Fatal(err)
	}
	var report []byte
	var versions []byte
	if err = s.pool.QueryRow(ctx, `SELECT source_preparation,source_policy_versions FROM hosts WHERE id=$1`, host).Scan(&report, &versions); err != nil {
		t.Fatal(err)
	}
	if report != nil || versions != nil {
		t.Fatal("legacy registration retained effective permission")
	}
}
func TestMalformedOrForeignReportCannotAuthorize(t *testing.T) {
	s, host, p := dbTest(t)
	ctx := ConnectionContext(context.Background())
	if err := s.Register(ctx, host, map[string]int{"steam_preparation": 1}); err != nil {
		t.Fatal(err)
	}
	r := &Reports{Steam: Report{PolicyRevision: p.Revision, Images: []ImageReport{{Image: p.Images[0], PreparationEnabled: true, ConsumptionEnabled: true, State: "ready", Reason: "none"}}}}
	r.Steam.Images[0].Version = "foreign"
	if s.Report(ctx, host, r) == nil {
		t.Fatal("accepted foreign version")
	}
	if _, err := s.Params(ctx, host); err == nil {
		t.Fatal("rejected report authorized job")
	}
	r.Steam.Images = []ImageReport{}
	if err := s.Report(ctx, host, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Params(ctx, host); err == nil {
		t.Fatal("empty acknowledgement authorized preparation")
	}
}

func TestOldConnectionCannotAcknowledgeNewEpoch(t *testing.T) {
	s, host, p := dbTest(t)
	old := ConnectionContext(context.Background())
	fresh := ConnectionContext(context.Background())
	r := &Reports{Steam: Report{PolicyRevision: p.Revision, Images: []ImageReport{{Image: p.Images[0], PreparationEnabled: true, ConsumptionEnabled: true, State: "deferred", Reason: "none"}}}}
	if err := s.Register(old, host, map[string]int{"steam_preparation": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Report(old, host, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(fresh, host, map[string]int{"steam_preparation": 1}); err != nil {
		t.Fatal(err)
	}
	if s.Report(old, host, r) == nil {
		t.Fatal("displaced websocket acknowledged current connection")
	}
	if _, err := s.Params(fresh, host); err == nil {
		t.Fatal("displaced websocket authorized work")
	}
	if err := s.Report(fresh, host, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Params(fresh, host); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatedDeferredAcknowledgementDoesNotRetriggerEvent(t *testing.T) {
	s, host, p := dbTest(t)
	ctx := ConnectionContext(context.Background())
	if err := s.Register(ctx, host, map[string]int{"steam_preparation": 1}); err != nil {
		t.Fatal(err)
	}
	report := &Reports{Steam: Report{PolicyRevision: p.Revision, Images: []ImageReport{{Image: p.Images[0], PreparationEnabled: true, ConsumptionEnabled: true, State: "deferred", Reason: "host_busy"}}}}
	changed, err := s.AcceptReport(ctx, host, report)
	if err != nil || !changed {
		t.Fatalf("first ack: %v %v", changed, err)
	}
	report.Steam.Images[0].Detail = "Still waiting for a user session"
	changed, err = s.AcceptReport(ctx, host, report)
	if err != nil || changed {
		t.Fatalf("deferred report retriggered event and bypassed job backoff: %v %v", changed, err)
	}
	report.Steam.Images[0].PreparationEnabled = false
	if _, err = s.AcceptReport(ctx, host, report); err != nil {
		t.Fatal(err)
	}
	report.Steam.Images[0].PreparationEnabled = true
	changed, err = s.AcceptReport(ctx, host, report)
	if err != nil || !changed {
		t.Fatalf("restored permission failed to reconcile: %v %v", changed, err)
	}
}

func TestStaleClosedJobReconcilesAfterClosingStaleRetry(t *testing.T) {
	s, host, p := dbTest(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `INSERT INTO jobs(id,name,plane,scope,schedule_kind) VALUES('template.warmup','Steam','agent','host','event') ON CONFLICT(id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	stale := map[string]any{"image_id": p.Images[0].ImageID, "registry_ref": p.Images[0].RegistryRef, "version": p.Images[0].Version, "policy_revision": "1"}
	raw, _ := json.Marshal(stale)
	var id string
	if err := s.pool.QueryRow(ctx, `INSERT INTO job_runs(job_id,host_id,state,trigger,scheduled_for,params) VALUES('template.warmup',$1,'pending','event',now()+interval '10 minutes',$2) RETURNING id::text`, host, raw).Scan(&id); err != nil {
		t.Fatal(err)
	}
	changed, err := s.ReconcileClosedJob(ctx, host, raw)
	if err != nil || !changed {
		t.Fatalf("stale close: %v %v", changed, err)
	}
	var state string
	if err = s.pool.QueryRow(ctx, `SELECT state FROM job_runs WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "skipped" {
		t.Fatal("old retry remains open and absorbs new generation event")
	}
	stale["policy_revision"] = p.Revision
	raw, _ = json.Marshal(stale)
	if err = s.pool.QueryRow(ctx, `INSERT INTO job_runs(job_id,host_id,state,trigger,scheduled_for,params) VALUES('template.warmup',$1,'pending','event',now()+interval '10 minutes',$2) RETURNING id::text`, host, raw).Scan(&id); err != nil {
		t.Fatal(err)
	}
	changed, err = s.ReconcileClosedJob(ctx, host, raw)
	if err != nil || changed {
		t.Fatalf("same generation must retain backoff/exhaustion: %v %v", changed, err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT state FROM job_runs WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		t.Fatal("same-generation retry was discarded")
	}
}
