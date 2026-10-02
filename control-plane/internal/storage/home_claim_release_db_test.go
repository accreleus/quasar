package storage

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// forgetHostHoldingHome reproduces #379: a user's home lives on one host, and
// that host is forgotten the way DELETE /v1/hosts/{id} does it (tombstone its
// homes, then delete the row, whose trigger leaves the claim claim_owner_missing).
func forgetHostHoldingHome(t *testing.T, pool *pgxpool.Pool) (user, app, homeID string) {
	t.Helper()
	ctx := context.Background()
	user = seedUser(t, pool, "claim-release@test.local")
	app = seedApp(t, pool, "Claim Release")
	_, err := pool.Exec(ctx, `UPDATE apps SET managed_home=true,home_container_path='/home/quasar' WHERE id=$1::uuid`, app)
	must(t, err)
	host := seedHost(t, pool)
	homeID = insertHome(t, pool, user, app, host)
	_, err = pool.Exec(ctx, `INSERT INTO managed_home_claims (user_id,canonical_app_id,host_id,state,materialized_at)
		VALUES ($1::uuid,$2::uuid,$3::uuid,'materialized',now())`, user, app, host)
	must(t, err)
	_, err = pool.Exec(ctx, `UPDATE user_homes SET gc_after=now() WHERE host_id=$1::uuid`, host)
	must(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM hosts WHERE id=$1::uuid`, host)
	must(t, err)
	return user, app, homeID
}

func releaseReq(user, app, reason string) ReleaseHomeClaimReq {
	return ReleaseHomeClaimReq{UserID: user, AppID: app, ExpectedState: "conflict", ExpectedConflictReason: reason}
}

func TestReleaseHomeClaimSettlesAForgottenHost(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	user, app, homeID := forgetHostHoldingHome(t, pool)
	mgr := NewLocal(pool, t.TempDir())

	var reason string
	must(t, pool.QueryRow(ctx, `SELECT conflict_reason FROM managed_home_claims WHERE user_id=$1::uuid`, user).Scan(&reason))
	if reason != "claim_owner_missing" {
		t.Fatalf("forgotten host left reason %q", reason)
	}

	// A stale read is refused and changes nothing.
	if _, err := mgr.ReleaseHomeClaim(ctx, releaseReq(user, app, "gc_pending")); !errors.Is(err, ErrClaimChanged) {
		t.Fatalf("stale expectation: %v", err)
	}
	released, err := mgr.ReleaseHomeClaim(ctx, releaseReq(user, app, "claim_owner_missing"))
	must(t, err)
	if released.RowsRemoved != 1 || released.CanonicalAppID != app || released.ConflictReason != "claim_owner_missing" {
		t.Fatalf("released = %+v", released)
	}
	var claims, homes int
	must(t, pool.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM managed_home_claims WHERE user_id=$1::uuid),
		(SELECT COUNT(*) FROM user_homes WHERE id=$2::uuid)`, user, homeID).Scan(&claims, &homes))
	if claims != 0 || homes != 0 {
		t.Fatalf("after release: claims=%d homes=%d", claims, homes)
	}
	// A second release finds nothing.
	if _, err := mgr.ReleaseHomeClaim(ctx, releaseReq(user, app, "claim_owner_missing")); !errors.Is(err, ErrClaimNotFound) {
		t.Fatalf("second release: %v", err)
	}
	// The user can get a home again: on a host that comes back, EnsureHome works.
	back := seedHostWithSecret(t, pool, "back", "s")
	if _, err := mgr.EnsureHome(ctx, user, app, back, "/home/quasar"); err != nil {
		t.Fatalf("EnsureHome after release: %v", err)
	}
}

