package compat

// remove_db_test.go — C10 (#1074): disabling, deleting and force-removing a tool, against a real
// database (make test-db). Semantics: control-api.md §Compatibility tools.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

// session seeds a session of cadmin in state, created at created, holding the given mounted names.
func (h *harness) session(state string, created time.Time, names ...string) string {
	h.t.Helper()
	ctx := context.Background()
	var app, id string
	if err := h.pool.QueryRow(ctx, `SELECT id::text FROM apps WHERE name = 'compat-app'`).Scan(&app); err != nil {
		if err := h.pool.QueryRow(ctx, `INSERT INTO apps (name, default_width, default_height, default_fps, default_bitrate_kbps)
			VALUES ('compat-app', 1280, 720, 60, 6000) RETURNING id::text`).Scan(&app); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := h.pool.QueryRow(ctx, `INSERT INTO sessions (user_id, app_id, state, width, height, fps, bitrate_kbps, created_at, compat_tool_names)
		VALUES ((SELECT id FROM users WHERE username = 'cadmin'), $1::uuid, $2, 1280, 720, 60, 6000, $3, $4::text[]) RETURNING id::text`,
		app, state, created, names).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) sessionState(id string) string {
	h.t.Helper()
	var s string
	if err := h.pool.QueryRow(context.Background(), `SELECT state FROM sessions WHERE id = $1::uuid`, id).Scan(&s); err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *harness) forceRemove(id, token string) (int, map[string]any) {
	return h.do("POST", "/v1/admin/compat-tools/"+id+"/force-remove", token, `{}`)
}

func TestForceRemoveClearsMappingsAndDefaultStopsHoldersAndAudits(t *testing.T) {
	h := newHarness(t)
	a := h.host("a", true)
	id, sha, name := h.ge()
	h.enable(id, true)
	h.exec(`INSERT INTO host_compat_tools (host_id, name, tag, sha512, bytes) VALUES ($1::uuid, $2, $3, $4, 5)`, a, name, recommended[0].Tag, sha)
	h.exec(`INSERT INTO compat_tool_mappings (provider, external_id, tool_id) VALUES ('steam', '480', $1::uuid), ('steam', '570', $1::uuid)`, id)
	h.exec(`UPDATE instance_settings SET compat_tool_default_id = $1::uuid`, id)
	past := time.Now().Add(-time.Minute)
	holder := h.session("running", past, name)
	other := h.session("running", past, "GE-Proton9-1")
	h.session("stopped", past, name)

	code, out := h.forceRemove(id, h.admin)
	if code != http.StatusOK || out["mappings_cleared"] != 2.0 || out["default_cleared"] != true || out["sessions_stopped"] != 1.0 {
		t.Fatalf("force-remove: %d %v", code, out)
	}
	if tool, _ := out["tool"].(map[string]any); tool["enabled"] != false || tool["mappings"] != 0.0 || tool["is_default"] != false {
		t.Errorf("tool after force-remove = %v", tool)
	}
	if got := strings.Join(h.stopped, ","); got != holder {
		t.Errorf("stopped %q, want only the session that mounted the tool (%s)", got, holder)
	}
	if s := h.sessionState(other); s != "running" {
		t.Errorf("a session on another tool is %s", s)
	}
	if n := h.count(`SELECT count(*) FROM compat_tool_mappings WHERE tool_id = $1::uuid`, id); n != 0 {
		t.Errorf("%d mappings left", n)
	}
	if n := h.count(`SELECT count(*) FROM instance_settings WHERE compat_tool_default_id IS NOT NULL`); n != 0 {
		t.Error("the fleet default still names the tool")
	}
	if n := h.count(`SELECT count(*) FROM admin_activity WHERE action = 'compat_tool.force_remove' AND target_id = $1
		AND (details->>'mappings_cleared')::int = 2 AND (details->>'sessions_stopped')::int = 1 AND details->>'default_cleared' = 'true'
		AND details->'stopped_session_ids' = to_jsonb(ARRAY[$2::text]) AND jsonb_typeof(details->'host_ids') = 'array' AND (details->>'host_count')::int = (SELECT count(*) FROM hosts)`, id, holder); n != 1 {
		t.Errorf("%d force_remove audit rows with the counts, the stopped session ids and a host list, want 1", n)
	}

	// The host is told to remove the tool without waiting for anything.
	run := h.claim("a")
	if len(run) != 1 || run[0]["params"].(map[string]any)["action"] != "remove" || run[0]["params"].(map[string]any)["name"] != name {
		t.Fatalf("host runs = %v, want the removal of %s", run, name)
	}
	// Every holder ended in the first pass, so the mark is already clear.
	if n := h.count(`SELECT count(*) FROM compat_tools WHERE id = $1::uuid AND force_remove_pending_at IS NOT NULL`, id); n != 0 {
		t.Error("the mark stayed after every holder ended")
	}
}

func TestTheStopStepIsRetriedUntilEveryHolderEnds(t *testing.T) {
	h := newHarness(t)
	id, _, name := h.ge()
	holder := h.session("running", time.Now().Add(-time.Minute), name)
	h.stopErr = errors.New("agent unreachable")
	if code, out := h.forceRemove(id, h.admin); code != http.StatusOK || out["sessions_stopped"] != 0.0 {
		t.Fatalf("force-remove with a failing stop: %d %v", code, out)
	}
	if err := h.store.StopHolders(context.Background()); err == nil {
		t.Error("a failed stop is not reported")
	}
	if n := h.count(`SELECT count(*) FROM compat_tools WHERE id = $1::uuid AND force_remove_pending_at IS NOT NULL`, id); n != 1 {
		t.Error("the mark cleared while a holder was still live")
	}
	// A session that mounts the name after the mark is not the stop step's to end.
	late := h.session("running", time.Now().Add(time.Minute), name)
	h.mu.Lock()
	h.stopErr = nil
	h.mu.Unlock()
	if err := h.store.StopHolders(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.sessionState(holder) != "stopped" || h.sessionState(late) != "running" {
		t.Errorf("holder %s, later session %s: want the holder stopped and the later one left", h.sessionState(holder), h.sessionState(late))
	}
	if n := h.count(`SELECT count(*) FROM compat_tools WHERE id = $1::uuid AND force_remove_pending_at IS NOT NULL`, id); n != 0 {
		t.Error("the mark stayed after every session it targets ended")
	}
}

func TestForceRemoveRefusals(t *testing.T) {
	h := newHarness(t)
	id, _, name := h.ge()
	if code, body := h.forceRemove(h.id["proton_10"], h.admin); code != http.StatusConflict || code_(body) != "conflict" {
		t.Errorf("a Valve build: %d %v, want 409 conflict", code, body)
	}
	if code, _ := h.forceRemove(zeroHost, h.admin); code != http.StatusNotFound {
		t.Errorf("an unknown tool: %d, want 404", code)
	}
	if code, _ := h.forceRemove(id, h.user); code != http.StatusForbidden {
		t.Errorf("as a user: %d, want 403", code)
	}
	// A repeat on a disabled tool still stops what holds the name.
	h.session("running", time.Now().Add(-time.Minute), name)
	if code, out := h.forceRemove(id, h.admin); code != http.StatusOK || out["sessions_stopped"] != 1.0 {
		t.Errorf("force-remove of a never-enabled tool: %d %v", code, out)
	}
}

func TestDeleteRemovesANeverEnabledTagRowOnly(t *testing.T) {
	h := newHarness(t)
	tagID := h.addTag()
	if code, _ := h.do("DELETE", "/v1/admin/compat-tools/"+tagID, h.user, ""); code != http.StatusForbidden {
		t.Errorf("as a user: %d, want 403", code)
	}
	if code, _ := h.do("DELETE", "/v1/admin/compat-tools/"+zeroHost, h.admin, ""); code != http.StatusNotFound {
		t.Errorf("an unknown tool: %d, want 404", code)
	}
	if code, body := h.do("DELETE", "/v1/admin/compat-tools/"+h.id["proton_10"], h.admin, ""); code != http.StatusConflict || code_(body) != "conflict" {
		t.Errorf("a Valve build: %d %v, want 409", code, body)
	}
	recID, _, _ := h.ge()
	if code, _ := h.do("DELETE", "/v1/admin/compat-tools/"+recID, h.admin, ""); code != http.StatusConflict {
		t.Errorf("a recommended row the release still ships: %d, want 409", code)
	}
	if code, _ := h.do("DELETE", "/v1/admin/compat-tools/"+tagID, h.admin, ""); code != http.StatusNoContent {
		t.Fatalf("a never-enabled tag row: %d, want 204", code)
	}
	if n := h.count(`SELECT count(*) FROM compat_tools WHERE id = $1::uuid`, tagID); n != 0 {
		t.Error("the row is still there")
	}
	if n := h.count(`SELECT count(*) FROM admin_activity WHERE action = 'compat_tool.delete' AND target_id = $1 AND details->>'tag' = $2
		AND details->'host_ids' = '[]'::jsonb`, tagID, addTag); n != 1 {
		t.Errorf("%d delete audit rows, want 1", n)
	}
	// A row that was ever enabled goes through disable, never delete.
	again := h.addTag()
	h.enable(again, true)
	h.enable(again, false)
	if code, _ := h.do("DELETE", "/v1/admin/compat-tools/"+again, h.admin, ""); code != http.StatusConflict {
		t.Errorf("a tool once enabled: %d, want 409", code)
	}
	// A recommended row the current release no longer ships is a never-enabled row too.
	h.exec(`UPDATE compat_tools SET recommended = false WHERE id = $1::uuid`, recID)
	if code, _ := h.do("DELETE", "/v1/admin/compat-tools/"+recID, h.admin, ""); code != http.StatusNoContent {
		t.Errorf("an unshipped never-enabled row: %d, want 204", code)
	}
}

func TestDeleteOfAStuckPinConflictInsertsTheShippedEntry(t *testing.T) {
	h := newHarness(t)
	r := recommended[0]
	id, _, name := h.ge()
	h.exec(`UPDATE compat_tools SET asset_sha512 = $2, pin_conflict = true, first_enabled_at = now() WHERE id = $1::uuid`, id, strings.Repeat("ef", 64))
	del := func() int {
		code, _ := h.do("DELETE", "/v1/admin/compat-tools/"+id, h.admin, "")
		return code
	}
	h.exec(`INSERT INTO compat_tool_mappings (provider, external_id, tool_id) VALUES ('steam', '480', $1::uuid)`, id)
	if code := del(); code != http.StatusConflict {
		t.Errorf("a mapped tool: %d, want 409", code)
	}
	h.exec(`DELETE FROM compat_tool_mappings`)
	host := h.host("a", true)
	h.exec(`INSERT INTO host_compat_tools (host_id, name, tag, sha512, bytes) VALUES ($1::uuid, $2, $3, $4, 5)`, host, name, r.Tag, strings.Repeat("ef", 64))
	if code := del(); code != http.StatusConflict {
		t.Errorf("a tool a host still holds: %d, want 409", code)
	}
	h.exec(`DELETE FROM host_compat_tools`)
	live := h.session("running", time.Now(), name)
	if code := del(); code != http.StatusConflict {
		t.Errorf("a tool a live session mounts: %d, want 409", code)
	}
	h.exec(`UPDATE sessions SET state = 'stopped' WHERE id = $1::uuid`, live)
	if code := del(); code != http.StatusNoContent {
		t.Fatalf("a stuck pin conflict nothing uses: %d, want 204", code)
	}
	var sha string
	var rec, pin, enabled bool
	if err := h.pool.QueryRow(context.Background(), `SELECT asset_sha512, recommended, pin_conflict, enabled FROM compat_tools WHERE tag = $1`,
		r.Tag).Scan(&sha, &rec, &pin, &enabled); err != nil || sha != r.SHA512 || !rec || pin || enabled {
		t.Errorf("the shipped entry: sha ok=%v recommended=%v pin_conflict=%v enabled=%v (%v)", sha == r.SHA512, rec, pin, enabled, err)
	}
}

// A mapping PUT holds FOR SHARE on the tool row until it commits. The disable must count mappings
// in a statement that starts after it has the row's FOR UPDATE, or it misses that mapping.
func TestDisableCountsMappingsAfterTakingTheToolLock(t *testing.T) {
	h := newHarness(t)
	id, _, _ := h.ge()
	h.enable(id, true)
	ctx := context.Background()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT 1 FROM compat_tools WHERE id = $1::uuid FOR SHARE`, id); err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	go func() {
		_, err := h.store.SetEnabled(ctx, id, false)
		res <- err
	}()
	select {
	case err := <-res:
		t.Fatalf("the disable did not wait for the mapping writer's share lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := tx.Exec(ctx, `INSERT INTO compat_tool_mappings (provider, external_id, tool_id) VALUES ('steam', '480', $1::uuid), ('steam', '570', $1::uuid)`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var inUse *InUseError
	if err := <-res; !errors.As(err, &inUse) || inUse.Mappings != 2 {
		t.Fatalf("the disable returned %v, want in_use naming the 2 mappings", err)
	}
	if n := h.count(`SELECT count(*) FROM compat_tools WHERE id = $1::uuid AND enabled`, id); n != 1 {
		t.Error("the tool was disabled under a mapping")
	}
}

func TestDisablingAnInstalledToolRemovesItFromEveryHost(t *testing.T) {
	h := newHarness(t)
	a, b := h.host("a", true), h.host("b", true)
	id, sha, name := h.ge()
	h.enable(id, true)
	for _, host := range []string{a, b} {
		h.exec(`INSERT INTO host_compat_tools (host_id, name, tag, sha512, bytes) VALUES ($1::uuid, $2, $3, $4, 1717986918)`, host, name, recommended[0].Tag, sha)
	}
	h.exec(`DELETE FROM job_runs WHERE job_id = $1`, JobID)
	if code, _ := h.enable(id, false); code != http.StatusOK {
		t.Fatalf("disable: %d", code)
	}
	for _, node := range []string{"a", "b"} {
		runs := h.claim(node)
		if len(runs) != 1 || runs[0]["params"].(map[string]any)["action"] != "remove" || runs[0]["params"].(map[string]any)["name"] != name {
			t.Fatalf("host %s runs = %v, want the removal of %s", node, runs, name)
		}
		h.report(node, runs[0]["run_id"].(string), "succeeded", map[string]any{"action": "remove", "tool_id": id, "name": name, "sha512": sha, "bytes": 0, "already_installed": false})
	}
	if n := h.count(`SELECT count(*) FROM admin_activity WHERE action = 'compat_tool.remove' AND details->>'state' = 'succeeded'`); n != 2 {
		t.Errorf("%d removal audit rows, want one per host", n)
	}
}

// Two id lists of a big force-remove must still fit the audit row's 4096-byte bound, or the row is
// lost; each list is cut and says so.
func TestAForceRemoveOfManySessionsStillWritesItsAuditRow(t *testing.T) {
	h := newHarness(t)
	id, _, name := h.ge()
	for i := 0; i < 120; i++ {
		h.host(fmt.Sprintf("m%d", i), true)
		h.session("running", time.Now().Add(-time.Minute), name)
	}
	if code, out := h.forceRemove(id, h.admin); code != http.StatusOK || out["sessions_stopped"] != 120.0 {
		t.Fatalf("force-remove: %d %v", code, out)
	}
	if n := h.count(`SELECT count(*) FROM admin_activity WHERE action = 'compat_tool.force_remove' AND target_id = $1
		AND jsonb_array_length(details->'stopped_session_ids') <= 100 AND details->>'stopped_session_ids_truncated' = 'true'
		AND details->>'host_ids_truncated' = 'true' AND (details->>'host_count')::int = (SELECT count(*) FROM hosts)`, id); n != 1 {
		t.Errorf("%d force_remove audit rows for 120 sessions and 120 hosts, want 1 (truncated, host_count = every host)", n)
	}
}

func TestAForceRemoveWaitingOverTenMinutesWarnsEveryPass(t *testing.T) {
	h := newHarness(t)
	id, _, name := h.ge()
	holder := h.session("running", time.Now().Add(-time.Hour), name)
	h.stopErr = errors.New("agent unreachable")
	h.forceRemove(id, h.admin)
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	_ = h.store.StopHolders(context.Background())
	if strings.Contains(buf.String(), "has waited") {
		t.Fatalf("warned before the force-remove was 10 minutes old: %s", buf.String())
	}
	h.exec(`UPDATE compat_tools SET force_remove_pending_at = now() - interval '11 minutes' WHERE id = $1::uuid`, id)
	_ = h.store.StopHolders(context.Background())
	_ = h.store.StopHolders(context.Background())
	if got := strings.Count(buf.String(), "has waited"); got != 2 || !strings.Contains(buf.String(), holder) || !strings.Contains(buf.String(), "holders=1") {
		t.Errorf("warnings = %d, want one per pass naming the tool, the holder count and the oldest holder %s:\n%s", got, holder, buf.String())
	}
}
