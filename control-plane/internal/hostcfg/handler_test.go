package hostcfg

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeDispatcher records every command Send'd to it. err, if set, is returned
// by every Send call (without recording it) — used to simulate an unreachable
// agent (e.g. dropped between the online-check and dispatch).
type fakeDispatcher struct {
	sent []any
	err  error
}

func (f *fakeDispatcher) Send(_ string, v any) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, v)
	return nil
}

func TestDecideLiveKnobNoRestart(t *testing.T) {
	// Changing a live knob (gop) never triggers a restart, even with live sessions.
	d := decide(map[string]any{}, patchReq{Overrides: map[string]any{"gop": float64(90)}}, 3)
	if d.needsRestart {
		t.Error("gop change must not need restart")
	}
	if d.blocked {
		t.Error("live-knob change must never be blocked")
	}
	if d.merged["gop"] != float64(90) {
		t.Errorf("merged gop = %v, want 90", d.merged["gop"])
	}
}

func TestDecideRestartKnobLiveSessionsNoConfirmBlocks(t *testing.T) {
	// Restart-class knob (encoder) + live sessions + no confirm → blocked (409).
	d := decide(map[string]any{}, patchReq{Overrides: map[string]any{"encoder": "va"}}, 2)
	if !d.needsRestart {
		t.Error("encoder change must need restart")
	}
	if !d.blocked {
		t.Error("restart-class change with live sessions and no confirm must block")
	}
	if d.liveSessions != 2 {
		t.Errorf("liveSessions = %d, want 2", d.liveSessions)
	}
}

func TestDecideRestartKnobConfirmProceeds(t *testing.T) {
	// Restart-class knob + confirm → proceeds (upsert + restart), not blocked.
	d := decide(map[string]any{}, patchReq{Overrides: map[string]any{"encoder": "va"}, RestartConfirm: true}, 2)
	if !d.needsRestart {
		t.Error("encoder change must need restart")
	}
	if d.blocked {
		t.Error("confirmed restart-class change must not block")
	}
	if d.merged["encoder"] != "va" {
		t.Errorf("merged encoder = %v, want va", d.merged["encoder"])
	}
}

func TestDecideRestartKnobNoLiveSessionsProceeds(t *testing.T) {
	// Restart-class knob without confirm but no live sessions → proceeds.
	d := decide(map[string]any{}, patchReq{Overrides: map[string]any{"encoder": "va"}}, 0)
	if !d.needsRestart {
		t.Error("encoder change must need restart")
	}
	if d.blocked {
		t.Error("no live sessions must not block")
	}
}

func TestDecideNullClearsOverride(t *testing.T) {
	// A null value removes the key from the merged map (clears the override).
	d := decide(map[string]any{"gop": float64(90)}, patchReq{Overrides: map[string]any{"gop": nil}}, 0)
	if _, present := d.merged["gop"]; present {
		t.Errorf("null override must clear the key, got merged=%v", d.merged)
	}
}

func TestCatalogEndpoint(t *testing.T) {
	h := NewHandler(nil, &fakeDispatcher{}, nil)
	rr := httptest.NewRecorder()
	h.handleCatalog(rr, httptest.NewRequest(http.MethodGet, "/v1/admin/config/catalog", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body struct {
		Knobs []Knob `json:"knobs"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Knobs) == 0 {
		t.Error("catalog returned no knobs")
	}
}

func TestPatch409BodyShape(t *testing.T) {
	// Verify the 409 conflict body nests live_sessions *inside* the error object,
	// matching control-api.md: {"error":{"code":…,"message":…,"live_sessions":N}}.
	// decide() is already unit-tested; this test drives the actual handlePatch
	// JSON-emission path by calling writeConflictBody directly — the same helper
	// the handler calls — so a shape drift breaks this test.
	rr := httptest.NewRecorder()
	writeConflictBody(rr, 3)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	var body struct {
		Error struct {
			Code         string `json:"code"`
			Message      string `json:"message"`
			LiveSessions int    `json:"live_sessions"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 409 body: %v\nraw: %s", err, rr.Body.Bytes())
	}
	if body.Error.Code != "restart_required" {
		t.Errorf("error.code = %q, want restart_required", body.Error.Code)
	}
	if body.Error.LiveSessions != 3 {
		t.Errorf("error.live_sessions = %d, want 3", body.Error.LiveSessions)
	}
}

func TestPatchBadValueReturns400(t *testing.T) {
	h := NewHandler(nil, &fakeDispatcher{}, fakeCounter(0))
	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/hosts/h1/settings",
		strings.NewReader(`{"overrides":{"target_usage":9}}`))
	req.SetPathValue("id", "h1")
	rr := httptest.NewRecorder()
	h.handlePatch(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (target_usage max 7)", rr.Code)
	}
}

// TestPatchNullClearNonNullablePassesValidation verifies that the handler's
// validation step does NOT reject a null value for a non-nullable knob.
// We test the ValidatePatch call that handlePatch makes rather than driving the
// full HTTP path (which needs a DB-backed store).
func TestPatchNullClearNonNullablePassesValidation(t *testing.T) {
	// encoder is non-nullable — previously Validate rejected null here.
	if err := ValidatePatch(map[string]any{"encoder": nil}); err != nil {
		t.Fatalf("handlePatch validation must not reject null-clear of encoder: %v", err)
	}
	// gop is non-nullable too.
	if err := ValidatePatch(map[string]any{"gop": nil}); err != nil {
		t.Fatalf("handlePatch validation must not reject null-clear of gop: %v", err)
	}
}

// TestPatchUnknownKeyNullReturns400 confirms that a null value for an unknown key
// is still rejected — you can't clear a key that isn't in the catalog.
func TestPatchUnknownKeyNullReturns400(t *testing.T) {
	h := NewHandler(nil, &fakeDispatcher{}, fakeCounter(0))
	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/hosts/h1/settings",
		strings.NewReader(`{"overrides":{"bogus":null}}`))
	req.SetPathValue("id", "h1")
	rr := httptest.NewRecorder()
	h.handlePatch(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown key (null or not) must return 400, got %d", rr.Code)
	}
}

