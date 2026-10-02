package storage

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type databaseHomeRoot struct{ pool *pgxpool.Pool }

func (r databaseHomeRoot) HomeRoot(ctx context.Context, hostID string) (string, error) {
	var root string
	err := r.pool.QueryRow(ctx, `SELECT effective_settings->>'home_root' FROM hosts WHERE id=$1::uuid`, hostID).Scan(&root)
	return root, err
}

func (r databaseHomeRoot) HomeRootTx(ctx context.Context, tx pgx.Tx, hostID string) (string, error) {
	var root string
	err := tx.QueryRow(ctx, `SELECT effective_settings->>'home_root' FROM hosts WHERE id=$1::uuid`, hostID).Scan(&root)
	return root, err
}

// A first claim holds a pooled transaction. Resolving the root through that
// transaction must work even when the pool has just one connection; otherwise
// simultaneous launches can exhaust production's pool with lock waiters.
func TestEnsureHomeDoesNotNeedASecondPoolConnection(t *testing.T) {
	pool := testDB(t)
	userID := seedUser(t, pool, "one-connection@test.local")
	appID := seedApp(t, pool, "One Connection")
	hostID := seedHost(t, pool)
	if _, err := pool.Exec(context.Background(), `UPDATE hosts SET effective_settings='{"home_root":"/srv/homes"}'::jsonb WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	limited, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(limited.Close)
	mgr := New(limited, fixedProvider("local"), databaseHomeRoot{pool: limited})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	mount, err := mgr.EnsureHome(ctx, userID, appID, hostID, "/home/quasar")
	if err != nil || !strings.HasPrefix(mount, "/srv/homes/") {
		t.Fatalf("single-connection home claim: mount=%q err=%v", mount, err)
	}
}

// A home-root edit takes the host row lock. A launch must hold that same lock
// from root resolution through home creation, so the edit sees the new home
// instead of validating an empty host and leaving a sticky ref at the old root.
func TestEnsureHomeSerializesRootResolutionAndCreationWithHostEdit(t *testing.T) {
	pool := testDB(t)
	userID := seedUser(t, pool, "home-root-race@test.local")
	appID := seedApp(t, pool, "Home Root Race")
	hostID := seedHost(t, pool)
	entered := make(chan struct{})
	release := make(chan struct{})
	root := func() (string, error) {
		close(entered)
		<-release
		return "/srv/homes", nil
	}
	mgr := New(pool, fixedProvider("local"), HostRootResolverFuncs{
		Read:   func(context.Context, string) (string, error) { return root() },
		Locked: func(context.Context, pgx.Tx, string) (string, error) { return root() },
	})
	result := make(chan error, 1)
	go func() {
		_, err := mgr.EnsureHome(context.Background(), userID, appID, hostID, "/home/quasar")
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("home root resolution did not start")
	}
	// A policy writer needs this lock before it validates existing homes. It
	// must be unable to pass the launch while its root is already resolved.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := pool.Exec(ctx, `SELECT id FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID)
	if !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		t.Fatalf("host edit passed a root-resolving launch: %v", err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	var ref string
	if err := pool.QueryRow(context.Background(), `SELECT ref FROM user_homes WHERE host_id=$1::uuid`, hostID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ref, "/srv/homes/") {
		t.Fatalf("new home ref = %q", ref)
	}
}

// #418: a reconfigured QUASAR_HOME_ROOT leaves existing homes under the old
// root. Quasar does not move data: the next launch re-points the row to the same
// <user>/<app> path under the current root, for EnsureHome and for a derived
// tile's RequireHome alike. A ref whose place under the root cannot be derived,
// or that would land on another home's path, is a home conflict, never reuse.
func TestHomeOutsideAReconfiguredRootIsRepointedNeverReused(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	userID := seedUser(t, pool, "repoint@test.local")
	appID := seedApp(t, pool, "Repoint")
	hostID := seedHost(t, pool)
	setRoot := func(root string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE hosts SET effective_settings=jsonb_build_object('home_root',$2::text) WHERE id=$1::uuid`, hostID, root); err != nil {
			t.Fatal(err)
		}
	}
	readRef := func() (string, string) {
		t.Helper()
		var id, ref string
		must(t, pool.QueryRow(ctx, `SELECT id::text,ref FROM user_homes WHERE user_id=$1::uuid AND app_id=$2::uuid AND host_id=$3::uuid`, userID, appID, hostID).Scan(&id, &ref))
		return id, ref
	}
	mgr := New(pool, fixedProvider("local"), databaseHomeRoot{pool: pool})
	setRoot("/srv/homes")
	if _, err := mgr.EnsureHome(ctx, userID, appID, hostID, "/home/quasar"); err != nil {
		t.Fatal(err)
	}
	homeID, oldRef := readRef()
	relative := strings.TrimPrefix(oldRef, "/srv/homes/")

	setRoot("/data/homes")
	mount, err := mgr.EnsureHome(ctx, userID, appID, hostID, "/home/quasar")
	if err != nil || mount != "/data/homes/"+relative+":/home/quasar:rw" {
		t.Fatalf("launch after reconfigure: mount=%q err=%v", mount, err)
	}
	if id, ref := readRef(); id != homeID || ref != "/data/homes/"+relative {
		t.Fatalf("row after reconfigure: id=%s ref=%s", id, ref)
	}

	setRoot("/data/other")
	mount, err = mgr.RequireHome(ctx, userID, appID, hostID, "/home/quasar")
	if err != nil || mount != "/data/other/"+relative+":/home/quasar:rw" {
		t.Fatalf("derived launch after reconfigure: mount=%q err=%v", mount, err)
	}

	// Another home already holds the target path: refuse, keep the row.
	otherUser := seedUser(t, pool, "repoint-other@test.local")
	mustExec(t, pool, `INSERT INTO user_homes (user_id, app_id, host_id, provider, ref) VALUES ($1::uuid,$2::uuid,$3::uuid,'local',$4)`, otherUser, appID, hostID, "/data/next/"+relative)
	setRoot("/data/next")
	if _, err := mgr.EnsureHome(ctx, userID, appID, hostID, "/home/quasar"); !errors.Is(err, ErrHomeConflict) {
		t.Fatalf("colliding re-point: err=%v", err)
	}
	if _, ref := readRef(); ref != "/data/other/"+relative {
		t.Fatalf("refused re-point moved the row to %s", ref)
	}

	// No <user>/<app> path to carry over: refuse, keep the row.
	mustExec(t, pool, `UPDATE user_homes SET ref='/legacy' WHERE id=$1::uuid`, homeID)
	for _, launch := range []func() (string, error){
		func() (string, error) { return mgr.EnsureHome(ctx, userID, appID, hostID, "/home/quasar") },
		func() (string, error) { return mgr.RequireHome(ctx, userID, appID, hostID, "/home/quasar") },
	} {
		if _, err := launch(); !errors.Is(err, ErrHomeConflict) {
			t.Fatalf("underivable re-point: err=%v", err)
		}
	}
	if _, ref := readRef(); ref != "/legacy" {
		t.Fatalf("refused re-point changed the row to %s", ref)
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
