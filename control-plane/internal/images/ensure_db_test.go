// ensure_db_test.go — image-management P2 acceptance, TEST_DATABASE_URL-gated
// like every other DB test in this repo (make test-db provisions the database
// that makes them actually run).
package images

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

const (
	imgID  = "steam"
	imgRef = "ghcr.io/accreleus/quasar-steam:sha-969cc14ea168"
	imgVer = "2026.08.07"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ensureDB is testDB plus the P2 tables and a hosts row: host_images has FKs to
// both hosts and image_catalog, so an ensure test needs a real fleet.
func ensureDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE host_images, installed_images, hosts CASCADE`); err != nil {
		t.Fatalf("truncate p2 tables: %v", err)
	}
	return pool
}

// seedCatalog inserts the catalog row every test ensures against.
func seedCatalog(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO image_catalog (id, manifest_version, display_name, kind, version, registry_ref, raw)
		VALUES ($1, 1, 'Steam', 'prebuilt', $2, $3, '{}'::jsonb)
		ON CONFLICT (id) DO NOTHING
	`, imgID, imgVer, imgRef); err != nil {
		t.Fatalf("seed image_catalog: %v", err)
	}
}

// install adopts the catalog image — the ensure-everywhere set. P3 gives this an
// API; P2 seeds it exactly as the spec says the phase is exercised. registry_ref
// is captured "at adoption" exactly like the real install path must: it is the
// value CURRENTLY in image_catalog at the moment of this call, not a live join.
func install(t *testing.T, pool *pgxpool.Pool, lazy bool) {
	t.Helper()
	installAt(t, pool, lazy, imgVer, imgRef)
}

// installAt is install with an explicit (version, registry_ref) pair — the seam
// the adopted-ref-vs-synced-catalog test uses to adopt at one ref and then move
// image_catalog.registry_ref out from under it.
func installAt(t *testing.T, pool *pgxpool.Pool, lazy bool, version, registryRef string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO installed_images (image_id, version, registry_ref, lazy) VALUES ($1, $2, $3, $4)
		ON CONFLICT (image_id) DO UPDATE SET version = EXCLUDED.version, registry_ref = EXCLUDED.registry_ref, lazy = EXCLUDED.lazy
	`, imgID, version, registryRef, lazy); err != nil {
		t.Fatalf("seed installed_images: %v", err)
	}
}

// --- P4: template catalog/adoption seeding -----------------------------------

const (
	tplID         = "xfce-desktop"
	tplVer        = "2026.08.08"
	tplContextSHA = "cafebabecafebabecafebabecafebabecafebabe"
	tplDockerfile = "xfce-desktop/Dockerfile"
	// tplContextRepo is the repo a template adoption freezes at install — the
	// value dispatch builds image_build.context_url against (P4 frozen inputs).
	tplContextRepo = "accreleus/quasar-images"
	// tplBuildArgs is the frozen string=>string build_args every P4 build
	// dispatch test asserts against.
	tplBuildArgs = `{"BASE":"ubuntu:24.04"}`
)

// tplLocalTag is the CP-assigned build tag Install/Update render — reused
// here so a test asserting a dispatch's local_tag matches the exact
// production formula, not a hand-typed copy of it.
func tplLocalTag(version string) string { return localTag(tplID, version) }

// seedTemplateCatalog inserts a kind=template catalog row with build_args, the
// entry the P4 ensure/build-dispatch tests build against.
func seedTemplateCatalog(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO image_catalog (id, manifest_version, display_name, kind, version, dockerfile, build_args, context_sha, raw)
		VALUES ($1, 1, 'XFCE Desktop', 'template', $2, $3, '{"BASE":"ubuntu:24.04"}'::jsonb, $4, '{}'::jsonb)
		ON CONFLICT (id) DO UPDATE SET version = EXCLUDED.version, dockerfile = EXCLUDED.dockerfile,
			build_args = EXCLUDED.build_args, context_sha = EXCLUDED.context_sha
	`, tplID, tplVer, tplDockerfile, tplContextSHA); err != nil {
		t.Fatalf("seed template image_catalog: %v", err)
	}
}

// installTemplate adopts the template image — the ensure-everywhere set, P4
// analogue of install/installAt above. It freezes context_repo/context_sha/
// dockerfile/build_args INTO installed_images exactly as the real Install path
// does, because dispatch (installedNonLazyQuery) now reads them from the
// adoption row, never live from image_catalog (P4 fleet-split twin fix).
func installTemplate(t *testing.T, pool *pgxpool.Pool, lazy bool) {
	t.Helper()
	installTemplateFrozen(t, pool, lazy, tplContextRepo, tplContextSHA, tplDockerfile, tplBuildArgs)
}

