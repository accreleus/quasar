// warmup_trigger_db_test.go — jobs framework WP5: the #488 golden-home warm-up
// trigger, which moved from the agent's own ImageManager observer to this
// package's image-ensure choke point. TEST_DATABASE_URL-gated like every other
// DB test here (make test-db provisions the database that makes them run).
package images

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/preparation"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeEnqueuer records every warm-up enqueue an image_state produced.
type fakeEnqueuer struct {
	mu    sync.Mutex
	calls []enqueueCall
	err   error
	ch    chan enqueueCall
}

type enqueueCall struct {
	JobID  string
	HostID string
	Params map[string]any
}

func newEnqueuer() *fakeEnqueuer {
	return &fakeEnqueuer{ch: make(chan enqueueCall, 16)}
}

func (q *fakeEnqueuer) EnqueueJob(_ context.Context, jobID, hostID string, params any) error {
	q.mu.Lock()
	p, _ := params.(map[string]any)
	c := enqueueCall{JobID: jobID, HostID: hostID, Params: p}
	q.calls = append(q.calls, c)
	err := q.err
	q.mu.Unlock()
	q.ch <- c
	return err
}

// wait returns the next enqueue, or fails the test. The trigger is asynchronous
// (it must never delay the WS read loop that ingests an image_state), so every
// assertion goes through this rather than through a sleep.
func (q *fakeEnqueuer) wait(t *testing.T) enqueueCall {
	t.Helper()
	select {
	case c := <-q.ch:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a template.warmup enqueue")
		return enqueueCall{}
	}
}

func (q *fakeEnqueuer) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.calls)
}

// Steam preparation is explicitly opted in by adopted identity, never HOME or
// WorkingDir alone. These fixtures exercise the real policy migration/report path.
const preparationRef = "ghcr.io/accreleus/quasar-steam@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func seedPreparationImage(t *testing.T, pool *pgxpool.Pool, lazy bool) {
	t.Helper()
	seedCatalog(t, pool)
	if _, err := pool.Exec(context.Background(), `INSERT INTO instance_settings(id) VALUES(true) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE image_catalog SET registry_ref=$1,library_provider='steam',runtime='{"managed_home":true,"home_container_path":"/home/quasar"}' WHERE id=$2`, preparationRef, imgID); err != nil {
		t.Fatal(err)
	}
	installAt(t, pool, lazy, imgVer, preparationRef)
}
func acknowledgePreparation(t *testing.T, pool *pgxpool.Pool, hostID string) string {
	t.Helper()
	ctx := preparation.ConnectionContext(context.Background())
	store := preparation.New(pool)
	if err := store.Register(ctx, hostID, map[string]int{"steam_preparation": 1}); err != nil {
		t.Fatal(err)
	}
	policy, err := store.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.Images) != 1 {
		t.Fatalf("expected one eligible adopted Steam image: %+v", policy)
	}
	if err = store.Report(ctx, hostID, &preparation.Reports{Steam: preparation.Report{PolicyRevision: policy.Revision, Images: []preparation.ImageReport{{Image: policy.Images[0], PreparationEnabled: true, ConsumptionEnabled: true, State: "waiting_image", Reason: "image_not_ready"}}}}); err != nil {
		t.Fatal(err)
	}
	return policy.Revision
}
func expectPreparationParams(t *testing.T, c enqueueCall, hostID, revision string) {
	t.Helper()
	if c.JobID != "template.warmup" || c.HostID != hostID {
		t.Fatalf("wrong scoped job: %+v", c)
	}
	if c.Params["image_id"] != imgID || c.Params["registry_ref"] != preparationRef || c.Params["version"] != imgVer || c.Params["policy_revision"] != revision {
		t.Fatalf("job must carry exact adopted identity and acknowledged revision: %+v", c.Params)
	}
}

