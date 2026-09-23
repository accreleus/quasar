package hostcfg

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func confirmPolicyGroups(t *testing.T, pool *pgxpool.Pool, hostID string, groups ...string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `UPDATE hosts SET config_policy_confirmed_groups=$2::jsonb,config_policy_ever_owned_groups=$2::jsonb WHERE id=$1::uuid`, hostID, mustJSON(t, groups))
	if err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPolicySaveIdleTimeoutCASAndDurableObligation(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "idle_timeout_secs")
	ctx := context.Background()
	first, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{
		"idle_timeout_secs": {Source: "explicit", Value: float64(900)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != "1" || first.Groups["idle_timeout_secs"].Status != "pending" {
		t.Fatalf("saved policy = %+v", first)
	}
	if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{
		"idle_timeout_secs": {Source: "explicit", Value: float64(600)},
	}, nil); err != ErrStaleRevision {
		t.Fatalf("stale edit = %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM host_reconcile_obligations WHERE host_id=$1::uuid AND kind='setting' AND resource_key='idle_timeout_secs' AND revision=1`, hostID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("durable obligation count = %d", n)
	}
	reloaded, err := store.GetPolicy(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Revision != "1" || reloaded.Choices["idle_timeout_secs"].Value != float64(900) {
		t.Fatalf("reload = %+v", reloaded)
	}
}

func TestConcurrentPolicyEditsOnlyOneWins(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "idle_timeout_secs")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, secs := range []float64{600, 900} {
		wg.Add(1)
		go func(value float64) {
			defer wg.Done()
			_, err := store.SavePolicy(context.Background(), hostID, "0", map[string]PolicyChoice{"idle_timeout_secs": {Source: "explicit", Value: value}}, nil)
			results <- err
		}(secs)
	}
	wg.Wait()
	close(results)
	success, stale := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if err == ErrStaleRevision {
			stale++
		} else {
			t.Fatalf("unexpected concurrent edit error: %v", err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("concurrent edits: success=%d stale=%d", success, stale)
	}
}

func TestPolicyRejectsAutomaticIdleTimeoutWithoutPartialSave(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "idle_timeout_secs")
	_, err := store.SavePolicy(context.Background(), hostID, "0", map[string]PolicyChoice{
		"idle_timeout_secs": {Source: "automatic"},
	}, nil)
	if err == nil {
		t.Fatal("automatic idle timeout accepted")
	}
	view, err := store.GetPolicy(context.Background(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Revision != "0" {
		t.Fatalf("invalid edit changed revision: %s", view.Revision)
	}
}

func TestPolicyObservationRequiresCurrentRevisionAndDigest(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "idle_timeout_secs")
	ctx := context.Background()
	first, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{"idle_timeout_secs": {Source: "explicit", Value: float64(900)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SavePolicy(ctx, hostID, "1", map[string]PolicyChoice{"idle_timeout_secs": {Source: "explicit", Value: float64(600)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := first.Groups["idle_timeout_secs"]
	if ok, err := store.ObservePolicyApplied(ctx, hostID, "idle_timeout_secs", old.DesiredRevision, *old.DesiredDigest, "next_session", "00000000-0000-4000-8000-000000000001"); err != nil || ok {
		t.Fatalf("old observation: ok=%v err=%v", ok, err)
	}
	current := second.Groups["idle_timeout_secs"]
	if ok, err := store.ObservePolicyApplied(ctx, hostID, "idle_timeout_secs", current.DesiredRevision, *current.DesiredDigest, "next_session", "00000000-0000-4000-8000-000000000001"); err != nil || !ok {
		t.Fatalf("current observation: ok=%v err=%v", ok, err)
	}
	view, err := store.GetPolicy(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Groups["idle_timeout_secs"].Status != "applied" {
		t.Fatalf("group state = %+v", view.Groups["idle_timeout_secs"])
	}
}

func TestLegacyClearSelectsDeploymentAndSharesRevision(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "hardware", "idle_timeout_secs")
	ctx := context.Background()
	if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{"encoder": {Source: "automatic"}}, nil); err != nil {
		t.Fatal(err)
	}
	view, err := store.SaveLegacyPatch(ctx, hostID, map[string]any{"encoder": nil}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if view.Revision != "2" || view.Choices["encoder"].Source != "deployment" {
		t.Fatalf("legacy clear = %+v", view)
	}
	if _, err := store.SavePolicy(ctx, hostID, "1", map[string]PolicyChoice{"idle_timeout_secs": {Source: "explicit", Value: float64(900)}}, nil); err != ErrStaleRevision {
		t.Fatalf("stale typed edit after legacy write = %v", err)
	}
}
