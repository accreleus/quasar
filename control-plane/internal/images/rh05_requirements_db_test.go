package images

import (
	"context"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// The Ensurer is the public preparation seam: policy and adoption are saved in
// Postgres, then the connected fleet sees only the selected adopted version.
func TestSelectedManagedImageRequirementReconcilesWithoutFleetwidePull(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	if _, err := pool.Exec(context.Background(), `INSERT INTO installed_images(image_id,version,registry_ref,lazy)
		VALUES($1,$2,$3,false)`, imgID, imgVer, imgRef); err != nil {
		t.Fatal(err)
	}
	h1 := seedHost(t, pool, "selected-host")
	h2 := seedHost(t, pool, "unselected-host")
	ctx := context.Background()
	var appID string
	if err := pool.QueryRow(ctx, `INSERT INTO apps(name,runtime_spec) VALUES
		('selected managed app',jsonb_build_object('image',$1::text)) RETURNING id::text`, imgRef).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE app_placement SET mode='fixed' WHERE app_id=$1::uuid`, appID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app_placement_hosts(app_id,host_id) VALUES($1::uuid,$2::uuid)`, appID, h1); err != nil {
		t.Fatal(err)
	}
	fleet := newFleet(h1, h2)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()
	if err := e.EnsureAll(ctx); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if got := fleet.waitEnsure(t); got.HostID != h1 || got.ImageID != imgID || got.RegistryRef != imgRef {
		t.Fatalf("selected requirement dispatched %+v", got)
	}
	fleet.noMoreEnsures(t, 0)

	// A reconnect is level triggered; it must not turn a previously unselected
	// host's cached absence into authority to pull the image.
	e.AgentImagesRegistered(ctx, h2, []agentws.RegisterImage{}, true)
	e.Wait()
	fleet.noMoreEnsures(t, 0)
	// The operator selects that host while disconnected. The durable placement
	// row is enough; reconnect reconciliation obtains the adopted version.
	fleet.mu.Lock()
	fleet.hosts = []string{h1}
	fleet.mu.Unlock()
	if _, err := pool.Exec(ctx, `INSERT INTO app_placement_hosts(app_id,host_id)
		VALUES($1::uuid,$2::uuid)`, appID, h2); err != nil {
		t.Fatal(err)
	}
	fleet.mu.Lock()
	fleet.hosts = []string{h1, h2}
	fleet.mu.Unlock()
	e.AgentImagesRegistered(ctx, h2, []agentws.RegisterImage{}, true)
	e.Wait()
	if got := fleet.waitEnsure(t); got.HostID != h2 || got.Version != imgVer {
		t.Fatalf("reconnect selected requirement: %+v", got)
	}
}

func TestPresetBackedAppOnNewHostUsesAdoptedImage(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO installed_images(image_id,version,registry_ref,lazy)
		VALUES($1,$2,$3,false)`, imgID, imgVer, imgRef); err != nil {
		t.Fatal(err)
	}
	var preset string
	if err := pool.QueryRow(ctx, `INSERT INTO runtime_presets(name,image) VALUES('selected preset',$1)
		RETURNING id::text`, imgRef).Scan(&preset); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO apps(name,runtime_preset_id,runtime_spec)
		VALUES('preset backed', $1::uuid, '{"image":42}'::jsonb)`, preset); err != nil {
		t.Fatal(err)
	}
	host := seedHost(t, pool, "new-preset-host")
	fleet := newFleet(host)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()
	if err := e.EnsureAll(ctx); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if got := fleet.waitEnsure(t); got.HostID != host || got.RegistryRef != imgRef {
		t.Fatalf("preset-backed preparation: %+v", got)
	}
}

func TestSharedAdoptedImageRequirementIsUnionAndRemovalKeepsCache(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO installed_images(image_id,version,registry_ref,lazy)
		VALUES($1,$2,$3,false)`, imgID, imgVer, imgRef); err != nil {
		t.Fatal(err)
	}
	host := seedHost(t, pool, "shared-requirement-host")
	var first, custom string
	for i, target := range []*string{&first, &custom} {
		if err := pool.QueryRow(ctx, `INSERT INTO apps(name,runtime_spec)
			VALUES($1,jsonb_build_object('image',$2::text)) RETURNING id::text`,
			[]string{"canonical app", "custom shared image"}[i], imgRef).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	fleet := newFleet(host)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()
	if err := e.EnsureAll(ctx); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if got := fleet.waitEnsure(t); got.ImageID != imgID {
		t.Fatalf("shared requirement dispatch: %+v", got)
	}
	fleet.noMoreEnsures(t, 0)
	if _, err := pool.Exec(ctx, `DELETE FROM apps WHERE id=$1::uuid`, first); err != nil {
		t.Fatal(err)
	}
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "absent"})
	if err := e.EnsureAll(ctx); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if got := fleet.waitEnsure(t); got.ImageID != imgID {
		t.Fatalf("remaining custom app requirement: %+v", got)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM apps WHERE id=$1::uuid`, custom); err != nil {
		t.Fatal(err)
	}
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "absent"})
	if err := e.EnsureAll(ctx); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	fleet.noMoreEnsures(t, 50*time.Millisecond)
	if _, ok := hostStateOf(t, pool, host); !ok {
		t.Fatal("removing final requirement deleted cache inventory")
	}
}