func TestImageReadyEnqueuesTheWarmUp(t *testing.T) {
	for _, lazy := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "already-present-lazy"}[lazy], func(t *testing.T) {
			pool := ensureDB(t)
			seedPreparationImage(t, pool, lazy)
			hostID := seedHost(t, pool, "ready-steam")
			revision := acknowledgePreparation(t, pool, hostID)
			e := NewEnsurer(pool, newFleet(hostID), testLog())
			defer e.Close()
			q := newEnqueuer()
			e.SetJobEnqueuer(q)
			e.AgentImageState(context.Background(), hostID, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "ready"})
			expectPreparationParams(t, q.wait(t), hostID, revision)
		})
	}
}

func TestUnsupportedReadyImagesDoNotPrepareEvenWithHomeMetadata(t *testing.T) {
	for _, kind := range []string{"custom-repository", "template"} {
		t.Run(kind, func(t *testing.T) {
			pool := ensureDB(t)
			ctx := context.Background()
			if _, err := pool.Exec(ctx, `INSERT INTO instance_settings(id) VALUES(true) ON CONFLICT DO NOTHING`); err != nil {
				t.Fatal(err)
			}
			imageID, version := imgID, imgVer
			if kind == "template" {
				seedTemplateCatalog(t, pool)
				installTemplate(t, pool, false)
				imageID, version = tplID, tplVer
			} else {
				seedCatalog(t, pool)
				if _, err := pool.Exec(ctx, `UPDATE image_catalog SET library_provider='steam',runtime='{"managed_home":true,"home_container_path":"/home/quasar"}' WHERE id=$1`, imgID); err != nil {
					t.Fatal(err)
				}
				installAt(t, pool, false, imgVer, "ghcr.io/example/custom-steam@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			}
			hostID := seedHost(t, pool, "unsupported-home")
			if err := preparation.New(pool).Register(preparation.ConnectionContext(ctx), hostID, map[string]int{"steam_preparation": 1}); err != nil {
				t.Fatal(err)
			}
			e := NewEnsurer(pool, newFleet(hostID), testLog())
			defer e.Close()
			q := newEnqueuer()
			e.SetJobEnqueuer(q)
			e.AgentImageState(ctx, hostID, agentws.ImageStateMsg{ImageID: imageID, Version: version, State: "ready"})
			// Wait for the real decision without cancelling the lookup as Close would.
			e.wg.Wait()
			if q.count() != 0 {
				t.Fatal("unsupported image queued preparation")
			}
			if _, err := e.WarmupParamsForHost(ctx, hostID); err == nil {
				t.Fatal("manual trigger admitted unsupported image")
			}
		})
	}
}

func TestPreparationReadyEventRequiresCurrentHostPermission(t *testing.T) {
	cases := []struct{ name, change string }{
		{"source-off", `UPDATE instance_settings SET steam_preparation_enabled=false,steam_preparation_revision=steam_preparation_revision+1`},
		{"legacy-agent", `UPDATE hosts SET source_policy_versions=NULL`},
		{"reconnected-unacknowledged", `UPDATE hosts SET source_preparation=NULL,source_preparation_reported_at=NULL`},
		{"stale-acknowledgement", `UPDATE instance_settings SET steam_preparation_revision=steam_preparation_revision+1`},
		{"host-opt-out", `UPDATE hosts SET source_preparation=jsonb_set(source_preparation,'{steam,images,0,preparation_enabled}','false')`},
		{"offline", `UPDATE hosts SET status='offline'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := ensureDB(t)
			seedPreparationImage(t, pool, false)
			hostID := seedHost(t, pool, "guarded-steam")
			acknowledgePreparation(t, pool, hostID)
			if _, err := pool.Exec(context.Background(), tc.change); err != nil {
				t.Fatal(err)
			}
			e := NewEnsurer(pool, newFleet(hostID), testLog())
			defer e.Close()
			q := newEnqueuer()
			e.SetJobEnqueuer(q)
			e.AgentImageState(context.Background(), hostID, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "ready"})
			e.wg.Wait()
			if q.count() != 0 {
				t.Fatal("image ready bypassed source policy/host acknowledgement")
			}
			if _, err := e.WarmupParamsForHost(context.Background(), hostID); err == nil {
				t.Fatal("manual trigger bypassed policy")
			}
		})
	}
}

func TestNonReadyImageStatesDoNotEnqueueAWarmUp(t *testing.T) {
	pool := ensureDB(t)
	seedPreparationImage(t, pool, false)
	hostID := seedHost(t, pool, "states-steam")
	revision := acknowledgePreparation(t, pool, hostID)
	e := NewEnsurer(pool, newFleet(hostID), testLog(), WithRetry(0, time.Second))
	defer e.Close()
	q := newEnqueuer()
	e.SetJobEnqueuer(q)
	for _, state := range []string{"pulling", "failed", "absent"} {
		e.AgentImageState(context.Background(), hostID, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: state})
		e.wg.Wait()
		if q.count() != 0 {
			t.Fatalf("%s enqueued work before image ready", state)
		}
	}
	e.AgentImageState(context.Background(), hostID, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "ready"})
	expectPreparationParams(t, q.wait(t), hostID, revision)
}

func TestAFailedWarmUpEnqueueDoesNotAffectTheImageState(t *testing.T) {
	pool := ensureDB(t)
	seedPreparationImage(t, pool, false)
	hostID := seedHost(t, pool, "best-effort-steam")
	acknowledgePreparation(t, pool, hostID)
	e := NewEnsurer(pool, newFleet(hostID), testLog())
	defer e.Close()
	q := newEnqueuer()
	q.err = errors.New("jobs unavailable")
	e.SetJobEnqueuer(q)
	e.AgentImageState(context.Background(), hostID, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "ready"})
	q.wait(t)
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM host_images WHERE host_id=$1::uuid AND image_id=$2`, hostID, imgID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "ready" {
		t.Fatalf("background preparation changed installation readiness: %s", state)
	}
}

func TestWarmupParamsForHostResolvesTheAdoptedImage(t *testing.T) {
	pool := ensureDB(t)
	seedPreparationImage(t, pool, false)
	hostID := seedHost(t, pool, "manual-steam")
	revision := acknowledgePreparation(t, pool, hostID)
	ctx := context.Background()
	if _, err := upsertHostImage(ctx, pool, hostID, imgID, imgVer, "ready", "", nil); err != nil {
		t.Fatal(err)
	}
	e := NewEnsurer(pool, newFleet(hostID), testLog())
	defer e.Close()
	got, err := e.WarmupParamsForHost(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	params, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("unexpected params type %T", got)
	}
	expectPreparationParams(t, enqueueCall{"template.warmup", hostID, params}, hostID, revision)
}

func TestWarmupParamsForHostRefusesAHostWithNothingReady(t *testing.T) {
	pool := ensureDB(t)
	seedPreparationImage(t, pool, false)
	hostID := seedHost(t, pool, "missing-steam")
	acknowledgePreparation(t, pool, hostID)
	ctx := context.Background()
	e := NewEnsurer(pool, newFleet(hostID), testLog())
	defer e.Close()
	if _, err := e.WarmupParamsForHost(ctx, hostID); err == nil {
		t.Fatal("manual trigger accepted absent image")
	}
	for _, state := range []string{"pulling", "failed"} {
		if _, err := upsertHostImage(ctx, pool, hostID, imgID, imgVer, state, "", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := e.WarmupParamsForHost(ctx, hostID); err == nil {
			t.Fatalf("manual trigger accepted %s image", state)
		}
	}
}

func TestNoEnqueuerWiredIsNotAnError(t *testing.T) {
	pool := ensureDB(t)
	seedPreparationImage(t, pool, false)
	hostID := seedHost(t, pool, "no-queue-steam")
	acknowledgePreparation(t, pool, hostID)
	e := NewEnsurer(pool, newFleet(hostID), testLog())
	defer e.Close()
	e.AgentImageState(context.Background(), hostID, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "ready"})
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM host_images WHERE host_id=$1::uuid AND image_id=$2`, hostID, imgID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "ready" {
		t.Fatalf("installation lost ready state without queue: %s", state)
	}
}
