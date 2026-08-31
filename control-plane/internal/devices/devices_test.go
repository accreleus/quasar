package devices

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

// P4-08 integration tests — user_devices store + HTTP handler.
// Skipped without TEST_DATABASE_URL (provided by go-test-db); same pattern as
// internal/session tests (lifecycle_test.go).

// --- test DB setup -----------------------------------------------------------

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	if err := migrate.Run(migrations.FS, dbURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM user_devices;
		DELETE FROM auth_tokens;
		DELETE FROM sessions;
		DELETE FROM gpus;
		DELETE FROM hosts;
		DELETE FROM apps;
		DELETE FROM users;
	`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newServer wires a test HTTP server with the auth + devices handlers.
func newServer(t *testing.T, pool *pgxpool.Pool) (*httptest.Server, *auth.Service) {
	t.Helper()
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	store := NewStore(pool)

	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	dh := NewHandler(store, nil)
	dh.Register(mux, authHandler.RequireAuth)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, authSvc
}

// loginTok registers (if needed) and logs in a user, returning the bearer token.
func registerAndLogin(t *testing.T, svc *auth.Service, email, user, pass string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := svc.Register(ctx, email, user, pass); err != nil {
		// may already exist from a prior sub-test in the same pool; just log in
		_ = err
	}
	tok, err := svc.Login(ctx, email, pass, "test-agent/1.0")
	if err != nil {
		t.Fatalf("login %s: %v", email, err)
	}
	return tok.Plaintext
}

func doRequest(t *testing.T, method, url, bearer string, body any) *http.Response {
	t.Helper()
	var rdr interface{ Read([]byte) (int, error) }
	switch b := body.(type) {
	case nil:
		rdr = nil
	case string:
		rdr = strings.NewReader(b)
	default:
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(b)
		rdr = &buf
	}
	var req *http.Request
	if rdr != nil {
		req, _ = http.NewRequest(method, url, rdr)
	} else {
		req, _ = http.NewRequest(method, url, nil)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func countDevices(t *testing.T, pool *pgxpool.Pool, userID, deviceKey string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_devices WHERE user_id = $1::uuid AND device_key = $2`,
		userID, deviceKey).Scan(&n); err != nil {
		t.Fatalf("count devices: %v", err)
	}
	return n
}

// --- store-layer tests -------------------------------------------------------

