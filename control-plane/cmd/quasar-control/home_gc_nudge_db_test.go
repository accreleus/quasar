// home_gc_nudge_db_test.go — #92 end to end through the real wiring: the
// dev-agent identity reaper deletes an expired identity, homeReaper enqueues
// `home.gc` on the host that held its homes, and the run the agent's next poll
// claims is that one. The live failure this pins was the last link: the nudge
// coalesced onto a schedule-created run dated two hours out and inherited its
// wait. TEST_DATABASE_URL-gated; `make test-db` runs it.
package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/jobs"
)

func seedHomeFixtures(t *testing.T, pool *pgxpool.Pool, userID string) (hostID string) {
	t.Helper()
	ctx := context.Background()
	var appID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height,
		 default_fps, default_bitrate_kbps, runtime_spec)
		VALUES ($1, 512, 1, 1280, 720, 30, 2000, '{}') RETURNING id::text`,
		t.Name()+"-app").Scan(&appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO hosts (node_name, status) VALUES ($1, 'online') RETURNING id::text`,
		t.Name()+"-host").Scan(&hostID); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_homes (user_id, app_id, host_id, provider, ref)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'local', '/homes/x')`,
		userID, appID, hostID); err != nil {
		t.Fatalf("seed home: %v", err)
	}
	return hostID
}

func TestReapNudgeLeavesAClaimableHomeGCRun(t *testing.T) {
	// The tick is off only so the dispatcher cannot materialize a run of its own
	// mid-test; nothing on the nudge path (Enqueue, the claim) reads the switch.
	t.Setenv("QUASAR_JOBS", "0")
	pool := jobsTestDB(t)
	cfg := jobsTestConfig(t)
	ctx := context.Background()

	svc, err := NewServices(cfg, pool, discardSlogLogger(), nil)
	if err != nil {
		t.Fatalf("NewServices: %v", err)
	}
	defer svc.Stop()

	tok, err := svc.authSvc.MintEphemeral(ctx, auth.RoleUser, time.Minute)
	if err != nil {
		t.Fatalf("mint ephemeral: %v", err)
	}
	hostID := seedHomeFixtures(t, pool, tok.User.ID)

	// The schedule's own pending run, two hours out — the state the host was in.
	store := jobs.NewStore(pool)
	queued, m, err := store.Materialize(ctx, jobs.MaterializeParams{
		JobID: homeGCJobID, HostID: hostID, Trigger: jobs.TriggerSchedule,
		ScheduledFor: time.Now().Add(2 * time.Hour),
	})
	if err != nil || m != jobs.RunCreated {
		t.Fatalf("seed the scheduled run: m=%v err=%v", m, err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE users SET ephemeral_expires_at = now() - interval '1 minute' WHERE id::text = $1`,
		tok.User.ID); err != nil {
		t.Fatalf("expire identity: %v", err)
	}
	rep, err := svc.authSvc.ReapEphemeral(ctx)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if rep.Deleted != 1 || rep.HostsNudged != 1 {
		t.Fatalf("reap report = %+v, want one identity deleted and one host nudged", rep)
	}

	claimed, err := store.ClaimDue(ctx, jobs.ClaimOptions{
		Plane: jobs.PlaneAgent, HostID: hostID, Now: time.Now(), Limit: 5,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != queued.ID {
		t.Fatalf("the nudged home.gc run is not claimable on the next poll: %+v", claimed)
	}
}
