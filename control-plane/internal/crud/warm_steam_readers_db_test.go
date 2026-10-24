package crud

// Amendment 39 (#759): the user-facing readers in this package leave a parked
// session (sessions.kind = 'parked') out. control-api.md §Warm Steam, "What
// leaves a parked session out". DB-backed: run via `make test-db`.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedParkedSession inserts a parked row directly. Parked rows carry no host or
// GPU here: none of the readers under test joins them.
func seedParkedSession(t *testing.T, pool *pgxpool.Pool, userID, appID, state string, started, ended *time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO sessions (user_id, app_id, state, kind, started_at, ended_at,
			width, height, fps, bitrate_kbps)
		VALUES ($1::uuid, $2::uuid, $3, 'parked', $4, $5, 1280, 720, 60, 6000)`,
		userID, appID, state, started, ended); err != nil {
		t.Fatalf("seed parked session (%s): %v", state, err)
	}
}

func railItemFor(t *testing.T, items []map[string]any, appID string) map[string]any {
	t.Helper()
	for _, it := range items {
		if it["app_id"] == appID {
			return it
		}
	}
	t.Fatalf("app %s is not on the rail: %v", appID, items)
	return nil
}

// A parked row feeds none of the rail's session-derived reasons. The parked app
// still sits on the rail as a plain entitled app (recently_added), with nothing
// session-derived on it.
func TestHighlightsLeaveParkedSessionOut(t *testing.T) {
	f := newHighlightsFixture(t)
	now := time.Now()
	h := func(d time.Duration) *time.Time { return ptime(now.Add(d)) }

	// The Steam shape: one app with a finished play session and a live parked one.
	steam := f.seedApp(t, "steam-parent", now.Add(-72*time.Hour))
	f.seedSession(t, steam, "stopped", now.Add(-2*time.Hour), h(-2*time.Hour), h(-time.Hour))
	seedParkedSession(t, f.pool, f.userID, steam, "running", h(-30*time.Minute), nil)

	// An app whose only history is a finished parked session.
	parkedOnly := f.seedApp(t, "parked-only", now.Add(-72*time.Hour))
	seedParkedSession(t, f.pool, f.userID, parkedOnly, "stopped", h(-5*time.Hour), h(-4*time.Hour))

	status, items := f.highlights(t, "")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}

	got := railItemFor(t, items, steam)
	if got["reason"] != reasonMostPlayed {
		t.Errorf("steam reason = %v, want %q (the parked row must not make it live)", got["reason"], reasonMostPlayed)
	}
	if got["session_id"] != nil {
		t.Errorf("steam session_id = %v, want null", got["session_id"])
	}
	if secs, _ := got["play_seconds"].(float64); secs < 3599 || secs > 3601 {
		t.Errorf("steam play_seconds = %v, want 3600 from the play session alone", got["play_seconds"])
	}

	got = railItemFor(t, items, parkedOnly)
	if got["reason"] != reasonRecentlyAdded {
		t.Errorf("parked-only reason = %v, want %q", got["reason"], reasonRecentlyAdded)
	}
	if got["play_seconds"] != float64(0) || got["last_played_at"] != nil || got["session_id"] != nil {
		t.Errorf("parked-only carries session-derived data: %v", got)
	}
}

func TestPlayHistoryLeavesParkedSessionOut(t *testing.T) {
	f := newTwoUsers(t)
	now := time.Now()
	h := func(d time.Duration) *time.Time { return ptime(now.Add(d)) }

	steam := f.seedApp(t, "steam-parent", now.Add(-time.Hour))
	f.sessionFor(t, f.userID, steam, "stopped", h(-3*time.Hour), h(-2*time.Hour)) // 3600 s
	seedParkedSession(t, f.pool, f.userID, steam, "running", h(-10*time.Minute), nil)

	parkedOnly := f.seedApp(t, "parked-only", now.Add(-time.Hour))
	seedParkedSession(t, f.pool, f.userID, parkedOnly, "stopped", h(-5*time.Hour), h(-4*time.Hour))

	items := f.playHistory(t, f.token)
	if len(items) != 1 || items[0]["app_id"] != steam {
		t.Fatalf("items %v, want only the app with a play session", items)
	}
	if items[0]["sessions"] != float64(1) {
		t.Errorf("sessions = %v, want 1 (the parked row is not a session of play)", items[0]["sessions"])
	}
	if total, _ := items[0]["total_seconds"].(float64); total < 3599 || total > 3601 {
		t.Errorf("total_seconds = %v, want 3600 from the play session alone", items[0]["total_seconds"])
	}
}

func TestAdminAppsSessions30dCountsPlayOnly(t *testing.T) {
	pool := testPool(t)
	s := &store{pool: pool}
	ctx := context.Background()
	ids := seedBasic(t, pool)

	insertSession(t, pool, ids, "running")
	insertSession(t, pool, ids, "stopped")
	now := time.Now()
	seedParkedSession(t, pool, ids.userID, ids.appID, "running", ptime(now.Add(-time.Minute)), nil)
	seedParkedSession(t, pool, ids.userID, ids.appID, "stopped", ptime(now.Add(-time.Hour)), ptime(now.Add(-30*time.Minute)))

	apps, _, err := s.listAllApps(ctx, ids.userID, "", 50)
	if err != nil {
		t.Fatalf("listAllApps: %v", err)
	}
	for _, a := range apps {
		if a.ID == ids.appID {
			if a.Sessions30d != 2 {
				t.Errorf("sessions_30d = %d, want 2 (two play rows, two parked rows left out)", a.Sessions30d)
			}
			return
		}
	}
	t.Fatalf("app %s missing from the admin list", ids.appID)
}
