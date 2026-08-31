package session

// launch_profiles_http_test.go — HTTP-level coverage of the UI-P4 admin
// surfaces: authorization, the H.264 floor rule, the warnings, and the two
// refuse-if-in-use 409s.
//
// AUTHORIZATION IS THE SERVER'S JOB. The admin UI hiding a route or disabling a
// Delete button is a UX affordance and NEVER the access control (CLAUDE.md
// invariant #6), so every new route is checked against a valid NON-ADMIN bearer
// token here. `403` must precede any resource lookup, so a non-admin never even
// learns whether an id exists.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newProfileAdminServer builds the session HTTP surface with a registered admin
// and a registered non-admin, returning both tokens.
func newProfileAdminServer(t *testing.T, pool *pgxpool.Pool) (url, adminTok, userTok string) {
	t.Helper()
	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "admin@test.local", "adminuser", "quasar-fixture-pw-08"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE email='admin@test.local'`); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	if _, err := authSvc.Register(ctx, "plain@test.local", "plainuser", "quasar-fixture-pw-08"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	return srv.URL, loginTok(t, authSvc, "admin@test.local", "quasar-fixture-pw-08"),
		loginTok(t, authSvc, "plain@test.local", "quasar-fixture-pw-08")
}

// TestProfileAdminRoutesRejectNonAdmin covers EVERY route UI-P4 adds or gains,
// in both directions: 401 without a token, 403 with a valid non-admin one.
func TestProfileAdminRoutesRejectNonAdmin(t *testing.T) {
	pool := testDB(t)
	base, adminTok, userTok := newProfileAdminServer(t, pool)

	routes := []struct {
		method string
		path   string
		body   any
	}{
		{"GET", "/v1/admin/stream-profiles", nil},
		{"POST", "/v1/admin/stream-profiles", map[string]any{"id": "x", "display_name": "x", "codec": "h264", "width": 1, "height": 1, "fps": 1, "nominal_bitrate_kbps": 1}},
		{"PATCH", "/v1/admin/stream-profiles/1080p60-h264", map[string]any{"display_name": "x"}},
		{"DELETE", "/v1/admin/stream-profiles/1080p60-h264", nil},
		{"GET", "/v1/admin/launch-profiles", nil},
		{"POST", "/v1/admin/launch-profiles", map[string]any{"id": "x", "display_name": "x", "rungs": []string{"1080p60-h264"}}},
		{"GET", "/v1/admin/launch-profiles/1080p60", nil},
		{"PATCH", "/v1/admin/launch-profiles/1080p60", map[string]any{"display_name": "x"}},
		{"DELETE", "/v1/admin/launch-profiles/1080p60", nil},
	}

	for _, r := range routes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			resp := doJSON(t, r.method, base+r.path, "", r.body)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("no token: got %d, want 401", resp.StatusCode)
			}
			_ = resp.Body.Close()

			resp = doJSON(t, r.method, base+r.path, userTok, r.body)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("non-admin token: got %d, want 403", resp.StatusCode)
			}
			_ = resp.Body.Close()
		})
	}

	// Sanity: the admin token actually reaches the handlers, so the 403s above are
	// the role gate and not a routing accident.
	resp := doJSON(t, "GET", base+"/v1/admin/launch-profiles", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("admin GET /v1/admin/launch-profiles = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// launchProfileBody mirrors the BARE LaunchProfile the contract declares for
// every launch-profile read/write response — no envelope.
type launchProfileBody struct {
	ID    string `json:"id"`
	Rungs []struct {
		Position      int32 `json:"position"`
		StreamProfile struct {
			ID    string `json:"id"`
			Codec string `json:"codec"`
		} `json:"stream_profile"`
	} `json:"rungs"`
	UsedBy struct {
		Apps            []struct{ ID, Name string } `json:"apps"`
		GlobalDefault   bool                        `json:"global_default"`
		UserPreferences int                         `json:"user_preferences"`
	} `json:"used_by"`
	Warnings []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"warnings"`
}

func decodeLaunchProfile(t *testing.T, resp *http.Response) launchProfileBody {
	t.Helper()
	var body launchProfileBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode launch profile: %v", err)
	}
	_ = resp.Body.Close()
	return body
}

