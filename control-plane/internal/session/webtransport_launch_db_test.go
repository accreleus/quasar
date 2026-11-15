package session

// WebTransport at launch and at reconnect, end to end against the 0111 schema: the plan's
// decision persists, the 201 carries transport_options beside signaling, only the transport
// token's hash reaches the agent, and the reconnect answers after the agent's ack.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
)

const wtCertA = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

type wtFixture struct {
	pool    *pgxpool.Pool
	store   *Store
	disp    *fakeDispatcher
	coord   *Coordinator
	userID  string
	appID   string
	hostID  string
	authSvc *auth.Service
	srv     *httptest.Server
	userTok string
}

// newWTFixture seeds a host that reports a WebTransport endpoint, a device that proves
// WebTransport + WebCodecs for h264 and opus, and sets the 1080p60 profile's transport.
func newWTFixture(t *testing.T, profileTransport string) *wtFixture {
	t.Helper()
	pool := testDB(t)
	_, appID, hostID := seed1080pApp(t, pool)
	store := NewStore(pool)
	disp := newFakeDispatcher(true)
	// The fake signals every acked command on a buffer of 8 that nothing reads here.
	stopDrain := make(chan struct{})
	go func() {
		for {
			select {
			case <-disp.sawAssign:
			case <-stopDrain:
				return
			}
		}
	}()
	t.Cleanup(func() { close(stopDrain) })
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	must(t, exec(t, pool, `UPDATE launch_profiles SET transport = $2 WHERE id = $1`, "1080p60", profileTransport))
	must(t, exec(t, pool, `UPDATE hosts SET transports = '["webrtc","webtransport"]', wt_port = 4433,
		wt_addresses = '["192.0.2.10"]',
		wt_certificates = jsonb_build_array(jsonb_build_object('sha256', $2::text,
			'not_before', now() - interval '1 day', 'not_after', now() + interval '10 days'))
		WHERE id = $1::uuid`, hostID, wtCertA))
	caps := `{"bandwidth_kbps":50000,"rtt_ms":5,"max_decode_height":2160,"measured_at":"` +
		time.Now().UTC().Format(time.RFC3339) + `","codecs":{"h264":true},
		"webtransport":{"available":true,"datagrams":true},
		"webcodecs":{"h264":true,"hevc":true,"av1":true,"opus_decode":true,"opus_encode":true}}`

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	NewHandler(coord, store).Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, err := authSvc.Register(ctx, "wt@test.local", "wtuser", "quasar-fixture-pw-04")
	if err != nil {
		t.Fatal(err)
	}
	must(t, exec(t, pool, `INSERT INTO user_devices (user_id, device_key, capabilities)
		SELECT $1::uuid, 'wt-device', $2::jsonb`, u.ID, caps))
	return &wtFixture{pool: pool, store: store, disp: disp, coord: coord, userID: u.ID, appID: appID,
		hostID: hostID, authSvc: authSvc, srv: srv, userTok: loginTok(t, authSvc, "wt@test.local", "quasar-fixture-pw-04")}
}

