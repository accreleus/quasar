package storage

// Integration tests for the human-readable names on GET /v1/admin/storage/homes.
// Requires TEST_DATABASE_URL (silently skipped without it — see gc_test.go).
//
// The regression these exist to hold down: the names are resolved with LEFT
// JOINs, so a home whose app/user/host row is already gone STAYS in the listing
// with a null name. An inner join would pass a naive "names resolve" test and
// silently drop exactly the orphaned bytes an admin opened this page to find.

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The three names are `required` in openapi.yaml's AdminHome (nullable, not
// optional), so the wire object must always carry the keys — present-and-null
// for an orphaned home, never omitted. `omitempty` on any of them would break
// the contract, and the apitest only ever sees an empty homes list, so nothing
// downstream would catch it.
func TestHomeRespAlwaysCarriesTheNameKeys(t *testing.T) {
	name := "Steam"
	b, err := json.Marshal(toHomeResp(Home{ID: "h1", AppName: &name}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"user_id", "app_id", "host_id", "username", "app_name", "host_name"} {
		if _, ok := got[k]; !ok {
			t.Errorf("key %q missing from the wire object (omitempty?): %s", k, b)
		}
	}
	for _, k := range []string{"username", "host_name"} {
		if string(got[k]) != "null" {
			t.Errorf("unresolved %s = %s, want null", k, got[k])
		}
	}
	if string(got["app_name"]) != `"Steam"` {
		t.Errorf("app_name = %s, want \"Steam\"", got["app_name"])
	}
}

// seedNamedUser inserts a user whose username and email DIFFER, so a test that
// asserts on the username cannot be satisfied by the email column by accident.
func seedNamedUser(t *testing.T, pool *pgxpool.Pool, username, email string) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(),
		`INSERT INTO users (email, username, password_hash) VALUES ($1, $2, 'x') RETURNING id::text`,
		email, username).Scan(&id))
	return id
}

// seedNamedHost inserts a host with a caller-chosen node_name (hosts.node_name
// is UNIQUE, so the shared seedHost's hard-coded 'h' can only be used once).
func seedNamedHost(t *testing.T, pool *pgxpool.Pool, nodeName string) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(),
		`INSERT INTO hosts (node_name, status) VALUES ($1,'online') RETURNING id::text`, nodeName).Scan(&id))
	return id
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestListHomes_ResolvesAllThreeNames(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	// username deliberately != email: the response must carry users.username,
	// which is what the admin Users page shows.
	u := seedNamedUser(t, pool, "nameduser", "named@test.local")
	app := seedApp(t, pool, "Named App")
	host := seedNamedHost(t, pool, "named-node")
	id := insertHome(t, pool, u, app, host)

	homes, _, err := mgr.ListHomes(ctx, ListHomesOpts{})
	must(t, err)
	if len(homes) != 1 {
		t.Fatalf("want 1 home, got %d", len(homes))
	}
	h := homes[0]
	if h.ID != id {
		t.Fatalf("home id = %s, want %s", h.ID, id)
	}
	if deref(h.Username) != "nameduser" {
		t.Errorf("username = %s, want nameduser (users.username, not email)", deref(h.Username))
	}
	if deref(h.AppName) != "Named App" {
		t.Errorf("app_name = %s, want %q", deref(h.AppName), "Named App")
	}
	if deref(h.HostName) != "named-node" {
		t.Errorf("host_name = %s, want named-node (hosts.node_name)", deref(h.HostName))
	}
	// The ids stay: they are what the tombstone action keys on.
	if h.UserID == nil || *h.UserID != u || h.AppID == nil || *h.AppID != app || h.HostID == nil || *h.HostID != host {
		t.Errorf("ids not preserved alongside names: user=%v app=%v host=%v", h.UserID, h.AppID, h.HostID)
	}
}

