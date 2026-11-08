package session

// launch_profile_delete_audit_db_test.go — #1183: deleting a launch profile
// records every app whose menu the delete widens (an emptied allow-list is
// unrestricted again). That list grows with the catalogue, and past
// admin_activity's 4096-byte bound boundedDetails refused the whole delete row,
// which TryRecord only logged. The row must be written, cut to 40 names and to
// 2 KiB of them, with the true count beside it. DB-gated like the rest
// (make test-db).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
)

func TestLaunchProfileDeleteAuditsATruncatedWidenedAllowList(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	NewHandler(coord, store, audit.NewStore(pool)).Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	tok := captureAdminToken(t, pool, authSvc)

	for _, tc := range []struct {
		name    string
		apps    int
		appName func(i int) string
		want    int
	}{
		// The count cap, from erratum 25: 45 apps, 40 kept.
		{"forty-five apps", 45, func(i int) string { return fmt.Sprintf("Widened app %02d", i) }, 40},
		// The byte budget. Three names this long render to about 6 KiB, which used
		// to drop the whole row; one of them fits, cut to that one, and the row is
		// still written.
		{"names too long for the row", 3, func(i int) string { return strings.Repeat("n", 2000) + fmt.Sprint(i) }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A chain of this test's own: the seeded ones are shared with every other
			// test in the package (launch_profiles is not reset between them) and must
			// not be deleted here. The rung is a seeded one, which this test only reads.
			const rung = "1080p60-h264"
			profileID := "audit-widen-" + fmt.Sprint(tc.apps)
			must(t, exec(t, pool, `INSERT INTO launch_profiles (id, display_name, description, visibility, sort_order)
				VALUES ($1, $1, '', 'user', 5)`, profileID))
			t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM launch_profiles WHERE id = $1`, profileID) })
			must(t, exec(t, pool, `INSERT INTO launch_profile_rungs (launch_profile_id, stream_profile_id, position)
				VALUES ($1, $2, 1)`, profileID, rung))

			for i := 0; i < tc.apps; i++ {
				appID := insertApp(t, pool, tc.appName(i), 512, 1)
				allowLaunchProfiles(t, pool, appID, profileID)
			}

			resp := doJSON(t, http.MethodDelete, srv.URL+"/v1/admin/launch-profiles/"+profileID, tok, nil)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("DELETE: %d, want 204", resp.StatusCode)
			}

			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_activity
				WHERE action = 'launch_profile.delete' AND target_id = $1
				AND jsonb_array_length(details->'allow_list_widened_apps') = $2
				AND details->>'allow_list_widened_apps_truncated' = 'true'
				AND (details->>'allow_list_widened_app_count')::int = $3`,
				profileID, tc.want, tc.apps).Scan(&n); err != nil {
				t.Fatalf("read the delete audit row: %v", err)
			}
			if n != 1 {
				t.Errorf("%d launch_profile.delete rows with %d of %d names and the true count, want 1",
					n, tc.want, tc.apps)
			}
		})
	}
}