// installTemplateFrozen is installTemplate with explicit frozen build inputs —
// the seam the unresolved-context test uses to adopt with an empty context_sha.
func installTemplateFrozen(t *testing.T, pool *pgxpool.Pool, lazy bool, contextRepo, contextSHA, dockerfile, buildArgs string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO installed_images
			(image_id, version, local_tag, context_repo, context_sha, dockerfile, build_args, lazy)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8)
		ON CONFLICT (image_id) DO UPDATE SET version = EXCLUDED.version, local_tag = EXCLUDED.local_tag,
			context_repo = EXCLUDED.context_repo, context_sha = EXCLUDED.context_sha,
			dockerfile = EXCLUDED.dockerfile, build_args = EXCLUDED.build_args, lazy = EXCLUDED.lazy
	`, tplID, tplVer, tplLocalTag(tplVer), contextRepo, contextSHA, dockerfile, buildArgs, lazy); err != nil {
		t.Fatalf("seed template installed_images: %v", err)
	}
}

func seedHost(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO hosts (node_name, status, capacity_detection)
		VALUES ($1, 'online', 'ok') RETURNING id::text`, name).Scan(&id); err != nil {
		t.Fatalf("seed host %s: %v", name, err)
	}
	return id
}

// --- the fake agent ----------------------------------------------------------

type ensureCall struct {
	HostID, ImageID, RegistryRef, Version string
}

// buildCall records one image_build dispatch (P4 template install/update).
type buildCall struct {
	HostID, ImageID, ContextURL, ContextSubdir, Dockerfile, LocalTag, Version string
	BuildArgs                                                                 map[string]string
}

// fakeFleet is a fake node fleet: it answers ConnectedHosts, acks (or rejects)
// image_ensure/image_build, and records every dispatch so a test can drive the
// image_state stream back exactly as a real agent would.
type fakeFleet struct {
	mu      sync.Mutex
	hosts   []string
	calls   []ensureCall
	removes []removeCall
	builds  []buildCall
	reject  string // non-empty ⇒ ack{ok:false, error}
	sendErr error
	ch      chan ensureCall
	rmCh    chan removeCall
	buildCh chan buildCall
	gate    chan struct{} // non-nil ⇒ SendImageEnsure blocks on it before acking
}

// removeCall records one image_remove dispatch (P3 uninstall).
type removeCall struct {
	HostID, ImageID string
}

func newFleet(hosts ...string) *fakeFleet {
	return &fakeFleet{
		hosts: hosts, ch: make(chan ensureCall, 64), rmCh: make(chan removeCall, 64),
		buildCh: make(chan buildCall, 64),
	}
}

func (f *fakeFleet) ConnectedHosts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hosts...)
}

func (f *fakeFleet) SendImageEnsure(_ context.Context, hostID, _, imageID, ref, version string) (agentws.AckResult, error) {
	f.mu.Lock()
	c := ensureCall{HostID: hostID, ImageID: imageID, RegistryRef: ref, Version: version}
	f.calls = append(f.calls, c)
	reject, sendErr, gate := f.reject, f.sendErr, f.gate
	f.mu.Unlock()
	f.ch <- c
	if gate != nil {
		// Hold the ensure "in flight" until the test releases it, so a test can
		// enqueue a competing remove for the same target while this ensure owns
		// the target's worker.
		<-gate
	}
	if sendErr != nil {
		return agentws.AckResult{}, sendErr
	}
	if reject != "" {
		return agentws.AckResult{OK: false, Error: reject}, nil
	}
	return agentws.AckResult{OK: true}, nil
}

// SendImageBuild is the P4 template analogue of SendImageEnsure above.
func (f *fakeFleet) SendImageBuild(_ context.Context, hostID, _, imageID, contextURL, contextSubdir, dockerfile string, buildArgs map[string]string, localTag, version string) (agentws.AckResult, error) {
	f.mu.Lock()
	c := buildCall{
		HostID: hostID, ImageID: imageID, ContextURL: contextURL, ContextSubdir: contextSubdir,
		Dockerfile: dockerfile, BuildArgs: buildArgs, LocalTag: localTag, Version: version,
	}
	f.builds = append(f.builds, c)
	reject, sendErr, gate := f.reject, f.sendErr, f.gate
	f.mu.Unlock()
	f.buildCh <- c
	if gate != nil {
		<-gate
	}
	if sendErr != nil {
		return agentws.AckResult{}, sendErr
	}
	if reject != "" {
		return agentws.AckResult{OK: false, Error: reject}, nil
	}
	return agentws.AckResult{OK: true}, nil
}

// waitBuild returns the next dispatched image_build, or fails the test.
func (f *fakeFleet) waitBuild(t *testing.T) buildCall {
	t.Helper()
	select {
	case c := <-f.buildCh:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an image_build dispatch")
		return buildCall{}
	}
}

