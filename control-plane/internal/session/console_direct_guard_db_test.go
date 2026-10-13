package session

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/console"
)

// setAppKind overrides the kind column insertAppWithSpec and its siblings
// leave at the schema default ('game'), for a fixture row that is testing
// something else (direct_display, a preset merge) and needs a console-
// eligible kind so that is the only thing under test on that row.
func setAppKind(t *testing.T, pool *pgxpool.Pool, appID, kind string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `UPDATE apps SET kind = $1 WHERE id::text = $2`, kind, appID)
	must(t, err)
}

// eligible is the test's own copy of the combined rule DirectApps' SQL and
// DefaultAppCheck's Go both apply: direct_display AND a console-eligible
// kind/parent. It exists so the assertions below read as "what SHOULD this
// row's answer be", computed independently of either production twin.
func eligible(app LaunchApp) bool {
	return console.RuntimeSpecDirect(app.RuntimeSpec) &&
		console.KindAllowsConsoleDefault(app.Kind, app.ParentAppID != "")
}

// TestConsoleDirectAppsMatchLaunchSpec guards the SQL twins: console.Store's
// DirectApps and DefaultAppFacts resolve the effective runtime_spec and test
// direct_display in SQL like RuntimeSpecDirect, and test kind/parent in SQL
// like KindAllowsConsoleDefault, while the launch backstop reads
// GetLaunchApp's spec through those same Go functions. For every row all
// three must agree.
func TestConsoleDirectAppsMatchLaunchSpec(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	preset := insertPreset(t, pool, "console-guard-preset", "desktop:1", `[]`, `{}`, `[]`, false, "")
	ids := map[string]string{
		"true":                insertAppWithSpec(t, pool, "guard-true", `{"image":"a:1","direct_display":true}`, nil, false, ""),
		"false":               insertAppWithSpec(t, pool, "guard-false", `{"image":"a:1","direct_display":false}`, nil, false, ""),
		"absent":              insertAppWithSpec(t, pool, "guard-absent", `{"image":"a:1"}`, nil, false, ""),
		"string":              insertAppWithSpec(t, pool, "guard-string", `{"image":"a:1","direct_display":"true"}`, nil, false, ""),
		"number":              insertAppWithSpec(t, pool, "guard-number", `{"direct_display":1}`, nil, false, ""),
		"empty":               insertAppWithSpec(t, pool, "guard-empty", `{}`, nil, false, ""),
		"preset, app says so": insertAppWithSpec(t, pool, "guard-preset-true", `{"direct_display":true}`, &preset, false, ""),
		"preset, app silent":  insertAppWithSpec(t, pool, "guard-preset-silent", `{}`, &preset, false, ""),
	}
	// These rows are testing direct_display alone, so pin them to a
	// console-eligible kind — otherwise every one would fail on kind (the
	// schema default is 'game') and the direct_display dimension would never
	// be exercised through DirectApps.
	for _, id := range ids {
		setAppKind(t, pool, id, "desktop")
	}
	// A game (no parent) that DOES declare direct_display: kind alone must
	// still exclude it, independent of the tile/parent path below.
	ids["direct game, no parent"] = insertAppWithSpec(t, pool, "guard-direct-game", `{"image":"a:1","direct_display":true}`, nil, false, "")

	directParent := seedSteamApp(t, pool, `{"image":"steam:1","direct_display":true}`)
	ids["direct parent"] = directParent // seedSteamApp's own kind is 'launcher'
	ids["tile of a direct parent"] = seedTile(t, pool, directParent, "Guard Tile", "1145360")

	direct, err := console.NewStore(pool).DirectApps(ctx)
	must(t, err)
	listed := map[string]bool{}
	for _, a := range direct {
		listed[a.ID] = true
	}

	cstore := console.NewStore(pool)
	for name, id := range ids {
		app, err := store.GetLaunchApp(ctx, id)
		must(t, err)
		want := eligible(app)

		if listed[id] != want {
			t.Errorf("%s: DirectApps lists it = %v, want %v (direct=%v, kind=%q, parent=%q)",
				name, listed[id], want, console.RuntimeSpecDirect(app.RuntimeSpec), app.Kind, app.ParentAppID)
		}
		facts, err := cstore.DefaultAppFacts(ctx, id)
		must(t, err)
		gotFacts := facts.Found && facts.Direct && console.KindAllowsConsoleDefault(facts.Kind, facts.ParentAppID != "")
		if !facts.Found || gotFacts != want {
			t.Errorf("%s: DefaultAppFacts = %+v, want eligible = %v", name, facts, want)
		}
	}
	// The table must exercise every answer, or the guard proves nothing.
	if !listed[ids["true"]] || listed[ids["string"]] {
		t.Fatalf("fixture did not exercise the direct_display dimension: listed = %v", listed)
	}
	if listed[ids["direct game, no parent"]] {
		t.Fatalf("a game that declares direct_display must still be excluded by kind: listed = %v", listed)
	}
	if !listed[ids["direct parent"]] {
		t.Fatalf("a direct-capable launcher with no parent must be offered: listed = %v", listed)
	}
	if listed[ids["tile of a direct parent"]] {
		t.Fatalf("a tile must never be offered, even under a direct-capable parent: listed = %v", listed)
	}
}
