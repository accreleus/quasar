// warmup_trigger_db_test.go — jobs framework WP5: the #488 golden-home warm-up
// trigger, which moved from the agent's own ImageManager observer to this
// package's image-ensure choke point. TEST_DATABASE_URL-gated like every other
// DB test here (make test-db provisions the database that makes them run).
package images

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
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

// TestImageReadyEnqueuesTheWarmUp is the WP5 headline: this function is the ONE
// place the control plane learns an image reached `ready` on a host, so it is
// the one place the warm-up trigger belongs now that the schedule and the run
// window live control-plane side.
func TestImageReadyEnqueuesTheWarmUp(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	hostID := seedHost(t, pool, "host-a")

	e := NewEnsurer(pool, newFleet(hostID), testLog())
	defer e.Close()
	q := newEnqueuer()
	e.SetJobEnqueuer(q)

	e.AgentImageState(context.Background(), hostID, agentws.ImageStateMsg{
		ImageID: imgID, Version: imgVer, State: "ready",
	})

	c := q.wait(t)
	if c.JobID != "template.warmup" {
		t.Errorf("job id = %q, want template.warmup", c.JobID)
	}
	if c.HostID != hostID {
		t.Errorf("host id = %q, want %q (the warm-up is host-scoped)", c.HostID, hostID)
	}
	// The params carry the ADOPTED ref, read back from installed_images rather
	// than taken from the agent's report — the #440 lesson: a mutable tag must
	// never be what a host boots.
	if got := c.Params["registry_ref"]; got != imgRef {
		t.Errorf("params registry_ref = %v, want the adopted ref %q", got, imgRef)
	}
	if got := c.Params["image_id"]; got != imgID {
		t.Errorf("params image_id = %v, want %q", got, imgID)
	}
	if got := c.Params["version"]; got != imgVer {
		t.Errorf("params version = %v, want %q", got, imgVer)
	}
}

// A template adoption carries a local_tag instead of a registry_ref, and the
// warm-up must boot THAT — the same adoptedImageRef rule every dispatch uses.
func TestImageReadyEnqueuesTheWarmUpWithATemplatesLocalTag(t *testing.T) {
	pool := ensureDB(t)
	seedTemplateCatalog(t, pool)
	installTemplate(t, pool, false)
	hostID := seedHost(t, pool, "host-a-tpl")

	e := NewEnsurer(pool, newFleet(hostID), testLog())
	defer e.Close()
	q := newEnqueuer()
	e.SetJobEnqueuer(q)

	e.AgentImageState(context.Background(), hostID, agentws.ImageStateMsg{
		ImageID: tplID, Version: tplVer, State: "ready",
	})

	c := q.wait(t)
	if got := c.Params["registry_ref"]; got != tplLocalTag(tplVer) {
		t.Errorf("params registry_ref = %v, want the frozen local tag %q", got, tplLocalTag(tplVer))
	}
}

// Only `ready` triggers a warm-up. A pull that is still running, or one that
// failed, must not queue work for a host that has nothing to warm up.
func TestNonReadyImageStatesDoNotEnqueueAWarmUp(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	hostID := seedHost(t, pool, "host-a-states")

	// maxAttempts 0 so the `failed` state's retry path stays out of the way.
	e := NewEnsurer(pool, newFleet(hostID), testLog(), WithRetry(0, time.Second))
	defer e.Close()
	q := newEnqueuer()
	e.SetJobEnqueuer(q)

	ctx := context.Background()
	for _, state := range []string{"pulling", "failed", "absent"} {
		e.AgentImageState(ctx, hostID, agentws.ImageStateMsg{
			ImageID: imgID, Version: imgVer, State: state,
		})
	}
	// Then a real ready, whose enqueue is the synchronization point: if any of
	// the three above had enqueued, it would be first out of the channel.
	e.AgentImageState(ctx, hostID, agentws.ImageStateMsg{
		ImageID: imgID, Version: imgVer, State: "ready",
	})
	c := q.wait(t)
	if c.JobID != "template.warmup" {
		t.Fatalf("first enqueue came from a non-ready state: %+v", c)
	}
	if n := q.count(); n != 1 {
		t.Errorf("enqueue count = %d, want 1 (only `ready` triggers a warm-up)", n)
	}
}

// A LAZY adoption is never pushed to a host, so there is nothing to warm up —
// and an image whose adoption vanished between the report and the lookup must
// not resurrect a withdrawn ensure as a warm-up either.
func TestALazyAdoptionDoesNotEnqueueAWarmUp(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, true) // lazy
	hostID := seedHost(t, pool, "host-a-lazy")

	e := NewEnsurer(pool, newFleet(hostID), testLog())
	defer e.Close()
	q := newEnqueuer()
	e.SetJobEnqueuer(q)

	e.AgentImageState(context.Background(), hostID, agentws.ImageStateMsg{
		ImageID: imgID, Version: imgVer, State: "ready",
	})
	// Close() waits for the trigger goroutine, so by the time it returns the
	// decision has been made — no sleep needed.
	e.Close()
	if n := q.count(); n != 0 {
		t.Errorf("enqueue count = %d, want 0 for a lazy adoption", n)
	}
}