// noMoreBuilds asserts nothing further is dispatched within a short window.
func (f *fakeFleet) noMoreBuilds(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case c := <-f.buildCh:
		t.Fatalf("unexpected image_build: %+v", c)
	case <-time.After(within):
	}
}

func (f *fakeFleet) SendImageRemove(_ context.Context, hostID, _, imageID string) (agentws.AckResult, error) {
	f.mu.Lock()
	c := removeCall{HostID: hostID, ImageID: imageID}
	f.removes = append(f.removes, c)
	sendErr := f.sendErr
	f.mu.Unlock()
	f.rmCh <- c
	if sendErr != nil {
		return agentws.AckResult{}, sendErr
	}
	return agentws.AckResult{OK: true}, nil
}

// waitRemove returns the next dispatched image_remove, or fails the test.
func (f *fakeFleet) waitRemove(t *testing.T) removeCall {
	t.Helper()
	select {
	case c := <-f.rmCh:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an image_remove dispatch")
		return removeCall{}
	}
}

// noMoreRemoves asserts nothing further is dispatched within a short window.
func (f *fakeFleet) noMoreRemoves(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case c := <-f.rmCh:
		t.Fatalf("unexpected image_remove: %+v", c)
	case <-time.After(within):
	}
}

func (f *fakeFleet) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// waitEnsure returns the next dispatched ensure, or fails the test.
func (f *fakeFleet) waitEnsure(t *testing.T) ensureCall {
	t.Helper()
	select {
	case c := <-f.ch:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an image_ensure dispatch")
		return ensureCall{}
	}
}

// noMoreEnsures asserts nothing further is dispatched within a short window.
func (f *fakeFleet) noMoreEnsures(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case c := <-f.ch:
		t.Fatalf("unexpected extra image_ensure: %+v", c)
	case <-time.After(within):
	}
}

// hostStateOf reads one host's entry out of the SERVED envelope — the same
// bytes GET /v1/admin/images returns, not a private query.
func hostStateOf(t *testing.T, pool *pgxpool.Pool, hostID string) (ImageHostState, bool) {
	t.Helper()
	env, err := NewStore(pool).Envelope(context.Background())
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	for _, img := range env.Images {
		if img.ID != imgID {
			continue
		}
		for _, hs := range img.Hosts {
			if hs.HostID == hostID {
				return hs, true
			}
		}
	}
	return ImageHostState{}, false
}

// --- image_state ingest -------------------------------------------------------

// TestImageStateStreamAbsentPullingReady is the acceptance line: a fake agent
// drives absent → pulling → ready and each transition is visible in the served
// catalog, per host.
func TestImageStateStreamAbsentPullingReady(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	host := seedHost(t, pool, "host-a")
	e := NewEnsurer(pool, nil, testLog())
	defer e.Close()
	ctx := context.Background()

	for _, step := range []struct {
		state string
		pct   int32
		bytes int64
	}{
		{"absent", 0, 0},
		{"pulling", 42, 1234567},
		{"ready", 100, 9876543210},
	} {
		e.AgentImageState(ctx, host, agentws.ImageStateMsg{
			Type: "image_state", ImageID: imgID, Version: imgVer,
			State: step.state, ProgressPct: step.pct, Bytes: step.bytes,
		})
		hs, ok := hostStateOf(t, pool, host)
		if !ok {
			t.Fatalf("state %s: no host entry in the served catalog", step.state)
		}
		if hs.State != step.state {
			t.Fatalf("state: got %q want %q", hs.State, step.state)
		}
		if hs.NodeName != "host-a" {
			t.Fatalf("node_name: got %q want host-a", hs.NodeName)
		}
		if hs.Version == nil || *hs.Version != imgVer {
			t.Fatalf("version: got %v want %s", hs.Version, imgVer)
		}
		if hs.Error != nil {
			t.Fatalf("error must be nil for state %s: %v", step.state, *hs.Error)
		}
		if step.bytes > 0 && (hs.Bytes == nil || *hs.Bytes != step.bytes) {
			t.Fatalf("bytes: got %v want %d", hs.Bytes, step.bytes)
		}
	}
}

// TestImageStateFailedSurfacesReadableError — a failed pull must reach the admin
// with the agent's operator-readable message intact.
func TestImageStateFailedSurfacesReadableError(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	host := seedHost(t, pool, "host-a")
	e := NewEnsurer(pool, nil, testLog()) // nil dispatcher ⇒ no retry storm
	defer e.Close()

	const msg = "insufficient disk: 3.2 GiB free, image needs ~9 GiB"
	e.AgentImageState(context.Background(), host, agentws.ImageStateMsg{
		Type: "image_state", ImageID: imgID, Version: imgVer, State: "failed", Error: msg,
	})
	hs, ok := hostStateOf(t, pool, host)
	if !ok {
		t.Fatal("no host entry in the served catalog")
	}
	if hs.State != "failed" {
		t.Fatalf("state: got %q want failed", hs.State)
	}
	if hs.Error == nil || *hs.Error != msg {
		t.Fatalf("error: got %v want %q", hs.Error, msg)
	}

	// A later ready must clear the stale message rather than carry it forward.
	e.AgentImageState(context.Background(), host, agentws.ImageStateMsg{
		Type: "image_state", ImageID: imgID, Version: imgVer, State: "ready", Error: msg,
	})
	hs, _ = hostStateOf(t, pool, host)
	if hs.Error != nil {
		t.Fatalf("error must clear on ready: %v", *hs.Error)
	}
}

