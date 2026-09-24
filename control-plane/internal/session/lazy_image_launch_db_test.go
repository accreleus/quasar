package session

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const lazyDigestRef = "ghcr.io/accreleus/quasar-steam@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func installLazyDigest(t *testing.T, pool *pgxpool.Pool, appID string) {
	t.Helper()
	installCatalogImage(t, pool, true)
	_, err := pool.Exec(context.Background(), `UPDATE installed_images SET registry_ref=$1 WHERE image_id=$2`, lazyDigestRef, testImageID)
	must(t, err)
	setAppImage(t, pool, appID, lazyDigestRef)
}

// A lazy adoption prepares its pinned image on assignment. It has no eager
// host_images ready row before the first launch, even when the daemon already
// has the exact image. The operator launch must be admitted so the agent can
// perform that preparation; eager adoptions keep their prior-ready gate.
func TestOperatorLaunchAdmitsLazyManagedImageWithoutPriorReadyReport(t *testing.T) {
	pool := testDB(t)
	f := newEntLaunchFixture(t, pool)
	installLazyDigest(t, pool, f.openAppID)

	if status, code := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusCreated {
		t.Fatalf("POST /v1/sessions for lazy adopted image = %d (%s), want 201", status, code)
	}
}

func TestOperatorLaunchKeepsEagerManagedImageReadyGate(t *testing.T) {
	pool := testDB(t)
	f := newEntLaunchFixture(t, pool)
	installCatalogImage(t, pool, false)
	setAppImage(t, pool, f.openAppID, testImageRef)

	if status, code := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusServiceUnavailable || code != "no_host_available" {
		t.Fatalf("POST /v1/sessions before eager image ready = %d (%s), want 503 no_host_available", status, code)
	}
	setHostImage(t, pool, f.hostID, "ready", testImageVer)
	if status, code := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusCreated {
		t.Fatalf("POST /v1/sessions after eager image ready = %d (%s), want 201", status, code)
	}
}

func TestOperatorLaunchRequiresPriorReadyForLazyTemplateAndUnpinnedRef(t *testing.T) {
	t.Run("template", func(t *testing.T) {
		pool := testDB(t)
		f := newEntLaunchFixture(t, pool)
		installCatalogImage(t, pool, true)
		_, err := pool.Exec(context.Background(), `UPDATE image_catalog SET kind='template' WHERE id=$1`, testImageID)
		must(t, err)
		_, err = pool.Exec(context.Background(), `UPDATE installed_images SET registry_ref='',local_tag='quasar/steam:test' WHERE image_id=$1`, testImageID)
		must(t, err)
		setAppImage(t, pool, f.openAppID, "quasar/steam:test")
		if status, _ := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusServiceUnavailable {
			t.Fatalf("lazy template without ready report = %d, want 503", status)
		}
		assertNoAppSession(t, pool, f.openAppID)
	})
	t.Run("unpinned prebuilt", func(t *testing.T) {
		pool := testDB(t)
		f := newEntLaunchFixture(t, pool)
		installCatalogImage(t, pool, true)
		setAppImage(t, pool, f.openAppID, testImageRef)
		if status, _ := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusServiceUnavailable {
			t.Fatalf("lazy unpinned prebuilt without ready report = %d, want 503", status)
		}
		assertNoAppSession(t, pool, f.openAppID)
	})
}

