package session

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// TestDerivedTileLaunchNeedsTheParentsEntitlement — a derived tile is launchable
// only by a user entitled to both the tile and its parent provider app (#497).
// Checked over POST /v1/sessions (IsEntitled, the pre-check) and directly on
// ScheduleAndCreate (the FOR SHARE boundary), which a caller can reach without
// the pre-check.
func TestDerivedTileLaunchNeedsTheParentsEntitlement(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	f := newEntLaunchFixture(t, pool)
	store := NewStore(pool)

	var parent, tile string
	must(t, pool.QueryRow(ctx, `INSERT INTO apps
		(name, library_provider, default_vram_mb, default_encode_slots,
		 default_width, default_height, default_fps, default_bitrate_kbps)
		VALUES ('Steam', 'steam', 1024, 1, 1280, 720, 60, 6000)
		RETURNING id::text`).Scan(&parent))
	must(t, pool.QueryRow(ctx, `INSERT INTO apps
		(name, kind, parent_app_id, external_source, external_id, origin)
		VALUES ('Redout', 'game', $1::uuid, 'steam', '517710', 'discovered')
		RETURNING id::text`, parent).Scan(&tile))
	must(t, execEnt(ctx, pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by, source_ref)
		VALUES ('user', $1::uuid, $2::uuid, 'provider', 'library:steam:517710')`, f.userID, tile))

	direct := func() error {
		_, err := store.ScheduleAndCreate(ctx, CreateParams{
			UserID: f.userID, AppID: tile,
			Width: 1280, Height: 720, FPS: 60, BitrateKbps: 6000,
			H264Profile: "constrained-baseline", NeedEncodeSlots: 1,
			TokenExpires: time.Now().Add(time.Minute),
		})
		return err
	}

	// The tile's own grant, and nothing on the parent: refused at both gates.
	if status, code := launchStatus(t, f.base, f.userTok, tile); status != http.StatusForbidden {
		t.Errorf("tile launch with no parent entitlement: got %d (%s), want 403", status, code)
	}
	if err := direct(); !errors.Is(err, ErrNotEntitled) {
		t.Errorf("ScheduleAndCreate with no parent entitlement: got %v, want ErrNotEntitled", err)
	}

	// Restored on the parent: neither gate refuses on entitlement.
	must(t, execEnt(ctx, pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('user', $1::uuid, $2::uuid, 'admin')`, f.userID, parent))
	if status, code := launchStatus(t, f.base, f.userTok, tile); status == http.StatusForbidden {
		t.Errorf("tile launch with the parent restored: got 403 (%s)", code)
	}
	if err := direct(); errors.Is(err, ErrNotEntitled) {
		t.Errorf("ScheduleAndCreate with the parent restored: got %v", err)
	}
}
