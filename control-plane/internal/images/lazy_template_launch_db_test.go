package images

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// A first lazy launch sends the frozen adopted build, then waits for the
// authenticated ready observation. The acceptance ack alone is insufficient.
func TestLazyTemplateFirstLaunchBuildsAndWaitsForReady(t *testing.T) {
	pool := ensureDB(t)
	seedTemplateCatalog(t, pool)
	installTemplate(t, pool, true)
	host := seedHost(t, pool, "lazy-host")
	fleet := newFleet(host)
	e := NewEnsurer(pool, fleet, testLog(), WithLazyBuildTimeout(3*time.Second))
	defer e.Close()
	result := make(chan error, 1)
	go func() { result <- e.PrepareLazyTemplate(context.Background(), host, tplLocalTag(tplVer)) }()
	build := fleet.waitBuild(t)
	if build.ImageID != tplID || build.LocalTag != tplLocalTag(tplVer) || build.Version != tplVer ||
		build.ContextURL != "https://codeload.github.com/accreleus/quasar-images/tar.gz/"+tplContextSHA ||
		build.BuildArgs["BASE"] != "ubuntu:24.04" {
		t.Fatalf("build did not use frozen adoption: %+v", build)
	}
	select {
	case err := <-result:
		t.Fatalf("ack treated as ready: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	e.AgentImageState(context.Background(), host, agentws.ImageStateMsg{ImageID: tplID, Version: "wrong", State: "ready"})
	select {
	case err := <-result:
		t.Fatalf("wrong version treated as ready: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	e.AgentImageState(context.Background(), host, agentws.ImageStateMsg{ImageID: tplID, Version: tplVer, State: "ready"})
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("matching ready did not complete")
	}
}

func TestConcurrentLazyTemplateLaunchesShareOneBuild(t *testing.T) {
	pool := ensureDB(t)
	seedTemplateCatalog(t, pool)
	installTemplate(t, pool, true)
	host := seedHost(t, pool, "lazy-concurrent")
	fleet := newFleet(host)
	e := NewEnsurer(pool, fleet, testLog(), WithLazyBuildTimeout(3*time.Second))
	defer e.Close()
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- e.PrepareLazyTemplate(context.Background(), host, tplLocalTag(tplVer)) }()
	}
	_ = fleet.waitBuild(t)
	e.AgentImageState(context.Background(), host, agentws.ImageStateMsg{ImageID: tplID, Version: tplVer, State: "ready"})
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("launch did not finish")
		}
	}
	fleet.mu.Lock()
	count := len(fleet.builds)
	fleet.mu.Unlock()
	if count != 1 {
		t.Fatalf("concurrent launches sent %d image_build commands, want one", count)
	}
}

func TestLazyTemplateReadyFromCurrentRegisterSkipsBuild(t *testing.T) {
	pool := ensureDB(t)
	seedTemplateCatalog(t, pool)
	installTemplate(t, pool, true)
	host := seedHost(t, pool, "lazy-cached")
	fleet := newFleet(host)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()
	e.AgentImagesRegistered(context.Background(), host,
		[]agentws.RegisterImage{{ImageID: tplID, Version: tplVer, State: "ready"}}, true)
	e.Wait()
	if err := e.PrepareLazyTemplate(context.Background(), host, tplLocalTag(tplVer)); err != nil {
		t.Fatal(err)
	}
	fleet.mu.Lock()
	count := len(fleet.builds)
	fleet.mu.Unlock()
	if count != 0 {
		t.Fatalf("verified cache dispatched %d builds", count)
	}
}

func TestLazyTemplateReconnectAndFailureDoNotAssign(t *testing.T) {
	for _, mode := range []string{"reconnect", "failed", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			pool := ensureDB(t)
			seedTemplateCatalog(t, pool)
			installTemplate(t, pool, true)
			host := seedHost(t, pool, "lazy-"+mode)
			fleet := newFleet(host)
			e := NewEnsurer(pool, fleet, testLog(), WithLazyBuildTimeout(500*time.Millisecond))
			defer e.Close()
			result := make(chan error, 1)
			go func() { result <- e.PrepareLazyTemplate(context.Background(), host, tplLocalTag(tplVer)) }()
			_ = fleet.waitBuild(t)
			switch mode {
			case "reconnect":
				fleet.mu.Lock()
				fleet.epoch = "test-epoch-2"
				fleet.mu.Unlock()
			case "failed":
				e.AgentImageState(context.Background(), host, agentws.ImageStateMsg{ImageID: tplID, Version: tplVer, State: "failed"})
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("unverified build succeeded")
				} else if mode == "reconnect" && !strings.Contains(err.Error(), "reconnected") {
					t.Fatalf("unexpected reconnect error: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("unverified build did not fail within bound")
			}
		})
	}
}

// Catalog drift under a lazy adoption must not be mistaken for "not a lazy
// template": that no-op would let the launch reach assignment with a local tag
// nothing built.
func TestLazyTemplateKindDriftFailsClosed(t *testing.T) {
	pool := ensureDB(t)
	seedTemplateCatalog(t, pool)
	installTemplate(t, pool, true)
	host := seedHost(t, pool, "lazy-drift")
	fleet := newFleet(host)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()
	if _, err := pool.Exec(context.Background(), `UPDATE image_catalog SET kind='prebuilt' WHERE id=$1`, tplID); err != nil {
		t.Fatal(err)
	}
	if err := e.PrepareLazyTemplate(context.Background(), host, tplLocalTag(tplVer)); err == nil {
		t.Fatal("drifted lazy adoption prepared without error")
	}
	fleet.mu.Lock()
	count := len(fleet.builds)
	fleet.mu.Unlock()
	if count != 0 {
		t.Fatalf("drifted lazy adoption dispatched %d builds", count)
	}
}

