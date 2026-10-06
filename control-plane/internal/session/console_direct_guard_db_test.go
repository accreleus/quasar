package session

import (
	"context"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/console"
)

// TestConsoleDirectAppsMatchLaunchSpec guards the SQL twin: console.Store's
// DirectApps and DefaultAppFacts resolve the effective runtime_spec and test
// direct_display in SQL, while the launch backstop reads GetLaunchApp's spec
// through console.RuntimeSpecDirect. For every row both must agree.
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
	directParent := seedSteamApp(t, pool, `{"image":"steam:1","direct_display":true}`)
	ids["direct parent"] = directParent
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
		want := console.RuntimeSpecDirect(app.RuntimeSpec)

		if listed[id] != want {
			t.Errorf("%s: DirectApps lists it = %v, launch spec says direct = %v", name, listed[id], want)
		}
		facts, err := cstore.DefaultAppFacts(ctx, id)
		must(t, err)
		if !facts.Found || facts.Direct != want {
			t.Errorf("%s: DefaultAppFacts = %+v, launch spec says direct = %v", name, facts, want)
		}
	}
	// The table must exercise both answers, or the guard proves nothing.
	if !listed[ids["true"]] || listed[ids["string"]] || !listed[ids["tile of a direct parent"]] {
		t.Fatalf("fixture did not exercise both answers: listed = %v", listed)
	}
}
