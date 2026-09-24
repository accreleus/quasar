package agentws

import (
	"encoding/json"
	"testing"
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
	if r.updateImageVersions(c, ImageVersionsStateMsg{InventoryRevision: "1", ImageVersionsComplete: true}) {
		t.Fatal("missing versions array declared complete")
	}
	if r.updateImageVersions(c, ImageVersionsStateMsg{InventoryRevision: "1", ImageVersionsComplete: true, ImageVersions: []ImageVersionEntry{bad, bad}}) {
		t.Fatal("duplicate exact identity declared complete")
	}
	bad.RuntimeImageID = ""
	if r.updateImageVersions(c, ImageVersionsStateMsg{InventoryRevision: "1", ImageVersionsComplete: true, ImageVersions: []ImageVersionEntry{bad}}) {
		t.Fatal("missing runtime daemon ID declared complete")
	}
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