// An eager template or an unmanaged ref is not this seam's concern.
func TestLazyTemplatePreparationIgnoresEagerAndUnmanagedRefs(t *testing.T) {
	pool := ensureDB(t)
	seedTemplateCatalog(t, pool)
	installTemplate(t, pool, false)
	host := seedHost(t, pool, "lazy-eager")
	fleet := newFleet(host)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()
	for _, ref := range []string{tplLocalTag(tplVer), "quasar-local/unmanaged:1", "ghcr.io/example/app:1"} {
		if err := e.PrepareLazyTemplate(context.Background(), host, ref); err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
	}
}

func TestLazyTemplateAdoptionChangeAndCancellationDoNotAssign(t *testing.T) {
	for _, mode := range []string{"version", "uninstalled", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			pool := ensureDB(t)
			seedTemplateCatalog(t, pool)
			installTemplate(t, pool, true)
			host := seedHost(t, pool, "lazy-change-"+mode)
			fleet := newFleet(host)
			e := NewEnsurer(pool, fleet, testLog(), WithLazyBuildTimeout(3*time.Second))
			defer e.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- e.PrepareLazyTemplate(ctx, host, tplLocalTag(tplVer)) }()
			_ = fleet.waitBuild(t)
			switch mode {
			case "version":
				if _, err := pool.Exec(context.Background(), `UPDATE installed_images SET version='2099.01.01' WHERE image_id=$1`, tplID); err != nil {
					t.Fatal(err)
				}
			case "uninstalled":
				if _, err := pool.Exec(context.Background(), `DELETE FROM installed_images WHERE image_id=$1`, tplID); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				cancel()
			}
			// A ready report for the originally adopted build must not rescue
			// a launch whose adoption moved or whose caller went away.
			e.AgentImageState(context.Background(), host, agentws.ImageStateMsg{ImageID: tplID, Version: tplVer, State: "ready"})
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("preparation succeeded after adoption change or cancellation")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("preparation did not end")
			}
		})
	}
}

// A failure from an earlier build on the same connection must not end the
// next launch's wait before its own build reports.
func TestLazyTemplateRetryAfterFailureWaitsForNewBuild(t *testing.T) {
	pool := ensureDB(t)
	seedTemplateCatalog(t, pool)
	installTemplate(t, pool, true)
	host := seedHost(t, pool, "lazy-retry")
	fleet := newFleet(host)
	e := NewEnsurer(pool, fleet, testLog(), WithLazyBuildTimeout(3*time.Second))
	defer e.Close()
	first := make(chan error, 1)
	go func() { first <- e.PrepareLazyTemplate(context.Background(), host, tplLocalTag(tplVer)) }()
	_ = fleet.waitBuild(t)
	e.AgentImageState(context.Background(), host, agentws.ImageStateMsg{ImageID: tplID, Version: tplVer, State: "failed", Error: "context fetch"})
	if err := <-first; err == nil {
		t.Fatal("failed build succeeded")
	}
	second := make(chan error, 1)
	go func() { second <- e.PrepareLazyTemplate(context.Background(), host, tplLocalTag(tplVer)) }()
	_ = fleet.waitBuild(t)
	select {
	case err := <-second:
		t.Fatalf("retry ended on the earlier failure: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	e.AgentImageState(context.Background(), host, agentws.ImageStateMsg{ImageID: tplID, Version: tplVer, State: "ready"})
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("retry did not complete on its own ready")
	}
}

// The register snapshot is older than every live report on its connection.
func TestLazyTemplateSnapshotDoesNotEraseLiveReady(t *testing.T) {
	pool := ensureDB(t)
	seedTemplateCatalog(t, pool)
	installTemplate(t, pool, true)
	host := seedHost(t, pool, "lazy-snapshot")
	fleet := newFleet(host)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()
	e.AgentImageState(context.Background(), host, agentws.ImageStateMsg{ImageID: tplID, Version: tplVer, State: "ready"})
	e.observeSnapshotStates(host, []agentws.RegisterImage{{ImageID: tplID, Version: tplVer, State: "absent"}}, []string{tplID}, "")
	if err := e.PrepareLazyTemplate(context.Background(), host, tplLocalTag(tplVer)); err != nil {
		t.Fatal(err)
	}
	fleet.mu.Lock()
	count := len(fleet.builds)
	fleet.mu.Unlock()
	if count != 0 {
		t.Fatalf("stale snapshot erased live ready; %d builds sent", count)
	}
}

func TestLazyTemplateUnsupportedHostFailsWithoutWaiting(t *testing.T) {
	pool := ensureDB(t)
	seedTemplateCatalog(t, pool)
	installTemplate(t, pool, true)
	host := seedHost(t, pool, "lazy-old-agent")
	fleet := newFleet(host)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()
	e.markUnsupported(host)
	start := time.Now()
	if err := e.PrepareLazyTemplate(context.Background(), host, tplLocalTag(tplVer)); err == nil {
		t.Fatal("unsupported host prepared a template")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("unsupported host waited for a build")
	}
}