// THE BEST-EFFORT RULE. A warm-up is a background optimization: a dispatcher
// that refuses the enqueue (jobs disabled, template.warmup not registered in
// this build, a DB hiccup) must not affect the image_state ingest at all — the
// host_images row still says ready.
func TestAFailedWarmUpEnqueueDoesNotAffectTheImageState(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	hostID := seedHost(t, pool, "host-a-besteffort")

	e := NewEnsurer(pool, newFleet(hostID), testLog())
	defer e.Close()
	q := newEnqueuer()
	q.err = errors.New("jobs: not found")
	e.SetJobEnqueuer(q)

	e.AgentImageState(context.Background(), hostID, agentws.ImageStateMsg{
		ImageID: imgID, Version: imgVer, State: "ready",
	})
	q.wait(t)

	var state string
	if err := pool.QueryRow(context.Background(),
		`SELECT state FROM host_images WHERE host_id = $1::uuid AND image_id = $2`,
		hostID, imgID).Scan(&state); err != nil {
		t.Fatalf("read host_images: %v", err)
	}
	if state != "ready" {
		t.Errorf("host_images.state = %q, want ready — the warm-up trigger must never affect ingest", state)
	}
}

// --- the MANUAL trigger's params resolver (jobs Definition.ResolveParams) ----

// THE DEFECT THIS CLOSES: an admin's "Run now" on template.warmup carries no
// image-ready event, so before the resolver the run was materialized with an
// EMPTY params blob and the host failed it with `template.warmup params
// incomplete (image_id="" registry_ref="" version="")`. The resolver reads the
// same adoption rows the event path reads, so both send identical params.
func TestWarmupParamsForHostResolvesTheAdoptedImage(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	hostID := seedHost(t, pool, "host-a-manual")
	ctx := context.Background()
	if _, err := upsertHostImage(ctx, pool, hostID, imgID, imgVer, "ready", "", nil); err != nil {
		t.Fatalf("seed host_images ready: %v", err)
	}

	e := NewEnsurer(pool, newFleet(hostID), testLog())
	defer e.Close()

	got, err := e.WarmupParamsForHost(ctx, hostID)
	if err != nil {
		t.Fatalf("WarmupParamsForHost: %v", err)
	}
	p, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("params = %T, want map[string]any", got)
	}
	// Byte-for-byte the event path's three fields, including the ADOPTED ref
	// (the #440 lesson: a mutable tag must never be what a host boots).
	if p["image_id"] != imgID || p["registry_ref"] != imgRef || p["version"] != imgVer {
		t.Errorf("params = %v, want image_id=%q registry_ref=%q version=%q",
			p, imgID, imgRef, imgVer)
	}
}

// A template adoption resolves to its frozen local tag here too — the manual
// path must not diverge from the event path on the ref rule.
func TestWarmupParamsForHostUsesATemplatesLocalTag(t *testing.T) {
	pool := ensureDB(t)
	seedTemplateCatalog(t, pool)
	installTemplate(t, pool, false)
	hostID := seedHost(t, pool, "host-a-manual-tpl")
	ctx := context.Background()
	if _, err := upsertHostImage(ctx, pool, hostID, tplID, tplVer, "ready", "", nil); err != nil {
		t.Fatalf("seed host_images ready: %v", err)
	}

	e := NewEnsurer(pool, newFleet(hostID), testLog())
	defer e.Close()

	got, err := e.WarmupParamsForHost(ctx, hostID)
	if err != nil {
		t.Fatalf("WarmupParamsForHost: %v", err)
	}
	if ref := got.(map[string]any)["registry_ref"]; ref != tplLocalTag(tplVer) {
		t.Errorf("registry_ref = %v, want the frozen local tag %q", ref, tplLocalTag(tplVer))
	}
}

// A host with nothing ready REFUSES the trigger with a reason about the HOST,
// rather than queueing a run that fails on the agent with a message about the
// framework's own params.
func TestWarmupParamsForHostRefusesAHostWithNothingReady(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	hostID := seedHost(t, pool, "host-a-manual-empty")

	e := NewEnsurer(pool, newFleet(hostID), testLog())
	defer e.Close()

	_, err := e.WarmupParamsForHost(context.Background(), hostID)
	if err == nil {
		t.Fatal("want an error for a host with no ready image")
	}
	if !strings.Contains(err.Error(), "nothing to warm up") {
		t.Errorf("error = %q, want it to say there is nothing to warm up", err)
	}
	// And an image that is adopted but only PULLING is not ready either.
	if _, err := upsertHostImage(context.Background(), pool, hostID, imgID, imgVer, "pulling", "", nil); err != nil {
		t.Fatalf("seed host_images pulling: %v", err)
	}
	if _, err := e.WarmupParamsForHost(context.Background(), hostID); err == nil {
		t.Error("a `pulling` image must not resolve as warm-up-able")
	}
}

// With no enqueuer wired (QUASAR_JOBS off, or a build that never registered the
// job) the ingest path is byte-for-byte what it was before adoption.
func TestNoEnqueuerWiredIsNotAnError(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	hostID := seedHost(t, pool, "host-a-noqueue")

	e := NewEnsurer(pool, newFleet(hostID), testLog())
	defer e.Close()

	e.AgentImageState(context.Background(), hostID, agentws.ImageStateMsg{
		ImageID: imgID, Version: imgVer, State: "ready",
	})
	var state string
	if err := pool.QueryRow(context.Background(),
		`SELECT state FROM host_images WHERE host_id = $1::uuid AND image_id = $2`,
		hostID, imgID).Scan(&state); err != nil {
		t.Fatalf("read host_images: %v", err)
	}
	if state != "ready" {
		t.Errorf("host_images.state = %q, want ready", state)
	}
}