func (f *wtFixture) launch(t *testing.T, body map[string]any) (int, map[string]any) {
	t.Helper()
	body["app_id"] = f.appID
	body["profile_id"] = "1080p60"
	resp := doJSON(t, "POST", f.srv.URL+"/v1/sessions", f.userTok, body)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// launchSecond launches a second session for the same user (the quota allows it).
func (f *wtFixture) launchSecond(t *testing.T) (int, string) {
	t.Helper()
	status, body := f.launch(t, map[string]any{})
	if status != http.StatusCreated {
		t.Fatalf("second launch: %d %v", status, body)
	}
	return status, body["session"].(map[string]any)["id"].(string)
}

func (f *wtFixture) waitDispatched(t *testing.T) {
	t.Helper()
	waitFor(t, func() bool { return len(f.disp.types()) >= 2 })
}

func tokenRows(t *testing.T, pool *pgxpool.Pool, sessionID string) map[string][]string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT kind, token_hash FROM session_tokens WHERE session_id = $1::uuid ORDER BY created_at`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var kind, hash string
		must(t, rows.Scan(&kind, &hash))
		out[kind] = append(out[kind], hash)
	}
	return out
}

func TestLaunchChoosesWebTransport(t *testing.T) {
	f := newWTFixture(t, "webtransport")
	status, body := f.launch(t, map[string]any{})
	if status != http.StatusCreated {
		t.Fatalf("launch: %d %v", status, body)
	}
	f.waitDispatched(t)

	sess := body["session"].(map[string]any)
	if sess["transport"] != "webtransport" || sess["transport_reason"] != nil || sess["transport_fallback_reason"] != nil {
		t.Errorf("session transport fields: %v", sess)
	}
	// The signaling block is always there: WebRTC is the fallback floor.
	sig, ok := body["signaling"].(map[string]any)
	if !ok || sig["token"] == "" || sig["url"] == "" {
		t.Fatalf("signaling block missing or empty: %v", body["signaling"])
	}
	wt := body["transport_options"].(map[string]any)["webtransport"].(map[string]any)
	if wt["url"] != "https://192.0.2.10:4433/wt" || wt["token"] == "" || wt["expires_at"] == "" {
		t.Errorf("webtransport options: %v", wt)
	}
	hashes := wt["cert_hashes"].([]any)
	if len(hashes) != 1 || hashes[0].(map[string]any)["algorithm"] != "sha-256" || hashes[0].(map[string]any)["value"] != wtCertA {
		t.Errorf("cert_hashes: %v", hashes)
	}
	if wt["token"] == sig["token"] {
		t.Error("the transport token equals the signaling token")
	}

	// Persisted, and only the transport token's hash went to the agent.
	id := sess["id"].(string)
	got, err := f.store.Get(context.Background(), id)
	if err != nil || got.Transport != "webtransport" || got.TransportReason != nil {
		t.Fatalf("persisted: %+v (%v)", got, err)
	}
	assign := f.disp.lastAssignCmd()
	if assign.Transport != "webtransport" || assign.WTTokenHash != hashToken(wt["token"].(string)) || assign.WTTokenExpiresAt == nil {
		t.Errorf("assign: %+v", assign)
	}
	if assign.WTTokenHash == hashToken(sig["token"].(string)) {
		t.Error("the agent was sent the signaling token's hash")
	}
	rows := tokenRows(t, f.pool, id)
	if len(rows["signaling"]) != 1 || len(rows["transport"]) != 1 || rows["transport"][0] != assign.WTTokenHash {
		t.Errorf("token rows: %v", rows)
	}
	// The stored form is a hash: the plaintext is nowhere in the table.
	var n int
	must(t, f.pool.QueryRow(context.Background(), `SELECT count(*) FROM session_tokens WHERE token_hash = $1 OR token_hash = $2`,
		wt["token"], sig["token"]).Scan(&n))
	if n != 0 {
		t.Error("a plaintext token is stored")
	}
}

// A "default" profile launches on the instance setting, read once per launch (amendment 61):
// webtransport follows it, a request for WebTransport on a webrtc instance is refused with
// instance_default_disallows, and changing the setting moves the next launch only.
func TestLaunchFollowsTheInstanceDefaultTransport(t *testing.T) {
	setDefault := func(t *testing.T, f *wtFixture, v string) {
		t.Helper()
		must(t, exec(t, f.pool, `INSERT INTO instance_settings (id, default_transport) VALUES (true, $1)
			ON CONFLICT (id) DO UPDATE SET default_transport = EXCLUDED.default_transport`, v))
	}
	tests := []struct {
		name, setting string
		request       map[string]any
		wantTransport string
		wantReason    any
	}{
		{"webtransport instance, no request", "webtransport", map[string]any{}, "webtransport", nil},
		{"webtransport instance, asked for webrtc", "webtransport", map[string]any{"transport": "webrtc"}, "webrtc", nil},
		{"webrtc instance, no request", "webrtc", map[string]any{}, "webrtc", nil},
		{"webrtc instance, asked for webtransport", "webrtc", map[string]any{"transport": "webtransport"}, "webrtc", "instance_default_disallows"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newWTFixture(t, "default")
			setDefault(t, f, tc.setting)
			status, body := f.launch(t, tc.request)
			if status != http.StatusCreated {
				t.Fatalf("launch: %d %v", status, body)
			}
			f.waitDispatched(t)
			sess := body["session"].(map[string]any)
			if sess["transport"] != tc.wantTransport || sess["transport_reason"] != tc.wantReason {
				t.Errorf("transport = %v (reason %v), want %s (%v)", sess["transport"], sess["transport_reason"], tc.wantTransport, tc.wantReason)
			}
		})
	}

	t.Run("a running session keeps its transport", func(t *testing.T) {
		f := newWTFixture(t, "default")
		setDefault(t, f, "webtransport")
		_, body := f.launch(t, map[string]any{})
		id := body["session"].(map[string]any)["id"].(string)
		setDefault(t, f, "webrtc")
		if got, err := f.store.Get(context.Background(), id); err != nil || got.Transport != "webtransport" {
			t.Errorf("running session transport = %q (%v)", got.Transport, err)
		}
		_, next := f.launch(t, map[string]any{})
		if next["session"].(map[string]any)["transport"] != "webrtc" {
			t.Errorf("next launch = %v, want webrtc", next["session"])
		}
	})
}

func TestLaunchFallsBackToWebRTCWithAReason(t *testing.T) {
	tests := []struct {
		name       string
		profile    string
		request    map[string]any
		prep       func(t *testing.T, f *wtFixture)
		wantReason any
	}{
		{"default profile, no request", "webrtc", map[string]any{}, nil, nil},
		{"request on a webrtc profile", "webrtc", map[string]any{"transport": "webtransport"}, nil, "profile_disallows"},
		{"opt-in without the request", "webtransport_opt_in", map[string]any{}, nil, nil},
		{"user asks for webrtc", "webtransport", map[string]any{"transport": "webrtc"}, nil, nil},
		{"host never reported an endpoint", "webtransport", map[string]any{}, func(t *testing.T, f *wtFixture) {
			must(t, exec(t, f.pool, `UPDATE hosts SET transports = NULL, wt_port = NULL, wt_addresses = NULL, wt_certificates = NULL`))
		}, "host_no_endpoint"},
		{"every certificate expired", "webtransport", map[string]any{}, func(t *testing.T, f *wtFixture) {
			must(t, exec(t, f.pool, `UPDATE hosts SET wt_certificates = jsonb_build_array(jsonb_build_object(
				'sha256', 'x', 'not_before', now() - interval '20 days', 'not_after', now() - interval '1 day'))`))
		}, "host_no_endpoint"},
		{"device lacks webtransport", "webtransport", map[string]any{}, func(t *testing.T, f *wtFixture) {
			must(t, exec(t, f.pool, `UPDATE user_devices SET capabilities = capabilities - 'webtransport'`))
		}, "device_no_webtransport"},
		{"device cannot decode h264 in webcodecs", "webtransport", map[string]any{}, func(t *testing.T, f *wtFixture) {
			must(t, exec(t, f.pool, `UPDATE user_devices SET capabilities = jsonb_set(capabilities, '{webcodecs,h264}', 'false')`))
		}, "device_no_webcodecs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newWTFixture(t, tc.profile)
			if tc.prep != nil {
				tc.prep(t, f)
			}
			status, body := f.launch(t, tc.request)
			if status != http.StatusCreated {
				t.Fatalf("launch: %d %v — a refused WebTransport must never refuse the launch", status, body)
			}
			f.waitDispatched(t)
			sess := body["session"].(map[string]any)
			if sess["transport"] != "webrtc" || sess["transport_reason"] != tc.wantReason {
				t.Errorf("transport = %v (reason %v), want webrtc (%v)", sess["transport"], sess["transport_reason"], tc.wantReason)
			}
			if _, has := body["transport_options"]; has {
				t.Error("a WebRTC session's 201 carries transport_options")
			}
			if body["signaling"] == nil {
				t.Error("signaling block missing")
			}
			assign := f.disp.lastAssignCmd()
			if assign.Transport != "" || assign.WTTokenHash != "" || assign.WTTokenExpiresAt != nil {
				t.Errorf("a WebRTC assign carries transport fields: %+v", assign)
			}
			if rows := tokenRows(t, f.pool, sess["id"].(string)); len(rows["transport"]) != 0 {
				t.Errorf("a WebRTC session minted a transport token: %v", rows)
			}
			got, _ := f.store.Get(context.Background(), sess["id"].(string))
			if got.Transport != "webrtc" {
				t.Errorf("persisted transport = %q", got.Transport)
			}
		})
	}
}

func TestLaunchRejectsAnUnknownTransport(t *testing.T) {
	f := newWTFixture(t, "webtransport")
	if status, _ := f.launch(t, map[string]any{"transport": "quic"}); status != http.StatusBadRequest {
		t.Errorf("transport=quic: %d, want 400", status)
	}
}

// Edge override: the URL is the admin's, the response carries no hashes, and the agent still
// gets only the transport token.
func TestLaunchThroughAnEdgeOverride(t *testing.T) {
	f := newWTFixture(t, "webtransport_opt_in")
	must(t, exec(t, f.pool, `UPDATE hosts SET wt_public_url = 'https://play.example.com/wt/a', wt_public_certificate = 'public'`))
	status, body := f.launch(t, map[string]any{"transport": "webtransport"})
	if status != http.StatusCreated {
		t.Fatalf("launch: %d %v", status, body)
	}
	wt := body["transport_options"].(map[string]any)["webtransport"].(map[string]any)
	if wt["url"] != "https://play.example.com/wt/a" {
		t.Errorf("url = %v", wt["url"])
	}
	if h, ok := wt["cert_hashes"].([]any); !ok || len(h) != 0 {
		t.Errorf("cert_hashes = %v, want [] (always serialized)", wt["cert_hashes"])
	}
}

func (f *wtFixture) transportToken(t *testing.T, sessionID, bearer string) (int, map[string]any) {
	t.Helper()
	resp := doJSON(t, "POST", f.srv.URL+"/v1/sessions/"+sessionID+"/transport-token", bearer, nil)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func errCodeOf(body map[string]any) string {
	if e, ok := body["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

func TestTransportTokenEndpoint(t *testing.T) {
	f := newWTFixture(t, "webtransport")
	_, launched := f.launch(t, map[string]any{})
	f.waitDispatched(t)
	id := launched["session"].(map[string]any)["id"].(string)

	// 201 only after the agent's ack, and the agent got the hash of the token the client gets.
	status, body := f.transportToken(t, id, f.userTok)
	if status != http.StatusCreated {
		t.Fatalf("mint: %d %v", status, body)
	}
	wt := body["transport_options"].(map[string]any)["webtransport"].(map[string]any)
	cmds := f.disp.transportTokenCmds()
	if len(cmds) != 1 || cmds[0].SessionID != id || cmds[0].WTTokenHash != hashToken(wt["token"].(string)) {
		t.Fatalf("session_transport_token = %+v for token %v", cmds, wt["token"])
	}
	if wt["url"] != "https://192.0.2.10:4433/wt" || len(wt["cert_hashes"].([]any)) != 1 {
		t.Errorf("options: %v", wt)
	}
	if rows := tokenRows(t, f.pool, id); len(rows["transport"]) != 2 || len(rows["signaling"]) != 1 {
		t.Errorf("rows after a reconnect mint: %v", rows)
	}
	// Concurrent mints are independent tokens.
	_, again := f.transportToken(t, id, f.userTok)
	if again["transport_options"].(map[string]any)["webtransport"].(map[string]any)["token"] == wt["token"] {
		t.Error("two mints returned the same token")
	}

	// The endpoint is resolved again at every mint: a changed override is picked up.
	must(t, exec(t, f.pool, `UPDATE hosts SET wt_public_url = 'https://play.example.com/wt/b'`))
	_, third := f.transportToken(t, id, f.userTok)
	if u := third["transport_options"].(map[string]any)["webtransport"].(map[string]any)["url"]; u != "https://play.example.com/wt/b" {
		t.Errorf("url after an override = %v", u)
	}

	// No ack, no token: the response is withheld and says retry.
	f.disp.setAck(true, context.DeadlineExceeded)
	status, body = f.transportToken(t, id, f.userTok)
	if status != http.StatusServiceUnavailable || errCodeOf(body) != "agent_not_connected" || body["transport_options"] != nil {
		t.Errorf("unacked mint: %d %v", status, body)
	}
	// ack{ok:false}: the agent cannot take it, so the client uses WebRTC.
	f.disp.setAck(false, nil)
	status, body = f.transportToken(t, id, f.userTok)
	if status != http.StatusConflict || errCodeOf(body) != "transport_unavailable" || body["transport_options"] != nil {
		t.Errorf("refused mint: %d %v", status, body)
	}
	f.disp.setAck(true, nil)

	// Ownership: another user's session is 404.
	if _, err := f.authSvc.Register(context.Background(), "wt2@test.local", "wtuser2", "quasar-fixture-pw-05"); err != nil {
		t.Fatal(err)
	}
	if status, _ := f.transportToken(t, id, loginTok(t, f.authSvc, "wt2@test.local", "quasar-fixture-pw-05")); status != http.StatusNotFound {
		t.Errorf("another user: %d, want 404", status)
	}
	if status, _ := f.transportToken(t, id, ""); status != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", status)
	}

	// The endpoint is no longer usable: transport_unavailable.
	must(t, exec(t, f.pool, `UPDATE hosts SET transports = NULL, wt_public_url = NULL`))
	if status, body = f.transportToken(t, id, f.userTok); status != http.StatusConflict || errCodeOf(body) != "transport_unavailable" {
		t.Errorf("unusable endpoint: %d %v", status, body)
	}

	// A terminal session is not reconnectable.
	must(t, exec(t, f.pool, `UPDATE sessions SET state = 'stopped' WHERE id = $1::uuid`, id))
	if status, body = f.transportToken(t, id, f.userTok); status != http.StatusConflict || errCodeOf(body) != "session_not_reconnectable" {
		t.Errorf("terminal: %d %v", status, body)
	}
}

func TestTransportTokenOnAWebRTCSession(t *testing.T) {
	f := newWTFixture(t, "webrtc")
	_, launched := f.launch(t, map[string]any{})
	f.waitDispatched(t)
	id := launched["session"].(map[string]any)["id"].(string)
	status, body := f.transportToken(t, id, f.userTok)
	if status != http.StatusConflict || errCodeOf(body) != "transport_unavailable" {
		t.Errorf("webrtc session: %d %v, want 409 transport_unavailable", status, body)
	}
	if n := len(f.disp.transportTokenCmds()); n != 0 {
		t.Errorf("%d session_transport_token sent for a WebRTC session", n)
	}
}

// A signaling token cannot be redeemed as the transport credential and vice versa; the agent only
// ever sees transport hashes (TestLaunchChoosesWebTransport) and /v1/signal only redeems
// signaling ones (TestSignalingRedeemRefusesATransportToken).
func TestReconnectSendsOnlyTransportHashes(t *testing.T) {
	f := newWTFixture(t, "webtransport")
	_, launched := f.launch(t, map[string]any{})
	f.waitDispatched(t)
	id := launched["session"].(map[string]any)["id"].(string)
	sigHash := hashToken(launched["signaling"].(map[string]any)["token"].(string))
	if status, _ := f.transportToken(t, id, f.userTok); status != http.StatusCreated {
		t.Fatalf("mint: %d", status)
	}
	for _, c := range f.disp.transportTokenCmds() {
		if c.WTTokenHash == sigHash {
			t.Error("a signaling token's hash was sent as a transport token")
		}
	}
	// A fresh signaling token minted after is still a signaling credential.
	sig, err := f.store.MintSignalingToken(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ConsumeSignalingToken(context.Background(), sig.Plaintext); err != nil {
		t.Errorf("signaling token after a transport mint: %v", err)
	}
}

func TestTransportFallbackEventRecordsTheReason(t *testing.T) {
	f := newWTFixture(t, "webtransport")
	_, launched := f.launch(t, map[string]any{})
	f.waitDispatched(t)
	id := launched["session"].(map[string]any)["id"].(string)

	post := func(reasons ...any) {
		t.Helper()
		var events []map[string]any
		for i, r := range reasons {
			events = append(events, map[string]any{
				"ts_unix_ms": nowMs() + int64(100*(i+1)), "type": "transport.fallback",
				"payload": map[string]any{"from": "webtransport", "to": "webrtc", "reason": r},
			})
		}
		resp := doJSON(t, "POST", f.srv.URL+"/v1/sessions/"+id+"/trace/events", f.userTok, map[string]any{"events": events})
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("post: %d", resp.StatusCode)
		}
	}
	read := func() *string {
		t.Helper()
		got, err := f.store.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return got.TransportFallbackReason
	}

	if read() != nil {
		t.Fatal("a fresh session has a fallback reason")
	}
	post("connect_timeout")
	if r := read(); r == nil || *r != "connect_timeout" {
		t.Fatalf("reason = %v, want connect_timeout", r)
	}
	// Latest wins; malformed reasons are dropped and never overwrite.
	post("decode_error", "Bad Reason!", 7, strings.Repeat("a", 65), "")
	if r := read(); r == nil || *r != "decode_error" {
		t.Errorf("reason = %v, want decode_error (malformed ones dropped)", r)
	}
	post("stream_lost")
	if r := read(); r == nil || *r != "stream_lost" {
		t.Errorf("reason = %v, want stream_lost (latest wins)", r)
	}
	// A terminal session keeps the reason it ended with.
	must(t, exec(t, f.pool, `UPDATE sessions SET state = 'stopped' WHERE id = $1::uuid`, id))
	post("internal")
	if r := read(); r == nil || *r != "stream_lost" {
		t.Errorf("terminal session reason = %v, want stream_lost kept", r)
	}
	// The session body serves it.
	resp := doJSON(t, "GET", f.srv.URL+"/v1/sessions/"+id, f.userTok, nil)
	defer resp.Body.Close()
	var body struct {
		Session map[string]any `json:"session"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Session["transport_fallback_reason"] != "stream_lost" || body.Session["transport"] != "webtransport" {
		t.Errorf("session body: %v", body.Session)
	}
}

