package session

// device_scope_db_test.go — every per-client decision is keyed on the device
// making the request, not on the account's most recently seen one.
//
// DB-gated like the rest of the package: testDB skips without TEST_DATABASE_URL,
// and `make test-db` is the runner that supplies it.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
)

// seedDeviceCaps upserts one (user, device_key) row with the given capabilities
// and an explicit last_seen_at, returning its user_devices id. last_seen_at is
// explicit because the ordering between two devices is what these tests vary.
func seedDeviceCaps(t *testing.T, pool *pgxpool.Pool, userID, deviceKey string, caps map[string]any, lastSeen time.Time) string {
	t.Helper()
	if _, ok := caps["measured_at"]; !ok {
		caps["measured_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(caps)
	if err != nil {
		t.Fatalf("marshal caps: %v", err)
	}
	var id string
	must(t, pool.QueryRow(context.Background(), `
		INSERT INTO user_devices (user_id, device_key, capabilities, last_seen_at)
		VALUES ($1::uuid, $2, $3::jsonb, $4)
		ON CONFLICT (user_id, device_key) DO UPDATE
		    SET capabilities = EXCLUDED.capabilities,
		        last_seen_at = EXCLUDED.last_seen_at
		RETURNING id::text
	`, userID, deviceKey, raw, lastSeen).Scan(&id))
	return id
}

// goodLinkCaps are the network numbers every fixture here wants: generous enough
// that only the varied dimension (codecs, decode profiles, tier) decides.
func goodLinkCaps() map[string]any {
	return map[string]any{"bandwidth_kbps": 50000, "rtt_ms": 5, "max_decode_height": 2160}
}

// seedDeviceProbe is seedDeviceCaps with a codec probe on a good link.
func seedDeviceProbe(t *testing.T, pool *pgxpool.Pool, userID, deviceKey string, hevc, av1 bool, lastSeen time.Time) string {
	t.Helper()
	caps := goodLinkCaps()
	caps["codecs"] = map[string]bool{"h264": true, "hevc": hevc, "av1": av1}
	return seedDeviceCaps(t, pool, userID, deviceKey, caps, lastSeen)
}

// touchDevice moves a device to the front of the last_seen_at ordering the
// fallback read uses.
func touchDevice(t *testing.T, pool *pgxpool.Pool, deviceID string, lastSeen time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE user_devices SET last_seen_at = $2 WHERE id = $1::uuid`, deviceID, lastSeen); err != nil {
		t.Fatalf("touch device: %v", err)
	}
}

func seedUser(t *testing.T, pool *pgxpool.Pool, email, username string) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(),
		`INSERT INTO users (email, username, password_hash) VALUES ($1,$2,'x') RETURNING id::text`,
		email, username).Scan(&id))
	return id
}

func newUUID(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&id))
	return id
}

// syncBuffer collects log output; the store logs from whatever goroutine calls it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureDefaultLogs redirects slog.Default for one test.
func captureDefaultLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func loginTokWithDevice(t *testing.T, svc *auth.Service, email, pass, deviceKey string) string {
	t.Helper()
	tok, err := svc.LoginWithDevice(context.Background(), email, pass, "", deviceKey)
	if err != nil {
		t.Fatalf("login %s (device %s): %v", email, deviceKey, err)
	}
	return tok.Plaintext
}

// --- store: the resolver's own rules -----------------------------------------

// TestDeviceScopeIsOwnerScoped: another account's device id must resolve as
// absent. Dropping the user_id predicate from deviceScopeByID makes this fail.
func TestDeviceScopeIsOwnerScoped(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	userA := seedUser(t, pool, "owner-a@test.local", "ownera")
	userB := seedUser(t, pool, "owner-b@test.local", "ownerb")
	seedDeviceProbe(t, pool, userA, "a-device", true, false, time.Now())
	foreign := seedDeviceProbe(t, pool, userB, "b-device", true, true, time.Now())

	scope, err := store.ResolveDeviceScope(ctx, userA, foreign, scopeSiteProfiles)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if scope.DeviceKey == "b-device" || (scope.Probe != nil && scope.Probe.AV1) {
		t.Fatalf("resolved another account's device: key=%q probe=%+v", scope.DeviceKey, scope.Probe)
	}
	if !scope.Fallback {
		t.Errorf("foreign device id must resolve as absent (fallback), got Fallback=false")
	}
	if scope.DeviceKey != "a-device" {
		t.Errorf("device key = %q, want the owner's own latest-seen device", scope.DeviceKey)
	}
}

// TestForeignDeviceIDDoesNotDecideALaunch is the same rule at the launch seam:
// a session device_id from another account cannot lift this launch's codec.
func TestForeignDeviceIDDoesNotDecideALaunch(t *testing.T) {
	pool := testDB(t)
	userID, appID, hostID := seed1080pApp(t, pool)
	coord := newTestCoordinator(t, NewStore(pool), newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	enableChainCodecs(t, pool, "1080p60", "av1", "h264")
	setHostCodecs(t, pool, hostID, `["h264","h265","av1"]`)
	seedDeviceProbe(t, pool, userID, "own-device", true, false, time.Now())

	other := seedUser(t, pool, "foreign@test.local", "foreignuser")
	foreign := seedDeviceProbe(t, pool, other, "foreign-device", true, true, time.Now())

	res, err := coord.LaunchByProfile(ctx, userID,
		LaunchParams{AppID: appID, ProfileID: "1080p60", IsAdmin: true, DeviceID: foreign})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.Codec != "h264" {
		t.Errorf("session codec = %q, want h264 — a foreign device id must not supply the probe",
			res.Session.Codec)
	}
}

// TestUnknownDeviceIDFallsBackAndLogs: a well-formed id naming no row (deleted,
// or minted elsewhere) resolves exactly like an unbound caller.
func TestUnknownDeviceIDFallsBackAndLogs(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	userID := seedUser(t, pool, "unknown-dev@test.local", "unknowndev")
	seedDeviceProbe(t, pool, userID, "only-device", true, true, time.Now())

	logs := captureDefaultLogs(t)
	scope, err := store.ResolveDeviceScope(ctx, userID, newUUID(t, pool), scopeSiteRung)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !scope.Fallback {
		t.Errorf("Fallback = false, want true for an id naming no row")
	}
	if scope.DeviceKey != "only-device" || scope.Probe == nil || !scope.Probe.AV1 {
		t.Errorf("scope = %+v, want the account's latest-seen device", scope)
	}
	out := logs.String()
	if !strings.Contains(out, "device-scope fallback") || !strings.Contains(out, scopeSiteRung) {
		t.Errorf("fallback log missing the tag or the site: %q", out)
	}
}

// --- launch: the resolved rung follows the launching device -------------------

// TestLaunchProbeIsScopedToTheLaunchingDevice: two devices, different av1 decode.
// The resolved codec follows the launching device in either ordering.
func TestLaunchProbeIsScopedToTheLaunchingDevice(t *testing.T) {
	pool := testDB(t)
	userID, appID, hostID := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	enableChainCodecs(t, pool, "1080p60", "av1", "h264")
	setHostCodecs(t, pool, hostID, `["h264","h265","av1"]`)

	now := time.Now()
	browser := seedDeviceProbe(t, pool, userID, "browser-av1", true, true, now.Add(-time.Hour))
	native := seedDeviceProbe(t, pool, userID, "native-no-av1", true, false, now)

	res, err := coord.LaunchByProfile(ctx, userID,
		LaunchParams{AppID: appID, ProfileID: "1080p60", IsAdmin: true, DeviceID: browser})
	if err != nil {
		t.Fatalf("browser launch: %v", err)
	}
	if res.Session.Codec != "av1" {
		t.Errorf("browser session codec = %q, want av1 (its own probe, not the latest-seen device's)",
			res.Session.Codec)
	}
	if res.Session.DeviceID == nil || *res.Session.DeviceID != browser {
		t.Errorf("sessions.device_id = %v, want %s", res.Session.DeviceID, browser)
	}
	stopSessionRow(t, pool, res.Session.ID)

	touchDevice(t, pool, browser, time.Now().Add(time.Hour))
	res, err = coord.LaunchByProfile(ctx, userID,
		LaunchParams{AppID: appID, ProfileID: "1080p60", IsAdmin: true, DeviceID: native})
	if err != nil {
		t.Fatalf("native launch: %v", err)
	}
	if res.Session.Codec != "h264" {
		t.Errorf("native session codec = %q, want h264 (that device cannot decode av1)", res.Session.Codec)
	}
	if res.Session.DeviceID == nil || *res.Session.DeviceID != native {
		t.Errorf("sessions.device_id = %v, want %s", res.Session.DeviceID, native)
	}
}

// TestNativeH264LiftIsScopedToTheLaunchingDevice: the Path-B lift reads the
// launching device's decode matrix, so a browser device on the same account
// neither grants nor withholds it.
func TestNativeH264LiftIsScopedToTheLaunchingDevice(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	coord := newTestCoordinator(t, NewStore(pool), newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	now := time.Now()
	nativeCaps := goodLinkCaps()
	nativeCaps["client_type"] = "native"
	nativeCaps["decode"] = map[string]any{"h264": map[string]any{"profiles": []string{"constrained-baseline", "main", "high"}}}
	nativeDev := seedDeviceCaps(t, pool, userID, "native-high", nativeCaps, now.Add(-time.Hour))
	browserDev := seedDeviceCaps(t, pool, userID, "browser", goodLinkCaps(), now)

	// Bound to the native device while the browser is the latest-seen row.
	res, err := coord.LaunchByProfile(ctx, userID,
		LaunchParams{AppID: appID, ProfileID: "1080p60", ClientType: "native", DeviceID: nativeDev})
	if err != nil {
		t.Fatalf("native launch: %v", err)
	}
	if res.Session.H264Profile != "high" {
		t.Errorf("h264_profile = %q, want high (the launching device decodes it)", res.Session.H264Profile)
	}
	stopSessionRow(t, pool, res.Session.ID)

	// Bound to the browser device while the native one is the latest-seen row.
	touchDevice(t, pool, nativeDev, time.Now().Add(time.Hour))
	res, err = coord.LaunchByProfile(ctx, userID,
		LaunchParams{AppID: appID, ProfileID: "1080p60", ClientType: "native", DeviceID: browserDev})
	if err != nil {
		t.Fatalf("browser launch: %v", err)
	}
	if res.Session.H264Profile != "constrained-baseline" {
		t.Errorf("h264_profile = %q, want the floor (this device's probe is not native)", res.Session.H264Profile)
	}
}

// TestLegacyTierIsScopedToTheLaunchingDevice: the tier ladder on the legacy path
// (no profile_id, explicit override) reads the launching device's link probe.
func TestLegacyTierIsScopedToTheLaunchingDevice(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	coord := newTestCoordinator(t, NewStore(pool), newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	now := time.Now()
	fastDev := seedDeviceCaps(t, pool, userID, "fast-link", goodLinkCaps(), now.Add(-time.Hour))
	slowDev := seedDeviceCaps(t, pool, userID, "slow-link",
		map[string]any{"bandwidth_kbps": 4000, "rtt_ms": 150, "max_decode_height": 720}, now)

	bitrate := int32(5000)
	launch := func(deviceID string) Session {
		t.Helper()
		res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{
			AppID:    appID,
			DeviceID: deviceID,
			Override: StreamOverride{BitrateKbps: &bitrate},
		})
		if err != nil {
			t.Fatalf("legacy launch: %v", err)
		}
		return res.Session
	}

	sess := launch(fastDev)
	if sess.Height != 1080 || sess.FPS != 60 {
		t.Errorf("fast device tier = %dx%d@%d, want 1080p60 (its own link probe)", sess.Width, sess.Height, sess.FPS)
	}
	stopSessionRow(t, pool, sess.ID)

	touchDevice(t, pool, fastDev, time.Now().Add(time.Hour))
	sess = launch(slowDev)
	if sess.Height != 720 || sess.FPS != 30 {
		t.Errorf("slow device tier = %dx%d@%d, want 720p30 (its own link probe)", sess.Width, sess.Height, sess.FPS)
	}
}

// TestDecodeFailureHistoryIsScopedToItsDevice: a decode failure recorded for one
// device must not clamp another device on the same account.
func TestDecodeFailureHistoryIsScopedToItsDevice(t *testing.T) {
	pool := testDB(t)
	userID, appID, hostID := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	enableChainCodecs(t, pool, "1080p60", "hevc", "h264")
	setHostCodecs(t, pool, hostID, `["h264","h265"]`)

	now := time.Now()
	deviceB := seedDeviceProbe(t, pool, userID, "device-b", true, false, now.Add(-time.Hour))
	deviceA := seedDeviceProbe(t, pool, userID, "device-a", true, false, now)

	if err := store.RecordProfileOutcome(ctx, userID, "device-a", "1080p60", "h265",
		outcomeFail, strptr("decode_degrading")); err != nil {
		t.Fatalf("record h265 fail for device A: %v", err)
	}

	res, err := coord.LaunchByProfile(ctx, userID,
		LaunchParams{AppID: appID, ProfileID: "1080p60", IsAdmin: true, DeviceID: deviceB})
	if err != nil {
		t.Fatalf("device B launch: %v", err)
	}
	if res.Session.Codec != "h265" {
		t.Errorf("device B session codec = %q, want h265 (another device's failure must not clamp it)",
			res.Session.Codec)
	}
	stopSessionRow(t, pool, res.Session.ID)

	res, err = coord.LaunchByProfile(ctx, userID,
		LaunchParams{AppID: appID, ProfileID: "1080p60", IsAdmin: true, DeviceID: deviceA})
	if err != nil {
		t.Fatalf("device A launch: %v", err)
	}
	if res.Session.Codec != "h264" {
		t.Errorf("device A session codec = %q, want h264 (its own failure clamps h265)", res.Session.Codec)
	}
}

// TestUnboundLaunchFallsBackToTheLatestSeenDevice: an unbound caller still gets a
// probe-informed launch, from the account's latest-seen device.
func TestUnboundLaunchFallsBackToTheLatestSeenDevice(t *testing.T) {
	pool := testDB(t)
	userID, appID, hostID := seed1080pApp(t, pool)
	coord := newTestCoordinator(t, NewStore(pool), newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	enableChainCodecs(t, pool, "1080p60", "av1", "h264")
	setHostCodecs(t, pool, hostID, `["h264","h265","av1"]`)

	now := time.Now()
	av1Device := seedDeviceProbe(t, pool, userID, "fallback-av1", true, true, now.Add(-time.Hour))
	seedDeviceProbe(t, pool, userID, "fallback-no-av1", true, false, now)

	res, err := coord.LaunchByProfile(ctx, userID,
		LaunchParams{AppID: appID, ProfileID: "1080p60", IsAdmin: true})
	if err != nil {
		t.Fatalf("unbound launch: %v", err)
	}
	if res.Session.Codec != "h264" {
		t.Errorf("unbound session codec = %q, want h264 (latest-seen device cannot decode av1)", res.Session.Codec)
	}
	if res.Session.DeviceID != nil {
		t.Errorf("sessions.device_id = %v, want NULL for an unbound caller", *res.Session.DeviceID)
	}
	stopSessionRow(t, pool, res.Session.ID)

	touchDevice(t, pool, av1Device, time.Now().Add(time.Hour))
	res, err = coord.LaunchByProfile(ctx, userID,
		LaunchParams{AppID: appID, ProfileID: "1080p60", IsAdmin: true})
	if err != nil {
		t.Fatalf("second unbound launch: %v", err)
	}
	if res.Session.Codec != "av1" {
		t.Errorf("unbound session codec = %q, want av1 (the latest-seen device now decodes it)", res.Session.Codec)
	}
}

// --- GET /v1/me/profiles ------------------------------------------------------

// TestProfilesEndpointScopesTheProbeToTheRequestingDevice: two tokens, one
// account, different capabilities.codecs — each token sees its own eligibility.
func TestProfilesEndpointScopesTheProbeToTheRequestingDevice(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()

	const email, pass = "twodev@test.local", "quasar-fixture-pw-08"
	u, err := authSvc.Register(ctx, email, "twodevuser", pass)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Each login binds its token to its own user_devices row.
	browserTok := loginTokWithDevice(t, authSvc, email, pass, "browser-av1")
	nativeTok := loginTokWithDevice(t, authSvc, email, pass, "native-no-av1")

	enableChainCodecs(t, pool, "1440p60", "av1", "hevc", "h264")

	now := time.Now()
	browser := seedDeviceProbe(t, pool, u.ID, "browser-av1", true, true, now.Add(-time.Hour))
	seedDeviceProbe(t, pool, u.ID, "native-no-av1", true, false, now)

	av1Eligible := func(tok string) string {
		t.Helper()
		resp, body := getProfiles(t, srv.URL+"/v1/me/profiles", tok)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/me/profiles = %d, want 200", resp.StatusCode)
		}
		r := rungByID(body, "1440p60-av1")
		if r == nil {
			t.Fatalf("1440p60-av1 rung missing from the response")
		}
		return r.Eligibility
	}

	if got := av1Eligible(browserTok); got != "eligible" {
		t.Errorf("browser token: 1440p60-av1 eligibility = %q, want eligible", got)
	}
	if got := av1Eligible(nativeTok); got != "ineligible" {
		t.Errorf("native token: 1440p60-av1 eligibility = %q, want ineligible", got)
	}

	touchDevice(t, pool, browser, time.Now().Add(time.Hour))
	if got := av1Eligible(nativeTok); got != "ineligible" {
		t.Errorf("native token (browser seen last): 1440p60-av1 eligibility = %q, want ineligible", got)
	}
	if got := av1Eligible(browserTok); got != "eligible" {
		t.Errorf("browser token (browser seen last): 1440p60-av1 eligibility = %q, want eligible", got)
	}
}

// TestProfilesEndpointUnboundTokenFallsBackToLatestSeen: an unbound token still
// gets a probe-informed answer.
func TestProfilesEndpointUnboundTokenFallsBackToLatestSeen(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()

	const email, pass = "unbound@test.local", "quasar-fixture-pw-08"
	u, err := authSvc.Register(ctx, email, "unbounduser", pass)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	tok := loginTok(t, authSvc, email, pass) // no device_key ⇒ auth_tokens.device_id NULL

	enableChainCodecs(t, pool, "1440p60", "av1", "hevc", "h264")
	now := time.Now()
	av1Device := seedDeviceProbe(t, pool, u.ID, "fallback-av1", true, true, now.Add(-time.Hour))
	seedDeviceProbe(t, pool, u.ID, "fallback-no-av1", true, false, now)

	resp, body := getProfiles(t, srv.URL+"/v1/me/profiles", tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/me/profiles = %d, want 200", resp.StatusCode)
	}
	if r := rungByID(body, "1440p60-av1"); r == nil {
		t.Fatalf("1440p60-av1 rung missing")
	} else if r.Eligibility != "ineligible" {
		t.Errorf("unbound token: 1440p60-av1 eligibility = %q, want ineligible (latest-seen device has no av1)", r.Eligibility)
	}

	touchDevice(t, pool, av1Device, time.Now().Add(time.Hour))
	resp, body = getProfiles(t, srv.URL+"/v1/me/profiles", tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/me/profiles = %d, want 200", resp.StatusCode)
	}
	if r := rungByID(body, "1440p60-av1"); r == nil {
		t.Fatalf("1440p60-av1 rung missing")
	} else if r.Eligibility != "eligible" {
		t.Errorf("unbound token: 1440p60-av1 eligibility = %q, want eligible (latest-seen device now decodes av1)", r.Eligibility)
	}
}

// --- the session stamp and the outcome key ------------------------------------

// TestPostSessionsStampsTheRequestingDevice: the launch records the token's bound
// device, which is what every later read of that session scopes by.
func TestPostSessionsStampsTheRequestingDevice(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newMetricsServer(t, pool)
	ctx := context.Background()

	_, appID, hostID := seed1080pApp(t, pool)
	setHostCodecs(t, pool, hostID, `["h264"]`)

	const email, pass = "stamp@test.local", "quasar-fixture-pw-08"
	u, err := authSvc.Register(ctx, email, "stampuser", pass)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	boundTok := loginTokWithDevice(t, authSvc, email, pass, "stamp-device")
	deviceID := seedDeviceProbe(t, pool, u.ID, "stamp-device", false, false, time.Now())

	launch := func(tok string) Session {
		t.Helper()
		resp := doJSON(t, "POST", srv.URL+"/v1/sessions", tok,
			map[string]any{"app_id": appID, "profile_id": "1080p60"})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("POST /v1/sessions = %d, want 201", resp.StatusCode)
		}
		var body struct {
			Session struct {
				ID string `json:"id"`
			} `json:"session"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode launch body: %v", err)
		}
		sess, err := store.Get(ctx, body.Session.ID)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		return sess
	}

	sess := launch(boundTok)
	if sess.DeviceID == nil || *sess.DeviceID != deviceID {
		t.Errorf("sessions.device_id = %v, want the token's bound device %s", sess.DeviceID, deviceID)
	}
	stopSessionRow(t, pool, sess.ID)

	sess = launch(loginTok(t, authSvc, email, pass))
	if sess.DeviceID != nil {
		t.Errorf("sessions.device_id = %v, want NULL for a token with no device binding", *sess.DeviceID)
	}
}