// TestImageStateDrops covers the two "drop, do not store / do not fatal" rules:
// an image_id outside image_catalog, and a state outside the schema's CHECK.
func TestImageStateDrops(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	host := seedHost(t, pool, "host-a")
	e := NewEnsurer(pool, nil, testLog())
	defer e.Close()
	ctx := context.Background()

	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: "not-in-catalog", State: "ready"})
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, State: "teleporting"})

	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM host_images`).Scan(&n); err != nil {
		t.Fatalf("count host_images: %v", err)
	}
	if n != 0 {
		t.Fatalf("dropped reports wrote %d rows, want 0", n)
	}
}

// --- ensure everywhere --------------------------------------------------------

// TestEnsureEverywhereOneHostAndTwo is the spec's ensure acceptance: seeding
// installed_images reaches ready on a one-host fleet, and on a two-host fleet
// both hosts get their own ensure and their own visible state.
func TestEnsureEverywhereOneHostAndTwo(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	h1 := seedHost(t, pool, "host-a")
	fleet := newFleet(h1)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()
	ctx := context.Background()

	if err := e.EnsureAll(ctx); err != nil {
		t.Fatalf("ensure all: %v", err)
	}
	e.Wait()
	c := fleet.waitEnsure(t)
	if c.HostID != h1 || c.ImageID != imgID || c.RegistryRef != imgRef || c.Version != imgVer {
		t.Fatalf("ensure dispatched: %+v", c)
	}
	// The fake agent completes the pull.
	e.AgentImageState(ctx, h1, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "ready"})
	if hs, ok := hostStateOf(t, pool, h1); !ok || hs.State != "ready" {
		t.Fatalf("host 1 state: %+v (found=%v)", hs, ok)
	}

	// A second trigger must NOT re-dispatch: the host is already ready at this
	// version. Ensure is level-based, not a queue that fires on every poke.
	if err := e.EnsureAll(ctx); err != nil {
		t.Fatalf("ensure all (2): %v", err)
	}
	e.Wait()
	if got := fleet.count(); got != 1 {
		t.Fatalf("re-ensure of a ready host: %d dispatches, want 1", got)
	}

	// Grow the fleet: only the new host is ensured, and it reaches ready too.
	h2 := seedHost(t, pool, "host-b")
	fleet.mu.Lock()
	fleet.hosts = append(fleet.hosts, h2)
	fleet.mu.Unlock()
	if err := e.EnsureAll(ctx); err != nil {
		t.Fatalf("ensure all (3): %v", err)
	}
	e.Wait()
	if c := fleet.waitEnsure(t); c.HostID != h2 {
		t.Fatalf("second-host ensure went to %s, want %s", c.HostID, h2)
	}
	e.AgentImageState(ctx, h2, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "ready"})

	for _, h := range []string{h1, h2} {
		hs, ok := hostStateOf(t, pool, h)
		if !ok || hs.State != "ready" {
			t.Fatalf("host %s: %+v (found=%v)", h, hs, ok)
		}
	}
	// And the catalog reports the image as adopted.
	env, err := NewStore(pool).Envelope(ctx)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	for _, img := range env.Images {
		if img.ID != imgID {
			continue
		}
		if !img.Installed || img.InstalledVersion == nil || *img.InstalledVersion != imgVer {
			t.Fatalf("installed state: %+v", img)
		}
		if len(img.Hosts) != 2 {
			t.Fatalf("hosts[]: %d entries, want 2", len(img.Hosts))
		}
	}
}

// TestEnsureDispatchesImageBuildForTemplate is the P4 acceptance line: a
// template adoption dispatches image_build, not image_ensure, with a codeload
// context_url pinned to the resolved context_sha, the dockerfile split into
// (context_subdir, dockerfile), the catalog's build_args, and the adopted
// local_tag/version.
func TestEnsureDispatchesImageBuildForTemplate(t *testing.T) {
	pool := ensureDB(t)
	seedTemplateCatalog(t, pool)
	installTemplate(t, pool, false)
	h1 := seedHost(t, pool, "host-a")
	fleet := newFleet(h1)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()
	ctx := context.Background()

	if err := e.EnsureAll(ctx); err != nil {
		t.Fatalf("ensure all: %v", err)
	}
	e.Wait()
	fleet.noMoreEnsures(t, 200*time.Millisecond) // never an image_ensure for a template
	c := fleet.waitBuild(t)
	if c.HostID != h1 || c.ImageID != tplID {
		t.Fatalf("build dispatched to: %+v", c)
	}
	wantURL := "https://codeload.github.com/accreleus/quasar-images/tar.gz/" + tplContextSHA
	if c.ContextURL != wantURL {
		t.Fatalf("context_url: got %q want %q", c.ContextURL, wantURL)
	}
	if c.ContextSubdir != "xfce-desktop" || c.Dockerfile != "Dockerfile" {
		t.Fatalf("dockerfile split: subdir=%q dockerfile=%q, want subdir=xfce-desktop dockerfile=Dockerfile",
			c.ContextSubdir, c.Dockerfile)
	}
	if c.LocalTag != tplLocalTag(tplVer) || c.Version != tplVer {
		t.Fatalf("local_tag/version: got %q/%q want %q/%q", c.LocalTag, c.Version, tplLocalTag(tplVer), tplVer)
	}
	if c.BuildArgs["BASE"] != "ubuntu:24.04" {
		t.Fatalf("build_args: got %+v want BASE=ubuntu:24.04", c.BuildArgs)
	}

	// The fake agent completes the build exactly as it would a pull — same
	// image_state stream, same host_images row.
	e.AgentImageState(ctx, h1, agentws.ImageStateMsg{ImageID: tplID, Version: tplVer, State: "building", ProgressPct: 40})
	e.AgentImageState(ctx, h1, agentws.ImageStateMsg{ImageID: tplID, Version: tplVer, State: "ready"})

	env, err := NewStore(pool).Envelope(ctx)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	for _, img := range env.Images {
		if img.ID != tplID {
			continue
		}
		if img.LocalTag == nil || *img.LocalTag != tplLocalTag(tplVer) {
			t.Fatalf("envelope local_tag: got %v want %q", img.LocalTag, tplLocalTag(tplVer))
		}
		if img.ContextSHA == nil || *img.ContextSHA != tplContextSHA {
			t.Fatalf("envelope context_sha: got %v want %q", img.ContextSHA, tplContextSHA)
		}
		if len(img.Hosts) != 1 || img.Hosts[0].State != "ready" {
			t.Fatalf("hosts[]: %+v", img.Hosts)
		}
	}
}

// TestEnsureSkipsUnresolvedTemplateContext — a template whose catalog
// context_sha is empty (a sync that never resolved it) must never dispatch a
// broken image_build with an empty context_url; the target is simply skipped.
func TestEnsureSkipsUnresolvedTemplateContext(t *testing.T) {
	pool := ensureDB(t)
	seedTemplateCatalog(t, pool)
	// Adopt with an empty FROZEN context_sha — dispatch reads the adoption row,
	// not image_catalog, so this is where an unresolved context surfaces now.
	installTemplateFrozen(t, pool, false, tplContextRepo, "", tplDockerfile, tplBuildArgs)
	h1 := seedHost(t, pool, "host-a")
	fleet := newFleet(h1)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()

	if err := e.EnsureAll(context.Background()); err != nil {
		t.Fatalf("ensure all: %v", err)
	}
	e.Wait()
	fleet.noMoreBuilds(t, 200*time.Millisecond)
	fleet.noMoreEnsures(t, 50*time.Millisecond)
}

// TestEnsureSkipsLazyImages — a lazy adoption is pulled on demand (P3 UX), never
// pushed across the fleet.
func TestEnsureSkipsLazyImages(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, true) // lazy
	h := seedHost(t, pool, "host-a")
	fleet := newFleet(h)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()

	if err := e.EnsureAll(context.Background()); err != nil {
		t.Fatalf("ensure all: %v", err)
	}
	e.Wait()
	if got := fleet.count(); got != 0 {
		t.Fatalf("lazy image dispatched %d ensures, want 0", got)
	}
}

// TestEnsureRetriesFailedThenStops — a reported failure is retried with backoff,
// and the retry budget is finite: after it, the row is LEFT failed for the admin
// rather than hammering the registry forever.
func TestEnsureRetriesFailedThenStops(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	h := seedHost(t, pool, "host-a")
	fleet := newFleet(h)
	e := NewEnsurer(pool, fleet, testLog(), WithRetry(2, 5*time.Millisecond))
	defer e.Close()
	ctx := context.Background()

	if err := e.EnsureAll(ctx); err != nil {
		t.Fatalf("ensure all: %v", err)
	}
	fleet.waitEnsure(t) // the first, un-prompted dispatch

	// Two failures ⇒ two retries (the budget).
	for i := 0; i < 2; i++ {
		e.AgentImageState(ctx, h, agentws.ImageStateMsg{
			ImageID: imgID, Version: imgVer, State: "failed", Error: "registry auth denied"})
		fleet.waitEnsure(t)
	}
	// The third failure is past the budget: nothing more is dispatched, and the
	// failure stays visible.
	e.AgentImageState(ctx, h, agentws.ImageStateMsg{
		ImageID: imgID, Version: imgVer, State: "failed", Error: "registry auth denied"})
	fleet.noMoreEnsures(t, 150*time.Millisecond)
	hs, ok := hostStateOf(t, pool, h)
	if !ok || hs.State != "failed" || hs.Error == nil {
		t.Fatalf("exhausted retries must leave a visible failure: %+v (found=%v)", hs, ok)
	}
}

// TestEnsureRejectionRecordedNotRetried — ack{ok:false} means un-actionable on
// its face (agent-api.md), so it is recorded for the operator and not retried.
func TestEnsureRejectionRecordedNotRetried(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	h := seedHost(t, pool, "host-a")
	fleet := newFleet(h)
	fleet.reject = "managed images disabled on this agent"
	e := NewEnsurer(pool, fleet, testLog(), WithRetry(3, 5*time.Millisecond))
	defer e.Close()

	if err := e.EnsureAll(context.Background()); err != nil {
		t.Fatalf("ensure all: %v", err)
	}
	fleet.waitEnsure(t)
	e.Wait()
	hs, ok := hostStateOf(t, pool, h)
	if !ok || hs.State != "failed" || hs.Error == nil || *hs.Error != fleet.reject {
		t.Fatalf("rejection not recorded: %+v (found=%v)", hs, ok)
	}
	fleet.noMoreEnsures(t, 100*time.Millisecond)
}

// --- ensure/remove ordering (review round) ------------------------------------

// TestRemoveOrdersAfterInflightEnsure is the review-round acceptance for the
// ensure/remove ordering bug: a remove for a (host, image) must not overtake an
// in-flight ensure for the SAME target. Before the fix the two used different
// inflight keys and ran concurrently, so a remove could complete first and leave
// the image present after uninstall. Here the ensure is held mid-flight, a remove
// is enqueued for the same target, and the remove must be dispatched only AFTER
// the ensure completes — proving they share one serialized worker.
func TestRemoveOrdersAfterInflightEnsure(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	h := seedHost(t, pool, "host-a")
	fleet := newFleet(h)
	fleet.gate = make(chan struct{})
	e := NewEnsurer(pool, fleet, testLog())
	// Release the gate no matter how the test exits, so a mid-test t.Fatal cannot
	// leave the worker blocked and hang e.Close()'s wg.Wait(). Idempotent.
	var once sync.Once
	releaseGate := func() { once.Do(func() { close(fleet.gate) }) }
	defer e.Close()
	defer releaseGate()
	ctx := context.Background()

	// Kick the ensure; it blocks inside the fake agent holding the target worker.
	if err := e.EnsureAll(ctx); err != nil {
		t.Fatalf("ensure all: %v", err)
	}
	if c := fleet.waitEnsure(t); c.HostID != h {
		t.Fatalf("ensure went to %s, want %s", c.HostID, h)
	}
	// While the ensure is in flight, enqueue a remove for the same target. It must
	// NOT be dispatched yet — the worker is busy with the ensure.
	e.RemoveImage(ctx, imgID, []string{h})
	fleet.noMoreRemoves(t, 100*time.Millisecond)

	// Release the ensure; the queued remove now runs behind it.
	releaseGate()
	rc := fleet.waitRemove(t)
	if rc.HostID != h || rc.ImageID != imgID {
		t.Fatalf("remove after ensure: %+v", rc)
	}
}

// --- register reconciliation ---------------------------------------------------

// TestRegisterReconciliationFlipsLostImage is the spec's reconciliation
// acceptance: an agent that reconnects WITHOUT an image it previously had must
// stop reading as ready — otherwise the scheduler keeps placing sessions onto a
// host with no image.
func TestRegisterReconciliationFlipsLostImage(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	h := seedHost(t, pool, "host-a")
	fleet := newFleet(h)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()
	ctx := context.Background()

	e.AgentImageState(ctx, h, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "ready"})
	if hs, _ := hostStateOf(t, pool, h); hs.State != "ready" {
		t.Fatalf("precondition: state %q, want ready", hs.State)
	}

	// Reconnect reporting an EMPTY set: the agent has none of them.
	e.AgentImagesRegistered(ctx, h, []agentws.RegisterImage{}, true)
	e.Wait()
	hs, ok := hostStateOf(t, pool, h)
	if !ok || hs.State != "absent" {
		t.Fatalf("lost image still reads %+v (found=%v), want absent", hs, ok)
	}
	// ...and the missing image is re-ensured on that same connect.
	if c := fleet.waitEnsure(t); c.HostID != h {
		t.Fatalf("re-ensure went to %s, want %s", c.HostID, h)
	}
}

// TestRegisterWithoutImagesFieldChangesNothing — an older agent sends no
// `images` key at all. Absent ⇒ keep the stored rows, exactly like every other
// keep-if-absent field on this wire.
func TestRegisterWithoutImagesFieldChangesNothing(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	h := seedHost(t, pool, "host-a")
	fleet := newFleet(h)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()
	ctx := context.Background()

	e.AgentImageState(ctx, h, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "ready"})

	e.AgentImagesRegistered(ctx, h, nil, false) // no images field
	e.Wait()
	hs, ok := hostStateOf(t, pool, h)
	if !ok || hs.State != "ready" {
		t.Fatalf("state changed on a report-less register: %+v (found=%v)", hs, ok)
	}
	// Nothing to ensure either — the host already has it.
	fleet.noMoreEnsures(t, 100*time.Millisecond)
}

// TestRegisterReconciliationAcceptsReportedState — the reported ids are written
// wholesale, and an unknown id in the report is dropped without poisoning the
// rest of the batch.
func TestRegisterReconciliationAcceptsReportedState(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	h := seedHost(t, pool, "host-a")
	e := NewEnsurer(pool, nil, testLog())
	defer e.Close()

	e.AgentImagesRegistered(context.Background(), h, []agentws.RegisterImage{
		{ImageID: "ghost", Version: "1", State: "ready"}, // not in image_catalog
		{ImageID: imgID, Version: imgVer, State: "ready"},
	}, true)
	e.Wait()

	hs, ok := hostStateOf(t, pool, h)
	if !ok || hs.State != "ready" || hs.Version == nil || *hs.Version != imgVer {
		t.Fatalf("reported state not applied: %+v (found=%v)", hs, ok)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM host_images`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("host_images rows = %d, want 1 (the unknown id must be dropped)", n)
	}
}

