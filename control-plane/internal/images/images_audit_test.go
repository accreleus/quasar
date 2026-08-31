package images

// images_audit_test.go — the six image-management actions were unrecorded (UI
// v3 amendment §3). An install moves what every host in the fleet runs, so it
// belongs in the operator's history. Requires Postgres: make test-db.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func imageAuditActions(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT action FROM admin_activity ORDER BY id`)
	mustT(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		mustT(t, rows.Scan(&a))
		out = append(out, a)
	}
	return out
}

func imageAuditRow(t *testing.T, pool *pgxpool.Pool, action string) (target string, details map[string]any) {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(target_id, ''), details FROM admin_activity
		WHERE action = $1 ORDER BY id DESC LIMIT 1`, action).Scan(&target, &raw); err != nil {
		t.Fatalf("read %s row: %v", action, err)
	}
	mustT(t, json.Unmarshal(raw, &details))
	return target, details
}

func clearActivity(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `DELETE FROM admin_activity`)
	mustT(t, err)
}

func TestImageActionsAreAudited(t *testing.T) {
	env, _ := newActionsEnv(t, "host-a")
	seedCatalogDigest(t, env.pool, imgVer, imgDigest)
	clearActivity(t, env.pool)

	steps := []struct {
		method, path, body string
		want               int
		action             string
	}{
		{http.MethodPost, "/install", `{"lazy":false}`, http.StatusCreated, "image.installed"},
		{http.MethodPost, "/pin", "", http.StatusNoContent, "image.pinned"},
		{http.MethodDelete, "/pin", "", http.StatusNoContent, "image.unpinned"},
		{http.MethodDelete, "/install", "", http.StatusNoContent, "image.uninstalled"},
	}
	var want []string
	for _, s := range steps {
		code, body := env.do(t, s.method, "/v1/admin/images/"+imgID+s.path, s.body)
		if code != s.want {
			t.Fatalf("%s %s: status %d body %s, want %d", s.method, s.path, code, body, s.want)
		}
		want = append(want, s.action)
	}

	got := imageAuditActions(t, env.pool)
	if len(got) != len(want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("recorded %v, want %v", got, want)
		}
	}
	if target, details := imageAuditRow(t, env.pool, "image.installed"); target != imgID {
		t.Errorf("image.installed target_id = %q, want %q (details %v)", target, imgID, details)
	}
}

// TestImageUpdateAndSyncAreAudited: update carries whether it actually moved
// (applied:false is a 200 no-op), and a sync records the attempt even when the
// fetch failed — that is the row explaining a stale catalog.
func TestImageUpdateAndSyncAreAudited(t *testing.T) {
	env, _ := newActionsEnv(t, "host-a")
	seedCatalogDigest(t, env.pool, imgVer, imgDigest)

	code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", `{"lazy":false}`)
	if code != http.StatusCreated {
		t.Fatalf("install: status %d body %s", code, body)
	}
	clearActivity(t, env.pool)

	seedCatalogDigest(t, env.pool, imgVer2, imgDigest2)
	if code, body = env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/update", ""); code != http.StatusOK {
		t.Fatalf("update: status %d body %s, want 200", code, body)
	}
	target, details := imageAuditRow(t, env.pool, "image.updated")
	if target != imgID {
		t.Errorf("image.updated target_id = %q, want %q", target, imgID)
	}
	if applied, ok := details["applied"].(bool); !ok || !applied {
		t.Errorf("image.updated details = %v, want applied:true", details)
	}

	clearActivity(t, env.pool)
	if code, body = env.do(t, http.MethodPost, "/v1/admin/images/sync", ""); code != http.StatusOK {
		t.Fatalf("sync: status %d body %s, want 200", code, body)
	}
	if _, details = imageAuditRow(t, env.pool, "image.synced"); details["sync_error"] == nil {
		t.Errorf("image.synced details = %v, want a sync_error flag", details)
	}
}

// TestRefusedImageActionIsNotAudited: a 409 changed nothing on any host, so the
// feed must not report an update that never happened.
func TestRefusedImageActionIsNotAudited(t *testing.T) {
	env, _ := newActionsEnv(t, "host-a")
	seedCatalogDigest(t, env.pool, imgVer, imgDigest)

	if code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", ""); code != http.StatusCreated {
		t.Fatalf("install: status %d body %s", code, body)
	}
	if code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/pin", ""); code != http.StatusNoContent {
		t.Fatalf("pin: status %d body %s", code, body)
	}
	clearActivity(t, env.pool)

	// Pinned: update is a 409 by contract.
	code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/update", "")
	if code != http.StatusConflict {
		t.Fatalf("update while pinned: status %d body %s, want 409", code, body)
	}
	if got := imageAuditActions(t, env.pool); len(got) != 0 {
		t.Errorf("a refused update recorded %v", got)
	}
}