// A session that is stopping or already over is not reconnectable, and the answer comes before
// any endpoint or agent check: nothing is sent to the agent and no token is minted.
func TestTransportTokenRefusedForStoppingAndTerminalSessions(t *testing.T) {
	for _, state := range []string{"stopping", "stopped", "failed"} {
		t.Run(state, func(t *testing.T) {
			f := newWTFixture(t, "webtransport")
			_, launched := f.launch(t, map[string]any{})
			f.waitDispatched(t)
			id := launched["session"].(map[string]any)["id"].(string)
			// An unusable endpoint would answer transport_unavailable if the guard came later.
			must(t, exec(t, f.pool, `UPDATE hosts SET transports = NULL`))
			must(t, exec(t, f.pool, `UPDATE sessions SET state = $2 WHERE id = $1::uuid`, id, state))

			status, body := f.transportToken(t, id, f.userTok)
			if status != http.StatusConflict || errCodeOf(body) != "session_not_reconnectable" {
				t.Errorf("%s: %d %v, want 409 session_not_reconnectable", state, status, body)
			}
			if n := len(f.disp.transportTokenCmds()); n != 0 {
				t.Errorf("%d session_transport_token sent for a %s session", n, state)
			}
			if rows := tokenRows(t, f.pool, id); len(rows["transport"]) != 1 {
				t.Errorf("a %s session minted a token: %v", state, rows)
			}
		})
	}
}