// The row must SURVIVE its app being deleted. Asserting only "app_name is null"
// would pass against an inner join that dropped the row entirely, so this
// asserts presence first.
func TestListHomes_DeletedAppStillListed(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	u := seedNamedUser(t, pool, "orphanuser", "orphan@test.local")
	app := seedApp(t, pool, "Doomed App")
	host := seedNamedHost(t, pool, "orphan-node")
	id := insertHome(t, pool, u, app, host)

	// user_homes.app_id is ON DELETE SET NULL, so the home outlives the app.
	if _, err := pool.Exec(ctx, `DELETE FROM apps WHERE id::text = $1`, app); err != nil {
		t.Fatalf("delete app: %v", err)
	}

	homes, _, err := mgr.ListHomes(ctx, ListHomesOpts{})
	must(t, err)

	var found *Home
	for i := range homes {
		if homes[i].ID == id {
			found = &homes[i]
		}
	}
	if found == nil {
		t.Fatalf("home %s vanished from the listing after its app was deleted "+
			"(inner join?) — got %d rows: %+v", id, len(homes), homes)
	}
	if found.AppID != nil {
		t.Errorf("app_id = %v, want null after the app row was deleted", *found.AppID)
	}
	if found.AppName != nil {
		t.Errorf("app_name = %q, want null after the app row was deleted", *found.AppName)
	}
	// The rest of the row is intact — its bytes are still accounted for.
	if deref(found.Username) != "orphanuser" {
		t.Errorf("username = %s, want orphanuser", deref(found.Username))
	}
	if deref(found.HostName) != "orphan-node" {
		t.Errorf("host_name = %s, want orphan-node", deref(found.HostName))
	}
}

// A home with no host pinned resolves host_name to null rather than dropping.
func TestListHomes_NullHostStillListed(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	u := seedNamedUser(t, pool, "nohostuser", "nohost@test.local")
	app := seedApp(t, pool, "Hostless App")

	var id string
	must(t, pool.QueryRow(ctx, `
		INSERT INTO user_homes (user_id, app_id, host_id, provider, ref)
		VALUES ($1::uuid, $2::uuid, NULL, 'volume', 'vol-nohost')
		RETURNING id::text`, u, app).Scan(&id))

	homes, _, err := mgr.ListHomes(ctx, ListHomesOpts{})
	must(t, err)
	if len(homes) != 1 || homes[0].ID != id {
		t.Fatalf("home with NULL host_id missing from listing: %+v", homes)
	}
	if homes[0].HostName != nil {
		t.Errorf("host_name = %q, want null for a home with no host", *homes[0].HostName)
	}
	if deref(homes[0].AppName) != "Hostless App" {
		t.Errorf("app_name = %s, want %q", deref(homes[0].AppName), "Hostless App")
	}
}

