package session

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A lazy adoption prepares its pinned image on assignment. It has no eager
// host_images ready row before the first launch, even when the daemon already
// has the exact image. The operator launch must be admitted so the agent can
// perform that preparation; eager adoptions keep their prior-ready gate.
func TestOperatorLaunchAdmitsLazyManagedImageWithoutPriorReadyReport(t *testing.T) {
	pool := testDB(t)
	f := newEntLaunchFixture(t, pool)
	installCatalogImage(t, pool, true)
	setAppImage(t, pool, f.openAppID, testImageRef)

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

func TestOperatorLaunchKeepsLazyImageCleanupFences(t *testing.T) {
	pool := testDB(t)
	f := newEntLaunchFixture(t, pool)
	installCatalogImage(t, pool, true)
	setAppImage(t, pool, f.openAppID, testImageRef)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO host_image_operation_fences(host_id,image_id,state,generation)
		VALUES($1::uuid,$2,'removing',1)`, f.hostID, testImageID)
	must(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO host_image_cleanup_attempts
		(id,host_id,image_id,version,image_ref,runtime_image_id,generation,state,created_at,updated_at)
		VALUES('22222222-2222-4222-8222-222222222222',$1::uuid,$2,$3,$4,'sha256:daemon',1,'removing',now()-interval '2 seconds',now()-interval '1 second')`,
		f.hostID, testImageID, testImageVer, testImageRef)
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
	if status, code := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusServiceUnavailable || code != "no_host_available" {
		t.Fatalf("POST /v1/sessions before later ready report = %d (%s), want 503 no_host_available", status, code)
	}
	assertNoAppSession(t, pool, f.openAppID)
	setHostImage(t, pool, f.hostID, "ready", testImageVer)
	if status, code := launchStatus(t, f.base, f.userTok, f.openAppID); status != http.StatusCreated {
		t.Fatalf("POST /v1/sessions after later ready report = %d (%s), want 201", status, code)
	}
}

func assertNoAppSession(t *testing.T, pool *pgxpool.Pool, appID string) {
	t.Helper()
	var count int
	must(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM sessions WHERE app_id=$1::uuid`, appID).Scan(&count))
	if count != 0 {
		t.Fatalf("refused image launch persisted %d session(s), want none", count)
	}
}
