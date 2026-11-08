package compat

// converge_db_test.go — amendment 56 (#1071): enabling a recommended GE tool makes it desired on
// every compat_tools_v1 host, runs carry the host's first difference when claimed, failed holds,
// and the per-host install state. Against a real database (make test-db).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const zeroHost = "00000000-0000-0000-0000-000000000000"

// nodeSecrets authenticates an agent whose secret is its node name (the test's stand-in for the
// node_secret check).
type nodeSecrets struct{ pool *pgxpool.Pool }

func (n nodeSecrets) AuthAgentHost(ctx context.Context, node, secret string) (string, error) {
	if node != secret {
		return "", errors.New("bad secret")
	}
	var id string
	err := n.pool.QueryRow(ctx, `SELECT id::text FROM hosts WHERE node_name = $1`, node).Scan(&id)
	return id, err
}

func (h *harness) host(name string, supported bool) string {
	h.t.Helper()
	var id string
	if err := h.pool.QueryRow(context.Background(), `INSERT INTO hosts (node_name) VALUES ($1) RETURNING id::text`,
		"compat-"+name).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	h.setSupported(id, supported)
	return id
}

func (h *harness) setSupported(id string, v bool) {
	h.mu.Lock()
	h.supported[id] = v
	h.mu.Unlock()
}

// agent calls an agent route as the named host.
func (h *harness) agent(method, path, node string, body any) (int, map[string]any) {
	h.t.Helper()
	var rdr *bytes.Reader
	b, _ := json.Marshal(body)
	rdr = bytes.NewReader(b)
	req, _ := http.NewRequest(method, h.srv.URL+path, rdr)
	req.Header.Set("Authorization", "Bearer compat-"+node)
	req.Header.Set("X-Quasar-Node", "compat-"+node)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// claim takes the host's pending converge runs as its agent would.
func (h *harness) claim(node string) []map[string]any {
	h.t.Helper()
	code, out := h.agent("GET", "/v1/agent/jobs/pending", node, nil)
	if code != http.StatusOK {
		h.t.Fatalf("pending: %d %v", code, out)
	}
	var runs []map[string]any
	for _, r := range out["runs"].([]any) {
		runs = append(runs, r.(map[string]any))
	}
	return runs
}

func (h *harness) report(node, runID, state string, summary map[string]any) {
	h.t.Helper()
	body := map[string]any{"run_id": runID, "state": state, "summary": summary}
	if code, out := h.agent("POST", "/v1/agent/jobs/report", node, body); code != http.StatusOK {
		h.t.Fatalf("report: %d %v", code, out)
	}
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(), sql, args...); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) count(sql string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) ge() (id, sha, name string) {
	h.t.Helper()
	r := recommended[0]
	if err := h.pool.QueryRow(context.Background(), `SELECT id::text FROM compat_tools WHERE tag = $1`, r.Tag).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id, r.SHA512, r.SteamName
}

func (h *harness) card(host string) map[string]any {
	h.t.Helper()
	code, out := h.do("GET", "/v1/admin/hosts/"+host+"/compat-tools", h.admin, "")
	if code != http.StatusOK {
		h.t.Fatalf("host card: %d %v", code, out)
	}
	return out
}

func toolState(card map[string]any, toolID string) map[string]any {
	for _, t := range card["tools"].([]any) {
		if m := t.(map[string]any); m["tool_id"] == toolID {
			return m
		}
	}
	return nil
}

func (h *harness) hostCounts(toolID string) map[string]any {
	h.t.Helper()
	_, out := h.do("GET", "/v1/admin/compat-tools", h.admin, "")
	for _, it := range out["items"].([]any) {
		if m := it.(map[string]any); m["id"] == toolID {
			return m["hosts"].(map[string]any)
		}
	}
	h.t.Fatalf("tool %s not listed", toolID)
	return nil
}

func (h *harness) enable(id string, on bool) (int, map[string]any) {
	h.t.Helper()
	body := `{"enabled":false}`
	if on {
		body = `{"enabled":true}`
	}
	return h.do("PATCH", "/v1/admin/compat-tools/"+id, h.admin, body)
}