// An admin is not the owner: another user's session is a 404 and the agent hears nothing.
func TestTransportTokenAdminCannotMintForAnotherUser(t *testing.T) {
	f := newWTFixture(t, "webtransport")
	_, launched := f.launch(t, map[string]any{})
	f.waitDispatched(t)
	id := launched["session"].(map[string]any)["id"].(string)

	if _, err := f.authSvc.Register(context.Background(), "wtadmin@test.local", "wtadmin", "quasar-fixture-pw-06"); err != nil {
		t.Fatal(err)
	}
	must(t, exec(t, f.pool, `UPDATE users SET role = 'admin' WHERE email = 'wtadmin@test.local'`))
	adminTok := loginTok(t, f.authSvc, "wtadmin@test.local", "quasar-fixture-pw-06")

	status, body := f.transportToken(t, id, adminTok)
	if status != http.StatusNotFound || body["transport_options"] != nil {
		t.Errorf("admin on another user's session: %d %v, want 404", status, body)
	}
	if n := len(f.disp.transportTokenCmds()); n != 0 {
		t.Errorf("%d session_transport_token sent on an admin's request", n)
	}
	if rows := tokenRows(t, f.pool, id); len(rows["transport"]) != 1 {
		t.Errorf("an admin's request minted a token: %v", rows)
	}
}

