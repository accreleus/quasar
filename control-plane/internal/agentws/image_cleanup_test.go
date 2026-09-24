package agentws

import "testing"

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