func TestFailedImageDoesNotRearmOnPeriodicRequirementScan(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	host := seedHost(t, pool, "failed-requirement-host")
	fleet := newFleet(host)
	e := NewEnsurer(pool, fleet, testLog(), WithRetry(0, time.Millisecond))
	defer e.Close()
	ctx := context.Background()
	if err := e.EnsureAll(ctx); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	fleet.waitEnsure(t)
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "failed", Error: "registry denied"})
	if err := e.EnsureAll(ctx); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	fleet.noMoreEnsures(t, 50*time.Millisecond)
	status, ok := hostStateOf(t, pool, host)
	if !ok || status.State != "failed" {
		t.Fatalf("failure changed by scan: %+v (found=%v)", status, ok)
	}
	// A new control-plane process has no in-memory retry budget. The durable
	// failed row still prevents a loop until an explicit Retry or new version.
	e.Close()
	restarted := NewEnsurer(pool, fleet, testLog())
	defer restarted.Close()
	if err := restarted.EnsureAll(ctx); err != nil {
		t.Fatal(err)
	}
	restarted.Wait()
	fleet.noMoreEnsures(t, 0)
}

func TestReconnectResumesOnlyStaleProgressFromPreviousConnection(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	host := seedHost(t, pool, "reconnected-progress-host")
	fleet := newFleet(host)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()
	ctx := context.Background()
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "pulling"})
	if err := e.EnsureHost(ctx, host); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	fleet.noMoreEnsures(t, 0) // current progress is not duplicated
	e.AgentImagesRegistered(ctx, host, []agentws.RegisterImage{}, true)
	e.Wait()
	if got := fleet.waitEnsure(t); got.HostID != host || got.ImageID != imgID {
		t.Fatalf("omitted previous-connection progress did not resume: %+v", got)
	}
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "building"})
	e.AgentImagesRegistered(ctx, host, nil, false)
	e.Wait()
	if got := fleet.waitEnsure(t); got.HostID != host || got.ImageID != imgID {
		t.Fatalf("legacy register left stale build in progress: %+v", got)
	}
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, State: "pulling"})
	if err := e.EnsureAll(ctx); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	fleet.noMoreEnsures(t, 0) // empty legacy version is active progress now
	e.AgentImagesRegistered(ctx, host, nil, false)
	e.Wait()
	if got := fleet.waitEnsure(t); got.HostID != host || got.ImageID != imgID {
		t.Fatalf("legacy blank-version progress did not resume on reconnect: %+v", got)
	}
}

