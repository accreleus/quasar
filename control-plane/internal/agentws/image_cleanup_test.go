package agentws

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestImageInventoryReconcileSendsEmptyIdentityArray(t *testing.T) {
	r := NewRegistry(nil)
	c := newConn("host", nil)
	c.imageCleanupV1 = true
	r.add(c)
	if err := r.SendImageInventoryReconcile("host", ImageInventoryReconcileCmd{ID: "empty-managed-set"}); err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(<-c.out, &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire["identities"]) != "[]" {
		t.Fatalf("reconcile identities on wire = %s, want []", wire["identities"])
	}
}

func TestImageCleanupInventoryUsesOnlyCurrentAuthenticatedConnection(t *testing.T) {
	r := NewRegistry(nil)
	old := newConn("host", nil)
	old.connectionIncarnation = "old"
	old.imageCleanupV1 = true
	r.add(old)
	entry := ImageVersionEntry{ImageID: "steam", Version: "v1", ImageRef: "ref", RuntimeImageID: "sha256:one", State: "present"}
	if !r.updateImageVersions(old, ImageVersionsStateMsg{InventoryRevision: "1", ImageVersionsComplete: true, ImageVersions: []ImageVersionEntry{entry}}) {
		t.Fatal("first complete snapshot rejected")
	}
	if r.updateImageVersions(old, ImageVersionsStateMsg{InventoryRevision: "1", ImageVersionsComplete: false}) {
		t.Fatal("duplicate revision accepted")
	}
	snap, ok := r.ImageCleanupSnapshot("host")
	if !ok || !snap.Complete || len(snap.Versions) != 1 {
		t.Fatalf("current snapshot = %+v %v", snap, ok)
	}
	fresh := newConn("host", nil)
	fresh.connectionIncarnation = "new"
	fresh.imageCleanupV1 = true
	r.add(fresh)
	if r.updateImageVersions(old, ImageVersionsStateMsg{InventoryRevision: "2", ImageVersionsComplete: true, ImageVersions: []ImageVersionEntry{entry}}) {
		t.Fatal("displaced epoch changed inventory")
	}
	snap, ok = r.ImageCleanupSnapshot("host")
	if !ok || snap.ConnectionID != "new" || snap.Complete || len(snap.Versions) != 0 {
		t.Fatalf("reconnect retained stale cache: %+v %v", snap, ok)
	}
	if !r.updateImageVersions(fresh, ImageVersionsStateMsg{InventoryRevision: "1", ImageVersionsComplete: false, ImageVersions: []ImageVersionEntry{entry}}) {
		t.Fatal("incomplete snapshot rejected")
	}
	if !r.updateImageVersions(fresh, ImageVersionsStateMsg{InventoryRevision: "2", ImageVersionsComplete: true, ImageVersions: []ImageVersionEntry{entry}}) {
		t.Fatal("reconciled snapshot rejected")
	}
	r.removeWithLifecycle(fresh, func() {})
	if _, ok := r.ImageCleanupSnapshot("host"); ok {
		t.Fatal("offline host retained inventory")
	}
}

func TestImageCleanupRejectsMalformedCompleteInventory(t *testing.T) {
	r := NewRegistry(nil)
	c := newConn("host", nil)
	c.imageCleanupV1 = true
	r.add(c)
	bad := ImageVersionEntry{ImageID: "steam", Version: "v1", ImageRef: "ref", RuntimeImageID: "sha256:one", State: "present"}
	if !r.updateImageVersions(c, ImageVersionsStateMsg{InventoryRevision: "1", ImageVersionsComplete: true}) {
		t.Fatal("missing versions array did not revoke completeness")
	}
	if r.updateImageVersions(c, ImageVersionsStateMsg{InventoryRevision: "1", ImageVersionsComplete: true, ImageVersions: []ImageVersionEntry{bad, bad}}) {
		t.Fatal("stale duplicate exact identity changed inventory")
	}
	bad.RuntimeImageID = ""
	if !r.updateImageVersions(c, ImageVersionsStateMsg{InventoryRevision: "2", ImageVersionsComplete: true, ImageVersions: []ImageVersionEntry{bad}}) {
		t.Fatal("new malformed runtime daemon ID did not revoke completeness")
	}
	if got, _ := r.ImageCleanupSnapshot("host"); got.Complete {
		t.Fatalf("malformed inventory remained current: %+v", got)
	}
}

