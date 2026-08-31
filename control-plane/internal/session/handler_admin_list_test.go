package session

// handler_admin_list_test.go — GET /v1/admin/sessions ?state= (UI v3 amendment,
// 2026-08-28). DB-gated like the rest of this package (make test-db).
//
// What is load-bearing here:
//   - `active` is the NON-TERMINAL set, which is one state wider than the
//     reservation-holding set the scheduler uses. Getting that wrong silently
//     drops queued sessions off an operator's "live now" view.
//   - an unrecognized value is a 400 with code `invalid_state`, never a silent
//     fall back to `all`: a filter that quietly stops filtering shows an
//     operator exactly the sessions they asked not to see.
//   - NO parameter is byte-for-byte the pre-amendment response. That is the
//     whole additive claim, so it is asserted, not assumed.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// adminSessionIDs GETs /v1/admin/sessions with the given query and returns the
// ids in response order.
func adminSessionIDs(t *testing.T, base, tok, query string) []string {
	t.Helper()
	resp := doJSON(t, "GET", base+"/v1/admin/sessions"+query, tok, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/admin/sessions%s: got %d, want 200", query, resp.StatusCode)
	}
	var body struct {
		Items []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode admin sessions: %v", err)
	}
	ids := make([]string, 0, len(body.Items))
	for _, it := range body.Items {
		ids = append(ids, it.ID)
	}
	return ids
}

// seedStateSpread inserts one running, one stopped and one failed session for
// the same user/app/host, returning their ids in that order.
func seedStateSpread(t *testing.T, pool *pgxpool.Pool) (running, stopped, failed string, s seedIDs) {
	t.Helper()
	s = seed(t, pool, 4)
	running = insertSessionRow(t, pool, s.userID, s.appID, &s.hostID, "running")
	stopped = insertSessionRow(t, pool, s.userID, s.appID, &s.hostID, "stopped")
	failed = insertSessionRow(t, pool, s.userID, s.appID, &s.hostID, "failed")
	return running, stopped, failed, s
}

func TestAdminSessionsStateFilter(t *testing.T) {
	pool := testDB(t)
	base, adminTok, _ := newProfileAdminServer(t, pool)
	running, stopped, failed, _ := seedStateSpread(t, pool)

	cases := []struct {
		query string
		want  []string
	}{
		// No parameter: unchanged behaviour, every session, newest first.
		{"", []string{failed, stopped, running}},
		{"?state=all", []string{failed, stopped, running}},
		{"?state=active", []string{running}},
		{"?state=ended", []string{stopped}},
		{"?state=failed", []string{failed}},
	}
	for _, c := range cases {
		got := adminSessionIDs(t, base, adminTok, c.query)
		if len(got) != len(c.want) {
			t.Errorf("%q: got %d rows %v, want %d %v", c.query, len(got), got, len(c.want), c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q: row %d = %s, want %s", c.query, i, got[i], c.want[i])
			}
		}
	}
}

// TestAdminSessionsActiveCoversEveryNonTerminalState pins the definition of
// `active` against the state machine rather than against one example: every
// state that is not terminal must be returned, `pending` included — it holds no
// GPU reservation, but it is a session an operator can see and stop.
func TestAdminSessionsActiveCoversEveryNonTerminalState(t *testing.T) {
	pool := testDB(t)
	base, adminTok, _ := newProfileAdminServer(t, pool)
	s := seed(t, pool, 8)

	want := map[string]bool{}
	for _, st := range []string{"pending", "assigned", "starting", "running", "stopping"} {
		want[insertSessionRow(t, pool, s.userID, s.appID, &s.hostID, st)] = true
	}
	// Two terminal rows that must NOT appear.
	insertSessionRow(t, pool, s.userID, s.appID, &s.hostID, "stopped")
	insertSessionRow(t, pool, s.userID, s.appID, &s.hostID, "failed")

	got := adminSessionIDs(t, base, adminTok, "?state=active")
	if len(got) != len(want) {
		t.Fatalf("state=active returned %d rows, want %d (every non-terminal state)", len(got), len(want))
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("state=active returned a terminal session %s", id)
		}
	}
}

func TestAdminSessionsRejectsUnknownState(t *testing.T) {
	pool := testDB(t)
	base, adminTok, _ := newProfileAdminServer(t, pool)
	seedStateSpread(t, pool)

	resp := doJSON(t, "GET", base+"/v1/admin/sessions?state=bogus", adminTok, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("state=bogus: got %d, want 400 (never a silent fall back to all)", resp.StatusCode)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Code != "invalid_state" {
		t.Errorf("error code = %q, want invalid_state", body.Error.Code)
	}
}

// TestAdminSessionsStateFilterPages: the filter composes with cursor paging, and
// the lookahead counts FILTERED rows — a next_cursor computed over the unfiltered
// set would page past the end of a narrow filter.
func TestAdminSessionsStateFilterPages(t *testing.T) {
	pool := testDB(t)
	base, adminTok, _ := newProfileAdminServer(t, pool)
	s := seed(t, pool, 8)

	// Three active sessions interleaved with three terminal ones.
	var active []string
	for i := 0; i < 3; i++ {
		insertSessionRow(t, pool, s.userID, s.appID, &s.hostID, "stopped")
		active = append(active, insertSessionRow(t, pool, s.userID, s.appID, &s.hostID, "running"))
	}

	resp := doJSON(t, "GET", base+"/v1/admin/sessions?state=active&limit=2", adminTok, nil)
	defer resp.Body.Close()
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode page 1: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("page 1 returned %d rows, want 2", len(page.Items))
	}
	if page.NextCursor == nil {
		t.Fatalf("page 1 next_cursor is null; want a cursor (3 active rows, page size 2)")
	}
	ids := adminSessionIDs(t, base, adminTok, "?state=active&limit=2&cursor="+*page.NextCursor)
	if len(ids) != 1 || ids[0] != active[0] {
		t.Fatalf("page 2 = %v, want the oldest active session %s", ids, active[0])
	}
}
