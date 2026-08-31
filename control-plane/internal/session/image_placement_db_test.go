package session

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Image-management P2 scheduler tie-in (spec §"Scheduler tie-in"): launch
// placement must refuse a host whose managed image is not `ready`, and must
// leave every non-catalog launch exactly as it was.
//
// Integration tests — need Postgres (make test-db).

const (
	testImageID  = "steam"
	testImageRef = "ghcr.io/accreleus/quasar-steam:sha-969cc14ea168"
	testImageVer = "2026.08.07"
)

// installCatalogImage seeds image_catalog + installed_images for testImageID.
// It clears both tables first: testDB does not truncate them (they are P2
// additions), so on the shared test database a prior test's rows would
// otherwise decide this one's placement.
func installCatalogImage(t *testing.T, pool *pgxpool.Pool, lazy bool) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `DELETE FROM installed_images; DELETE FROM image_catalog;`)
	must(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO image_catalog (id, manifest_version, display_name, kind, version, registry_ref, raw)
		VALUES ($1, 1, 'Steam', 'prebuilt', $2, $3, '{}'::jsonb)
	`, testImageID, testImageVer, testImageRef)
	must(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO installed_images (image_id, version, registry_ref, lazy) VALUES ($1, $2, $3, $4)
	`, testImageID, testImageVer, testImageRef, lazy)
	must(t, err)
}

// setAppImage points the seeded app's runtime_spec at an image reference — what
// LaunchApp.Image() reads and what CreateParams.AppImage carries.
func setAppImage(t *testing.T, pool *pgxpool.Pool, appID, image string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE apps SET runtime_spec = jsonb_build_object('image', $2::text) WHERE id = $1::uuid`, appID, image)
	must(t, err)
}

// setHostImage writes a host_images row directly — the state the agent's
// image_state stream produces (that ingest path is tested in internal/images).
func setHostImage(t *testing.T, pool *pgxpool.Pool, hostID, state, version string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO host_images (host_id, image_id, version, state)
		VALUES ($1::uuid, $2, $3, $4)
		ON CONFLICT (host_id, image_id) DO UPDATE SET state = EXCLUDED.state, version = EXCLUDED.version
	`, hostID, testImageID, version, state)
	must(t, err)
}