func TestEnablingARecommendedToolInstallsItOnEverySupportedHost(t *testing.T) {
	h := newHarness(t)
	a, b, old := h.host("a", true), h.host("b", true), h.host("old", false)
	id, sha, name := h.ge()

	code, tool := h.enable(id, true)
	if code != http.StatusOK || tool["enabled"] != true {
		t.Fatalf("enable: %d %v", code, tool)
	}
	var details string
	if err := h.pool.QueryRow(context.Background(), `SELECT details::text FROM admin_activity
		WHERE action = 'compat_tool.enable' AND target_id = $1`, id).Scan(&details); err != nil {
		t.Fatalf("no enable audit row: %v", err)
	}
	for _, want := range []string{sha, a, b, old} {
		if !strings.Contains(details, want) {
			t.Errorf("enable audit %s lacks %s", details, want)
		}
	}
	if n := h.count(`SELECT count(*) FROM job_runs WHERE job_id = $1 AND state = 'pending'`, JobID); n != 2 {
		t.Errorf("%d pending runs, want one per compat_tools_v1 host", n)
	}
	if n := h.count(`SELECT count(*) FROM job_runs WHERE job_id = $1 AND host_id = $2::uuid`, JobID, old); n != 0 {
		t.Error("a host without compat_tools_v1 was given an install run")
	}
	counts := h.hostCounts(id)
	others := h.count(`SELECT count(*) FROM hosts WHERE node_name NOT LIKE 'compat-%'`)
	if counts["installing"] != 2.0 || counts["unsupported"] != float64(1+others) {
		t.Errorf("host counts = %v, want 2 installing (a run is due) and the rest unsupported", counts)
	}
	if code, again := h.enable(id, true); code != http.StatusOK || again["enabled"] != true {
		t.Errorf("a repeat enable: %d %v", code, again)
	}
	if n := h.count(`SELECT count(*) FROM admin_activity WHERE action = 'compat_tool.enable'`); n != 1 {
		t.Errorf("%d enable audit rows, want the repeat unaudited", n)
	}

	runs := h.claim("a")
	if len(runs) != 1 {
		t.Fatalf("host a claimed %d runs", len(runs))
	}
	p := runs[0]["params"].(map[string]any)
	if p["action"] != "install" || p["tag"] != "GE-Proton11-12" || p["asset"] != "x86_64" || p["sha512"] != sha ||
		p["expected_name"] != name || p["reserve_bytes"] != float64(10<<30) || p["size_bytes"] != 566859228.0 {
		t.Errorf("install params = %v", p)
	}
	for _, k := range []string{"url", "host", "path"} {
		if _, ok := p[k]; ok {
			t.Errorf("params carry %q (T1: only the tag)", k)
		}
	}
	if st := toolState(h.card(a), id); st["state"] != "installing" {
		t.Errorf("host a while running: %v", st)
	}

	h.report("a", runs[0]["run_id"].(string), "succeeded", map[string]any{"action": "install", "tool_id": id,
		"name": name, "sha512": sha, "bytes": 1602122207, "already_installed": false})
	h.exec(`INSERT INTO host_compat_tools (host_id, name, tag, sha512, bytes) VALUES ($1::uuid, $2, 'GE-Proton11-12', $3, 1602122207)`, a, name, sha)
	h.exec(`UPDATE hosts SET compat_tools_free_bytes = 182536110080, compat_tools_reported_at = now() WHERE id = $1::uuid`, a)
	card := h.card(a)
	st := toolState(card, id)
	if card["supported"] != true || card["free_bytes"] != 182536110080.0 || st["state"] != "installed" || st["bytes"] != 1602122207.0 {
		t.Errorf("host a card after the install: %v", card)
	}
	if c := h.card(old); c["supported"] != false || c["free_bytes"] != nil || toolState(c, id)["state"] != "unsupported" {
		t.Errorf("unsupported host card: %v", c)
	}
	var actor string
	if err := h.pool.QueryRow(context.Background(), `SELECT COALESCE(actor_user_id::text, '') FROM admin_activity
		WHERE action = 'compat_tool.install' AND target_id = $1`, a).Scan(&actor); err != nil || actor == "" {
		t.Errorf("install audit actor = %q (%v), want the enabling admin", actor, err)
	}

	// An agent that reconnects with compat_tools_v1 catches up.
	h.setSupported(old, true)
	h.conv.Host(context.Background(), old, false)
	if n := h.count(`SELECT count(*) FROM job_runs WHERE job_id = $1 AND host_id = $2::uuid AND state = 'pending'`, JobID, old); n != 1 {
		t.Errorf("the reconnected host has %d pending runs, want 1", n)
	}
}