func TestSuccessfulImageHistorySurvivesInventoryAndIgnoresStaleProof(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	install(t, pool, false)
	host := seedHost(t, pool, "history-host")
	e := NewEnsurer(pool, nil, testLog())
	defer e.Close()
	ctx := context.Background()
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "ready"})
	// The agent's ready message names only a version. Reinstalling different
	// content under that same label must not alias the retained identity.
	if _, err := pool.Exec(ctx, `DELETE FROM installed_images WHERE image_id=$1`, imgID); err != nil {
		t.Fatal(err)
	}
	otherRef := imgRef + "-reinstalled"
	if _, err := pool.Exec(ctx, `INSERT INTO installed_images(image_id,version,registry_ref,lazy)
		VALUES($1,$2,$3,false)`, imgID, imgVer, otherRef); err != nil {
		t.Fatal(err)
	}
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "ready"})
	var retainedRef string
	if err := pool.QueryRow(ctx, `SELECT current_identity->>'registry_ref' FROM host_image_success_history
		WHERE host_id=$1::uuid AND image_id=$2`, host, imgID).Scan(&retainedRef); err != nil {
		t.Fatal(err)
	}
	if retainedRef != imgRef {
		t.Fatalf("same-version reinstall aliased retention: %q", retainedRef)
	}
	// Adoption changes only by an explicit installed_images update. A stale
	// report for the old version cannot advance retention history.
	const next = "2026.09.23"
	if _, err := pool.Exec(ctx, `UPDATE installed_images SET version=$2 WHERE image_id=$1`, imgID, next); err != nil {
		t.Fatal(err)
	}
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "ready"})
	var current, previous, currentRef, previousRef string
	if err := pool.QueryRow(ctx, `SELECT current_version,COALESCE(previous_version,''),current_identity->>'registry_ref',COALESCE(previous_identity->>'registry_ref','') FROM host_image_success_history
		WHERE host_id=$1::uuid AND image_id=$2`, host, imgID).Scan(&current, &previous, &currentRef, &previousRef); err != nil {
		t.Fatal(err)
	}
	if current != imgVer || previous != "" || currentRef != imgRef || previousRef != "" {
		t.Fatalf("stale proof changed history: current=%q previous=%q currentRef=%q previousRef=%q", current, previous, currentRef, previousRef)
	}
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: next, State: "ready"})
	if _, err := pool.Exec(ctx, `DELETE FROM host_images WHERE host_id=$1::uuid AND image_id=$2`, host, imgID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT current_version,COALESCE(previous_version,''),current_identity->>'registry_ref',COALESCE(previous_identity->>'registry_ref','') FROM host_image_success_history
		WHERE host_id=$1::uuid AND image_id=$2`, host, imgID).Scan(&current, &previous, &currentRef, &previousRef); err != nil {
		t.Fatal(err)
	}
	if current != next || previous != imgVer || currentRef != otherRef || previousRef != imgRef {
		t.Fatalf("retained history: current=%q previous=%q currentRef=%q previousRef=%q", current, previous, currentRef, previousRef)
	}
}

func TestRegisteredImagesCommitIndependentlyWhenOneHistoryWriteFails(t *testing.T) {
	pool := ensureDB(t)
	ctx := context.Background()
	seedCatalog(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO image_catalog(id,manifest_version,display_name,kind,version,registry_ref,raw)
		VALUES('other-ready',1,'Other','prebuilt','v1','registry.example/other:v1','{}'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installed_images(image_id,version,registry_ref,lazy)
		VALUES('other-ready','v1','registry.example/other:v1',false)`); err != nil {
		t.Fatal(err)
	}
	host := seedHost(t, pool, "independent-history-host")
	if _, err := pool.Exec(ctx, `ALTER TABLE host_image_success_history
		ADD CONSTRAINT reject_one_history CHECK (image_id <> 'steam')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `ALTER TABLE host_image_success_history DROP CONSTRAINT IF EXISTS reject_one_history`)
	})
	e := NewEnsurer(pool, nil, testLog())
	defer e.Close()
	e.AgentImagesRegistered(ctx, host, []agentws.RegisterImage{
		{ImageID: imgID, Version: imgVer, State: "ready"},
		{ImageID: "other-ready", Version: "v1", State: "ready"},
	}, true)
	e.Wait()
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM host_images WHERE host_id=$1::uuid AND image_id='other-ready'`, host).Scan(&state); err != nil || state != "ready" {
		t.Fatalf("independent image lost: state=%q err=%v", state, err)
	}
	var successes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM host_image_success_history WHERE host_id=$1::uuid AND image_id='other-ready'`, host).Scan(&successes); err != nil || successes != 1 {
		t.Fatalf("independent history lost: count=%d err=%v", successes, err)
	}
}

func TestCurrentInventoryEvidenceResetsAtReconnect(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	host := seedHost(t, pool, "inventory-evidence-host")
	fleet := newFleet(host)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()
	ctx := context.Background()
	e.AgentImagesRegistered(ctx, host, []agentws.RegisterImage{}, true)
	e.Wait()
	if connected, observed, snapshot := e.CurrentImageEvidence(host, imgID); !connected || observed || !snapshot {
		t.Fatalf("empty current snapshot: connected=%v observed=%v snapshot=%v", connected, observed, snapshot)
	}
	e.AgentImageState(ctx, host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "ready"})
	if connected, observed, _ := e.CurrentImageEvidence(host, imgID); !connected || !observed {
		t.Fatalf("current ready observation: connected=%v observed=%v", connected, observed)
	}
	fleet.mu.Lock()
	fleet.hosts = nil
	fleet.mu.Unlock()
	if connected, _, _ := e.CurrentImageEvidence(host, imgID); connected {
		t.Fatal("disconnected host retained current evidence")
	}
	fleet.mu.Lock()
	fleet.hosts = []string{host}
	fleet.mu.Unlock()
	e.AgentImagesRegistered(ctx, host, nil, false)
	e.Wait()
	if connected, observed, snapshot := e.CurrentImageEvidence(host, imgID); !connected || observed || snapshot {
		t.Fatalf("legacy reconnect reused old inventory: connected=%v observed=%v snapshot=%v", connected, observed, snapshot)
	}
}

func TestImageFreePresetDoesNotRequireAnyAdoptedImage(t *testing.T) {
	pool := ensureDB(t)
	seedCatalog(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO installed_images(image_id,version,registry_ref,lazy)
      VALUES($1,$2,$3,false)`, imgID, imgVer, imgRef); err != nil {
		t.Fatal(err)
	}
	var preset string
	if err := pool.QueryRow(ctx, `INSERT INTO runtime_presets(name) VALUES('image free preset') RETURNING id::text`).Scan(&preset); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO apps(name,runtime_preset_id)
      VALUES('image free app',$1::uuid)`, preset); err != nil {
		t.Fatal(err)
	}
	host := seedHost(t, pool, "image-free-preset-host")
	fleet := newFleet(host)
	e := NewEnsurer(pool, fleet, testLog())
	defer e.Close()
	if err := e.EnsureAll(ctx); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	fleet.noMoreEnsures(t, 20*time.Millisecond)
}