// addHost adds a second online host + GPU, so placement has a real choice.
func addHost(t *testing.T, pool *pgxpool.Pool, name string, encodeSlots int) (hostID, gpuID string) {
	t.Helper()
	ctx := context.Background()
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, capacity_detection)
		VALUES ($1,'online','ok') RETURNING id::text`, name).Scan(&hostID))
	must(t, pool.QueryRow(ctx, `INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
		VALUES ($1, 0, 16384, $2) RETURNING id::text`, hostID, encodeSlots).Scan(&gpuID))
	return hostID, gpuID
}

// imageLaunch is launchParams with the P2 image field set, as the real launcher
// sets it from LaunchApp.Image().
func imageLaunch(s seedIDs, image string) CreateParams {
	p := launchParams(s)
	p.AppImage = image
	return p
}

// TestPlacementSkipsHostWithoutReadyImage is the load-bearing behaviour AND the
// mutation guard: host-1 has FAR more free encode slots, so the spread policy
// would pick it every time; it is chosen only if the image filter is missing.
// Deleting imageReadySQL from the candidate query fails this test on the first
// assertion.
func TestPlacementSkipsHostWithoutReadyImage(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8) // host-1: 8 slots — the spread winner absent any filter
	setQuota(t, pool, s.userID, 20)
	host2, gpu2 := addHost(t, pool, "host-2", 2)
	installCatalogImage(t, pool, false)
	setAppImage(t, pool, s.appID, testImageRef)
	ctx := context.Background()

	// Only host-2 has it. host-1 is mid-pull, which is emphatically not ready.
	setHostImage(t, pool, s.hostID, "pulling", testImageVer)
	setHostImage(t, pool, host2, "ready", testImageVer)

	sess, err := store.ScheduleAndCreate(ctx, imageLaunch(s, testImageRef))
	if err != nil {
		t.Fatalf("launch onto the ready host: %v", err)
	}
	if sess.HostID == nil || *sess.HostID != host2 {
		t.Fatalf("placed on host %v, want the image-ready host %s (gpu %s)", sess.HostID, host2, gpu2)
	}

	// Losing the image on the only ready host makes the fleet unable to serve it.
	// Not a retryable "busy": no host can run this app right now.
	setHostImage(t, pool, host2, "absent", testImageVer)
	before := countSessions(t, pool)
	_, err = store.ScheduleAndCreate(ctx, imageLaunch(s, testImageRef))
	if !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("launch with no ready host: got %v want ErrNoHostAvailable", err)
	}
	if after := countSessions(t, pool); after != before {
		t.Fatalf("refused launch persisted a row: %d → %d", before, after)
	}

	// A host holding an OLDER version must NOT count as ready (review round:
	// version-aware placement). The row says 'ready' but at the wrong build — a
	// stale host reporting a real, non-empty version is excluded until the
	// ensure orchestrator re-ensures it onto the adopted version.
	setHostImage(t, pool, host2, "ready", "2026.01.01")
	before = countSessions(t, pool)
	_, err = store.ScheduleAndCreate(ctx, imageLaunch(s, testImageRef))
	if !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("stale-version ready host must be excluded: got %v want ErrNoHostAvailable", err)
	}
	if after := countSessions(t, pool); after != before {
		t.Fatalf("refused stale-version launch persisted a row: %d → %d", before, after)
	}

	// A host reporting NO version at all (agent-api.md never requires
	// image_state.version) must still count as ready — failing an honest agent
	// closed would make it permanently unplaceable. This is the mutation-guard
	// direction too: an over-eager version filter (e.g. `hi.version = ii.version`
	// with no empty-string carve-out) fails this assertion.
	setHostImage(t, pool, host2, "ready", "")
	if _, err := store.ScheduleAndCreate(ctx, imageLaunch(s, testImageRef)); err != nil {
		t.Fatalf("version-less ready host must still be a candidate: %v", err)
	}
}

// TestPlacementFiltersAfterCatalogRefDrift is the round-2 regression: adoption
// happened at ref A, a later catalog sync moves image_catalog.registry_ref to
// ref B, but the app still launches ref A (nothing re-adopted it). Before the
// fix, imageReadySQL joined installed_images to image_catalog and matched
// ic.registry_ref — once the sync moved that column past ref A, the predicate
// no longer matched ANY installed_images row, NOT EXISTS went vacuously true,
// and a not-ready host stopped being filtered at all. Matching
// ii.registry_ref directly must keep filtering on the frozen adopted ref
// regardless of where the catalog's offer has since moved.
func TestPlacementFiltersAfterCatalogRefDrift(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	setQuota(t, pool, s.userID, 20)

	installCatalogImage(t, pool, false) // adopts testImageRef ("ref A") at testImageVer
	// Catalog syncs forward to a new offered ref ("ref B"); the adoption
	// (installed_images.registry_ref) stays frozen at ref A — this is the
	// documented invariant migration 0055's comment describes.
	const driftedRef = "ghcr.io/accreleus/quasar-steam:sha-driftedb33f"
	_, err := pool.Exec(context.Background(),
		`UPDATE image_catalog SET registry_ref = $1 WHERE id = $2`, driftedRef, testImageID)
	must(t, err)

	// The app still launches the adopted ref A, and no host has it ready.
	setAppImage(t, pool, s.appID, testImageRef)
	setHostImage(t, pool, s.hostID, "pulling", testImageVer)

	before := countSessions(t, pool)
	_, err = store.ScheduleAndCreate(context.Background(), imageLaunch(s, testImageRef))
	if !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("post-drift launch of the adopted ref must still be filtered: got %v want ErrNoHostAvailable", err)
	}
	if after := countSessions(t, pool); after != before {
		t.Fatalf("refused post-drift launch persisted a row: %d → %d", before, after)
	}

	// Once the host actually reports ready at the adopted version, placement
	// must succeed again — proves the filter is discriminating, not just
	// failing closed forever.
	setHostImage(t, pool, s.hostID, "ready", testImageVer)
	if _, err := store.ScheduleAndCreate(context.Background(), imageLaunch(s, testImageRef)); err != nil {
		t.Fatalf("ready-at-adopted-ref launch should succeed post-drift: %v", err)
	}
}

// TestPlacementIgnoresNonCatalogImage is the regression that matters most: every
// app that exists today runs an image the catalog does not manage, and P2 must
// not change one thing about how those place — even while a catalog image sits
// not-ready on the very same hosts.
func TestPlacementIgnoresNonCatalogImage(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	setQuota(t, pool, s.userID, 20)
	installCatalogImage(t, pool, false)
	// The managed image is absent everywhere...
	setHostImage(t, pool, s.hostID, "absent", testImageVer)
	// ...but this app runs a legacy ghcr image the catalog never heard of.
	const legacy = "ghcr.io/games-on-whales/wolf-legacy:1.2.3"
	setAppImage(t, pool, s.appID, legacy)

	sess, err := store.ScheduleAndCreate(context.Background(), imageLaunch(s, legacy))
	if err != nil {
		t.Fatalf("non-catalog launch must be unaffected by image state: %v", err)
	}
	if sess.HostID == nil || *sess.HostID != s.hostID {
		t.Fatalf("placed on %v, want %s", sess.HostID, s.hostID)
	}
}

// TestPlacementIgnoresUninstalledCatalogImage — an image the catalog OFFERS but
// this instance has not adopted must not gate placement either. The catalog is
// an offer; only installed_images is a commitment.
func TestPlacementIgnoresUninstalledCatalogImage(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	setQuota(t, pool, s.userID, 20)
	installCatalogImage(t, pool, false)
	// Withdraw the adoption, keep the catalog row.
	_, err := pool.Exec(context.Background(), `DELETE FROM installed_images`)
	must(t, err)
	setAppImage(t, pool, s.appID, testImageRef)
	setHostImage(t, pool, s.hostID, "absent", testImageVer)

	if _, err := store.ScheduleAndCreate(context.Background(), imageLaunch(s, testImageRef)); err != nil {
		t.Fatalf("uninstalled catalog image must not gate placement: %v", err)
	}
}

// installTemplateImage seeds image_catalog + installed_images for a
// kind=template adoption — the image-management P4 analogue of
// installCatalogImage: registry_ref stays empty, local_tag carries the
// CP-assigned build tag, exactly what actions.Install writes for a template.
func installTemplateImage(t *testing.T, pool *pgxpool.Pool, lazy bool) (imageID, localTag string) {
	t.Helper()
	ctx := context.Background()
	imageID = "xfce-desktop"
	version := "2026.08.08"
	localTag = "quasar-local/" + imageID + ":" + version
	_, err := pool.Exec(ctx, `DELETE FROM installed_images; DELETE FROM image_catalog;`)
	must(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO image_catalog (id, manifest_version, display_name, kind, version, dockerfile, context_sha, raw)
		VALUES ($1, 1, 'XFCE Desktop', 'template', $2, 'Dockerfile', 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeef', '{}'::jsonb)
	`, imageID, version)
	must(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO installed_images (image_id, version, local_tag, lazy) VALUES ($1, $2, $3, $4)
	`, imageID, version, localTag, lazy)
	must(t, err)
	return imageID, localTag
}

// setHostImageFor is setHostImage generalized to an arbitrary image_id — the
// fixed testImageID version only covers the prebuilt fixture.
func setHostImageFor(t *testing.T, pool *pgxpool.Pool, hostID, imageID, state, version string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO host_images (host_id, image_id, version, state)
		VALUES ($1::uuid, $2, $3, $4)
		ON CONFLICT (host_id, image_id) DO UPDATE SET state = EXCLUDED.state, version = EXCLUDED.version
	`, hostID, imageID, version, state)
	must(t, err)
}