func TestImageCleanupReferenceEvidenceRequiresBooleanOnlyOnPresent(t *testing.T) {
	r := NewRegistry(nil)
	c := newConn("host", nil)
	c.imageCleanupV1 = true
	r.add(c)
	falseValue, trueValue := false, true
	entry := ImageVersionEntry{ImageID: "steam", Version: "v1", ImageRef: "ref", RuntimeImageID: "sha256:one",
		State: "present", ContainerReferenced: &falseValue}
	if !r.updateImageVersions(c, ImageVersionsStateMsg{InventoryRevision: "1", ImageVersionsComplete: true,
		ImageVersions: []ImageVersionEntry{entry}}) {
		t.Fatal("complete unreferenced snapshot rejected")
	}
	entry.State, entry.ContainerReferenced = "absent", &trueValue
	if !r.updateImageVersions(c, ImageVersionsStateMsg{InventoryRevision: "2", ImageVersionsComplete: true,
		ImageVersions: []ImageVersionEntry{entry}}) {
		t.Fatal("malformed newer report did not revoke current inventory")
	}
	if got, _ := r.ImageCleanupSnapshot("host"); got.Complete || len(got.Versions) != 0 {
		t.Fatalf("non-present reference field left inventory current: %+v", got)
	}
	entry.State, entry.ContainerReferenced = "present", &trueValue
	if !r.updateImageVersions(c, ImageVersionsStateMsg{InventoryRevision: "3", ImageVersionsComplete: true,
		ImageVersions: []ImageVersionEntry{entry}}) {
		t.Fatal("complete referenced snapshot rejected")
	}
	if got, _ := r.ImageCleanupSnapshot("host"); !got.Complete || got.Versions[0].ContainerReferenced == nil || !*got.Versions[0].ContainerReferenced {
		t.Fatalf("true reference evidence lost: %+v", got)
	}
	if r.invalidateImageVersions(c, "2") {
		t.Fatal("stale malformed report revoked newer inventory")
	}
	if !r.invalidateImageVersions(c, "4") {
		t.Fatal("new malformed report did not revoke inventory")
	}
	if got, _ := r.ImageCleanupSnapshot("host"); got.Complete {
		t.Fatalf("new malformed report retained complete inventory: %+v", got)
	}
}

func TestImageVersionEntryRejectsNullContainerReference(t *testing.T) {
	var entry ImageVersionEntry
	err := json.Unmarshal([]byte(`{"image_id":"steam","version":"v1","image_ref":"ref","runtime_image_id":"sha256:one","state":"present","container_referenced":null}`), &entry)
	if err == nil {
		t.Fatal("explicit null container reference was accepted as omitted evidence")
	}
}

func TestRegisterKeepsAuthenticationWhenCleanupInventoryIsMalformed(t *testing.T) {
	var reg RegisterMsg
	err := json.Unmarshal([]byte(`{"type":"register","node_name":"host","agent_version":"test","auth":{"enrollment_token":"test-token"},"image_cleanup_v1":true,"image_versions_complete":true,"image_versions":[{"image_id":"steam","version":"v1","image_ref":"ref","runtime_image_id":"sha256:one","state":"present","container_referenced":null}]}`), &reg)
	if err != nil {
		t.Fatalf("malformed cleanup inventory rejected the whole registration: %v", err)
	}
	if reg.Type != "register" || reg.NodeName != "host" || len(reg.Auth) == 0 || !reg.ImageCleanupV1 {
		t.Fatalf("registration fields were lost: %+v", reg)
	}
}