func TestOperatorLaunchKeepsLazyImageCleanupFences(t *testing.T) {
	pool := testDB(t)
	f := newEntLaunchFixture(t, pool)
	installLazyDigest(t, pool, f.openAppID)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO host_image_operation_fences(host_id,image_id,state,generation)
		VALUES($1::uuid,$2,'removing',1)`, f.hostID, testImageID)
	must(t, err)
	// The current adoption itself must map the exact ref to this fence. A
	// command journal or prior success row need not exist yet. Placement must
	// exclude the host itself: if only the reservation recheck refused it, the
	// retry loop would exhaust and report capacity_exhausted instead.
	if status, code := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusServiceUnavailable || code != "no_host_available" {
		t.Fatalf("POST /v1/sessions with active fence before journal = %d (%s), want 503 no_host_available", status, code)
	}
	assertNoAppSession(t, pool, f.openAppID)
	_, err = pool.Exec(ctx, `INSERT INTO host_image_cleanup_attempts
		(id,host_id,image_id,version,image_ref,runtime_image_id,generation,state,created_at,updated_at)
		VALUES('22222222-2222-4222-8222-222222222222',$1::uuid,$2,$3,$4,'sha256:daemon',1,'removing',now()-interval '2 seconds',now()-interval '1 second')`,
		f.hostID, testImageID, testImageVer, lazyDigestRef)
	must(t, err)
	if status, code := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusServiceUnavailable || code != "no_host_available" {
		t.Fatalf("POST /v1/sessions during lazy image cleanup = %d (%s), want 503 no_host_available", status, code)
	}
	assertNoAppSession(t, pool, f.openAppID)

	_, err = pool.Exec(ctx, `UPDATE host_image_operation_fences SET state='idle' WHERE host_id=$1::uuid AND image_id=$2`, f.hostID, testImageID)
	must(t, err)
	_, err = pool.Exec(ctx, `UPDATE host_image_cleanup_attempts SET state='removed', updated_at=now()-interval '1 second'
		WHERE id='22222222-2222-4222-8222-222222222222'`)
	must(t, err)
	// An adopted lazy prebuilt digest is safe to assign now: the agent must
	// ensure it before starting the session. No forged ready report is needed.
	if status, code := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusCreated {
		t.Fatalf("POST /v1/sessions after confirmed removal = %d (%s), want 201", status, code)
	}
}

// A lazy update adopts a new digest. The removed attempt names the old ref,
// so it neither blocks nor is required for the new one.
func TestOperatorLaunchAdmitsUpdatedLazyDigestAfterOldRemoval(t *testing.T) {
	pool := testDB(t)
	f := newEntLaunchFixture(t, pool)
	installLazyDigest(t, pool, f.openAppID)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO host_image_operation_fences(host_id,image_id,state,generation)
		VALUES($1::uuid,$2,'idle',1)`, f.hostID, testImageID)
	must(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO host_image_cleanup_attempts
		(id,host_id,image_id,version,image_ref,runtime_image_id,generation,state,created_at,updated_at)
		VALUES('22222222-2222-4222-8222-222222222222',$1::uuid,$2,$3,$4,'sha256:daemon',1,'removed',now()-interval '2 seconds',now()-interval '1 second')`,
		f.hostID, testImageID, testImageVer, lazyDigestRef)
	must(t, err)
	const next = "ghcr.io/accreleus/quasar-steam@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	_, err = pool.Exec(ctx, `UPDATE installed_images SET registry_ref=$1, version='2026.09.01' WHERE image_id=$2`, next, testImageID)
	must(t, err)
	setAppImage(t, pool, f.openAppID, next)
	if status, code := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusCreated {
		t.Fatalf("POST /v1/sessions for updated lazy digest = %d (%s), want 201", status, code)
	}
}

// A cleanup that raises the removing fence while a lazy launch is placing
// wins: the launch waits on the fence row, then refuses the host.
func TestLazyDigestLaunchWaitsForUncommittedCleanupFence(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 2)
	setQuota(t, pool, s.userID, 20)
	installLazyDigest(t, pool, s.appID)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO host_image_operation_fences(host_id,image_id,state,generation)
		VALUES($1::uuid,$2,'idle',1)`, s.hostID, testImageID)
	must(t, err)
	tx, err := pool.Begin(ctx)
	must(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `UPDATE host_image_operation_fences SET state='removing'
		WHERE host_id=$1::uuid AND image_id=$2`, s.hostID, testImageID)
	must(t, err)
	result := make(chan error, 1)
	go func() {
		_, err := store.ScheduleAndCreate(ctx, imageLaunch(s, lazyDigestRef))
		result <- err
	}()
	// Commit only once the launch is parked on the fence row lock, so the
	// test proves the locked recheck waited rather than placement alone.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		must(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'
			AND query LIKE '%host_image_operation_fences%')`).Scan(&waiting))
		if waiting {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("lazy launch escaped uncommitted cleanup fence: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("lazy launch never waited on the cleanup fence row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	must(t, tx.Commit(ctx))
	select {
	case err := <-result:
		if !errors.Is(err, ErrNoHostAvailable) {
			t.Fatalf("lazy launch after cleanup committed = %v, want no host available", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lazy launch did not finish after cleanup committed")
	}
}

// Only the pinned digest earns the removal exception. A lazy tag ref still
// needs a ready report newer than its confirmed removal.
func TestOperatorLaunchBlocksLazyTagRefAfterExactRemoval(t *testing.T) {
	pool := testDB(t)
	f := newEntLaunchFixture(t, pool)
	installCatalogImage(t, pool, true)
	setAppImage(t, pool, f.openAppID, testImageRef)
	setHostImage(t, pool, f.hostID, "ready", testImageVer)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `UPDATE host_images SET updated_at=now()-interval '5 seconds' WHERE host_id=$1::uuid`, f.hostID)
	must(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO host_image_operation_fences(host_id,image_id,state,generation)
		VALUES($1::uuid,$2,'idle',1)`, f.hostID, testImageID)
	must(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO host_image_cleanup_attempts
		(id,host_id,image_id,version,image_ref,runtime_image_id,generation,state,created_at,updated_at)
		VALUES('22222222-2222-4222-8222-222222222222',$1::uuid,$2,$3,$4,'sha256:daemon',1,'removed',now()-interval '2 seconds',now()-interval '1 second')`,
		f.hostID, testImageID, testImageVer, testImageRef)
	must(t, err)
	if status, code := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusServiceUnavailable || code != "no_host_available" {
		t.Fatalf("POST /v1/sessions for removed lazy tag = %d (%s), want 503 no_host_available", status, code)
	}
	assertNoAppSession(t, pool, f.openAppID)
}

func TestOperatorLaunchBlocksPrunedLazyImageAfterExactRemoval(t *testing.T) {
	pool := testDB(t)
	f := newEntLaunchFixture(t, pool)
	installLazyDigest(t, pool, f.openAppID)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO host_image_operation_fences(host_id,image_id,state,generation)
		VALUES($1::uuid,$2,'idle',1)`, f.hostID, testImageID)
	must(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO host_image_cleanup_attempts
		(id,host_id,image_id,version,image_ref,runtime_image_id,generation,state,created_at,updated_at)
		VALUES('22222222-2222-4222-8222-222222222222',$1::uuid,$2,$3,$4,'sha256:daemon',1,'removed',now()-interval '2 seconds',now()-interval '1 second')`,
		f.hostID, testImageID, testImageVer, lazyDigestRef)
	must(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM installed_images WHERE image_id=$1`, testImageID)
	must(t, err)
	if status, _ := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusServiceUnavailable {
		t.Fatalf("POST /v1/sessions with pruned exact ref = %d, want 503", status)
	}
	assertNoAppSession(t, pool, f.openAppID)
}

func assertNoAppSession(t *testing.T, pool *pgxpool.Pool, appID string) {
	t.Helper()
	var count int
	must(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM sessions WHERE app_id=$1::uuid`, appID).Scan(&count))
	if count != 0 {
		t.Fatalf("refused image launch persisted %d session(s), want none", count)
	}
}
