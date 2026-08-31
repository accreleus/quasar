package session

// profile_write_decode_test.go — the stream-profile / launch-profile /
// profile-preference write bodies go through decodeJSON (handler.go), the same
// helper the launch and cert handlers use: a 1 MiB http.MaxBytesReader cap and
// DisallowUnknownFields. Before that they used a bare json.NewDecoder, so an
// unbounded body was read into memory and a misspelt field was silently dropped
// — the caller got a 200 and none of the change it asked for.
//
// The ingest endpoints (metrics_handler.go, trace_handler.go) deliberately keep
// their own larger cap WITHOUT DisallowUnknownFields, so an older/newer agent
// can add a telemetry field without being rejected. Do not "unify" them here.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestProfileWritesRejectUnknownFields — each body is otherwise VALID (the same
// shape the sibling tests get a 2xx from), so the 400 can only come from the
// unknown field. A misspelt field name is a client bug; answering 200 to a write
// that changed nothing is worse than answering 400.
func TestProfileWritesRejectUnknownFields(t *testing.T) {
	pool := testDB(t)
	base, adminTok, userTok := newProfileAdminServer(t, pool)
	seedChain(t, pool, "unk", []chainRung{
		{id: "unk-h264", codec: "h264", w: 1920, h: 1080, minBW: 14400},
	})

	cases := []struct {
		name   string
		method string
		path   string
		token  string
		// valid is a body the route accepts; unknown is the single extra key
		// that must turn it into a 400. Both halves are asserted, so the test
		// cannot pass because `valid` was quietly invalid all along.
		valid   map[string]any
		unknown string
		value   any
	}{
		{
			"POST stream-profile", "POST", "/v1/admin/stream-profiles", adminTok,
			map[string]any{
				"id": "unknown-field-rung", "display_name": "Rung", "codec": "hevc",
				"width": 2560, "height": 1440, "fps": 60, "nominal_bitrate_kbps": 9000,
			},
			// The catalog's per-rung codec LIST was removed in UI-P4 (a rung IS a
			// codec); docs/configuration.md still describes writing one here.
			"codecs", []any{map[string]any{"codec": "hevc", "status": "launchable"}},
		},
		{
			"PATCH stream-profile", "PATCH", "/v1/admin/stream-profiles/unk-h264", adminTok,
			map[string]any{"display_name": "Renamed"},
			"nominal_bitrate_kbps_typo", 9000,
		},
		{
			"POST launch-profile", "POST", "/v1/admin/launch-profiles", adminTok,
			map[string]any{
				"id": "unknown-field-chain", "display_name": "Chain",
				"rungs": []string{"unk-h264"},
			},
			// Positions are server-assigned from the rung ORDER; a client that
			// sends them is working from a stale mental model, not a valid body.
			"positions", []int{1},
		},
		{
			"PATCH launch-profile", "PATCH", "/v1/admin/launch-profiles/unk", adminTok,
			map[string]any{"display_name": "Renamed"},
			"sort_ordr", 3,
		},
		{
			"PATCH profile-policy", "PATCH", "/v1/admin/profile-policy", adminTok,
			map[string]any{"user_overrides_allowed": true},
			"default_profile_id", "unk",
		},
		{
			"PATCH profile-preferences", "PATCH", "/v1/me/profile-preferences", userTok,
			map[string]any{"default_profile_id": nil},
			"preferred_profile_id", "unk",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The unknown-field body first: it must be rejected, and rejecting it
			// leaves no row behind for the clean write below to collide with.
			dirty := map[string]any{c.unknown: c.value}
			for k, v := range c.valid {
				dirty[k] = v
			}
			resp := doJSON(t, c.method, base+c.path, c.token, dirty)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s %s with the unknown field %q = %d, want 400",
					c.method, c.path, c.unknown, resp.StatusCode)
			}
			_ = resp.Body.Close()

			resp = doJSON(t, c.method, base+c.path, c.token, c.valid)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode >= http.StatusBadRequest {
				t.Errorf("%s %s WITHOUT the unknown field = %d, want a 2xx — the 400 above "+
					"must come from the unknown field, not from an invalid body",
					c.method, c.path, resp.StatusCode)
			}
		})
	}
}

// TestProfileWriteRejectsOversizedBody — the 1 MiB MaxBytesReader cap. Without
// it an admin-authenticated caller could make the control plane buffer an
// arbitrarily large body before any validation ran.
func TestProfileWriteRejectsOversizedBody(t *testing.T) {
	pool := testDB(t)
	base, adminTok, _ := newProfileAdminServer(t, pool)

	huge := `{"id":"huge","display_name":"` + strings.Repeat("a", 2<<20) +
		`","codec":"h264","width":1920,"height":1080,"fps":60,"nominal_bitrate_kbps":12000}`
	resp := doJSON(t, "POST", base+"/v1/admin/stream-profiles", adminTok, huge)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a 2 MiB rung create = %d, want 400", resp.StatusCode)
	}
}

// TestProfilePreferencesAcceptsFullContractBody — openapi.yaml reuses
// ProfilePreferencesResponse as the PATCH /v1/me/profile-preferences REQUEST
// body, so echoing the whole GET response back is a contract-conformant write.
// DisallowUnknownFields must therefore not reject the two policy fields it
// carries; the handler ignores them, and they stay writable only through the
// admin-gated PATCH /v1/admin/profile-policy.
func TestProfilePreferencesAcceptsFullContractBody(t *testing.T) {
	pool := testDB(t)
	base, adminTok, userTok := newProfileAdminServer(t, pool)

	resp := doJSON(t, "PATCH", base+"/v1/me/profile-preferences", userTok, map[string]any{
		"default_profile_id":        nil,
		"global_default_profile_id": "1080p60",
		"user_overrides_allowed":    false,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("echoing the contract's own response shape back = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// …and ignoring them means IGNORING them: a non-admin must not be able to
	// move the global policy through their own preferences route.
	resp = doJSON(t, "GET", base+"/v1/admin/profile-policy", adminTok, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET profile-policy = %d, want 200", resp.StatusCode)
	}
	var policy struct {
		GlobalDefaultProfileID *string `json:"global_default_profile_id"`
		UserOverridesAllowed   bool    `json:"user_overrides_allowed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&policy); err != nil {
		t.Fatalf("decode profile policy: %v", err)
	}
	if policy.GlobalDefaultProfileID != nil {
		t.Errorf("global_default_profile_id = %q; a user preferences PATCH must not set it", *policy.GlobalDefaultProfileID)
	}
	if !policy.UserOverridesAllowed {
		t.Error("user_overrides_allowed = false; a user preferences PATCH must not clear it")
	}
}
