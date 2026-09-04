package auth

// #92: deleting a user tombstones its homes AND nudges every host that held one,
// so the backing store goes on the next agent poll instead of aging out of a
// grace window nobody is waiting on. TEST_DATABASE_URL-gated (see setup_test.go).
//
// That the nudge leaves a run the agent can claim NOW is asserted in
// cmd/quasar-control (TestReapNudgeLeavesAClaimableHomeGCRun): internal/jobs
// imports this package, so a test here cannot drive the real dispatcher.

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeReaper records the hosts it was nudged with.
type fakeReaper struct{ hosts [][]string }

func (f *fakeReaper) ReapHomesOn(_ context.Context, hostIDs []string) {
	f.hosts = append(f.hosts, append([]string(nil), hostIDs...))
}

func seedAppRow(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height,
		 default_fps, default_bitrate_kbps, runtime_spec)
		VALUES ($1, 512, 1, 1280, 720, 30, 2000, '{}') RETURNING id::text`, name).Scan(&id)
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}
	return id
}

func seedHostRow(t *testing.T, pool *pgxpool.Pool, nodeName string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO hosts (node_name, status) VALUES ($1, 'online') RETURNING id::text`,
		nodeName).Scan(&id)
	if err != nil {
		t.Fatalf("seed host: %v", err)
	}
	return id
}

func seedHomeRow(t *testing.T, pool *pgxpool.Pool, userID, appID, hostID string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO user_homes (user_id, app_id, host_id, provider, ref)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'local', '/homes/x')
		RETURNING id::text`, userID, appID, hostID).Scan(&id)
	if err != nil {
		t.Fatalf("seed home: %v", err)
	}
	return id
}

func TestDeleteUserNudgesEveryHostHoldingAHome(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	u, err := svc.Register(ctx, "reap1@test.invalid", "reap1", "quasar reap test passphrase")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	appA := seedAppRow(t, pool, t.Name()+"-app-a")
	appB := seedAppRow(t, pool, t.Name()+"-app-b")
	hostA := seedHostRow(t, pool, t.Name()+"-a")
	hostB := seedHostRow(t, pool, t.Name()+"-b")
	// Two homes on host A and one on host B: the nudge is per host, not per home.
	seedHomeRow(t, pool, u.ID, appA, hostA)
	seedHomeRow(t, pool, u.ID, appB, hostA)
	homeB := seedHomeRow(t, pool, u.ID, appA, hostB)

	fake := &fakeReaper{}
	svc.SetHomeReaper(fake)
	if err := svc.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	if len(fake.hosts) != 1 {
		t.Fatalf("reaper called %d times, want 1", len(fake.hosts))
	}
	got := append([]string(nil), fake.hosts[0]...)
	sort.Strings(got)
	want := []string{hostA, hostB}
	sort.Strings(want)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("nudged hosts = %v, want %v", got, want)
	}

	// And the rows the nudge refers to are tombstoned + orphaned, which is what
	// makes storage.gcReapable hand them to the agent with no grace.
	var gcAfter *time.Time
	var userID *string
	if err := pool.QueryRow(ctx,
		`SELECT gc_after, user_id::text FROM user_homes WHERE id::text = $1`, homeB).
		Scan(&gcAfter, &userID); err != nil {
		t.Fatalf("read home: %v", err)
	}
	if gcAfter == nil {
		t.Error("home was not tombstoned")
	}
	if userID != nil {
		t.Errorf("home user_id = %v, want NULL (orphaned by the FK)", *userID)
	}
}

// A user with no homes produces no nudge at all — an empty batch must not
// enqueue a job run on nothing.
func TestDeleteUserWithNoHomesDoesNotNudge(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	u, err := svc.Register(ctx, "reap2@test.invalid", "reap2", "quasar reap test passphrase")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	fake := &fakeReaper{}
	svc.SetHomeReaper(fake)
	if err := svc.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if len(fake.hosts) != 0 {
		t.Fatalf("reaper called with %v, want no call", fake.hosts)
	}
}

// The dev-agent identity reaper is the path that filled a host (#92): it must
// nudge once for the whole batch, covering every host any reaped identity held.
func TestReapEphemeralNudgesOncePerBatch(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	app := seedAppRow(t, pool, t.Name()+"-app")
	hostA := seedHostRow(t, pool, t.Name()+"-a")
	hostB := seedHostRow(t, pool, t.Name()+"-b")

	for _, host := range []string{hostA, hostB} {
		tok, err := svc.MintEphemeral(ctx, RoleUser, time.Minute)
		if err != nil {
			t.Fatalf("mint ephemeral: %v", err)
		}
		seedHomeRow(t, pool, tok.User.ID, app, host)
		if _, err := pool.Exec(ctx,
			`UPDATE users SET ephemeral_expires_at = now() - interval '1 minute' WHERE id::text = $1`,
			tok.User.ID); err != nil {
			t.Fatalf("expire identity: %v", err)
		}
	}

	fake := &fakeReaper{}
	svc.SetHomeReaper(fake)
	rep, err := svc.ReapEphemeral(ctx)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if rep.Deleted != 2 {
		t.Fatalf("deleted = %d, want 2", rep.Deleted)
	}
	if rep.HostsNudged != 2 {
		t.Errorf("HostsNudged = %d, want 2", rep.HostsNudged)
	}
	if len(fake.hosts) != 1 || len(fake.hosts[0]) != 2 {
		t.Fatalf("reaper calls = %v, want one call carrying both hosts", fake.hosts)
	}
}