// --- version/ref drift (review round #1) --------------------------------------

// TestEnsureUsesAdoptedRefNotSyncedCatalog is the review-round #1 acceptance:
// after a catalog sync moves image_catalog.registry_ref forward, an ensure for
// an ALREADY-ADOPTED image must still carry the ref it was adopted with — never
// the moved-on catalog ref stamped with the frozen adopted version. Getting
// this wrong means a joining host pulls NEW bits labelled with the OLD
// version, silently splitting the fleet.
func TestEnsureUsesAdoptedRefNotSyncedCatalog(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	installAt(t, pool, false, imgVer, imgRef) // adopt at the original ref
	h := seedHost(t, pool, "host-a")
	fleet := newFleet(h)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()
	ctx := context.Background()

	// A catalog sync moves the ref forward WITHOUT touching the adoption's own
	// version or registry_ref — exactly what a real sync does (Store.upsert only
	// ever writes image_catalog).
	const movedRef = "ghcr.io/accreleus/quasar-steam:sha-newerbuild000"
	if _, err := pool.Exec(ctx, `UPDATE image_catalog SET registry_ref = $2 WHERE id = $1`, imgID, movedRef); err != nil {
		t.Fatalf("simulate catalog sync: %v", err)
	}

	if err := e.EnsureAll(ctx); err != nil {
		t.Fatalf("ensure all: %v", err)
	}
	c := fleet.waitEnsure(t)
	if c.RegistryRef != imgRef {
		t.Fatalf("ensure dispatched ref %q, want the ADOPTED ref %q (catalog moved to %q)", c.RegistryRef, imgRef, movedRef)
	}
	if c.Version != imgVer {
		t.Fatalf("ensure dispatched version %q, want the adopted version %q", c.Version, imgVer)
	}
}

