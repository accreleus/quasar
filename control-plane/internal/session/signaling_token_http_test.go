package session

import (
	"context"
	"net/http"
	"testing"
)

// TestSignalingTokenOwnerOnly: an admin may read or stop another user's session
// but must not mint a token that attaches to its stream (#496, control-api.md).
func TestSignalingTokenOwnerOnly(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newMetricsServer(t, pool)
	ctx := context.Background()
	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	admin, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01")
	if err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE id=$1`, admin.ID); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	s := currentSeed(t, pool)
	ownerSID := sessionForUser(t, store, s, owner.ID)
	adminSID := sessionForUser(t, store, s, admin.ID)
	ownerTok := loginTok(t, authSvc, "owner@test.local", "quasar-fixture-pw-03")
	adminTok := loginTok(t, authSvc, "admin@test.local", "quasar-fixture-pw-01")

	mint := func(sid, tok string) int {
		t.Helper()
		resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/signaling-token", tok, nil)
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := mint(ownerSID, adminTok); got != http.StatusNotFound {
		t.Fatalf("admin minting for another user's session: got %d want 404", got)
	}
	if got := mint(ownerSID, ownerTok); got != http.StatusCreated {
		t.Fatalf("owner minting: got %d want 201", got)
	}
	if got := mint(adminSID, adminTok); got != http.StatusCreated {
		t.Fatalf("admin minting for own session: got %d want 201", got)
	}
}