func TestReleaseHomeClaimRefusesALiveLocationOrUse(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	user, app, _ := forgetHostHoldingHome(t, pool)
	mgr := NewLocal(pool, t.TempDir())

	// A copy recorded on a host that still exists is the rest of #347.
	other := seedHostWithSecret(t, pool, "other", "s")
	live := insertHome(t, pool, user, app, other)
	if _, err := mgr.ReleaseHomeClaim(ctx, releaseReq(user, app, "claim_owner_missing")); !errors.Is(err, ErrClaimNotReleasable) {
		t.Fatalf("live location: %v", err)
	}
	_, err := pool.Exec(ctx, `DELETE FROM user_homes WHERE id=$1::uuid`, live)
	must(t, err)

	// A pending home operation holds the claim.
	_, err = pool.Exec(ctx, `UPDATE managed_home_claims SET pending_home_session_id=$2::uuid,
		pending_home_token=$3::uuid,pending_home_started_at=now() WHERE user_id=$1::uuid`,
		user, "00000000-0000-4000-8000-000000000071", "00000000-0000-4000-8000-000000000072")
	must(t, err)
	if _, err := mgr.ReleaseHomeClaim(ctx, releaseReq(user, app, "claim_owner_missing")); !errors.Is(err, ErrClaimInUse) {
		t.Fatalf("pending hold: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE managed_home_claims SET pending_home_session_id=NULL,
		pending_home_token=NULL,pending_home_started_at=NULL WHERE user_id=$1::uuid`, user)
	must(t, err)

	// A claim that still has an owner host is never released.
	owned := seedUser(t, pool, "claim-owned@test.local")
	_, err = pool.Exec(ctx, `INSERT INTO managed_home_claims (user_id,canonical_app_id,host_id,state,conflict_reason)
		VALUES ($1::uuid,$2::uuid,$3::uuid,'conflict','gc_pending')`, owned, app, other)
	must(t, err)
	if _, err := mgr.ReleaseHomeClaim(ctx, releaseReq(owned, app, "gc_pending")); !errors.Is(err, ErrClaimNotReleasable) {
		t.Fatalf("owned claim: %v", err)
	}
}

func TestReleaseHomeClaimEndpoint(t *testing.T) {
	pool := testDB(t)
	user, app, _ := forgetHostHoldingHome(t, pool)
	mgr := NewLocal(pool, t.TempDir())
	rec := &recordingAuditor{}
	mux := http.NewServeMux()
	NewHandler(mgr, rec).Register(mux, func(h http.Handler) http.Handler { return h }, func(h http.Handler) http.Handler { return h })
	post := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/admin/storage/home-claims/release", bytes.NewBufferString(body)))
		return rr
	}
	good := `{"user_id":"` + user + `","app_id":"` + app + `","expected_state":"conflict","expected_conflict_reason":"claim_owner_missing","attestation":"host decommissioned, disk wiped"}`
	for body, want := range map[string]int{
		`{"user_id":"` + user + `","app_id":"` + app + `","expected_state":"conflict","expected_conflict_reason":"claim_owner_missing","attestation":"  "}`: http.StatusBadRequest,
		`{"user_id":"` + user + `","app_id":"` + app + `","expected_state":"conflict","expected_conflict_reason":"nope","attestation":"x"}`:                 http.StatusBadRequest,
		`{"user_id":"bad","app_id":"` + app + `","expected_state":"conflict","expected_conflict_reason":"claim_owner_missing","attestation":"x"}`:           http.StatusBadRequest,
		`{"user_id":"` + user + `","app_id":"` + app + `","expected_state":"conflict","expected_conflict_reason":"gc_pending","attestation":"x"}`:           http.StatusConflict,
		`{"extra":1}`: http.StatusBadRequest,
	} {
		if rr := post(body); rr.Code != want {
			t.Fatalf("%s = %d %s, want %d", body, rr.Code, rr.Body.String(), want)
		}
	}
	if rr := post(good); rr.Code != http.StatusNoContent {
		t.Fatalf("release = %d %s", rr.Code, rr.Body.String())
	}
	if len(rec.actions) != 1 || rec.actions[0] != "storage.home_claim.release" ||
		rec.details[0]["attestation"] != "host decommissioned, disk wiped" || rec.details[0]["rows_removed"] != 1 {
		t.Fatalf("audit = %v %v", rec.actions, rec.details)
	}
	if rr := post(good); rr.Code != http.StatusNotFound {
		t.Fatalf("second release = %d", rr.Code)
	}
}

type recordingAuditor struct {
	actions []string
	details []map[string]any
}

func (r *recordingAuditor) Record(_ context.Context, _, action, _, _ string, d map[string]any) error {
	r.actions = append(r.actions, action)
	r.details = append(r.details, d)
	return nil
}