// Ordering (created_at DESC) and the offset cursor are unaffected by the joins.
// created_at is now ambiguous across four joined tables — this fails loudly if
// the ORDER BY ever resolves to the wrong table's column.
func TestListHomes_OrderingAndPagingUnchanged(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	u := seedNamedUser(t, pool, "pageuser", "page@test.local")
	host := seedNamedHost(t, pool, "page-node")

	// 51 homes → one full page (50) plus a second page of 1.
	const total = 51
	if _, err := pool.Exec(ctx, `
		INSERT INTO apps (name, default_vram_mb, default_encode_slots, default_width,
		                  default_height, default_fps, default_bitrate_kbps, runtime_spec)
		SELECT 'page-app-' || lpad(g::text, 3, '0'), 512, 1, 1280, 720, 30, 2000, '{}'
		FROM generate_series(1, $1::int) g`, total); err != nil {
		t.Fatalf("seed apps: %v", err)
	}
	// Distinct created_at per home so DESC ordering is deterministic: the app
	// with the highest ordinal is the newest home.
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_homes (user_id, app_id, host_id, provider, ref, created_at)
		SELECT $1::uuid, a.id, $2::uuid, 'volume', 'vol-' || a.name,
		       now() + (right(a.name, 3)::int * interval '1 second')
		FROM apps a WHERE a.name LIKE 'page-app-%'`, u, host); err != nil {
		t.Fatalf("seed homes: %v", err)
	}

	page1, next, err := mgr.ListHomes(ctx, ListHomesOpts{})
	must(t, err)
	if len(page1) != 50 {
		t.Fatalf("page 1: want 50 rows, got %d", len(page1))
	}
	if next != "50" {
		t.Fatalf("next_cursor = %q, want \"50\"", next)
	}
	// Newest first, strictly descending.
	for i := 1; i < len(page1); i++ {
		if page1[i].CreatedAt.After(page1[i-1].CreatedAt) {
			t.Fatalf("ordering broken at %d: %s after %s", i, page1[i].CreatedAt, page1[i-1].CreatedAt)
		}
	}
	if deref(page1[0].AppName) != "page-app-051" {
		t.Errorf("newest row app_name = %s, want page-app-051", deref(page1[0].AppName))
	}

	page2, next2, err := mgr.ListHomes(ctx, ListHomesOpts{Cursor: next})
	must(t, err)
	if len(page2) != 1 {
		t.Fatalf("page 2: want 1 row, got %d", len(page2))
	}
	if next2 != "" {
		t.Errorf("page 2 next_cursor = %q, want empty (last page)", next2)
	}
	if deref(page2[0].AppName) != "page-app-001" {
		t.Errorf("oldest row app_name = %s, want page-app-001", deref(page2[0].AppName))
	}
	// No row is served twice and none is lost.
	seen := map[string]bool{}
	for _, h := range append(append([]Home{}, page1...), page2...) {
		if seen[h.ID] {
			t.Fatalf("home %s served on both pages", h.ID)
		}
		seen[h.ID] = true
	}
	if len(seen) != total {
		t.Errorf("saw %d distinct homes across both pages, want %d", len(seen), total)
	}
}

// queryCounter records every SQL statement pgx executes on a pool.
type queryCounter struct {
	mu   sync.Mutex
	sqls []string
}

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	c.sqls = append(c.sqls, data.SQL)
	c.mu.Unlock()
	return ctx
}

func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *queryCounter) reset() {
	c.mu.Lock()
	c.sqls = nil
	c.mu.Unlock()
}

func (c *queryCounter) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.sqls...)
}

// The names must be resolved by JOIN, not by a follow-up read per row. This
// counts the statements pgx actually issues and asserts the count is 1 and does
// not grow with the number of homes.
func TestListHomes_NamesResolveWithoutNPlusOne(t *testing.T) {
	pool := testDB(t) // migrates + truncates; also registers cleanup
	ctx := context.Background()

	counter := &queryCounter{}
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.Tracer = counter
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(traced.Close)
	mgr := NewLocal(traced, t.TempDir())

	u := seedNamedUser(t, pool, "npluser", "nplus@test.local")
	host := seedNamedHost(t, pool, "nplus-node")

	seedHomes := func(n int, prefix string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO apps (name, default_vram_mb, default_encode_slots, default_width,
			                  default_height, default_fps, default_bitrate_kbps, runtime_spec)
			SELECT $2::text || g::text, 512, 1, 1280, 720, 30, 2000, '{}'
			FROM generate_series(1, $1::int) g`, n, prefix); err != nil {
			t.Fatalf("seed apps: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO user_homes (user_id, app_id, host_id, provider, ref)
			SELECT $1::uuid, a.id, $2::uuid, 'volume', 'vol-' || a.name
			FROM apps a WHERE a.name LIKE $3 AND a.id NOT IN (SELECT app_id FROM user_homes WHERE app_id IS NOT NULL)`,
			u, host, prefix+"%"); err != nil {
			t.Fatalf("seed homes: %v", err)
		}
	}

	seedHomes(1, "nplus-a-")
	counter.reset()
	one, _, err := mgr.ListHomes(ctx, ListHomesOpts{})
	must(t, err)
	withOne := counter.snapshot()
	if len(one) != 1 {
		t.Fatalf("want 1 home, got %d", len(one))
	}
	if len(withOne) != 1 {
		t.Fatalf("ListHomes over 1 home issued %d statements, want 1: %q", len(withOne), withOne)
	}

	seedHomes(19, "nplus-b-")
	counter.reset()
	many, _, err := mgr.ListHomes(ctx, ListHomesOpts{})
	must(t, err)
	withMany := counter.snapshot()
	if len(many) != 20 {
		t.Fatalf("want 20 homes, got %d", len(many))
	}
	if len(withMany) != len(withOne) {
		t.Fatalf("query count scales with row count (N+1): %d statements for 1 home, "+
			"%d for 20: %q", len(withOne), len(withMany), withMany)
	}
	// And the names really did come back from that single statement.
	for _, h := range many {
		if h.Username == nil || h.AppName == nil || h.HostName == nil {
			t.Fatalf("home %s missing a resolved name from the single query: %+v", h.ID, h)
		}
	}
}
