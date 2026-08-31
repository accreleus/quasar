package crud

// #498 hardening tests: attribution logging + managed-row image validation on
// PATCH /v1/admin/runtime-presets/{id}. Require Postgres (TEST_DATABASE_URL);
// see presets_test.go's header — same shared-DB/-p 1 rules apply.

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// capturingHandler is a slog.Handler that records every emitted record so a
// test can assert a log line was written and inspect its attributes — there
// is no existing in-repo idiom for this (other tests only discard logs), so
// this is the minimal capture shim.
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (c *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	return nil
}

func (c *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capturingHandler) WithGroup(string) slog.Handler      { return c }

// findAttr returns the value of key on the first captured record whose
// message equals msg, and whether it was found at all.
func (c *capturingHandler) findAttr(msg, key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r.Message != msg {
			continue
		}
		var val any
		var found bool
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key {
				val, found = a.Value.Any(), true
				return false
			}
			return true
		})
		if found {
			return val, true
		}
	}
	return nil, false
}

func (c *capturingHandler) hasRecord(msg string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r.Message == msg {
			return true
		}
	}
	return false
}

// installedManagedImage seeds a minimal image_catalog row and returns its id,
// so a test can then point a runtime preset's managed_image_id at it (the
// managed-row shape #498 hardens against).
func installedManagedImage(t *testing.T, ctx context.Context, pool *pgxpool.Pool, imageID string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO image_catalog (id, manifest_version, display_name, kind, version, raw)
		VALUES ($1, 1, $1, 'prebuilt', '1.0', '{}'::jsonb)
	`, imageID); err != nil {
		t.Fatalf("seed image_catalog row %q: %v", imageID, err)
	}
}

// markPresetManaged points an existing preset's managed_image_id at imageID —
// the same shape internal/images/preset.go's insertManagedPreset writes at
// install, done directly here since materializing a full catalog install is
// out of scope for a crud-package test.
func markPresetManaged(t *testing.T, ctx context.Context, pool *pgxpool.Pool, presetID, imageID string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`UPDATE runtime_presets SET managed_image_id = $2 WHERE id::text = $1`, presetID, imageID); err != nil {
		t.Fatalf("mark preset %q managed by %q: %v", presetID, imageID, err)
	}
}

// TestManagedRuntimePresetImageValidation is the #498 regression: a managed
// preset's `image` may only be a digest ref or this image's own
// quasar-local/<id>: template prefix; anything else is refused with a 422 and
// the managed_preset_image_invalid code. The rejected fixture value
// ("quasar-xfce:dev") is the 2026-08-14 incident value verbatim.
func TestManagedRuntimePresetImageValidation(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@498managed.test", "managed498admin")

	const imageID = "kde-desktop"
	installedManagedImage(t, ctx, pool, imageID)

	_, body := post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{
		"name":  "KDE Desktop",
		"image": "ghcr.io/quasar/kde@sha256:" + strings.Repeat("a", 64),
	}, bearer)
	presetID := body["runtime_preset"].(map[string]any)["id"].(string)
	markPresetManaged(t, ctx, pool, presetID, imageID)

	// (1) managed + digest ref → accepted.
	resp, body := patch(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, map[string]any{
		"image": "ghcr.io/quasar/kde@sha256:" + strings.Repeat("b", 64),
	}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("managed preset + digest ref: want 200, got %d (%v)", resp.StatusCode, body)
	}

	// (2) managed + the 2026-08-14 incident value → 422, refused, code + message.
	resp, body = patch(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, map[string]any{
		"image": "quasar-xfce:dev",
	}, bearer)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("managed preset + hand-typed local tag: want 422, got %d (%v)", resp.StatusCode, body)
	}
	errBody, _ := body["error"].(map[string]any)
	if code, _ := errBody["code"].(string); code != "managed_preset_image_invalid" {
		t.Fatalf("want code managed_preset_image_invalid, got %v", errBody["code"])
	}
	msg, _ := errBody["message"].(string)
	if !strings.Contains(msg, "scratch") {
		t.Errorf("rejection message must point at creating a scratch preset instead, got: %q", msg)
	}
	// The rejected PATCH must not have written through.
	_, body = getReq(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, bearer)
	if got := body["runtime_preset"].(map[string]any)["image"]; got != "ghcr.io/quasar/kde@sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("a rejected managed-image PATCH must not change the stored image, got %v", got)
	}

	// (3) managed + this image's own quasar-local/<id>: template tag → accepted.
	resp, body = patch(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, map[string]any{
		"image": "quasar-local/" + imageID + ":v3",
	}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("managed preset + own quasar-local template tag: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if got := body["runtime_preset"].(map[string]any)["image"]; got != "quasar-local/"+imageID+":v3" {
		t.Fatalf("accepted template-tag PATCH did not apply: %v", got)
	}

	// A managed preset's OTHER fields patch normally — the guard is scoped to
	// `image` alone.
	resp, body = patch(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, map[string]any{
		"description": "touched",
	}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("managed preset non-image field PATCH: want 200, got %d (%v)", resp.StatusCode, body)
	}
}

// TestUnmanagedRuntimePresetImageStaysFreeText proves the guard is scoped to
// managed rows only — an admin-authored (scratch) preset keeps accepting any
// image string, exactly as before this hardening.
func TestUnmanagedRuntimePresetImageStaysFreeText(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@498unmanaged.test", "unmanaged498admin")

	_, body := post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{"name": "Scratch"}, bearer)
	presetID := body["runtime_preset"].(map[string]any)["id"].(string)

	resp, body := patch(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, map[string]any{
		"image": "quasar-xfce:dev",
	}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unmanaged preset + free-text image: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if got := body["runtime_preset"].(map[string]any)["image"]; got != "quasar-xfce:dev" {
		t.Fatalf("free-text image did not apply on an unmanaged preset: %v", got)
	}
}

// TestRuntimePresetPatchAttributionLogged is the #498 attribution-logging
// regression: a successful PATCH must emit one slog.Info line carrying the
// actor identity, the preset id, its managed_image_id, and the old->new image
// values — the write path this whole issue is about being unattributable.
func TestRuntimePresetPatchAttributionLogged(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@498attr.test", "attr498admin")

	const imageID = "attr-image"
	installedManagedImage(t, ctx, pool, imageID)

	_, body := post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{
		"name":  "Attributed",
		"image": "quasar-local/" + imageID + ":v1",
	}, bearer)
	presetID := body["runtime_preset"].(map[string]any)["id"].(string)
	markPresetManaged(t, ctx, pool, presetID, imageID)

	capLog := &capturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capLog))
	defer slog.SetDefault(prev)

	resp, body := patch(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, map[string]any{
		"image": "quasar-local/" + imageID + ":v2",
	}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch for attribution test: want 200, got %d (%v)", resp.StatusCode, body)
	}

	const msg = "admin PATCH: runtime preset updated"
	if !capLog.hasRecord(msg) {
		t.Fatalf("expected an attribution log line %q, none captured", msg)
	}
	if actorID, ok := capLog.findAttr(msg, "actor_id"); !ok || actorID == "" {
		t.Errorf("attribution log missing a non-empty actor_id, got %v (found=%v)", actorID, ok)
	}
	if pID, ok := capLog.findAttr(msg, "preset_id"); !ok || pID != presetID {
		t.Errorf("attribution log preset_id: want %q, got %v (found=%v)", presetID, pID, ok)
	}
	if mID, ok := capLog.findAttr(msg, "managed_image_id"); !ok || mID != imageID {
		t.Errorf("attribution log managed_image_id: want %q, got %v (found=%v)", imageID, mID, ok)
	}
	if oldImg, ok := capLog.findAttr(msg, "old_image"); !ok || oldImg != "quasar-local/"+imageID+":v1" {
		t.Errorf("attribution log old_image: want v1 tag, got %v (found=%v)", oldImg, ok)
	}
	if newImg, ok := capLog.findAttr(msg, "new_image"); !ok || newImg != "quasar-local/"+imageID+":v2" {
		t.Errorf("attribution log new_image: want v2 tag, got %v (found=%v)", newImg, ok)
	}
}

// TestRuntimePresetPatchNoopNotLogged: a PATCH whose only field matches the
// value already stored changes nothing, so it must not emit a misleading
// attribution line claiming a write happened.
func TestRuntimePresetPatchNoopNotLogged(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@498noop.test", "noop498admin")

	_, body := post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{
		"name":        "Noop",
		"description": "same",
	}, bearer)
	presetID := body["runtime_preset"].(map[string]any)["id"].(string)

	capLog := &capturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capLog))
	defer slog.SetDefault(prev)

	resp, body := patch(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, map[string]any{
		"description": "same",
	}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("noop patch: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if capLog.hasRecord("admin PATCH: runtime preset updated") {
		t.Errorf("a no-op PATCH (value unchanged) must not emit an attribution log line")
	}
}