func TestTransportTokenIsRateLimitedPerSession(t *testing.T) {
	f := newWTFixture(t, "webtransport")
	_, launched := f.launch(t, map[string]any{})
	f.waitDispatched(t)
	id := launched["session"].(map[string]any)["id"].(string)
	_, otherLaunch := f.launchSecond(t)

	for i := 0; i < statsRateBurst; i++ {
		if status, _ := f.transportToken(t, id, f.userTok); status != http.StatusCreated {
			t.Fatalf("request %d within the burst: %d, want 201", i+1, status)
		}
	}
	if status, body := f.transportToken(t, id, f.userTok); status != http.StatusTooManyRequests || errCodeOf(body) != "rate_limited" {
		t.Errorf("request past the burst: %d %v, want 429 rate_limited", status, body)
	}
	if n := len(f.disp.transportTokenCmds()); n != statsRateBurst {
		t.Errorf("the limited request reached the agent: %d commands, want %d", n, statsRateBurst)
	}
	// The bucket is the session's, not the spelling of its id in the URL.
	if status, _ := f.transportToken(t, strings.ToUpper(id), f.userTok); status != http.StatusTooManyRequests {
		t.Errorf("the same id uppercased: %d, want 429 (one bucket per session)", status)
	}
	// Per session: another session's bucket is untouched.
	if status, _ := f.transportToken(t, otherLaunch, f.userTok); status != http.StatusCreated {
		t.Errorf("a second session was limited by the first: %d", status)
	}
}

