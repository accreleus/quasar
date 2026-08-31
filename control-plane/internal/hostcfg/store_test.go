package hostcfg

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	if err := migrate.Run(migrations.FS, dbURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM host_settings; DELETE FROM hosts;`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedHost(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO hosts (node_name, status) VALUES ('h1', 'online') RETURNING id::text`).Scan(&id)
	if err != nil {
		t.Fatalf("seed host: %v", err)
	}
	return id
}

func TestStoreGetDefaultsEmpty(t *testing.T) {
	pool := testPool(t)
	s := NewStore(pool)
	hostID := seedHost(t, pool)
	got, err := s.Get(context.Background(), hostID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("fresh host overrides = %v, want empty", got)
	}
}

func TestStoreUpsertRoundTrip(t *testing.T) {
	pool := testPool(t)
	s := NewStore(pool)
	hostID := seedHost(t, pool)
	in := map[string]any{"gop": float64(90), "encoder": "va"}
	if err := s.Upsert(context.Background(), hostID, in, nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.Get(context.Background(), hostID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got["gop"] != float64(90) || got["encoder"] != "va" {
		t.Errorf("round-trip = %v", got)
	}
}

// TestStoreGetEffectiveNilBeforeReport verifies the host-observability
// contract: a host that has never had an agent report effective_settings
// returns nil, not an error or empty map (control-api.md GET .../settings
// "effective" is null when never reported).
func TestStoreGetEffectiveNilBeforeReport(t *testing.T) {
	pool := testPool(t)
	s := NewStore(pool)
	hostID := seedHost(t, pool)
	got, err := s.GetEffective(context.Background(), hostID)
	if err != nil {
		t.Fatalf("get effective: %v", err)
	}
	if got != nil {
		t.Errorf("effective before any report = %v, want nil", got)
	}
}

// TestStoreGetEffectiveReturnsReportedValue verifies a populated
// hosts.effective_settings JSONB round-trips through GetEffective.
func TestStoreGetEffectiveReturnsReportedValue(t *testing.T) {
	pool := testPool(t)
	s := NewStore(pool)
	hostID := seedHost(t, pool)
	if _, err := pool.Exec(context.Background(),
		`UPDATE hosts SET effective_settings = $2 WHERE id::text = $1`,
		hostID, `{"encoder":"va","render_node":"/dev/dri/renderD128"}`); err != nil {
		t.Fatalf("seed effective_settings: %v", err)
	}
	got, err := s.GetEffective(context.Background(), hostID)
	if err != nil {
		t.Fatalf("get effective: %v", err)
	}
	if got["encoder"] != "va" || got["render_node"] != "/dev/dri/renderD128" {
		t.Errorf("effective = %v, want the seeded map", got)
	}
}

// TestStoreGetCodecsNilBeforeReport pins the wizard-v2 §S5 contract's
// load-bearing half: a host whose agent never reported codecs reads back nil
// ("never reported"), NOT the ["h264"] floor session.Store.HostCodecs applies.
// The launch resolver may collapse the two; an operator surface may not, or it
// asserts an H.264-only host the control plane knows nothing about.
func TestStoreGetCodecsNilBeforeReport(t *testing.T) {
	pool := testPool(t)
	s := NewStore(pool)
	hostID := seedHost(t, pool)
	got, err := s.GetCodecs(context.Background(), hostID)
	if err != nil {
		t.Fatalf("get codecs: %v", err)
	}
	if got != nil {
		t.Errorf("codecs before any report = %v, want nil", got)
	}
}

// TestStoreGetCodecsReturnsReportedSet verifies a populated hosts.codecs JSONB
// round-trips verbatim (order preserved, not re-sorted or floor-injected).
func TestStoreGetCodecsReturnsReportedSet(t *testing.T) {
	pool := testPool(t)
	s := NewStore(pool)
	hostID := seedHost(t, pool)
	if _, err := pool.Exec(context.Background(),
		`UPDATE hosts SET codecs = $2 WHERE id::text = $1`,
		hostID, `["h264","av1"]`); err != nil {
		t.Fatalf("seed codecs: %v", err)
	}
	got, err := s.GetCodecs(context.Background(), hostID)
	if err != nil {
		t.Fatalf("get codecs: %v", err)
	}
	if len(got) != 2 || got[0] != "h264" || got[1] != "av1" {
		t.Errorf("codecs = %v, want [h264 av1] verbatim", got)
	}
}

// TestStoreGetCodecsEmptyArrayIsNil — an explicit `[]` is indistinguishable
// from "nothing to say" for the operator surface, so it degrades to the same
// "not reported" answer rather than rendering a host with zero codecs.
func TestStoreGetCodecsEmptyArrayIsNil(t *testing.T) {
	pool := testPool(t)
	s := NewStore(pool)
	hostID := seedHost(t, pool)
	if _, err := pool.Exec(context.Background(),
		`UPDATE hosts SET codecs = '[]'::jsonb WHERE id::text = $1`, hostID); err != nil {
		t.Fatalf("seed codecs: %v", err)
	}
	got, err := s.GetCodecs(context.Background(), hostID)
	if err != nil {
		t.Fatalf("get codecs: %v", err)
	}
	if got != nil {
		t.Errorf("empty codecs array = %v, want nil", got)
	}
}

// TestStoreGetCodecsUnknownHostIsNil — no host row is also "never reported"
// (the GET handler 404s on unknown ids elsewhere; this must not error out).
func TestStoreGetCodecsUnknownHostIsNil(t *testing.T) {
	pool := testPool(t)
	s := NewStore(pool)
	got, err := s.GetCodecs(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("get codecs: %v", err)
	}
	if got != nil {
		t.Errorf("codecs for unknown host = %v, want nil", got)
	}
}

// TestStoreHostStatus verifies HostStatus reports found=false for an unknown
// id and the real status column for a seeded host (host-observability-2,
// backs the restart endpoint's 404/409-offline checks).
func TestStoreHostStatus(t *testing.T) {
	pool := testPool(t)
	s := NewStore(pool)
	hostID := seedHost(t, pool)

	status, found, err := s.HostStatus(context.Background(), hostID)
	if err != nil {
		t.Fatalf("host status: %v", err)
	}
	if !found || status != "online" {
		t.Errorf("status/found = %q/%v, want online/true", status, found)
	}

	_, found, err = s.HostStatus(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("host status (unknown): %v", err)
	}
	if found {
		t.Error("found = true for a nonexistent host id")
	}
}

// TestStorePendingRestartRoundTrip verifies SetPendingRestart/GetPendingRestart
// round-trip and default to false for a fresh host.
func TestStorePendingRestartRoundTrip(t *testing.T) {
	pool := testPool(t)
	s := NewStore(pool)
	hostID := seedHost(t, pool)

	got, err := s.GetPendingRestart(context.Background(), hostID)
	if err != nil {
		t.Fatalf("get pending_restart: %v", err)
	}
	if got {
		t.Error("pending_restart on a fresh host = true, want false")
	}

	if err := s.SetPendingRestart(context.Background(), hostID, true); err != nil {
		t.Fatalf("set pending_restart: %v", err)
	}
	got, err = s.GetPendingRestart(context.Background(), hostID)
	if err != nil {
		t.Fatalf("get pending_restart after set: %v", err)
	}
	if !got {
		t.Error("pending_restart after SetPendingRestart(true) = false, want true")
	}

	if err := s.SetPendingRestart(context.Background(), hostID, false); err != nil {
		t.Fatalf("clear pending_restart: %v", err)
	}
	got, err = s.GetPendingRestart(context.Background(), hostID)
	if err != nil {
		t.Fatalf("get pending_restart after clear: %v", err)
	}
	if got {
		t.Error("pending_restart after SetPendingRestart(false) = true, want false")
	}
}

func TestStoreCascadeOnHostDelete(t *testing.T) {
	pool := testPool(t)
	s := NewStore(pool)
	hostID := seedHost(t, pool)
	if err := s.Upsert(context.Background(), hostID, map[string]any{"gop": float64(90)}, nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM hosts WHERE id::text = $1`, hostID); err != nil {
		t.Fatalf("delete host: %v", err)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM host_settings`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("host_settings rows after host delete = %d, want 0 (cascade)", n)
	}
}