// --- transactional register reconciliation (review round #2) ------------------

// faultyTx wraps a real pgx.Tx and fails the Nth call to Exec with a synthetic
// error, leaving every other method (QueryRow, Query, Commit, Rollback) to the
// real transaction — the seam the tests below use to prove a mid-transaction
// upsert failure rolls back the WHOLE reconciliation, not just the failed
// statement.
type faultyTx struct {
	pgx.Tx
	failOnExec int
	execN      int
}

func (f *faultyTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execN++
	if f.execN == f.failOnExec {
		return pgconn.CommandTag{}, errors.New("injected exec failure")
	}
	return f.Tx.Exec(ctx, sql, args...)
}

// faultyPool wraps *pgxpool.Pool, handing out a faultyTx from Begin so a test
// can inject a failure partway through a real transaction against the real
// database.
type faultyPool struct {
	*pgxpool.Pool
	failOnExec int
}

func (f *faultyPool) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &faultyTx{Tx: tx, failOnExec: f.failOnExec}, nil
}

// TestReconcileTransactional is the review-round #2 acceptance line: a
// mid-transaction upsert failure must roll back the ENTIRE reconciliation,
// including changes made earlier in the SAME call — not leave a
// partially-applied snapshot.
func TestReconcileTransactional(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	// A second catalog entry so the reconcile report has two rows to upsert —
	// the first succeeds inside the transaction, the second is where the fault
	// is injected.
	const imgID2 = "steam2"
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO image_catalog (id, manifest_version, display_name, kind, version, registry_ref, raw)
		VALUES ($1, 1, 'Steam 2', 'prebuilt', $2, $3, '{}'::jsonb)
	`, imgID2, imgVer, imgRef); err != nil {
		t.Fatalf("seed second catalog image: %v", err)
	}
	h := seedHost(t, pool, "host-a")

	fp := &faultyPool{Pool: pool, failOnExec: 2} // first upsert succeeds, second fails
	e := newEnsurer(fp, nil, testLog())
	defer e.Close()
	ctx := context.Background()

	e.reconcile(ctx, h, []agentws.RegisterImage{
		{ImageID: imgID, Version: imgVer, State: "ready"},
		{ImageID: imgID2, Version: imgVer, State: "ready"},
	})

	// Nothing from this reconciliation may have landed: the first upsert's
	// write was inside the same transaction as the second's failure, so it
	// must be rolled back too.
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM host_images WHERE host_id = $1::uuid`, h).Scan(&n); err != nil {
		t.Fatalf("count host_images: %v", err)
	}
	if n != 0 {
		t.Fatalf("host_images rows after rolled-back reconciliation = %d, want 0", n)
	}
}

