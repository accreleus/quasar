package images

import (
	"context"
	"strings"
	"testing"
)

// A historical permitted success cannot certify a later template generation.
// The newest claimed attempt is the only run whose outcome may prove the
// currently reported ready template's selected-requirement protection.
func TestSteamPublicationProtectionFollowsLatestClaimedAttempt(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	ref := "ghcr.io/accreleus/quasar-steam@sha256:" + strings.Repeat("a", 64)
	stmts := []string{
		`INSERT INTO instance_settings(id) VALUES(true)`,
		`INSERT INTO image_catalog(id,manifest_version,display_name,kind,version,registry_ref,runtime,library_provider,raw) VALUES('steam',1,'Steam','prebuilt','v1','unused','{"managed_home":true}','steam','{}')`,
		`INSERT INTO installed_images(image_id,version,registry_ref) VALUES('steam','v1','` + ref + `')`,
		`INSERT INTO hosts(id,node_name,status,source_policy_versions,source_preparation_connection_id,source_preparation) VALUES('34400000-0000-0000-0000-000000000001','permit-projection','online','{"steam_preparation":1,"template_publish_permit":1}','epoch',jsonb_build_object('steam',jsonb_build_object('policy_revision',(SELECT steam_preparation_revision::text FROM instance_settings WHERE id=true),'images',jsonb_build_array(jsonb_build_object('image_id','steam','registry_ref','` + ref + `'::text,'version','v1','preparation_enabled',true,'consumption_enabled',true,'state','ready','reason','none','template',jsonb_build_object('registry_ref','` + ref + `'::text,'version','v1'))))))`,
		`INSERT INTO host_images(host_id,image_id,version,state) VALUES('34400000-0000-0000-0000-000000000001','steam','v1','ready')`,
		`INSERT INTO jobs(id,name,plane,scope,schedule_kind) VALUES('template.warmup','warmup','agent','host','manual') ON CONFLICT (id) DO NOTHING`,
		`INSERT INTO job_runs(job_id,host_id,state,trigger,scheduled_for,claimed_at,finished_at,params,template_publish_claim_token,template_publish_connection_id,publish_permit_accepted_at)
		 VALUES('template.warmup','34400000-0000-0000-0000-000000000001','succeeded','manual',now()-interval '2 hours',now()-interval '2 hours',now()-interval '2 hours',
		 jsonb_build_object('image_id','steam','registry_ref','` + ref + `'::text,'version','v1','policy_revision',(SELECT steam_preparation_revision::text FROM instance_settings WHERE id=true)),
		 gen_random_uuid(),'epoch',now()-interval '2 hours')`,
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	s := NewStore(pool)
	check := func(want string) {
		t.Helper()
		states, err := s.hostStates(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(states["steam"]) != 1 || states["steam"][0].SteamPreparation == nil {
			t.Fatalf("missing Steam projection: %+v", states["steam"])
		}
		got := states["steam"][0].SteamPreparation.PublicationProtection
		if got != want {
			t.Fatalf("publication protection=%q want=%q", got, want)
		}
	}
	check("verified")
	var later string
	err := pool.QueryRow(ctx, `INSERT INTO job_runs(job_id,host_id,state,trigger,scheduled_for,claimed_at,params)
	 VALUES('template.warmup','34400000-0000-0000-0000-000000000001','running','manual',now()-interval '1 hour',now()-interval '1 hour',
	 jsonb_build_object('image_id','steam','registry_ref',$1::text,'version','v1','policy_revision',(SELECT steam_preparation_revision::text FROM instance_settings WHERE id=true))) RETURNING id::text`, ref).Scan(&later)
	if err != nil {
		t.Fatal(err)
	}
	check("limited_protection")
	if _, err := pool.Exec(ctx, `UPDATE job_runs SET state='succeeded',finished_at=now() WHERE id=$1::uuid`, later); err != nil {
		t.Fatal(err)
	}
	check("limited_protection")
}
