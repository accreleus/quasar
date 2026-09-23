//go:build rh05_contract_gap

package hostcfg

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// This red acceptance case is gated while the frozen agent contract has no
// authenticated deployment-baseline readback. Run it with
// `go test -tags rh05_contract_gap ./internal/hostcfg -run TestDeployment...`
// against the ephemeral Postgres harness after the contract amendment lands.
func TestDeploymentSourceCanBeVerifiedAfterPriorExplicitValue(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "idle_timeout_secs")
	ctx := context.Background()
	first, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{"idle_timeout_secs": {Source: "explicit", Value: float64(900)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	group := first.Groups["idle_timeout_secs"]
	if ok, err := store.ObservePolicyApplied(ctx, hostID, "idle_timeout_secs", group.DesiredRevision, *group.DesiredDigest, "next_session", "00000000-0000-4000-8000-000000000001"); err != nil || !ok {
		t.Fatalf("prior verified value: %v %v", ok, err)
	}
	if _, err := store.SavePolicy(ctx, hostID, "1", map[string]PolicyChoice{"idle_timeout_secs": {Source: "deployment"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.InvalidatePolicyEvidenceOnReconnect(ctx, hostID); err != nil {
		t.Fatal(err)
	}
	// The agent's existing capacity.effective_settings still reports the
	// journaled active 900. Its deployment baseline is 120, but that fact is
	// absent from the frozen wire, so no content-bound offer can be built.
	_, _ = pool.Exec(ctx, `UPDATE hosts SET effective_settings='{"idle_timeout_secs":"900"}'::jsonb WHERE id=$1::uuid`, hostID)
	_, _ = store.NextSessionOffer(ctx, hostID, "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000003", "00000000-0000-4000-8000-000000000004")
	h := NewHandler(store, &fakeDispatcher{}, nil)
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/hosts/"+hostID+"/policy", nil)
	r.SetPathValue("id", hostID)
	w := httptest.NewRecorder()
	h.handleGetPolicy(w, r)
	var view PolicyView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Groups["idle_timeout_secs"].Status != "applied" {
		t.Fatalf("deployment baseline 120 must eventually verify after reconnect; got %s", view.Groups["idle_timeout_secs"].Status)
	}
}