// TestReconcileFailedUpsertDoesNotDemote is the specific bug review round #2
// names: independent statements let the upsert loop fail partway through and
// STILL run demoteUnreportedReady afterward, incorrectly demoting a row the
// (now-failed) reconciliation never validly reported on. The transaction must
// prevent the demote from running at all when the upsert fails.
func TestReconcileFailedUpsertDoesNotDemote(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	h := seedHost(t, pool, "host-a")

	// Pre-existing state: this host is already ready. A correct implementation
	// must leave this untouched when the reconciliation that would have
	// (wrongly) demoted it fails before it can commit anything.
	e0 := NewEnsurer(pool, nil, testLog())
	e0.AgentImageState(context.Background(), h, agentws.ImageStateMsg{
		ImageID: imgID, Version: imgVer, State: "ready"})
	e0.Close()
	if hs, ok := hostStateOf(t, pool, h); !ok || hs.State != "ready" {
		t.Fatalf("precondition: state %+v (found=%v), want ready", hs, ok)
	}

	fp := &faultyPool{Pool: pool, failOnExec: 1} // the ONLY upsert in this report fails
	e := newEnsurer(fp, nil, testLog())
	defer e.Close()

	// Reports imgID as absent — if this reconciliation applied at all, imgID
	// would flip ready→absent via the upsert; instead the whole thing must roll
	// back and leave imgID exactly as it was.
	e.reconcile(context.Background(), h, []agentws.RegisterImage{
		{ImageID: imgID, Version: imgVer, State: "absent"},
	})

	hs, ok := hostStateOf(t, pool, h)
	if !ok || hs.State != "ready" {
		t.Fatalf("rolled-back reconciliation changed state to %+v (found=%v), want unchanged ready", hs, ok)
	}
}