func TestAFailedInstallHoldsUntilAnAdminReleasesIt(t *testing.T) {
	h := newHarness(t)
	b := h.host("b", true)
	id, sha, _ := h.ge()
	h.enable(id, true)

	run := h.claim("b")[0]
	h.report("b", run["run_id"].(string), "failed", map[string]any{"reason": "checksum_mismatch",
		"expected_sha512": sha, "actual_sha512": strings.Repeat("a", 128)})
	st := toolState(h.card(b), id)
	if st["state"] != "failed" || st["reason"] != "checksum_mismatch" {
		t.Errorf("after a checksum mismatch: %v", st)
	}
	if n := h.count(`SELECT count(*) FROM job_runs WHERE job_id = $1 AND state = 'pending'`, JobID); n != 0 {
		t.Errorf("%d runs re-materialized for a non-transient failure, want none (no reinstall loop)", n)
	}
	h.conv.Host(context.Background(), b, false)
	if n := h.count(`SELECT count(*) FROM job_runs WHERE job_id = $1 AND state = 'pending'`, JobID); n != 0 {
		t.Error("a capacity report or reconnect released a checksum hold")
	}
	if _, err := h.disp.RunNow(context.Background(), JobID, b, ""); err != nil {
		t.Fatal(err)
	}
	if again := h.claim("b"); len(again) != 1 || again[0]["params"].(map[string]any)["action"] != "install" {
		t.Errorf("Run now did not release the hold: %v", again)
	}
}

func TestATransientFailureRetriesAfterAnHour(t *testing.T) {
	h := newHarness(t)
	b := h.host("b", true)
	id, _, _ := h.ge()
	h.enable(id, true)
	run := h.claim("b")[0]
	h.report("b", run["run_id"].(string), "failed", map[string]any{"reason": "download_failed"})

	var at time.Time
	if err := h.pool.QueryRow(context.Background(), `SELECT scheduled_for FROM job_runs
		WHERE job_id = $1 AND state = 'pending'`, JobID).Scan(&at); err != nil {
		t.Fatalf("no run waiting for the hold's timer: %v", err)
	}
	if d := time.Until(at); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("the retry is due in %v, want 1 h", d)
	}
	h.conv.now = func() time.Time { return time.Now().Add(61 * time.Minute) }
	if p, err := h.conv.Params(context.Background(), b); err != nil || p == nil {
		t.Errorf("after the hour the install is a difference again: %v %v", p, err)
	}
}

func TestADeferredRetryRunsOnCurrentDesiredState(t *testing.T) {
	h := newHarness(t)
	h.host("b", true)
	id, _, _ := h.ge()
	h.enable(id, true)
	run := h.claim("b")[0]
	h.report("b", run["run_id"].(string), "deferred", map[string]any{"reason": "insufficient_disk"})
	if st := toolState(h.card(h.hostID("b")), id); st["state"] != "waiting" || st["reason"] != "insufficient_disk" {
		t.Errorf("deferred: %v", st)
	}
	if code, _ := h.enable(id, false); code != http.StatusOK {
		t.Fatalf("disable: %d", code)
	}
	if runs := h.claim("b"); len(runs) != 0 {
		t.Errorf("the retry was handed out with its old params after the tool was disabled: %v", runs)
	}
	var reason string
	if err := h.pool.QueryRow(context.Background(), `SELECT summary->>'reason' FROM job_runs
		WHERE job_id = $1 AND state = 'skipped'`, JobID).Scan(&reason); err != nil || reason != "no_difference" {
		t.Errorf("the retry closed with %q (%v), want skipped no_difference", reason, err)
	}
}

