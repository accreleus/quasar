package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/console"
)

// #580: a database at 0.4.1's schema (0098), console mode on with the Steam
// provider app as its default app, comes through the update with a console
// that launches: console_default_app passes and the app is offered, with no
// operator step. The rows 0103 must not touch stay as they were.
func TestMigration0103GivesAnUpgradedConsoleItsDirectApp(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, 98)
	pool, err := pgxpool.New(context.Background(), url)
	must(t, err)
	t.Cleanup(pool.Close)
	ctx := context.Background()

	// What a 0.4.1 control plane holds after syncing the catalog that declares
	// the key: it stored the runtime block whole, and stamped nothing.
	must(t, exec(t, pool, `
		INSERT INTO image_catalog (id, manifest_version, display_name, kind, version, registry_ref, runtime, library_provider, raw) VALUES
		('steam',  1, 'Steam', 'prebuilt', '2026.10.06', 'registry.example/steam:2026.10.06',
		 '{"gpu":true,"no_new_privileges":false,"direct_display":true}', 'steam', '{}'),
		('kde',    1, 'KDE',   'prebuilt', '2026.10.06', 'registry.example/kde:2026.10.06',
		 '{"gpu":true,"systempaths_unconfined":true,"direct_display":true}', NULL, '{}'),
		('xfce',   1, 'XFCE',  'prebuilt', '2026.10.04', 'registry.example/xfce:2026.10.04',
		 '{"gpu":true,"systempaths_unconfined":true}', NULL, '{}'),
		('behind', 1, 'Not updated yet', 'prebuilt', '2026.10.06', 'registry.example/behind:2026.10.06',
		 '{"gpu":true,"direct_display":true}', NULL, '{}')`))

	preset := func(name, managedImage string) string {
		t.Helper()
		var id string
		must(t, pool.QueryRow(ctx, `INSERT INTO runtime_presets (name, image, managed_image_id)
			VALUES ($1, 'registry.example/' || $1, NULLIF($2, '')) RETURNING id::text`, name, managedImage).Scan(&id))
		if managedImage != "" {
			version := "2026.10.06"
			switch managedImage {
			case "xfce":
				version = "2026.10.04"
			case "behind":
				version = "2026.09.30" // the catalog moved on; this install has not updated the image
			}
			must(t, exec(t, pool, `INSERT INTO installed_images (image_id, version, registry_ref, runtime_preset_id)
				VALUES ($1, $2, 'registry.example/' || $1, $3::uuid)`, managedImage, version, id))
		}
		return id
	}
	steamPreset, kdePreset := preset("steam", "steam"), preset("kde", "kde")
	xfcePreset, behindPreset, ownPreset := preset("xfce", "xfce"), preset("behind", "behind"), preset("operator-own", "")

	app := func(name, kind, provider, presetID, spec string) string {
		t.Helper()
		var id string
		must(t, pool.QueryRow(ctx, `INSERT INTO apps (name, kind, library_provider, runtime_preset_id, runtime_spec)
			VALUES ($1, $2, $3, NULLIF($4, '')::uuid, $5::jsonb) RETURNING id::text`, name, kind, provider, presetID, spec).Scan(&id))
		return id
	}
	steam := app("Steam", "launcher", "steam", steamPreset, `{"gpu":true,"no_new_privileges":false}`)
	kde := app("KDE Desktop", "desktop", "", kdePreset, `{"gpu":true,"systempaths_unconfined":true}`)
	rows := []struct {
		name, id string
		want     any // nil = absent
	}{
		{"the Steam provider app", steam, true},
		{"an operator's app on a catalog preset", kde, true},
		{"an app whose operator set false", app("KDE, never direct", "desktop", "", kdePreset, `{"gpu":true,"direct_display":false}`), false},
		{"an app whose manifest declares nothing", app("XFCE Desktop", "desktop", "", xfcePreset, `{"gpu":true}`), nil},
		{"an app whose image is behind the catalog", app("Behind", "desktop", "", behindPreset, `{"gpu":true}`), nil},
		{"an app the operator repointed at their own preset", app("Repointed", "launcher", "", ownPreset, `{"gpu":true}`), nil},
		{"an app on its own image", app("Own image", "desktop", "", "", `{"image":"registry.example/own:1"}`), nil},
	}
	tile := ""
	must(t, pool.QueryRow(ctx, `INSERT INTO apps (name, parent_app_id, external_source, external_id, origin, kind, default_vram_mb, default_encode_slots)
		VALUES ('A game', $1::uuid, 'steam', '730', 'discovered', 'game', 0, 0) RETURNING id::text`, steam).Scan(&tile))

	var hostID, userID string
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status) VALUES ('console-host', 'online') RETURNING id::text`).Scan(&hostID))
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash) VALUES ('console@test.local', 'console', 'x') RETURNING id::text`).Scan(&userID))
	must(t, exec(t, pool, `INSERT INTO console_config (host_id, config) VALUES ($1::uuid, jsonb_build_object(
		'enabled', true, 'auto_start_on_display', true, 'default_app', $2::text, 'default_user', $3::text,
		'compositor', 'weston', 'connector', 'auto'))`, hostID, steam, userID))

	check := func(when string) {
		t.Helper()
		cs := console.NewStore(pool)
		sparse, err := cs.Get(ctx, hostID)
		must(t, err)
		cfg, err := console.Resolve(sparse)
		must(t, err)
		if cfg.DefaultApp == nil || *cfg.DefaultApp != steam {
			t.Fatalf("%s: console default_app = %v, want the Steam app %s", when, cfg.DefaultApp, steam)
		}
		ready, err := cs.DefaultAppReadiness(ctx, cfg)
		must(t, err)
		if ready.Status != "pass" {
			t.Fatalf("%s: %s = %s (%s), want pass", when, console.DefaultAppCheckID, ready.Status, ready.Summary)
		}
		offered, err := cs.DirectApps(ctx)
		must(t, err)
		got := map[string]bool{}
		for _, a := range offered {
			got[a.ID] = true
		}
		if len(got) != 2 || !got[steam] || !got[kde] {
			t.Fatalf("%s: console offers %v, want exactly Steam and the KDE app", when, offered)
		}
		for _, r := range rows {
			var raw []byte
			must(t, pool.QueryRow(ctx, `SELECT runtime_spec FROM apps WHERE id = $1::uuid`, r.id).Scan(&raw))
			var spec map[string]any
			must(t, json.Unmarshal(raw, &spec))
			if spec["direct_display"] != r.want {
				t.Errorf("%s: %s has direct_display %v, want %v (spec %s)", when, r.name, spec["direct_display"], r.want, raw)
			}
			if spec["gpu"] == nil && spec["image"] == nil {
				t.Errorf("%s: %s lost its other keys: %s", when, r.name, raw)
			}
		}
		var tileSpec string
		must(t, pool.QueryRow(ctx, `SELECT runtime_spec::text FROM apps WHERE id = $1::uuid`, tile).Scan(&tileSpec))
		if tileSpec != "{}" {
			t.Errorf("%s: derived tile spec = %s, want {}", when, tileSpec)
		}
	}

	migrateTo(t, m, 103)
	check("after 0103")

	// The down file runs, and the up is a no-op on rows it already wrote.
	migrateTo(t, m, 102)
	migrateTo(t, m, 103)
	check("after down and up again")
}
