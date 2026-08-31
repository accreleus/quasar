package audit

// handler_filters_test.go — GET /v1/admin/activity filters, the actor-username
// join and the derived severity (UI v3 amendment). Requires Postgres: make test-db.
//
// What is load-bearing here:
//   - the username join must be LEFT: an audit row outlives its actor by design
//     (no FK on actor_user_id), and an inner join would silently delete history
//     from the feed the moment an admin is removed;
//   - every filter composes with the cursor, or paging a filtered feed leaks
//     rows the operator filtered out;
//   - `q` escapes LIKE wildcards, so a literal `_` is not a one-character
//     wildcard against every row on the page.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// seedActors inserts two users and returns their ids and usernames.
func seedActors(t *testing.T, s *Store) (devonID, miraID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `DELETE FROM admin_activity`); err != nil {
		t.Fatalf("truncate activity: %v", err)
	}
	// username carries the only plain UNIQUE constraint (email's is an expression
	// index on lower(email), so ON CONFLICT cannot infer it). Delete-then-insert
	// is simpler than either and leaves the ids fresh per test.
	ids := make([]string, 2)
	for i, u := range []struct{ email, name string }{
		{"devon@audit.test", "devon"}, {"mira@audit.test", "mira"},
	} {
		if _, err := s.pool.Exec(ctx, `DELETE FROM users WHERE username = $1`, u.name); err != nil {
			t.Fatalf("clear user %s: %v", u.name, err)
		}
		if err := s.pool.QueryRow(ctx, `
			INSERT INTO users (email, username, password_hash) VALUES ($1, $2, 'x')
			RETURNING id::text`, u.email, u.name).Scan(&ids[i]); err != nil {
			t.Fatalf("seed user %s: %v", u.name, err)
		}
	}
	return ids[0], ids[1]
}

// seedFeed writes the five rows every case below reads. The fifth has an actor
// id that matches no user — the state a deleted admin leaves behind.
func seedFeed(t *testing.T, s *Store) (devonID, miraID, ghostID string) {
	t.Helper()
	ctx := context.Background()
	devonID, miraID = seedActors(t, s)
	ghostID = "99999999-9999-4999-8999-999999999999"
	rows := []struct {
		actor, action, targetType, targetID string
	}{
		{devonID, "user.deleted", "user", "aaaaaaaa-0000-4000-8000-000000000001"},
		{devonID, "user.role_changed", "user", "aaaaaaaa-0000-4000-8000-000000000002"},
		{miraID, "app.artwork.set", "app", "bbbbbbbb-0000-4000-8000-000000000001"},
		{"", "session.failed", "session", "cccccccc-0000-4000-8000-000000000001"},
		{ghostID, "invite.revoked", "invite", "dddddddd-0000-4000-8000-000000000001"},
	}
	for _, r := range rows {
		if err := s.Record(ctx, r.actor, r.action, r.targetType, r.targetID, nil); err != nil {
			t.Fatalf("record %s: %v", r.action, err)
		}
	}
	return devonID, miraID, ghostID
}

func actions(items []Item) []string {
	out := make([]string, 0, len(items))
	for _, i := range items {
		out = append(out, i.Action)
	}
	return out
}

