package console

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeDispatcher struct{ sent []any }

func (d *fakeDispatcher) SendOrReconnect(hostID string, v any) error {
	d.sent = append(d.sent, v)
	return nil
}

func patchRequest(t *testing.T, hostID string, body map[string]any) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode patch: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/hosts/"+hostID+"/console-config", bytes.NewReader(raw))
	req.SetPathValue("id", hostID)
	return req
}

func getResponse(t *testing.T, h *Handler, hostID string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/hosts/"+hostID+"/console-config", nil)
	req.SetPathValue("id", hostID)
	rec := httptest.NewRecorder()
	h.handleGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", rec.Code, rec.Body.String())
	}
	out := map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	return out
}

// The admin GET passes the agent's access report through verbatim.
func TestGetReturnsStoredAccess(t *testing.T) {
	pool := testPool(t)
	hostID := seedTestHost(t, pool, "console-test-get-access")
	store := NewStore(pool)
	ctx := context.Background()

	if err := store.UpsertCapabilities(ctx, hostID, Capabilities{
		Connectors: []string{"DP-1"}, InputDevices: []InputDevicePath{},
		Access: &Access{State: "restored", Target: boolPtr(true), RequestID: strPtr("req-get-1"),
			Reason: strPtr("unhealthy"), Summary: "the node agent did not become healthy"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	h := NewHandler(store, &fakeDispatcher{})
	body := getResponse(t, h, hostID)
	caps, ok := body["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("no capabilities in response: %v", body)
	}
	access, ok := caps["access"].(map[string]any)
	if !ok {
		t.Fatalf("no access in capabilities: %v", caps)
	}
	if access["state"] != "restored" || access["request_id"] != "req-get-1" {
		t.Fatalf("access = %v, want the stored report passed through", access)
	}
}

// A PATCH that changes `enabled` on a host whose access is `applying` is
// refused with 409, and does not touch the stored config.
func TestPatchRefusedWhileApplying(t *testing.T) {
	pool := testPool(t)
	hostID := seedTestHost(t, pool, "console-test-patch-applying")
	store := NewStore(pool)
	ctx := context.Background()
	if err := store.UpsertCapabilities(ctx, hostID, Capabilities{
		Connectors: []string{}, InputDevices: []InputDevicePath{},
		Access: &Access{State: "applying", Target: boolPtr(true), RequestID: strPtr("req-applying"), Summary: "replacing the agent"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	h := NewHandler(store, &fakeDispatcher{})
	rec := httptest.NewRecorder()
	h.handlePatch(rec, patchRequest(t, hostID, map[string]any{"enabled": true}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("PATCH status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}

	_, enabled := rawConsoleConfig(t, pool, hostID)
	if enabled {
		t.Fatal("refused PATCH must not have written `enabled`")
	}
}

// A false→true PATCH is refused with 409 while access is `unsupported`; every
// other key still goes through, and a repeated true (no change) is accepted.
func TestPatchUnsupportedRefusesOnlyFalseToTrue(t *testing.T) {
	pool := testPool(t)
	hostID := seedTestHost(t, pool, "console-test-patch-unsupported")
	store := NewStore(pool)
	ctx := context.Background()
	if err := store.UpsertCapabilities(ctx, hostID, Capabilities{
		Connectors: []string{}, InputDevices: []InputDevicePath{},
		Access: &Access{State: "unsupported", Summary: "rootless engine: console access needs RH07-15"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	h := NewHandler(store, &fakeDispatcher{})

	// false -> true: refused.
	rec := httptest.NewRecorder()
	h.handlePatch(rec, patchRequest(t, hostID, map[string]any{"enabled": true}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("false->true status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}

	// Every other key still goes through while unsupported.
	rec = httptest.NewRecorder()
	h.handlePatch(rec, patchRequest(t, hostID, map[string]any{"auto_start_on_display": true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("unrelated key status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	// A repeated `enabled:true` PATCH while already true is a no-op change
	// and must not be refused (no false->true transition to gate).
	if err := store.Upsert(ctx, hostID, map[string]any{"enabled": true}, nil); err != nil {
		t.Fatalf("seed enabled=true: %v", err)
	}
	rec = httptest.NewRecorder()
	h.handlePatch(rec, patchRequest(t, hostID, map[string]any{"enabled": true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("repeated true status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// An accepted PATCH that changes `enabled` on a host reporting access sets
// the PATCH-pending placement hold; one that changes nothing else does not.
func TestPatchSetsPlacementHoldOnlyWhenEnabledChanges(t *testing.T) {
	pool := testPool(t)
	hostID := seedTestHost(t, pool, "console-test-patch-hold")
	store := NewStore(pool)
	ctx := context.Background()
	if err := store.UpsertCapabilities(ctx, hostID, Capabilities{
		Connectors: []string{}, InputDevices: []InputDevicePath{},
		Access: &Access{State: "on", Target: boolPtr(true), RequestID: strPtr("req-hold"), Summary: "on"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	h := NewHandler(store, &fakeDispatcher{})

	// A PATCH that changes nothing about `enabled` sets no hold.
	rec := httptest.NewRecorder()
	h.handlePatch(rec, patchRequest(t, hostID, map[string]any{"auto_start_on_display": true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if placementHoldPending(t, pool, hostID) {
		t.Fatal("a PATCH that did not change `enabled` must not set the hold")
	}

	// A PATCH that changes `enabled` sets the hold.
	rec = httptest.NewRecorder()
	h.handlePatch(rec, patchRequest(t, hostID, map[string]any{"enabled": true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !placementHoldPending(t, pool, hostID) {
		t.Fatal("a PATCH that changed `enabled` must set the placement hold")
	}
}

// A host with no access report is unaffected by any of amendment 18's rules:
// no 409, no hold, exactly the pre-amendment behaviour.
func TestPatchHostWithNoAccessReportIsUnaffected(t *testing.T) {
	pool := testPool(t)
	hostID := seedTestHost(t, pool, "console-test-patch-noaccess")
	store := NewStore(pool)

	h := NewHandler(store, &fakeDispatcher{})
	rec := httptest.NewRecorder()
	h.handlePatch(rec, patchRequest(t, hostID, map[string]any{"enabled": true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if placementHoldPending(t, pool, hostID) {
		t.Fatal("a host with no access report must never be held")
	}
}