// TestReconcileErrorDispatchesAdoptedImagesDirectly is the review-round #3
// (Alice round-3) acceptance: a reconcile failure must not leave EnsureHost
// trusting stale host_images rows. With reconciliation fault-injected to
// fail, AgentImagesRegistered must still dispatch image_ensure for every
// adopted non-lazy image directly — bypassing the (now-untrustworthy)
// DB-backed readiness check EnsureHost/hostHasImage would otherwise apply.
func TestReconcileErrorDispatchesAdoptedImagesDirectly(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	h := seedHost(t, pool, "host-a")
	fleet := newFleet(h)

	// Fault-inject the reconcile transaction's very first Exec (the upsert for
	// the one reported image) so reconcile returns an error before it ever
	// commits.
	fp := &faultyPool{Pool: pool, failOnExec: 1}
	e := newEnsurer(fp, fleet, testLog())
	defer e.Close()
	ctx := context.Background()

	e.AgentImagesRegistered(ctx, h, []agentws.RegisterImage{
		{ImageID: imgID, Version: imgVer, State: "ready"},
	}, true)
	e.Wait()

	// Despite the reported state being "ready", reconciliation never committed
	// (it rolled back), so host_images still shows nothing — and the fallback
	// path must have dispatched the adopted image directly anyway, not relied
	// on (or been blocked by) that stale/absent row.
	c := fleet.waitEnsure(t)
	if c.HostID != h || c.ImageID != imgID || c.RegistryRef != imgRef || c.Version != imgVer {
		t.Fatalf("fallback dispatch: %+v", c)
	}
}