// TestDecideRestartChangeViaNullClear confirms that clearing an encoder override
// that differs from the default via null is detected as a restart-class change.
// (The old override is "va", the null patch clears it → resolved encoder reverts to
// "openh264"; old resolved was "va" → they differ → RestartChange must be true.)
func TestDecideRestartChangeViaNullClear(t *testing.T) {
	old := map[string]any{"encoder": "va"}
	d := decide(old, patchReq{Overrides: map[string]any{"encoder": nil}}, 0)
	if _, present := d.merged["encoder"]; present {
		t.Errorf("null-clear must remove encoder from merged, got %v", d.merged)
	}
	if !d.needsRestart {
		t.Error("clearing an encoder override that differs from default must be restart-class")
	}
}

// fakeCounter is a LiveSessionCounter returning a fixed count.
type fakeCounter int

func (c fakeCounter) LiveSessions(string) int { return int(c) }

// newRestartRequest builds a POST /v1/admin/hosts/{id}/restart request with
// the path value wired the same way the real mux does.
func newRestartRequest(hostID string, confirm bool) *http.Request {
	body := `{"confirm":false}`
	if confirm {
		body = `{"confirm":true}`
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/hosts/"+hostID+"/restart", strings.NewReader(body))
	req.SetPathValue("id", hostID)
	return req
}

// getSettingsBody drives handleGet for one host and decodes the raw body, so a
// test can assert on key PRESENCE (json.RawMessage per key) rather than only on
// decoded values — the difference between "codecs: null" and "no codecs key".
func getSettingsBody(t *testing.T, h *Handler, hostID string) map[string]json.RawMessage {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/hosts/"+hostID+"/settings", nil)
	req.SetPathValue("id", hostID)
	rr := httptest.NewRecorder()
	h.handleGet(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// TestGetSettingsCodecsNullBeforeReport — wizard-v2 §S5: the field is always
// PRESENT on GET, and is null (not omitted, not ["h264"]) for a host whose agent
// has never reported a codec set.
func TestGetSettingsCodecsNullBeforeReport(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	h := NewHandler(NewStore(pool), &fakeDispatcher{}, fakeCounter(0))
	body := getSettingsBody(t, h, hostID)
	raw, ok := body["codecs"]
	if !ok {
		t.Fatal("codecs key missing from GET .../settings body")
	}
	if string(raw) != "null" {
		t.Errorf("codecs = %s, want null before any capacity report", raw)
	}
}

// TestGetSettingsCodecsReportedSet — the reported set reaches the wire verbatim.
func TestGetSettingsCodecsReportedSet(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	if _, err := pool.Exec(context.Background(),
		`UPDATE hosts SET codecs = $2 WHERE id::text = $1`, hostID, `["h264","h265","av1"]`); err != nil {
		t.Fatalf("seed codecs: %v", err)
	}
	h := NewHandler(NewStore(pool), &fakeDispatcher{}, fakeCounter(0))
	body := getSettingsBody(t, h, hostID)
	if got := string(body["codecs"]); got != `["h264","h265","av1"]` {
		t.Errorf("codecs = %s, want the reported set verbatim", got)
	}
}

// TestPatchSettingsHasNoCodecsField — the amendment is GET-only. codecs is an
// agent observation, and a PATCH response echoing it would invite a client to
// treat it as writable state.
func TestPatchSettingsHasNoCodecsField(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	if _, err := pool.Exec(context.Background(),
		`UPDATE hosts SET codecs = $2 WHERE id::text = $1`, hostID, `["h264","av1"]`); err != nil {
		t.Fatalf("seed codecs: %v", err)
	}
	h := NewHandler(NewStore(pool), &fakeDispatcher{}, fakeCounter(0))
	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/hosts/"+hostID+"/settings",
		strings.NewReader(`{"overrides":{"gop":90}}`))
	req.SetPathValue("id", hostID)
	rr := httptest.NewRecorder()
	h.handlePatch(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := body["codecs"]; present {
		t.Error("PATCH .../settings must not return codecs — GET-only per the contract")
	}
}

// TestRestartUnknownHostReturns404 verifies the restart endpoint 404s for a
// host id that doesn't exist (host-observability-2).
func TestRestartUnknownHostReturns404(t *testing.T) {
	pool := testPool(t)
	h := NewHandler(NewStore(pool), &fakeDispatcher{}, fakeCounter(0))
	rr := httptest.NewRecorder()
	h.handleRestart(rr, newRestartRequest("00000000-0000-0000-0000-000000000000", false))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// TestRestartOfflineHostReturns409Conflict verifies an offline host (agent not
// connected) refuses the restart with a plain 409 conflict, distinct from the
// restart_required shape.
func TestRestartOfflineHostReturns409Conflict(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	if _, err := pool.Exec(context.Background(), `UPDATE hosts SET status='offline' WHERE id::text = $1`, hostID); err != nil {
		t.Fatalf("seed offline: %v", err)
	}
	h := NewHandler(NewStore(pool), &fakeDispatcher{}, fakeCounter(0))
	rr := httptest.NewRecorder()
	h.handleRestart(rr, newRestartRequest(hostID, false))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code == "restart_required" {
		t.Error("offline conflict must not use the restart_required shape (no live sessions involved)")
	}
}

// TestPatchRejectsACrossKnobHysteresisViolation drives handlePatch end-to-end:
// a host already has abr_ladder_res_recover_frac=0.7 persisted, and a PATCH
// tries to set abr_ladder_res_engage_frac=0.9 — which, merged with the
// existing recover_frac, collapses the hysteresis band. The 400 must name
// the other half of the pair, and the rejected value must not persist.
func TestPatchRejectsACrossKnobHysteresisViolation(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	store := NewStore(pool)
	if err := store.Upsert(context.Background(), hostID, map[string]any{"abr_ladder_res_recover_frac": 0.7}, nil); err != nil {
		t.Fatalf("seed recover_frac: %v", err)
	}
	h := NewHandler(store, &fakeDispatcher{}, fakeCounter(0))

	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/hosts/"+hostID+"/settings",
		strings.NewReader(`{"overrides":{"abr_ladder_res_engage_frac":0.9}}`))
	req.SetPathValue("id", hostID)
	rr := httptest.NewRecorder()
	h.handlePatch(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "abr_ladder_res_recover_frac") {
		t.Fatalf("the 400 must name the other half of the pair: %s", rr.Body.String())
	}
	got, err := store.Get(context.Background(), hostID)
	if err != nil {
		t.Fatalf("get overrides: %v", err)
	}
	if _, present := got["abr_ladder_res_engage_frac"]; present {
		t.Fatalf("a rejected patch must not persist: %#v", got)
	}
}

// TestPatchAcceptsTheLadderKnobs is the happy-path counterpart: a consistent
// set of ladder overrides persists normally.
func TestPatchAcceptsTheLadderKnobs(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	store := NewStore(pool)
	h := NewHandler(store, &fakeDispatcher{}, fakeCounter(0))

	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/hosts/"+hostID+"/settings",
		strings.NewReader(`{"overrides":{"abr_mode":"smooth","abr_ladder_resolution":true,"abr_ladder_res_min_height":1080}}`))
	req.SetPathValue("id", hostID)
	rr := httptest.NewRecorder()
	h.handlePatch(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	got, err := store.Get(context.Background(), hostID)
	if err != nil {
		t.Fatalf("get overrides: %v", err)
	}
	if got["abr_ladder_resolution"] != true {
		t.Fatalf("the override was not persisted: %#v", got)
	}
}

// TestRestartLiveSessionsNoConfirmReturns409RestartRequired mirrors the PATCH
// .../settings guard exactly (same shared liveSessionRestartBlocked helper).
func TestRestartLiveSessionsNoConfirmReturns409RestartRequired(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool) // seedHost inserts status='online'
	h := NewHandler(NewStore(pool), &fakeDispatcher{}, fakeCounter(2))
	rr := httptest.NewRecorder()
	h.handleRestart(rr, newRestartRequest(hostID, false))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	var body struct {
		Error struct {
			Code         string `json:"code"`
			LiveSessions int    `json:"live_sessions"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "restart_required" || body.Error.LiveSessions != 2 {
		t.Errorf("error body = %+v, want restart_required with live_sessions=2", body.Error)
	}
}

// TestRestartConfirmedProceedsAndSetsPendingRestart verifies the happy path:
// 200, restart_triggered true, the restart command dispatched, and
// pending_restart persisted on the host row.
func TestRestartConfirmedProceedsAndSetsPendingRestart(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	store := NewStore(pool)
	dispatcher := &fakeDispatcher{}
	h := NewHandler(store, dispatcher, fakeCounter(2))
	rr := httptest.NewRecorder()
	h.handleRestart(rr, newRestartRequest(hostID, true))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var body struct {
		RestartTriggered bool `json:"restart_triggered"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.RestartTriggered {
		t.Error("restart_triggered = false, want true")
	}
	if len(dispatcher.sent) != 1 {
		t.Fatalf("dispatcher.sent = %d commands, want 1", len(dispatcher.sent))
	}
	if cmd, ok := dispatcher.sent[0].(restartCmd); !ok || cmd.Type != "restart" {
		t.Errorf("dispatched command = %+v, want a restart command", dispatcher.sent[0])
	}
	pending, err := store.GetPendingRestart(context.Background(), hostID)
	if err != nil {
		t.Fatalf("get pending_restart: %v", err)
	}
	if !pending {
		t.Error("pending_restart not persisted true after confirmed restart")
	}
}

// TestRestartDispatchFailureReturns409AndDoesNotSetPendingRestart is the
// review-LOW regression test: if the agent drops between the online-check
// and the actual dispatch, Send fails — pending_restart must NOT stick true
// with no restart in flight. Response is 409 conflict, same posture as the
// offline branch.
func TestRestartDispatchFailureReturns409AndDoesNotSetPendingRestart(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	store := NewStore(pool)
	dispatcher := &fakeDispatcher{err: errors.New("connection reset")}
	h := NewHandler(store, dispatcher, fakeCounter(0))
	rr := httptest.NewRecorder()
	h.handleRestart(rr, newRestartRequest(hostID, false))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rr.Code, rr.Body.String())
	}
	pending, err := store.GetPendingRestart(context.Background(), hostID)
	if err != nil {
		t.Fatalf("get pending_restart: %v", err)
	}
	if pending {
		t.Error("pending_restart persisted true despite a failed dispatch")
	}
}

// TestPatchDispatchFailureReturns409AndDoesNotSetPendingRestart mirrors the
// restart-endpoint regression for PATCH .../settings: a restart-class change
// whose restart dispatch fails must not mark pending_restart, and must surface
// as 409 conflict — even though the overrides themselves already persisted
// (verified below via a direct store.Get, matching "override-persistence
// semantics unchanged").
func TestPatchDispatchFailureReturns409AndDoesNotSetPendingRestart(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	store := NewStore(pool)
	dispatcher := &fakeDispatcher{err: errors.New("connection reset")}
	h := NewHandler(store, dispatcher, fakeCounter(0))

	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/hosts/"+hostID+"/settings",
		strings.NewReader(`{"overrides":{"encoder":"va"}}`))
	req.SetPathValue("id", hostID)
	rr := httptest.NewRecorder()
	h.handlePatch(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rr.Code, rr.Body.String())
	}

	pending, err := store.GetPendingRestart(context.Background(), hostID)
	if err != nil {
		t.Fatalf("get pending_restart: %v", err)
	}
	if pending {
		t.Error("pending_restart persisted true despite a failed restart dispatch")
	}

	overrides, err := store.Get(context.Background(), hostID)
	if err != nil {
		t.Fatalf("get overrides: %v", err)
	}
	if overrides["encoder"] != "va" {
		t.Errorf("overrides = %v, want encoder=va still persisted despite the failed restart dispatch", overrides)
	}
}

