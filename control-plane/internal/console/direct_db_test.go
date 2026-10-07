package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedConsoleTestApp inserts an app named console-test-<name> with spec.
func seedConsoleTestApp(t *testing.T, pool *pgxpool.Pool, name, spec string, enabled bool, kind string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO apps (name, runtime_spec, enabled, kind) VALUES ($1, $2::jsonb, $3, $4) RETURNING id::text`,
		"console-test-"+name, spec, enabled, kind).Scan(&id); err != nil {
		t.Fatalf("seed app %s: %v", name, err)
	}
	return id
}

func seedConsoleTestTile(t *testing.T, pool *pgxpool.Pool, parentID, name, appid string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `INSERT INTO apps
		(name, parent_app_id, external_source, external_id, origin, kind, default_vram_mb, default_encode_slots)
		VALUES ($1, $2::uuid, 'steam', $3, 'discovered', 'game', 0, 0) RETURNING id::text`,
		"console-test-"+name, parentID, appid).Scan(&id); err != nil {
		t.Fatalf("seed tile %s: %v", name, err)
	}
	return id
}

type envelope struct {
	Config      ConsoleConfig    `json:"config"`
	DefaultApps []DefaultApp     `json:"default_apps"`
	Readiness   []ReadinessCheck `json:"readiness"`
	Raw         map[string]any   `json:"-"`
}

func decodeEnvelope(t *testing.T, body []byte) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, body)
	}
	if err := json.Unmarshal(body, &env.Raw); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return env
}

func getEnvelope(t *testing.T, h *Handler, hostID string) envelope {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/hosts/"+hostID+"/console-config", nil)
	req.SetPathValue("id", hostID)
	rec := httptest.NewRecorder()
	h.handleGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return decodeEnvelope(t, rec.Body.Bytes())
}

func hasApp(apps []DefaultApp, id string) bool {
	for _, a := range apps {
		if a.ID == id {
			return true
		}
	}
	return false
}

// The default-app list offers exactly the apps that can run direct: enabled,
// with runtime_spec.direct_display true AND kind desktop or launcher — never
// a derived tile, even through a direct-capable parent (#453 follow-up).
func TestGetOffersOnlyDirectApps(t *testing.T) {
	pool := testPool(t)
	hostID := seedTestHost(t, pool, "console-test-direct-list")
	direct := seedConsoleTestApp(t, pool, "kde", `{"image":"kde:1","direct_display":true}`, true, "desktop")
	nested := seedConsoleTestApp(t, pool, "xfce", `{"image":"xfce:1"}`, true, "desktop")
	saysFalse := seedConsoleTestApp(t, pool, "false", `{"image":"x:1","direct_display":false}`, true, "desktop")
	saysString := seedConsoleTestApp(t, pool, "string", `{"image":"x:1","direct_display":"true"}`, true, "desktop")
	disabled := seedConsoleTestApp(t, pool, "disabled", `{"image":"kde:1","direct_display":true}`, false, "desktop")
	game := seedConsoleTestApp(t, pool, "game", `{"image":"game:1","direct_display":true}`, true, "game")
	steam := seedConsoleTestApp(t, pool, "steam", `{"image":"steam:1","direct_display":true}`, true, "launcher")
	tile := seedConsoleTestTile(t, pool, steam, "hades", "1145360")

	env := getEnvelope(t, NewHandler(NewStore(pool), &fakeDispatcher{}), hostID)
	for _, want := range []string{direct, steam} {
		if !hasApp(env.DefaultApps, want) {
			t.Errorf("default_apps lacks direct app %s: %v", want, env.DefaultApps)
		}
	}
	for _, not := range []string{nested, saysFalse, saysString, disabled, game, tile} {
		if hasApp(env.DefaultApps, not) {
			t.Errorf("default_apps offers %s, which cannot run direct, is disabled, or is not a desktop/launcher: %v", not, env.DefaultApps)
		}
	}
	if len(env.Readiness) != 1 || env.Readiness[0].ID != DefaultAppCheckID || env.Readiness[0].Status != "skip" {
		t.Fatalf("readiness with no default app = %+v, want one skipped console_default_app", env.Readiness)
	}
	if _, ok := env.Raw["capabilities"].(map[string]any)["audio_sinks"]; ok {
		t.Fatal("capabilities still serve audio_sinks after amendment 19")
	}
}

// A Steam library tile is never a console default, even though its effective
// runtime_spec (its direct-capable parent's) declares direct_display: the
// readiness check fails naming it a game, not a desktop or launcher.
func TestDefaultAppGameTileFailsReadinessNamingKind(t *testing.T) {
	pool := testPool(t)
	hostID := seedTestHost(t, pool, "console-test-tile-kind")
	steam := seedConsoleTestApp(t, pool, "steam-kind", `{"image":"steam:1","direct_display":true}`, true, "launcher")
	tile := seedConsoleTestTile(t, pool, steam, "hades-kind", "1145361")
	h := NewHandler(NewStore(pool), &fakeDispatcher{})

	rec := httptest.NewRecorder()
	h.handlePatch(rec, patchRequest(t, hostID, map[string]any{"enabled": true, "default_app": tile}))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, body = %s", rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if len(env.Readiness) != 1 {
		t.Fatalf("readiness = %+v, want one check", env.Readiness)
	}
	c := env.Readiness[0]
	if c.ID != DefaultAppCheckID || c.Status != "fail" ||
		!strings.Contains(c.Summary, "console-test-hades-kind") ||
		!strings.Contains(c.Summary, "game") || !strings.Contains(c.Summary, "desktop or launcher") {
		t.Fatalf("check = %+v, want a fail naming the app a game, not a desktop or launcher", c)
	}
}

// A game app (not a tile) is equally excluded by kind alone.
func TestDefaultAppGameKindFailsReadinessNamingKind(t *testing.T) {
	pool := testPool(t)
	hostID := seedTestHost(t, pool, "console-test-game-kind")
	game := seedConsoleTestApp(t, pool, "solo-game", `{"image":"game:1","direct_display":true}`, true, "game")
	h := NewHandler(NewStore(pool), &fakeDispatcher{})

	rec := httptest.NewRecorder()
	h.handlePatch(rec, patchRequest(t, hostID, map[string]any{"enabled": true, "default_app": game}))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, body = %s", rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	c := env.Readiness[0]
	if c.ID != DefaultAppCheckID || c.Status != "fail" ||
		!strings.Contains(c.Summary, "console-test-solo-game") ||
		!strings.Contains(c.Summary, "game") || !strings.Contains(c.Summary, "desktop or launcher") {
		t.Fatalf("check = %+v, want a fail naming the app a game, not a desktop or launcher", c)
	}
}

// A default app without the direct key is accepted (it exists) and shows up
// as the failed console_default_app readiness check, naming the app.
func TestDefaultAppWithoutDirectKeyFailsReadiness(t *testing.T) {
	pool := testPool(t)
	hostID := seedTestHost(t, pool, "console-test-direct-check")
	nested := seedConsoleTestApp(t, pool, "nested-desktop", `{"image":"xfce:1"}`, true, "desktop")
	h := NewHandler(NewStore(pool), &fakeDispatcher{})

	rec := httptest.NewRecorder()
	h.handlePatch(rec, patchRequest(t, hostID, map[string]any{"enabled": true, "default_app": nested}))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, body = %s", rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if len(env.Readiness) != 1 {
		t.Fatalf("readiness = %+v, want one check", env.Readiness)
	}
	c := env.Readiness[0]
	if c.ID != DefaultAppCheckID || c.Status != "fail" ||
		!strings.Contains(c.Summary, "console-test-nested-desktop") || !strings.Contains(c.Summary, "cannot run direct") {
		t.Fatalf("check = %+v, want a fail naming the app and saying it cannot run direct", c)
	}

	// Declaring the key turns it into a pass on the next read.
	if _, err := pool.Exec(context.Background(),
		`UPDATE apps SET runtime_spec = runtime_spec || '{"direct_display":true}'::jsonb WHERE id::text = $1`, nested); err != nil {
		t.Fatalf("declare direct: %v", err)
	}
	if got := getEnvelope(t, h, hostID).Readiness[0].Status; got != "pass" {
		t.Fatalf("status after declaring direct_display = %q, want pass", got)
	}
}

// A PATCH naming a retired key is refused, and an accepted PATCH drops the
// retired keys a pre-migration row still carried.
func TestPatchRetiredKeysRefusedAndDroppedFromStoredRow(t *testing.T) {
	pool := testPool(t)
	hostID := seedTestHost(t, pool, "console-test-retired")
	store := NewStore(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO console_config (host_id, config)
		VALUES ($1::uuid, '{"enabled":true,"grab":true,"stream":true,"compositor":"weston"}')`, hostID); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}
	h := NewHandler(store, &fakeDispatcher{})

	// Read: the stale row resolves, retired keys ignored.
	if env := getEnvelope(t, h, hostID); !env.Config.Enabled {
		t.Fatalf("stale row did not resolve: %+v", env.Config)
	}

	rec := httptest.NewRecorder()
	h.handlePatch(rec, patchRequest(t, hostID, map[string]any{"stream": false}))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no longer a setting") {
		t.Fatalf("PATCH of a retired key: status = %d, body = %s, want 400 saying it is no longer a setting", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.handlePatch(rec, patchRequest(t, hostID, map[string]any{"auto_start_on_display": true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, body = %s", rec.Code, rec.Body.String())
	}
	stored, err := store.Get(ctx, hostID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(stored) != 2 || stored["enabled"] != true || stored["auto_start_on_display"] != true {
		t.Fatalf("stored config = %v, want only enabled and auto_start_on_display", stored)
	}
}