func (h *harness) hostID(name string) string {
	var id string
	if err := h.pool.QueryRow(context.Background(), `SELECT id::text FROM hosts WHERE node_name = $1`, "compat-"+name).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func TestRemovalsComeFirstAndCarryTheHostsOwnName(t *testing.T) {
	h := newHarness(t)
	a := h.host("a", true)
	id, _, _ := h.ge()
	stray := strings.Repeat("c", 128)
	h.exec(`INSERT INTO host_compat_tools (host_id, name, tag, sha512, bytes) VALUES ($1::uuid, 'GE-Proton9-1', 'GE-Proton9-1', $2, 5)`, a, stray)
	h.enable(id, true)
	p := h.claim("a")[0]["params"].(map[string]any)
	if p["action"] != "remove" || p["name"] != "GE-Proton9-1" || p["sha512"] != stray || p["tool_id"] != nil {
		t.Errorf("first difference = %v, want the stray removal", p)
	}
	card := h.card(a)
	var listed bool
	for _, e := range card["tools"].([]any) {
		m := e.(map[string]any)
		listed = listed || (m["tool_id"] == nil && m["steam_name"] == "GE-Proton9-1")
	}
	if !listed {
		t.Errorf("an installed tool no catalogue row matches is not on the card: %v", card)
	}
}

func TestEnableAndDisableRefusals(t *testing.T) {
	h := newHarness(t)
	id, _, _ := h.ge()
	h.exec(`UPDATE instance_settings SET compat_tools_max_enabled = 1`)
	h.exec(`INSERT INTO compat_tools (family, source, tag, display_name, enabled, asset_name, asset_size_bytes, asset_sha512)
		VALUES ('ge', 'tag', 'GE-Proton10-1', 'GE-Proton10-1', true, 'GE-Proton10-1.tar.gz', 5, $1)`, strings.Repeat("d", 128))
	if code, body := h.enable(id, true); code != http.StatusConflict || code_(body) != codeToolLimit {
		t.Errorf("past the cap: %d %v", code, body)
	}
	h.exec(`UPDATE instance_settings SET compat_tools_max_enabled = 8`)
	h.exec(`UPDATE compat_tools SET pin_conflict = true WHERE id = $1::uuid`, id)
	if code, body := h.enable(id, true); code != http.StatusConflict || code_(body) != codeToolPinConflict {
		t.Errorf("a pin conflict: %d %v", code, body)
	}
	h.exec(`UPDATE compat_tools SET pin_conflict = false WHERE id = $1::uuid`, id)
	if code, _ := h.enable(id, true); code != http.StatusOK {
		t.Fatalf("enable: %d", code)
	}
	h.exec(`UPDATE instance_settings SET compat_tool_default_id = $1::uuid`, id)
	if code, body := h.enable(id, false); code != http.StatusConflict || code_(body) != codeToolInUse {
		t.Errorf("disabling the default: %d %v", code, body)
	}
	if code, _ := h.do("PATCH", "/v1/admin/compat-tools/"+id, h.admin, `{"enabled":true,"x":1}`); code != http.StatusBadRequest {
		t.Errorf("an unknown key: %d, want 400", code)
	}
	if code, _ := h.do("PATCH", "/v1/admin/compat-tools/"+zeroHost, h.admin, `{"enabled":true}`); code != http.StatusNotFound {
		t.Errorf("an unknown tool: %d, want 404", code)
	}
	if code, _ := h.do("PATCH", "/v1/admin/compat-tools/"+id, h.user, `{"enabled":true}`); code != http.StatusForbidden {
		t.Errorf("as a user: %d, want 403", code)
	}
	if code, _ := h.do("GET", "/v1/admin/hosts/"+zeroHost+"/compat-tools", h.admin, ""); code != http.StatusNotFound {
		t.Errorf("an unknown host card: %d, want 404", code)
	}
}

func TestAGEMappingReachesOnlyAHostThatHoldsTheTool(t *testing.T) {
	h := newHarness(t)
	a, b := h.host("a", true), h.host("b", true)
	id, sha, name := h.ge()
	h.enable(id, true)
	h.exec(`INSERT INTO host_compat_tools (host_id, name, tag, sha512, bytes) VALUES ($1::uuid, $2, 'GE-Proton11-12', $3, 1)`, a, name, sha)
	h.exec(`INSERT INTO compat_tool_mappings (provider, external_id, tool_id, forced) VALUES ('steam', '1145360', $1::uuid, true)`, id)
	s := NewStore(h.pool)
	entries, mount, err := s.Entries(context.Background(), a)
	if err != nil || len(mount) != 1 || mount[0] != name || len(entries) != 1 || entries[0] != (Entry{"1145360", name, true}) {
		t.Errorf("host a: entries %v mount %v (%v)", entries, mount, err)
	}
	entries, mount, err = s.Entries(context.Background(), b)
	if err != nil || len(mount) != 0 || len(entries) != 0 {
		t.Errorf("host b lacks the tool: entries %v mount %v (%v), want neither", entries, mount, err)
	}
	h.exec(`UPDATE host_compat_tools SET name = 'GE-Proton11-12-other' WHERE host_id = $1::uuid`, a)
	if _, mount, _ := s.Entries(context.Background(), a); len(mount) != 0 {
		t.Error("a tool installed under another name was mounted")
	}
}

func code_(body map[string]any) any {
	e, _ := body["error"].(map[string]any)
	return e["code"]
}
