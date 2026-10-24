package devices

// Amendment 39 (#759): a parked session (sessions.kind = 'parked') never
// appears as a device's active_session_id, but revoking the device still
// collects it so the handler stops it. control-api.md §Warm Steam.
// DB-backed: run via `make test-db`.

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type warmFixture struct {
	pool          *pgxpool.Pool
	store         *Store
	userID, appID string
	deviceID      string
}

func newWarmFixture(t *testing.T) warmFixture {
	t.Helper()
	pool := testDB(t)
	ctx := context.Background()
	f := warmFixture{pool: pool, store: NewStore(pool)}
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('warm@test.local','warmuser','x') RETURNING id::text`).Scan(&f.userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height, default_fps, default_bitrate_kbps)
		VALUES ('steam-parent', 1024, 1, 1280, 720, 60, 6000) RETURNING id::text`).Scan(&f.appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	dev, err := f.store.Upsert(ctx, UpsertParams{
		UserID: f.userID, DeviceKey: "key-warm", UserAgent: "ua", Capabilities: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("seed device: %v", err)
	}
	f.deviceID = dev.ID
	return f
}

// session inserts a session launched from the fixture device; createdAt orders
// the active_session_id pick.
func (f warmFixture) session(t *testing.T, kind, state string, createdAt time.Time) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), `
		INSERT INTO sessions (user_id, app_id, device_id, kind, state, created_at,
			width, height, fps, bitrate_kbps)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, 1280, 720, 60, 6000)
		RETURNING id::text`,
		f.userID, f.appID, f.deviceID, kind, state, createdAt).Scan(&id); err != nil {
		t.Fatalf("seed %s session (%s): %v", kind, state, err)
	}
	return id
}

func (f warmFixture) activeSessionIDs(t *testing.T) (list, one *string) {
	t.Helper()
	ctx := context.Background()
	items, err := f.store.List(ctx, f.userID, "")
	if err != nil || len(items) != 1 {
		t.Fatalf("List: %d items, err %v", len(items), err)
	}
	got, err := f.store.getOne(ctx, f.userID, f.deviceID, "")
	if err != nil {
		t.Fatalf("getOne: %v", err)
	}
	return items[0].ActiveSessionID, got.ActiveSessionID
}

func derefOrNil(s *string) string {
	if s == nil {
		return "<null>"
	}
	return *s
}

// The parked row is the newer one, so an unfiltered subquery would pick it.
func TestActiveSessionIDSkipsParkedSession(t *testing.T) {
	f := newWarmFixture(t)
	now := time.Now()
	play := f.session(t, "play", "running", now.Add(-time.Hour))
	f.session(t, "parked", "running", now.Add(-time.Minute))

	list, one := f.activeSessionIDs(t)
	if derefOrNil(list) != play || derefOrNil(one) != play {
		t.Errorf("active_session_id list=%s single=%s, want the play session %s",
			derefOrNil(list), derefOrNil(one), play)
	}

	if _, err := f.pool.Exec(context.Background(),
		`UPDATE sessions SET state = 'stopped' WHERE id = $1::uuid`, play); err != nil {
		t.Fatalf("stop play session: %v", err)
	}
	list, one = f.activeSessionIDs(t)
	if list != nil || one != nil {
		t.Errorf("active_session_id list=%s single=%s, want null with only a parked session left",
			derefOrNil(list), derefOrNil(one))
	}
}

func TestRevokeStillCollectsParkedSession(t *testing.T) {
	f := newWarmFixture(t)
	now := time.Now()
	parked := f.session(t, "parked", "running", now.Add(-time.Minute))
	play := f.session(t, "play", "starting", now.Add(-time.Hour))
	f.session(t, "play", "stopped", now.Add(-2*time.Hour))

	ids, err := f.store.Revoke(context.Background(), f.userID, f.deviceID)
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	want := []string{parked, play}
	sort.Strings(want)
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != want[0] || ids[1] != want[1] {
		t.Errorf("Revoke returned %v, want the live parked and play sessions %v", ids, want)
	}
}
