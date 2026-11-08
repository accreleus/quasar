package session

// launch_profile_delete_audit_db_test.go — #1183 / #1209: deleting a launch
// profile records every app whose menu the delete widens (an emptied allow-list
// is unrestricted again). That list grows with the catalogue, and its entries
// are admin-supplied names, so neither the count nor a raw byte count can bound
// the row: admin_activity's CHECK measures the ENCODED details, where a quote
// costs two bytes and a control character six. The row must be written, cut
// down and flagged, never dropped. DB-gated like the rest (make test-db).

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
		minKept int
		appName func(i int) string
	}{
		// The count cap from erratum 25, then the encoded-size step: 40 of these
		// render to about 4 KiB of escaped quotes, so the list is halved to fit.
		{"forty-five quote-heavy names", 45, 1, func(i int) string {
			return fmt.Sprintf("%s-%02d", strings.Repeat(`"`, 50), i)
		}},
		// Three names of 2001 control characters each: 12 KiB apiece once encoded,
		// which used to drop the whole row. The last entry is dropped WHOLE rather
		// than cut — half a name is a different name — so nothing of the list
		// survives; the count and the flag are what is left.
		{"names too long for the row", 3, 0, func(i int) string {
			return strings.Repeat("\x01", 2001) + fmt.Sprint(i)
		}},
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

			var (
				kept   int
				size   int
				list   bool
				count  int
				rowCut bool
			)
			if err := pool.QueryRow(ctx, `SELECT jsonb_array_length(details->'allow_list_widened_apps'),
				octet_length(details::text),
				COALESCE((details->>'allow_list_widened_apps_truncated')::bool, false),
				(details->>'allow_list_widened_app_count')::int,
				COALESCE((details->>'details_truncated')::bool, false)
				FROM admin_activity WHERE action = 'launch_profile.delete' AND target_id = $1`,
				profileID).Scan(&kept, &size, &list, &count, &rowCut); err != nil {
				t.Fatalf("no launch_profile.delete audit row: %v", err)
			}
			if !list || !rowCut {
				t.Errorf("truncated flags = list:%v row:%v, want both true", list, rowCut)
			}
			if size > 4096 {
				t.Errorf("row is %d bytes, over the 4096-byte CHECK", size)
			}
			if kept < tc.minKept || kept >= tc.apps {
				t.Errorf("kept %d of %d names, want at least %d and fewer than all", kept, tc.apps, tc.minKept)
			}
			if count != tc.apps {
				t.Errorf("allow_list_widened_app_count = %d, want the true %d", count, tc.apps)
			}
		})
	}
}
