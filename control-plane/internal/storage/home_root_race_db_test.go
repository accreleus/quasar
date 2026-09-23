package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

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
	mgr := New(pool, fixedProvider("local"), HostRootResolverFunc(func(context.Context, string) (string, error) {
		close(entered)
		<-release
		return "/srv/homes", nil
	}))
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