// Two mints at once are independent tokens, each acked with its own hash.
func TestTransportTokenConcurrentMintsAreIndependent(t *testing.T) {
	f := newWTFixture(t, "webtransport")
	_, launched := f.launch(t, map[string]any{})
	f.waitDispatched(t)
	id := launched["session"].(map[string]any)["id"].(string)

	var wg sync.WaitGroup
	start := make(chan struct{})
	tokens := make([]string, 2)
	statuses := make([]int, 2)
	for i := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			status, body := f.transportToken(t, id, f.userTok)
			statuses[i] = status
			if status == http.StatusCreated {
				tokens[i] = body["transport_options"].(map[string]any)["webtransport"].(map[string]any)["token"].(string)
			}
		}()
	}
	close(start)
	wg.Wait()

	if statuses[0] != http.StatusCreated || statuses[1] != http.StatusCreated {
		t.Fatalf("statuses %v, want both 201", statuses)
	}
	if tokens[0] == tokens[1] {
		t.Fatal("two concurrent mints returned the same token")
	}
	acked := map[string]bool{}
	for _, c := range f.disp.transportTokenCmds() {
		acked[c.WTTokenHash] = true
	}
	for _, tok := range tokens {
		if !acked[hashToken(tok)] {
			t.Errorf("token %.8s was handed out without its hash reaching the agent", tok)
		}
	}
	if rows := tokenRows(t, f.pool, id); len(rows["transport"]) != 3 {
		t.Errorf("transport rows = %v, want the launch's and both mints", rows["transport"])
	}
}