// TestLaunchProfileH264FloorRule is the two-part rule, and the part that is a
// WARNING is the one a naive implementation gets wrong.
//
//   - NO h264 rung ⇒ 400 validation_failed. Hard, no grandfathering: migration
//     0036's fan-out guarantees every migrated chain satisfies it.
//   - h264 NOT LAST ⇒ ACCEPTED, with an `h264_floor_not_last` warning. Rejecting
//     would make a migrated chain whose stored codec order puts h264 first —
//     today's default order — permanently uneditable, and would add no safety,
//     because the real guarantee is the resolver's unconditional floor.
func TestLaunchProfileH264FloorRule(t *testing.T) {
	pool := testDB(t)
	base, adminTok, _ := newProfileAdminServer(t, pool)
	seedChain(t, pool, "src", []chainRung{
		{id: "src-av1", codec: "av1", w: 2560, h: 1440, minBW: 11000},
		{id: "src-hevc", codec: "hevc", w: 2560, h: 1440, minBW: 11000},
		{id: "src-h264", codec: "h264", w: 1920, h: 1080, minBW: 14400},
	})

	// (a) No h264 rung ⇒ 400.
	resp := doJSON(t, "POST", base+"/v1/admin/launch-profiles", adminTok, map[string]any{
		"id": "no-floor", "display_name": "No floor", "rungs": []string{"src-av1", "src-hevc"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a chain with no h264 rung = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// (b) h264 FIRST ⇒ accepted, warned. This is exactly the shape migration 0036
	//     produces for a materialised codec list, so it must remain editable.
	resp = doJSON(t, "POST", base+"/v1/admin/launch-profiles", adminTok, map[string]any{
		"id": "h264-first", "display_name": "H.264 first", "rungs": []string{"src-h264", "src-av1"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("h264-first chain = %d, want 201 (a warning is never a failure)", resp.StatusCode)
	}
	body := decodeLaunchProfile(t, resp)
	if !hasWarning(body, warnH264FloorNotLast) {
		t.Errorf("expected an %s warning, got %+v", warnH264FloorNotLast, body.Warnings)
	}

	// And it must stay PATCHable — the point of not rejecting.
	resp = doJSON(t, "PATCH", base+"/v1/admin/launch-profiles/h264-first", adminTok, map[string]any{
		"display_name": "Renamed",
	})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("PATCH of an h264-first chain = %d, want 200 — rejecting the order would make it uneditable", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// (c) The canonical order (h264 LAST) is accepted with no floor warning.
	resp = doJSON(t, "POST", base+"/v1/admin/launch-profiles", adminTok, map[string]any{
		"id": "good", "display_name": "Good", "rungs": []string{"src-av1", "src-hevc", "src-h264"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("canonical chain = %d, want 201", resp.StatusCode)
	}
	body = decodeLaunchProfile(t, resp)
	if hasWarning(body, warnH264FloorNotLast) {
		t.Errorf("h264 is last; got an %s warning anyway: %+v", warnH264FloorNotLast, body.Warnings)
	}
	// The floor rung's min_offer_bandwidth (14400) is HIGHER than the rungs above
	// it (11000): a floor that is harder to satisfy than the rung above it is a
	// misconfiguration, and the operator should see it.
	if !hasWarning(body, warnFloorNotLeastDemanding) {
		t.Errorf("expected a %s warning, got %+v", warnFloorNotLeastDemanding, body.Warnings)
	}

	// ORDER IS PREFERENCE and the server assigns position from the array order.
	for i, r := range body.Rungs {
		if r.Position != int32(i+1) {
			t.Errorf("rung %d position = %d, want %d", i, r.Position, i+1)
		}
	}
	if body.Rungs[0].StreamProfile.Codec != "av1" {
		t.Errorf("first rung codec = %q, want av1 (the written order)", body.Rungs[0].StreamProfile.Codec)
	}
}

// TestLaunchProfileWriteValidation covers the remaining write-time rules.
func TestLaunchProfileWriteValidation(t *testing.T) {
	pool := testDB(t)
	base, adminTok, _ := newProfileAdminServer(t, pool)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing id", map[string]any{"display_name": "x", "rungs": []string{"1080p60-h264"}}},
		{"missing display_name", map[string]any{"id": "x", "rungs": []string{"1080p60-h264"}}},
		{"empty rungs", map[string]any{"id": "x", "display_name": "x", "rungs": []string{}}},
		{"unknown rung id", map[string]any{"id": "x", "display_name": "x", "rungs": []string{"1080p60-h264", "nope"}}},
		{"duplicate rung", map[string]any{"id": "x", "display_name": "x", "rungs": []string{"1080p60-h264", "1080p60-h264"}}},
		{"bad visibility", map[string]any{"id": "x", "display_name": "x", "visibility": "public", "rungs": []string{"1080p60-h264"}}},
		// A LEGACY (pre-0036, codec IS NULL) stream_profiles row still EXISTS —
		// the expand migration keeps it so a code-level revert finds its data — and
		// GetStreamProfile does not filter to rungs, so it resolves happily. Listing
		// one as a rung is silently useless: resolveRung's catalogToWire fails, the
		// rung is recorded as `unknown_codec` and skipped on every single launch.
		// Reject it at write time rather than shipping a chain with a dead link.
		{"legacy codec-less stream profile as a rung", map[string]any{"id": "x", "display_name": "x", "rungs": []string{"1080p60", "1080p60-h264"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := doJSON(t, "POST", base+"/v1/admin/launch-profiles", adminTok, c.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("got %d, want 400", resp.StatusCode)
			}
			_ = resp.Body.Close()
		})
	}
}

// TestStreamProfileWriteRequiresACodec — a rung IS a codec (UI-P4 B3): the old
// `codecs[]` list and its launchable|future|unsupported status enum are gone, so
// a create without a codec is incoherent, not a "use the default".
func TestStreamProfileWriteRequiresACodec(t *testing.T) {
	pool := testDB(t)
	base, adminTok, _ := newProfileAdminServer(t, pool)

	resp := doJSON(t, "POST", base+"/v1/admin/stream-profiles", adminTok, map[string]any{
		"id": "no-codec", "display_name": "No codec",
		"width": 1920, "height": 1080, "fps": 60, "nominal_bitrate_kbps": 12000,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("rung create without a codec = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = doJSON(t, "POST", base+"/v1/admin/stream-profiles", adminTok, map[string]any{
		"id": "bad-codec", "display_name": "Bad codec", "codec": "h265", // WIRE name, not catalog
		"width": 1920, "height": 1080, "fps": 60, "nominal_bitrate_kbps": 12000,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("rung create with the wire codec name = %d, want 400 (the admin surface speaks the catalog vocabulary: hevc)", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = doJSON(t, "POST", base+"/v1/admin/stream-profiles", adminTok, map[string]any{
		"id": "good-rung", "display_name": "Good rung", "codec": "hevc",
		"width": 2560, "height": 1440, "fps": 60, "nominal_bitrate_kbps": 9000,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("valid rung create = %d, want 201", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// A duplicate id is a 409, not a silent overwrite.
	resp = doJSON(t, "POST", base+"/v1/admin/stream-profiles", adminTok, map[string]any{
		"id": "good-rung", "display_name": "Good rung", "codec": "hevc",
		"width": 2560, "height": 1440, "fps": 60, "nominal_bitrate_kbps": 9000,
	})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate rung id = %d, want 409", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestAdminDeleteRefusesInUse — both 409s over HTTP. The disabled Delete button
// in the admin UI is the client half; THIS is the enforcement.
func TestAdminDeleteRefusesInUse(t *testing.T) {
	pool := testDB(t)
	base, adminTok, _ := newProfileAdminServer(t, pool)
	ctx := context.Background()

	// (a) a rung listed by a launch profile.
	resp := doJSON(t, "DELETE", base+"/v1/admin/stream-profiles/1080p60-h264", adminTok, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("DELETE an in-use rung = %d, want 409", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// (b) a launch profile referenced by a user preference.
	var userID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email='plain@test.local'`).Scan(&userID); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_profile_preferences (user_id, default_profile_id) VALUES ($1::uuid, '1080p60')`, userID); err != nil {
		t.Fatalf("seed preference: %v", err)
	}
	resp = doJSON(t, "DELETE", base+"/v1/admin/launch-profiles/1080p60", adminTok, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("DELETE a referenced launch profile = %d, want 409", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// used_by must surface the reference so the UI can explain the refusal.
	resp = doJSON(t, "GET", base+"/v1/admin/launch-profiles/1080p60", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET launch profile = %d, want 200", resp.StatusCode)
	}
	body := decodeLaunchProfile(t, resp)
	if body.UsedBy.UserPreferences != 1 {
		t.Errorf("used_by.user_preferences = %d, want 1", body.UsedBy.UserPreferences)
	}

	// (c) a genuinely unreferenced launch profile deletes cleanly, proving the
	//     409s above are about the reference and not an undeletable object.
	resp = doJSON(t, "DELETE", base+"/v1/admin/launch-profiles/4k120", adminTok, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE an unreferenced launch profile = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = doJSON(t, "DELETE", base+"/v1/admin/launch-profiles/does-not-exist", adminTok, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE a missing launch profile = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestSessionResponseCarriesStreamProfileID — control-api.md amendment A3: EVERY
// session body carries `stream_profile_id`, the RUNG the launch resolved to.
//
// The value was written to the database and used internally from the first cut,
// but it never reached the wire: `sessionResp` had no such field, so a Tower gate
// run had to read Postgres directly to confirm which rung a session got. Phase 6
// is built on this field. Asserted on BOTH the create response and the read-back,
// because they are two different serialization paths through the same DTO and a
// half-wired field passes one of them.
func TestSessionResponseCarriesStreamProfileID(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "sess@test.local", "sessuser", "quasar-fixture-pw-08"); err != nil {
		t.Fatalf("register: %v", err)
	}
	tok := loginTok(t, authSvc, "sess@test.local", "quasar-fixture-pw-08")
	var userID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email='sess@test.local'`).Scan(&userID); err != nil {
		t.Fatalf("read user: %v", err)
	}

	var appID, hostID, gpuID string
	must(t, pool.QueryRow(ctx, `INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height, default_fps, default_bitrate_kbps)
		VALUES ('rung-app', 1024, 1, 1920, 1080, 60, 15000) RETURNING id::text`).Scan(&appID))
	entitleAll(t, pool, appID)
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, capacity_detection)
		VALUES ('host-rung','online','ok') RETURNING id::text`).Scan(&hostID))
	must(t, pool.QueryRow(ctx, `INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
		VALUES ($1, 0, 16384, 4) RETURNING id::text`, hostID).Scan(&gpuID))

	var body struct {
		Session struct {
			ID              string  `json:"id"`
			ProfileID       *string `json:"profile_id"`
			StreamProfileID *string `json:"stream_profile_id"`
		} `json:"session"`
	}

	resp := doJSON(t, "POST", srv.URL+"/v1/sessions", tok,
		map[string]any{"app_id": appID, "profile_id": "1080p60"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/sessions = %d, want 201", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode create body: %v", err)
	}
	_ = resp.Body.Close()

	if body.Session.ProfileID == nil || *body.Session.ProfileID != "1080p60" {
		t.Errorf("POST profile_id = %v, want 1080p60 (the LAUNCH profile: what the user picked)", body.Session.ProfileID)
	}
	if body.Session.StreamProfileID == nil {
		t.Fatal("POST /v1/sessions omitted stream_profile_id — contract amendment A3 requires it on every session body")
	}
	if *body.Session.StreamProfileID != "1080p60-h264" {
		t.Errorf("POST stream_profile_id = %q, want 1080p60-h264 (the RUNG: what they got)", *body.Session.StreamProfileID)
	}
	sessID := body.Session.ID

	// The read-back path is a different serialization of the same DTO.
	resp = doJSON(t, "GET", srv.URL+"/v1/sessions/"+sessID, tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/sessions/{id} = %d, want 200", resp.StatusCode)
	}
	var readBack struct {
		Session struct {
			ID              string  `json:"id"`
			StreamProfileID *string `json:"stream_profile_id"`
		} `json:"session"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&readBack); err != nil {
		t.Fatalf("decode read-back body: %v", err)
	}
	_ = resp.Body.Close()
	if readBack.Session.ID != sessID {
		t.Fatalf("GET returned session %q, want %q", readBack.Session.ID, sessID)
	}
	if readBack.Session.StreamProfileID == nil || *readBack.Session.StreamProfileID != "1080p60-h264" {
		t.Errorf("GET stream_profile_id = %v, want 1080p60-h264", readBack.Session.StreamProfileID)
	}
}

func hasWarning(b launchProfileBody, code string) bool {
	for _, w := range b.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}