func TestListFilters(t *testing.T) {
	s := testStore(t)
	devonID, _, _ := seedFeed(t, s)
	ctx := context.Background()
	since := time.Now().Add(-time.Hour)

	cases := []struct {
		name   string
		filter ListFilter
		want   []string
	}{
		{"unfiltered", ListFilter{}, []string{
			"invite.revoked", "session.failed", "app.artwork.set", "user.role_changed", "user.deleted"}},
		{"action prefix", ListFilter{Action: "user."}, []string{"user.role_changed", "user.deleted"}},
		{"action exact", ListFilter{Action: "user.deleted"}, []string{"user.deleted"}},
		{"actor", ListFilter{ActorUserID: devonID}, []string{"user.role_changed", "user.deleted"}},
		{"target type", ListFilter{TargetType: "app"}, []string{"app.artwork.set"}},
		{"since", ListFilter{Since: &since}, []string{
			"invite.revoked", "session.failed", "app.artwork.set", "user.role_changed", "user.deleted"}},
		{"q matches actor username", ListFilter{Q: "devon"}, []string{"user.role_changed", "user.deleted"}},
		{"q matches action", ListFilter{Q: "ARTWORK"}, []string{"app.artwork.set"}},
		{"q matches target id substring", ListFilter{Q: "cccccccc"}, []string{"session.failed"}},
		{"and-ed", ListFilter{Action: "user.", ActorUserID: devonID, TargetType: "user"},
			[]string{"user.role_changed", "user.deleted"}},
		{"since in the future excludes everything", ListFilter{Since: futureTime()}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			items, _, err := s.List(ctx, 0, 100, c.filter)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			got := actions(items)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

func futureTime() *time.Time {
	t := time.Now().Add(time.Hour)
	return &t
}

// TestListJoinsActorUsernameAndDerivesSeverity: the two derived fields, on the
// three actor shapes that exist — present, absent, and dangling.
func TestListJoinsActorUsernameAndDerivesSeverity(t *testing.T) {
	s := testStore(t)
	seedFeed(t, s)

	items, _, err := s.List(context.Background(), 0, 100, ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byAction := map[string]Item{}
	for _, i := range items {
		byAction[i.Action] = i
	}
	if len(byAction) != 5 {
		t.Fatalf("got %d distinct actions, want 5", len(byAction))
	}

	if u := byAction["user.deleted"].ActorUsername; u == nil || *u != "devon" {
		t.Errorf("user.deleted actor_username = %v, want devon", u)
	}
	if u := byAction["app.artwork.set"].ActorUsername; u == nil || *u != "mira" {
		t.Errorf("app.artwork.set actor_username = %v, want mira", u)
	}
	// No actor at all (a server-originated event).
	if u := byAction["session.failed"].ActorUsername; u != nil {
		t.Errorf("session.failed actor_username = %q, want nil", *u)
	}
	if byAction["session.failed"].ActorUserID != nil {
		t.Errorf("session.failed actor_user_id = %v, want nil", byAction["session.failed"].ActorUserID)
	}
	// A dangling actor id: the row must survive with a null name. This is why the
	// join is LEFT and why admin_activity carries no FK.
	ghost := byAction["invite.revoked"]
	if ghost.ActorUsername != nil {
		t.Errorf("deleted actor's actor_username = %q, want nil", *ghost.ActorUsername)
	}
	if ghost.ActorUserID == nil {
		t.Error("deleted actor's actor_user_id was dropped; the id is the only handle left")
	}

	want := map[string]string{
		"user.deleted": SeverityWarn, "invite.revoked": SeverityWarn,
		"session.failed":    SeverityErr,
		"user.role_changed": SeverityInfo, "app.artwork.set": SeverityInfo,
	}
	for action, sev := range want {
		if got := byAction[action].Severity; got != sev {
			t.Errorf("%s severity = %q, want %q", action, got, sev)
		}
	}
}

func TestListFilterComposesWithCursor(t *testing.T) {
	s := testStore(t)
	devonID, _, _ := seedFeed(t, s)
	ctx := context.Background()

	page1, next, err := s.List(ctx, 0, 1, ListFilter{ActorUserID: devonID})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1) != 1 || page1[0].Action != "user.role_changed" {
		t.Fatalf("page 1 = %v, want [user.role_changed]", actions(page1))
	}
	if next == nil {
		t.Fatal("page 1 next_cursor is nil; devon has two rows")
	}
	page2, next2, err := s.List(ctx, *next, 10, ListFilter{ActorUserID: devonID})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2) != 1 || page2[0].Action != "user.deleted" {
		t.Fatalf("page 2 = %v, want [user.deleted]", actions(page2))
	}
	if next2 != nil {
		t.Errorf("page 2 next_cursor = %v, want nil", *next2)
	}
}

// TestQEscapesLikeWildcards: `_` and `%` in operator input are literals. Without
// escaping, `q=_` matches every row with at least one character, which is a
// search box that silently returns the whole table.
func TestQEscapesLikeWildcards(t *testing.T) {
	s := testStore(t)
	seedFeed(t, s)
	ctx := context.Background()

	// Exactly one seeded action carries a literal underscore. Unescaped, `_` is a
	// one-character wildcard and this returns every row on the page.
	items, _, err := s.List(ctx, 0, 100, ListFilter{Q: "_"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].Action != "user.role_changed" {
		t.Errorf("q=_ returned %v, want [user.role_changed] — `_` is a literal, not a wildcard",
			actions(items))
	}
	if items, _, err = s.List(ctx, 0, 100, ListFilter{Q: "%"}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("q=%% returned %d rows; `%%` must be a literal", len(items))
	}
	// The escaping must not break an ordinary search.
	items, _, err = s.List(ctx, 0, 100, ListFilter{Q: "role_changed"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].Action != "user.role_changed" {
		t.Errorf("q=role_changed returned %v, want [user.role_changed]", actions(items))
	}
}

// --- HTTP surface -----------------------------------------------------------

func newActivityServer(t *testing.T, s *Store) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	NewHandler(s).Register(mux, func(next http.Handler) http.Handler { return next })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestActivityHandlerRejectsMalformedFilters(t *testing.T) {
	s := testStore(t)
	seedFeed(t, s)
	srv := newActivityServer(t, s)

	for _, q := range []string{"?since=yesterday", "?since=2026-08-01", "?actor_user_id=not-a-uuid"} {
		resp, err := http.Get(srv.URL + "/v1/admin/activity" + q)
		if err != nil {
			t.Fatalf("GET %s: %v", q, err)
		}
		code := resp.StatusCode
		resp.Body.Close()
		if code != http.StatusBadRequest {
			t.Errorf("GET %s: got %d, want 400", q, code)
		}
	}
}

func TestActivityHandlerServesFiltersAndDerivedFields(t *testing.T) {
	s := testStore(t)
	seedFeed(t, s)
	srv := newActivityServer(t, s)

	resp, err := http.Get(srv.URL + "/v1/admin/activity?action=user.&q=devon" +
		"&since=" + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
	var body struct {
		Items []struct {
			Action        string  `json:"action"`
			ActorUsername *string `json:"actor_username"`
			Severity      string  `json:"severity"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(body.Items))
	}
	for _, it := range body.Items {
		if it.ActorUsername == nil || *it.ActorUsername != "devon" {
			t.Errorf("%s actor_username = %v, want devon", it.Action, it.ActorUsername)
		}
		if it.Severity == "" {
			t.Errorf("%s severity is empty; it must always be serialized", it.Action)
		}
	}
}