// patchHomeRoot PATCHes {"overrides":{"home_root":value}} and returns the
// recorded response.
func patchHomeRoot(t *testing.T, h *Handler, hostID, value string) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(map[string]any{"overrides": map[string]any{"home_root": value}})
	if err != nil {
		t.Fatalf("marshal patch body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/hosts/"+hostID+"/settings", strings.NewReader(string(b)))
	req.SetPathValue("id", hostID)
	rr := httptest.NewRecorder()
	h.handlePatch(rr, req)
	return rr
}

// TestPatchHomeRootAcceptsExactReportedRoot verifies the exact agent-reported
// root is accepted (storage-root-constrained, wizard-v2 §S4c corrections).
func TestPatchHomeRootAcceptsExactReportedRoot(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	const root = "/var/lib/quasar/homes"
	if _, err := pool.Exec(context.Background(),
		`UPDATE hosts SET effective_settings = $2 WHERE id::text = $1`, hostID, `{"home_root":"`+root+`"}`); err != nil {
		t.Fatalf("seed effective_settings: %v", err)
	}
	h := NewHandler(NewStore(pool), &fakeDispatcher{}, fakeCounter(0))
	rr := patchHomeRoot(t, h, hostID, root)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestPatchHomeRootAcceptsSubpath verifies a subdirectory of the reported root
// is accepted.
func TestPatchHomeRootAcceptsSubpath(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	const root = "/var/lib/quasar/homes"
	if _, err := pool.Exec(context.Background(),
		`UPDATE hosts SET effective_settings = $2 WHERE id::text = $1`, hostID, `{"home_root":"`+root+`"}`); err != nil {
		t.Fatalf("seed effective_settings: %v", err)
	}
	h := NewHandler(NewStore(pool), &fakeDispatcher{}, fakeCounter(0))
	rr := patchHomeRoot(t, h, hostID, root+"/instance-a")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestPatchHomeRootRejectsSiblingPrefix is the classic prefix-bug regression:
// "<root>-evil" starts with "<root>" as a raw string but is a sibling
// directory, not a subpath, and must be rejected with 400.
func TestPatchHomeRootRejectsSiblingPrefix(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	const root = "/var/lib/quasar/homes"
	if _, err := pool.Exec(context.Background(),
		`UPDATE hosts SET effective_settings = $2 WHERE id::text = $1`, hostID, `{"home_root":"`+root+`"}`); err != nil {
		t.Fatalf("seed effective_settings: %v", err)
	}
	h := NewHandler(NewStore(pool), &fakeDispatcher{}, fakeCounter(0))
	rr := patchHomeRoot(t, h, hostID, root+"-evil")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestPatchHomeRootRejectsUnrelatedPath verifies an absolute path with no
// relation to the reported root is rejected.
func TestPatchHomeRootRejectsUnrelatedPath(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	if _, err := pool.Exec(context.Background(),
		`UPDATE hosts SET effective_settings = $2 WHERE id::text = $1`, hostID, `{"home_root":"/var/lib/quasar/homes"}`); err != nil {
		t.Fatalf("seed effective_settings: %v", err)
	}
	h := NewHandler(NewStore(pool), &fakeDispatcher{}, fakeCounter(0))
	rr := patchHomeRoot(t, h, hostID, "/data/somewhere/else")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestPatchHomeRootRejectsAnyValueWhenAgentReportedNoRoot verifies that a host
// whose agent has never reported a home_root refuses ANY non-empty value —
// there is nothing for it to be a subpath of.
func TestPatchHomeRootRejectsAnyValueWhenAgentReportedNoRoot(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool) // no effective_settings seeded — NULL
	h := NewHandler(NewStore(pool), &fakeDispatcher{}, fakeCounter(0))
	rr := patchHomeRoot(t, h, hostID, "/var/lib/quasar/homes")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "deploy/.env") {
		t.Errorf("error body must point at deploy/.env as the remedy, got: %s", rr.Body.String())
	}
}

// TestPatchHomeRootClearingAlwaysAllowed verifies the override can always be
// cleared (null), even for a host with no reported root at all.
func TestPatchHomeRootClearingAlwaysAllowed(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	store := NewStore(pool)
	// seed a stored override first
	if err := store.Upsert(context.Background(), hostID, map[string]any{"home_root": "/var/lib/quasar/homes/instance-a"}, nil); err != nil {
		t.Fatalf("seed override: %v", err)
	}
	h := NewHandler(store, &fakeDispatcher{}, fakeCounter(0))
	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/hosts/"+hostID+"/settings",
		strings.NewReader(`{"overrides":{"home_root":null}}`))
	req.SetPathValue("id", hostID)
	rr := httptest.NewRecorder()
	h.handlePatch(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	overrides, err := store.Get(context.Background(), hostID)
	if err != nil {
		t.Fatalf("get overrides: %v", err)
	}
	if _, present := overrides["home_root"]; present {
		t.Errorf("home_root override must be cleared, got %v", overrides)
	}
}