// TestPlacementMatchesTemplateLocalTag is the P4 generalization of
// TestPlacementSkipsHostWithoutReadyImage: a template app's image string
// (the local_tag an admin points runtime_spec.image at, exactly as it points
// a prebuilt app at a registry ref) must be matched against
// installed_images.local_tag, not registry_ref — imageReadySQL's OR clause.
func TestPlacementMatchesTemplateLocalTag(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	setQuota(t, pool, s.userID, 20)
	imageID, localTag := installTemplateImage(t, pool, false)
	setAppImage(t, pool, s.appID, localTag)

	// Not ready anywhere yet: refused.
	setHostImageFor(t, pool, s.hostID, imageID, "building", "2026.08.08")
	before := countSessions(t, pool)
	_, err := store.ScheduleAndCreate(context.Background(), imageLaunch(s, localTag))
	if !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("launch before the template build finished: got %v want ErrNoHostAvailable", err)
	}
	if after := countSessions(t, pool); after != before {
		t.Fatalf("refused launch persisted a row: %d → %d", before, after)
	}

	// Ready at the adopted (local_tag-carried) version: placeable.
	setHostImageFor(t, pool, s.hostID, imageID, "ready", "2026.08.08")
	sess, err := store.ScheduleAndCreate(context.Background(), imageLaunch(s, localTag))
	if err != nil {
		t.Fatalf("launch onto a ready template build: %v", err)
	}
	if sess.HostID == nil || *sess.HostID != s.hostID {
		t.Fatalf("placed on %v, want %s", sess.HostID, s.hostID)
	}
}

// TestLaunchAppImageResolution proves the value the launcher feeds the filter
// comes from the effective runtime spec — no DB needed for the parse itself.
func TestLaunchAppImageResolution(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec string
		want string
	}{
		{"plain", `{"image":"ghcr.io/x/y:1"}`, "ghcr.io/x/y:1"},
		{"trimmed", `{"image":"  ghcr.io/x/y:1  "}`, "ghcr.io/x/y:1"},
		{"absent", `{"args":[]}`, ""},
		{"empty spec", ``, ""},
		{"malformed", `{`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := LaunchApp{RuntimeSpec: []byte(tc.spec)}.Image()
			if got != tc.want {
				t.Fatalf("Image() = %q, want %q", got, tc.want)
			}
		})
	}
}