func TestImageCleanupMalformedReferenceWireRevokesInventory(t *testing.T) {
	pool := testPool(t)
	h := NewHandler(pool, "test-token", slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil, nil, nil)
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	badConn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := badConn.WriteJSON(map[string]any{"type": "register", "node_name": "cleanup-bad-auth",
		"agent_version": "test", "auth": map[string]string{"enrollment_token": "wrong-token"},
		"image_cleanup_v1": true, "image_versions_complete": true,
		"image_versions": []map[string]any{{"image_id": "steam", "version": "v1", "image_ref": "ref",
			"runtime_image_id": "sha256:one", "state": "present", "container_referenced": nil}},
	}); err != nil {
		t.Fatal(err)
	}
	var refused ErrorMsg
	if err := badConn.ReadJSON(&refused); err != nil {
		t.Fatal(err)
	}
	_ = badConn.Close()
	if refused.Code != "auth_failed" {
		t.Fatalf("malformed cleanup inventory bypassed authentication: %+v", refused)
	}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteJSON(map[string]any{"type": "register", "node_name": "cleanup-malformed-reference",
		"agent_version": "test", "auth": map[string]string{"enrollment_token": "test-token"},
		"image_cleanup_v1": true, "image_versions_complete": true,
		"image_versions": []map[string]any{{"image_id": "steam", "version": "v1", "image_ref": "ref",
			"runtime_image_id": "sha256:one", "state": "present", "container_referenced": nil}},
	}); err != nil {
		t.Fatal(err)
	}
	var registered RegisteredMsg
	if err := conn.ReadJSON(&registered); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(map[string]any{"type": "capacity", "host": map[string]any{"cpu_cores": 8, "mem_mb": 32000}, "gpus": []any{}}); err != nil {
		t.Fatal(err)
	}
	waitComplete := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if got, ok := h.registry.ImageCleanupSnapshot(registered.HostID); ok && got.Complete == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		got, _ := h.registry.ImageCleanupSnapshot(registered.HostID)
		t.Fatalf("inventory complete=%v, want %v", got.Complete, want)
	}
	waitComplete(false)
	if err := conn.WriteJSON(map[string]any{"type": "image_versions_state", "inventory_revision": "1", "image_versions_complete": true,
		"image_versions": []map[string]any{{"image_id": "steam", "version": "v1", "image_ref": "ref",
			"runtime_image_id": "sha256:one", "state": "present", "container_referenced": false}},
	}); err != nil {
		t.Fatal(err)
	}
	waitComplete(true)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"image_versions_state","inventory_revision":"2","image_versions_complete":true,"image_versions":[{"image_id":"steam","version":"v1","image_ref":"ref","runtime_image_id":"sha256:one","state":"present","container_referenced":"false"}]}`)); err != nil {
		t.Fatal(err)
	}
	waitComplete(false)
	if err := conn.WriteJSON(map[string]any{"type": "image_versions_state", "inventory_revision": "3", "image_versions_complete": true,
		"image_versions": []map[string]any{{"image_id": "steam", "version": "v1", "image_ref": "ref",
			"runtime_image_id": "sha256:one", "state": "present", "container_referenced": false}},
	}); err != nil {
		t.Fatal(err)
	}
	waitComplete(true)
	if err := conn.WriteJSON(map[string]any{"type": "image_versions_state", "inventory_revision": "4", "image_versions_complete": true,
		"image_versions": []map[string]any{{"image_id": "steam", "version": "v1", "image_ref": "ref",
			"runtime_image_id": "sha256:one", "state": "absent", "container_referenced": true}},
	}); err != nil {
		t.Fatal(err)
	}
	waitComplete(false)
}

func TestImageReconcileAckMarksOnlyFollowingCurrentRevision(t *testing.T) {
	r := NewRegistry(nil)
	c := newConn("host", nil)
	c.connectionIncarnation = "current"
	c.imageCleanupV1 = true
	r.add(c)
	entry := ImageVersionEntry{ImageID: "steam", Version: "v1", ImageRef: "ref", RuntimeImageID: "sha256:one", State: "present"}
	update := func(rev string) {
		t.Helper()
		if !r.updateImageVersions(c, ImageVersionsStateMsg{InventoryRevision: rev, ImageVersionsComplete: true, ImageVersions: []ImageVersionEntry{entry}}) {
			t.Fatalf("revision %s rejected", rev)
		}
	}
	update("1")
	if err := r.SendImageInventoryReconcile("host", ImageInventoryReconcileCmd{ID: "lost-ack"}); err != nil {
		t.Fatal(err)
	}
	if err := r.SendImageInventoryReconcile("host", ImageInventoryReconcileCmd{ID: "current-request"}); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	pending := len(c.imageReconcilePending)
	c.mu.Unlock()
	if pending != 1 {
		t.Fatalf("repeated lost acks retained %d reconcile IDs, want one", pending)
	}
	r.resolveAckFromConn(c, "lost-ack", AckResult{OK: true})
	update("2")
	if got, _ := r.ImageCleanupSnapshot("host"); got.ReconciledRevision != 0 {
		t.Fatalf("superseded ack marked stale snapshot fresh: %+v", got)
	}
	r.resolveAckFromConn(c, "current-request", AckResult{OK: false})
	update("3")
	if got, _ := r.ImageCleanupSnapshot("host"); got.ReconciledRevision != 0 {
		t.Fatalf("refused reconcile marked snapshot fresh: %+v", got)
	}
	if err := r.SendImageInventoryReconcile("host", ImageInventoryReconcileCmd{ID: "accepted"}); err != nil {
		t.Fatal(err)
	}
	update("4") // a pre-ack snapshot cannot prove the scan completed
	r.resolveAckFromConn(c, "accepted", AckResult{OK: true})
	update("5")
	if got, _ := r.ImageCleanupSnapshot("host"); got.ReconciledRevision != 5 || got.ReconciledRequestID != "accepted" {
		t.Fatalf("ack-following revision was not marked reconciled: %+v", got)
	}
	fresh := newConn("host", nil)
	fresh.connectionIncarnation = "new"
	fresh.imageCleanupV1 = true
	r.add(fresh)
	if got, _ := r.ImageCleanupSnapshot("host"); got.ReconciledRevision != 0 {
		t.Fatalf("reconnect inherited reconciliation marker: %+v", got)
	}
}