// TestUpsertInsertsAndUpdates: first call inserts a row; second call with the
// same (user_id, device_key) UPDATEs last_seen_at and capabilities — unique
// constraint holds (count stays 1).
func TestUpsertInsertsAndUpdates(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	// Seed a user.
	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('dev@test.local','devuser','x') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	caps1 := json.RawMessage(`{"codecs":{"h264":true},"bandwidth_kbps":10000,"rtt_ms":20}`)
	dev1, err := store.Upsert(ctx, UpsertParams{
		UserID:       userID,
		DeviceKey:    "key-aaa",
		UserAgent:    "Mozilla/5.0",
		Capabilities: caps1,
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if dev1.ID == "" {
		t.Fatal("expected a device id")
	}
	if countDevices(t, pool, userID, "key-aaa") != 1 {
		t.Fatal("expected exactly 1 row after first upsert")
	}

	// Small sleep so last_seen_at advances measurably.
	time.Sleep(10 * time.Millisecond)

	caps2 := json.RawMessage(`{"codecs":{"h264":true,"vp9":true},"bandwidth_kbps":50000,"rtt_ms":10}`)
	dev2, err := store.Upsert(ctx, UpsertParams{
		UserID:       userID,
		DeviceKey:    "key-aaa",
		UserAgent:    "Mozilla/5.0 Updated",
		Capabilities: caps2,
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	// Unique constraint must hold: still exactly 1 row.
	if countDevices(t, pool, userID, "key-aaa") != 1 {
		t.Fatal("expected exactly 1 row after second upsert (unique must hold)")
	}

	// first_seen_at must be unchanged; last_seen_at must have advanced.
	if !dev2.FirstSeenAt.Equal(dev1.FirstSeenAt) {
		t.Fatalf("first_seen_at changed: was %v, now %v", dev1.FirstSeenAt, dev2.FirstSeenAt)
	}
	if !dev2.LastSeenAt.After(dev1.LastSeenAt) {
		t.Fatalf("last_seen_at did not advance: before=%v after=%v", dev1.LastSeenAt, dev2.LastSeenAt)
	}

	// capabilities must reflect the second write.
	var row struct {
		Caps json.RawMessage
	}
	if err := pool.QueryRow(ctx,
		`SELECT capabilities FROM user_devices WHERE user_id = $1::uuid AND device_key = $2`,
		userID, "key-aaa").Scan(&row.Caps); err != nil {
		t.Fatalf("read caps: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(row.Caps, &m); err != nil {
		t.Fatalf("unmarshal caps: %v", err)
	}
	// bandwidth_kbps should be 50000 (second write), not 10000.
	if m["bandwidth_kbps"] != float64(50000) {
		t.Fatalf("capabilities not updated: bandwidth_kbps = %v", m["bandwidth_kbps"])
	}
}

// TestMeasuredAtIsServerStamped: regardless of a client-supplied measured_at,
// the stored value must be server-generated (a recent timestamp, not the client's).
func TestMeasuredAtIsServerStamped(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('stamp@test.local','stampuser','x') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// Client supplies a clearly wrong measured_at (year 2000).
	caps := json.RawMessage(`{"codecs":{"h264":true},"measured_at":"2000-01-01T00:00:00Z"}`)
	// Truncate to second so the comparison works when RFC3339 drops sub-second
	// precision (RFC3339 is second-granular: "2006-01-02T15:04:05Z").
	before := time.Now().UTC().Truncate(time.Second)
	_, err := store.Upsert(ctx, UpsertParams{
		UserID:       userID,
		DeviceKey:    "key-stamp",
		Capabilities: caps,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	after := time.Now().UTC().Add(time.Second) // one second grace for the RFC3339 truncation

	var rawCaps []byte
	if err := pool.QueryRow(ctx,
		`SELECT capabilities FROM user_devices WHERE user_id = $1::uuid AND device_key = $2`,
		userID, "key-stamp").Scan(&rawCaps); err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rawCaps, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var measuredAtStr string
	if err := json.Unmarshal(m["measured_at"], &measuredAtStr); err != nil {
		t.Fatalf("unmarshal measured_at: %v", err)
	}
	stampedAt, err := time.Parse(time.RFC3339, measuredAtStr)
	if err != nil {
		t.Fatalf("parse measured_at %q: %v", measuredAtStr, err)
	}
	// The stored measured_at must be between before and after (server-stamped),
	// NOT the client-supplied 2000-01-01.
	if stampedAt.Before(before) || stampedAt.After(after) {
		t.Fatalf("measured_at not server-stamped: got %v (expected between %v and %v)",
			stampedAt, before, after)
	}
}

// TestDifferentUsersDifferentDevices: two users with the same device_key get
// separate rows (no cross-user leakage, unique is per user).
func TestDifferentUsersDifferentDevices(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	var userA, userB string
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('a@test.local','userA','x') RETURNING id::text`).Scan(&userA); err != nil {
		t.Fatalf("seed userA: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('b@test.local','userB','x') RETURNING id::text`).Scan(&userB); err != nil {
		t.Fatalf("seed userB: %v", err)
	}

	caps := json.RawMessage(`{"codecs":{"h264":true}}`)
	if _, err := store.Upsert(ctx, UpsertParams{UserID: userA, DeviceKey: "shared-key", Capabilities: caps}); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	if _, err := store.Upsert(ctx, UpsertParams{UserID: userB, DeviceKey: "shared-key", Capabilities: caps}); err != nil {
		t.Fatalf("upsert B: %v", err)
	}

	// Each user has exactly 1 row; total is 2 (no collision on unique).
	var total int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM user_devices WHERE device_key = 'shared-key'`).Scan(&total); err != nil {
		t.Fatalf("count total: %v", err)
	}
	if total != 2 {
		t.Fatalf("expected 2 rows (one per user), got %d", total)
	}
}

// --- HTTP-layer tests --------------------------------------------------------

// TestHTTPUpsertOwnerScoped: user A can upsert their own device. The user_id in
// the stored row must be A's id (from the token), never anything from the body.
func TestHTTPUpsertOwnerScoped(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newServer(t, pool)

	tokA := registerAndLogin(t, authSvc, "a@http.local", "httpA", "quasar-fixture-pw-08")

	// Look up user A's id from the DB.
	var userAID string
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text FROM users WHERE email = 'a@http.local'`).Scan(&userAID); err != nil {
		t.Fatalf("fetch userA id: %v", err)
	}

	body := map[string]any{
		"device_key":   "browser-uuid-a",
		"capabilities": map[string]any{"codecs": map[string]bool{"h264": true}, "rtt_ms": 15},
	}
	resp := doRequest(t, http.MethodPost, srv.URL+"/v1/me/devices", tokA, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/me/devices: got %d want 200", resp.StatusCode)
	}

	var out struct {
		Device struct {
			ID          string `json:"id"`
			FirstSeenAt string `json:"first_seen_at"`
			LastSeenAt  string `json:"last_seen_at"`
		} `json:"device"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Device.ID == "" {
		t.Fatal("device id missing from response")
	}

	// Verify the stored row's user_id is userA (owner-scoped, not any body field).
	var storedUserID string
	if err := pool.QueryRow(context.Background(),
		`SELECT user_id::text FROM user_devices WHERE device_key = 'browser-uuid-a'`).
		Scan(&storedUserID); err != nil {
		t.Fatalf("read stored user_id: %v", err)
	}
	if storedUserID != userAID {
		t.Fatalf("owner-scoping broken: stored user_id=%q want %q", storedUserID, userAID)
	}
}

// TestHTTPUpsertRequiresAuth: no bearer token → 401.
func TestHTTPUpsertRequiresAuth(t *testing.T) {
	pool := testDB(t)
	srv, _ := newServer(t, pool)

	resp := doRequest(t, http.MethodPost, srv.URL+"/v1/me/devices", "", map[string]any{
		"device_key":   "some-key",
		"capabilities": map[string]any{},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth POST: got %d want 401", resp.StatusCode)
	}
}

// TestHTTPValidation: missing device_key → 400; oversize body → 400.
func TestHTTPValidation(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newServer(t, pool)
	tok := registerAndLogin(t, authSvc, "v@http.local", "vUser", "quasar-fixture-pw-08")

	// Missing device_key.
	resp := doRequest(t, http.MethodPost, srv.URL+"/v1/me/devices", tok,
		map[string]any{"capabilities": map[string]any{"h264": true}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing device_key: got %d want 400", resp.StatusCode)
	}

	// Oversize body (> 8 KB).
	big := `{"device_key":"k","capabilities":{"pad":"` + strings.Repeat("x", 10000) + `"}}`
	resp2 := doRequest(t, http.MethodPost, srv.URL+"/v1/me/devices", tok, big)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversize body: got %d want 400", resp2.StatusCode)
	}

	// Invalid capabilities (not an object).
	resp3 := doRequest(t, http.MethodPost, srv.URL+"/v1/me/devices", tok,
		map[string]any{"device_key": "k", "capabilities": "notanobject"})
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-object capabilities: got %d want 400", resp3.StatusCode)
	}
}

// TestHTTPRateLimit: hammering the endpoint past the burst ceiling → 429.
func TestHTTPRateLimit(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}

	// Wire handler with a tight limiter (burst=2) so we hit the ceiling fast.
	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	dh := &Handler{store: store, limiter: newRateLimiter(2, time.Hour)}
	dh.Register(mux, authHandler.RequireAuth)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	tok := registerAndLogin(t, authSvc, "rl@http.local", "rlUser", "quasar-fixture-pw-08")

	body := map[string]any{
		"device_key":   "rl-key",
		"capabilities": map[string]any{"codecs": map[string]bool{"h264": true}},
	}

	var hitLimit bool
	for i := 0; i < 10; i++ {
		resp := doRequest(t, http.MethodPost, srv.URL+"/v1/me/devices", tok, body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			hitLimit = true
			break
		}
	}
	if !hitLimit {
		t.Fatal("rate limiter never fired 429")
	}
}

// TestHTTPExtendedCapabilityRoundTrip: POST an extended AS10-08 certification
// record, then GET /v1/me/devices and confirm the extended fields (browser,
// display, features, profiles incl. h264_profile_decoded) round-trip, and
// measured_at is server-stamped.
func TestHTTPExtendedCapabilityRoundTrip(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newServer(t, pool)
	tok := registerAndLogin(t, authSvc, "cert@http.local", "certUser", "quasar-fixture-pw-08")

	caps := map[string]any{
		"client_type":       "web",
		"codecs":            map[string]bool{"h264": true, "hevc": false, "av1": false, "vp9": true},
		"max_decode_height": 2160,
		"bandwidth_kbps":    48000,
		"rtt_ms":            12,
		"browser":           map[string]any{"name": "Chrome", "version": "126.0.0.0"},
		"platform":          "macOS",
		"display":           map[string]any{"width": 2560, "height": 1440, "refresh_hz": 120},
		"features": map[string]any{
			"jitter_buffer_target": true, "playout_delay_hint": true,
			"pointer_lock": true, "coalesced_pointer_events": true, "gamepad": true,
		},
		"profiles": map[string]any{
			"1080p60": map[string]any{
				"h264_profile_decoded": "constrained-baseline",
				"decode_pass":          true,
				"present_pass":         true,
				"decode_ms":            1.4,
				"present_fps":          59.8,
			},
		},
	}
	body := map[string]any{"device_key": "cert-key", "capabilities": caps}

	resp := doRequest(t, http.MethodPost, srv.URL+"/v1/me/devices", tok, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST: got %d want 200", resp.StatusCode)
	}

	getResp := doRequest(t, http.MethodGet, srv.URL+"/v1/me/devices", tok, nil)
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET: got %d want 200", getResp.StatusCode)
	}
	var out struct {
		Devices []struct {
			ID           string         `json:"id"`
			DeviceKey    string         `json:"device_key"`
			Capabilities map[string]any `json:"capabilities"`
		} `json:"devices"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&out); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if len(out.Devices) != 1 {
		t.Fatalf("GET list: got %d devices want 1", len(out.Devices))
	}
	if out.Devices[0].DeviceKey != "cert-key" {
		t.Fatalf("device_key mismatch: %q", out.Devices[0].DeviceKey)
	}
	c := out.Devices[0].Capabilities
	if c["client_type"] != "web" {
		t.Fatalf("client_type lost: %v", c["client_type"])
	}
	browser, _ := c["browser"].(map[string]any)
	if browser == nil || browser["name"] != "Chrome" {
		t.Fatalf("browser not round-tripped: %v", c["browser"])
	}
	display, _ := c["display"].(map[string]any)
	if display == nil || display["refresh_hz"] != float64(120) {
		t.Fatalf("display not round-tripped: %v", c["display"])
	}
	feats, _ := c["features"].(map[string]any)
	if feats == nil || feats["coalesced_pointer_events"] != true {
		t.Fatalf("features not round-tripped: %v", c["features"])
	}
	profiles, _ := c["profiles"].(map[string]any)
	p1080, _ := profiles["1080p60"].(map[string]any)
	if p1080 == nil || p1080["h264_profile_decoded"] != "constrained-baseline" {
		t.Fatalf("per-profile h264_profile_decoded not round-tripped: %v", profiles)
	}
	// measured_at must be server-stamped (present, parseable, recent-ish).
	mAt, _ := c["measured_at"].(string)
	if mAt == "" {
		t.Fatal("measured_at not server-stamped on the returned blob")
	}
	if _, err := time.Parse(time.RFC3339, mAt); err != nil {
		t.Fatalf("measured_at not RFC3339: %q (%v)", mAt, err)
	}
}

// TestHTTPNativeCapabilityRoundTrip: POST a full AS10-12 native capability report
// (additive superset of the web probe), then GET it back and confirm every native
// field round-trips verbatim — report_version, os, the rich decode matrix, audio,
// input.controllers, metrics (p50+p95+render_path), health, and the per-profile
// cert with the higher H.264 profile the browser cannot decode.
func TestHTTPNativeCapabilityRoundTrip(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newServer(t, pool)
	tok := registerAndLogin(t, authSvc, "native@http.local", "nativeUser", "quasar-fixture-pw-08")

	caps := map[string]any{
		"report_version":    1,
		"client_type":       "native",
		"client_name":       "quasar-native-macos",
		"client_version":    "0.1.0",
		"platform":          "macOS",
		"os":                map[string]any{"name": "macOS", "version": "15.5", "arch": "arm64"},
		"display":           map[string]any{"width": 3456, "height": 2234, "device_pixel_ratio": 2.0, "refresh_hz": 120, "hdr": true, "vrr": true},
		"codecs":            map[string]bool{"h264": true, "hevc": true, "av1": true, "vp9": false},
		"max_decode_height": 2160,
		"decode": map[string]any{
			"h264": map[string]any{"hw": true, "profiles": []string{"constrained-baseline", "main", "high"}, "levels": []string{"4.2", "5.1"}, "max_height": 2160},
			"hevc": map[string]any{"hw": true, "profiles": []string{"main", "main10"}, "levels": []string{"5.1"}, "max_height": 2160},
		},
		"audio": map[string]any{"channels": 2, "sample_rate": 48000, "codecs": []string{"opus"}},
		"input": map[string]any{
			"raw_mouse": true, "keyboard": true, "high_rate_input": true,
			"controllers": []map[string]any{
				{"type": "xbox", "rumble": true, "haptics": false, "player": 0},
				{"type": "dualsense", "rumble": true, "haptics": true, "player": 1},
			},
		},
		"metrics": map[string]any{
			"decode_ms": 1.8, "present_fps": 59.9, "present_interval_sd_ms": 1.2,
			"glass_to_glass_ms_p50": 45.0, "glass_to_glass_ms_p95": 104.0,
			"interactive_ms_p50": 54.0, "jitter_buffer_ms": 20.0,
			"render_path": "webrtcbin+videotoolbox",
		},
		"health": map[string]any{"class": "smooth", "reason": ""},
		"profiles": map[string]any{
			"1080p60": map[string]any{
				"h264_profile_decoded": "high",
				"decode_pass":          true, "present_pass": true,
				"decode_ms": 1.6, "present_fps": 59.8,
			},
		},
		"bandwidth_kbps": 92000,
		"rtt_ms":         6,
	}
	body := map[string]any{"device_key": "native-key", "capabilities": caps}

	resp := doRequest(t, http.MethodPost, srv.URL+"/v1/me/devices", tok, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST: got %d want 200", resp.StatusCode)
	}

	getResp := doRequest(t, http.MethodGet, srv.URL+"/v1/me/devices", tok, nil)
	defer getResp.Body.Close()
	var out struct {
		Devices []struct {
			Capabilities json.RawMessage `json:"capabilities"`
		} `json:"devices"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&out); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if len(out.Devices) != 1 {
		t.Fatalf("GET list: got %d devices want 1", len(out.Devices))
	}

	// Decode through the optional typed view to assert the native fields survived.
	var nc NativeCapabilities
	if err := json.Unmarshal(out.Devices[0].Capabilities, &nc); err != nil {
		t.Fatalf("unmarshal native view: %v", err)
	}
	if nc.ReportVersion == nil || *nc.ReportVersion != 1 {
		t.Fatalf("report_version not round-tripped: %v", nc.ReportVersion)
	}
	if nc.ClientType != "native" {
		t.Fatalf("client_type not round-tripped: %q", nc.ClientType)
	}
	if nc.OS == nil || nc.OS.Arch != "arm64" {
		t.Fatalf("os not round-tripped: %v", nc.OS)
	}
	if nc.Decode == nil || nc.Decode.H264 == nil || !nc.Decode.H264.HW || len(nc.Decode.H264.Profiles) != 3 {
		t.Fatalf("decode matrix not round-tripped: %v", nc.Decode)
	}
	if nc.Audio == nil || len(nc.Audio.Codecs) != 1 || nc.Audio.Codecs[0] != "opus" {
		t.Fatalf("audio not round-tripped: %v", nc.Audio)
	}
	if nc.Input == nil || len(nc.Input.Controllers) != 2 || nc.Input.Controllers[1].Type != "dualsense" {
		t.Fatalf("input.controllers not round-tripped: %v", nc.Input)
	}
	if nc.Metrics == nil || nc.Metrics.GlassToGlassP95Ms == nil || *nc.Metrics.GlassToGlassP95Ms != 104.0 ||
		nc.Metrics.RenderPath != "webrtcbin+videotoolbox" {
		t.Fatalf("metrics not round-tripped: %v", nc.Metrics)
	}
	if nc.Health == nil || nc.Health.Class != "smooth" {
		t.Fatalf("health not round-tripped: %v", nc.Health)
	}
	if nc.Display == nil || nc.Display.HDR == nil || !*nc.Display.HDR || nc.Display.VRR == nil || !*nc.Display.VRR {
		t.Fatalf("display.hdr/vrr not round-tripped: %v", nc.Display)
	}
	cert, ok := nc.Profiles["1080p60"]
	if !ok || cert.H264ProfileDecoded != "high" {
		t.Fatalf("native profile cert (high H.264) not round-tripped: %v", nc.Profiles)
	}
	// measured_at must be server-stamped on the stored blob.
	if nc.MeasuredAt == "" {
		t.Fatal("measured_at not server-stamped on native report")
	}
}

// TestNativeWebSubsetUnaffected is the #1 additive-safety gate: an existing web
// capability blob (no native-only fields) must store BYTE-IDENTICALLY after the
// AS10-12 changes. We POST a web blob, GET the stored capabilities, strip the
// server-stamped measured_at, and require the result to equal the sanitized web
// input exactly — proving the native-report code path mutates nothing for a web
// client.
func TestNativeWebSubsetUnaffected(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newServer(t, pool)
	tok := registerAndLogin(t, authSvc, "subset@http.local", "subsetUser", "quasar-fixture-pw-08")

	// A representative web probe blob — exactly what web/src/webrtc/capability.ts
	// posts (the web subset). No report_version, no native sub-objects.
	webCaps := map[string]any{
		"client_type":       "web",
		"codecs":            map[string]bool{"h264": true, "hevc": false, "av1": false, "vp9": true},
		"max_decode_height": 2160,
		"bandwidth_kbps":    48000,
		"rtt_ms":            12,
		"browser":           map[string]any{"name": "Chrome", "version": "126.0.0.0"},
		"platform":          "macOS",
		"display":           map[string]any{"width": 2560, "height": 1440, "device_pixel_ratio": 2.0, "refresh_hz": 120},
		"features": map[string]any{
			"jitter_buffer_target": true, "playout_delay_hint": true,
			"pointer_lock": true, "coalesced_pointer_events": true, "gamepad": true,
		},
		"profiles": map[string]any{},
	}

	// Compute the expected stored shape: run the exact same sanitizer the store
	// uses on the same input. (measured_at is added by the store afterwards.)
	rawIn, err := json.Marshal(webCaps)
	if err != nil {
		t.Fatalf("marshal web caps: %v", err)
	}
	cleaned, err := sanitizeCapabilities(rawIn)
	if err != nil {
		t.Fatalf("sanitize web caps: %v", err)
	}
	var want map[string]any
	if err := json.Unmarshal(cleaned, &want); err != nil {
		t.Fatalf("unmarshal sanitized: %v", err)
	}

	resp := doRequest(t, http.MethodPost, srv.URL+"/v1/me/devices", tok,
		map[string]any{"device_key": "subset-key", "capabilities": webCaps})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST: got %d want 200", resp.StatusCode)
	}

	getResp := doRequest(t, http.MethodGet, srv.URL+"/v1/me/devices", tok, nil)
	defer getResp.Body.Close()
	var out struct {
		Devices []struct {
			Capabilities map[string]any `json:"capabilities"`
		} `json:"devices"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&out); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if len(out.Devices) != 1 {
		t.Fatalf("GET list: got %d devices want 1", len(out.Devices))
	}

	got := out.Devices[0].Capabilities
	// The only difference the store introduces is the server-stamped measured_at.
	if _, ok := got["measured_at"].(string); !ok {
		t.Fatal("expected a server-stamped measured_at on the stored web blob")
	}
	delete(got, "measured_at")

	// Byte-identical gate: the stored web blob (minus measured_at) must equal the
	// sanitized web input exactly. reflect.DeepEqual over the decoded maps is the
	// value-level form of "byte-identical" (both came from canonical json.Marshal).
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		t.Fatalf("web subset NOT stored identically after AS10-12 changes:\n got=%s\nwant=%s", gotJSON, wantJSON)
	}
}

// TestHTTPListEmpty: GET /v1/me/devices with no prior POST → 200 with an empty list
// (LP-SEC-01 D3: the endpoint is now a list, not single-latest — an absent device is an
// empty array, not a 404).
func TestHTTPListEmpty(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newServer(t, pool)
	tok := registerAndLogin(t, authSvc, "nodev@http.local", "nodevUser", "quasar-fixture-pw-08")

	resp := doRequest(t, http.MethodGet, srv.URL+"/v1/me/devices", tok, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET with no device: got %d want 200", resp.StatusCode)
	}
	var out struct {
		Devices []json.RawMessage `json:"devices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if len(out.Devices) != 0 {
		t.Fatalf("empty list: got %d devices want 0", len(out.Devices))
	}
}

// TestHTTPGetLatestRequiresAuth: GET /v1/me/devices without a token → 401.
func TestHTTPGetLatestRequiresAuth(t *testing.T) {
	pool := testDB(t)
	srv, _ := newServer(t, pool)

	resp := doRequest(t, http.MethodGet, srv.URL+"/v1/me/devices", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth GET: got %d want 401", resp.StatusCode)
	}
}

// TestHTTPSanitizationClampsStoredBlob: an oversized string in capabilities is
// clamped server-side before storage (GET returns the clamped value).
func TestHTTPSanitizationClampsStoredBlob(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newServer(t, pool)
	tok := registerAndLogin(t, authSvc, "san@http.local", "sanUser", "quasar-fixture-pw-08")

	// 600-char string (> maxStringLen=512) but small enough the whole body is < 8KB.
	bigName := strings.Repeat("z", 600)
	body := map[string]any{
		"device_key":   "san-key",
		"capabilities": map[string]any{"client_type": "bogus", "browser": map[string]any{"name": bigName}},
	}
	resp := doRequest(t, http.MethodPost, srv.URL+"/v1/me/devices", tok, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST: got %d want 200", resp.StatusCode)
	}

	getResp := doRequest(t, http.MethodGet, srv.URL+"/v1/me/devices", tok, nil)
	defer getResp.Body.Close()
	var out struct {
		Devices []struct {
			Capabilities map[string]any `json:"capabilities"`
		} `json:"devices"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Devices) != 1 {
		t.Fatalf("GET list: got %d devices want 1", len(out.Devices))
	}
	c := out.Devices[0].Capabilities
	if c["client_type"] != "web" {
		t.Fatalf("bad client_type not normalised: %v", c["client_type"])
	}
	browser, _ := c["browser"].(map[string]any)
	name, _ := browser["name"].(string)
	if len([]rune(name)) != maxStringLen {
		t.Fatalf("oversized string not clamped server-side: len=%d want %d", len([]rune(name)), maxStringLen)
	}
}

// TestHTTPIdempotentUpsert: posting the same device_key twice inserts 1 row then
// updates it (count stays 1).
func TestHTTPIdempotentUpsert(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newServer(t, pool)
	tok := registerAndLogin(t, authSvc, "idem@http.local", "idemUser", "quasar-fixture-pw-08")

	var userID string
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text FROM users WHERE email = 'idem@http.local'`).Scan(&userID); err != nil {
		t.Fatalf("fetch user id: %v", err)
	}

	body := map[string]any{
		"device_key":   "idem-key",
		"capabilities": map[string]any{"codecs": map[string]bool{"h264": true}},
	}

	resp := doRequest(t, http.MethodPost, srv.URL+"/v1/me/devices", tok, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first POST: got %d want 200", resp.StatusCode)
	}

	resp2 := doRequest(t, http.MethodPost, srv.URL+"/v1/me/devices", tok, body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second POST: got %d want 200", resp2.StatusCode)
	}

	if n := countDevices(t, pool, userID, "idem-key"); n != 1 {
		t.Fatalf("expected 1 row after 2 POSTs, got %d", n)
	}
}
