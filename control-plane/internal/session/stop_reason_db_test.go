package session

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// The closed vocabulary of control-api.md amendment 24: the other recorded
// session_stop reasons are internal and must read null.
func TestClientStopReasonIsAClosedSet(t *testing.T) {
	if got := clientStopReason(strptr(StopReasonEntitlementRevoked)); got == nil || *got != "entitlement_revoked" {
		t.Errorf("entitlement_revoked: got %v, want it passed through", got)
	}
	for _, internal := range []string{"user_requested", "admin", "host_draining", "error", "cert bench complete", ""} {
		if got := clientStopReason(&internal); got != nil {
			t.Errorf("%q reached the client as %q", internal, *got)
		}
	}
	if got := clientStopReason(nil); got != nil {
		t.Errorf("no recorded reason: got %q, want null", *got)
	}
}

// TestStopReasonReadsBackForARevokeOnly — control-api.md amendment 24 (#516):
// both stops of amendment 23 read back `entitlement_revoked` on the session, the
// reason outlives the agent's terminal report and a later stop, and an owner's
// own stop reads null.
func TestStopReasonReadsBackForARevokeOnly(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store, coord := newStopServer(t, pool)
	ctx := context.Background()
	s := seed(t, pool, 4)

	owner, err := authSvc.Register(ctx, "revoked@test.local", "revoked", "unrelated-pw-24")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	tok := loginTok(t, authSvc, "revoked@test.local", "unrelated-pw-24")
	sweptApp := insertApp(t, pool, "swept", 512, 1)
	keptApp := insertApp(t, pool, "kept", 512, 1)
	byRoute := insertSessionRow(t, pool, owner.ID, s.appID, &s.hostID, "running")
	bySweep := insertSessionRow(t, pool, owner.ID, sweptApp, &s.hostID, "running")
	byOwner := insertSessionRow(t, pool, owner.ID, keptApp, &s.hostID, "running")

	// The raw body, not sessionResp: the key must be serialized when null.
	read := func(id string) (state string, reason any) {
		t.Helper()
		resp := doJSON(t, http.MethodGet, srv.URL+"/v1/sessions/"+id, tok, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET session %s: %d", id, resp.StatusCode)
		}
		var body struct {
			Session map[string]any `json:"session"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode session %s: %v", id, err)
		}
		reason, ok := body.Session["stop_reason"]
		if !ok {
			t.Fatalf("session %s: stop_reason is not serialized", id)
		}
		return body.Session["state"].(string), reason
	}
	del := func(id string) {
		t.Helper()
		resp := doJSON(t, http.MethodDelete, srv.URL+"/v1/sessions/"+id, tok, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("DELETE session %s: %d", id, resp.StatusCode)
		}
	}

	if _, reason := read(byRoute); reason != nil {
		t.Fatalf("running session: stop_reason %v, want null", reason)
	}

	// The admin routes sweep the app they changed; the ticker sweeps everything.
	revoke := func(appID string) {
		t.Helper()
		must(t, execEnt(ctx, pool, `DELETE FROM entitlements WHERE app_id = $1::uuid`, appID))
	}
	revoke(s.appID)
	if _, err := coord.StopUnentitledSessions(ctx, s.appID); err != nil {
		t.Fatalf("route stop: %v", err)
	}
	revoke(sweptApp)
	if _, err := coord.StopUnentitledSessions(ctx, ""); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for name, id := range map[string]string{"route": byRoute, "sweep": bySweep} {
		if state, reason := read(id); state != "stopping" || reason != StopReasonEntitlementRevoked {
			t.Errorf("%s stop: state %s, stop_reason %v; want stopping, %s", name, state, reason, StopReasonEntitlementRevoked)
		}
	}

	del(byOwner)
	if state, reason := read(byOwner); state != "stopping" || reason != nil {
		t.Errorf("owner's own stop: state %s, stop_reason %v; want stopping, null", state, reason)
	}

	if _, err := store.TransitionFromHost(ctx, byRoute, s.hostID, StateStopped, strptr("pipeline stopped"), nil); err != nil {
		t.Fatalf("agent's stopped report: %v", err)
	}
	if state, reason := read(byRoute); state != "stopped" || reason != StopReasonEntitlementRevoked {
		t.Errorf("after the agent's terminal report: state %s, stop_reason %v; want stopped, %s", state, reason, StopReasonEntitlementRevoked)
	}
	del(bySweep)
	if _, reason := read(bySweep); reason != StopReasonEntitlementRevoked {
		t.Errorf("after the owner stopped an already revoked session: stop_reason %v, want %s", reason, StopReasonEntitlementRevoked)
	}
}