// TestClientHealthOutcomeKeysOnTheSessionsDevice: a live session's verdict is
// written under the device it launched from, so that device reads it back.
func TestClientHealthOutcomeKeysOnTheSessionsDevice(t *testing.T) {
	pool := testDB(t)
	store, coord, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	deviceID := seedDeviceProbe(t, pool, s.userID, "health-device", true, true, time.Now())

	sess := runningProfileSession(t, store, pool, s, "1080p60")
	if _, err := pool.Exec(ctx,
		`UPDATE sessions SET device_id = $2::uuid WHERE id::text = $1`, sess.ID, deviceID); err != nil {
		t.Fatalf("stamp session device: %v", err)
	}

	// The sample declares no device key of its own.
	coord.health.mu.Lock()
	coord.health.clientRuns[sess.ID] = &clientHealthRun{class: ClientHealthDecode, since: time.Now().Add(-time.Minute)}
	coord.health.mu.Unlock()
	coord.EvaluateClientHealth(ctx, sess.ID, ClientHealthSample{Class: ClientHealthDecode})

	banned, _ := store.RungFailures(ctx, s.userID, "health-device", mustGetLaunchProfile(t, store, "1080p60"))
	if !banned["1080p60-h264"] {
		t.Errorf("fail not recorded against the session's device key, got %v", banned)
	}
	other, _ := store.RungFailures(ctx, s.userID, "another-device", mustGetLaunchProfile(t, store, "1080p60"))
	if other["1080p60-h264"] {
		t.Errorf("the fail leaked onto another device's history: %v", other)
	}
}

// TestClientHealthOutcomeFallsBackToTheSampleDeviceKey: an unbound session keys
// on the sample's own device_key.
func TestClientHealthOutcomeFallsBackToTheSampleDeviceKey(t *testing.T) {
	pool := testDB(t)
	store, coord, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess := runningProfileSession(t, store, pool, s, "1080p60") // device_id stays NULL

	coord.health.mu.Lock()
	coord.health.clientRuns[sess.ID] = &clientHealthRun{class: ClientHealthDecode, since: time.Now().Add(-time.Minute)}
	coord.health.mu.Unlock()
	coord.EvaluateClientHealth(ctx, sess.ID, ClientHealthSample{Class: ClientHealthDecode, DeviceKey: "legacy-device"})

	banned, _ := store.RungFailures(ctx, s.userID, "legacy-device", mustGetLaunchProfile(t, store, "1080p60"))
	if !banned["1080p60-h264"] {
		t.Errorf("unbound session must still record against the sample's device key, got %v", banned)
	}
}
