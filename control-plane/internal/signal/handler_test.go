package signal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/internal/origins"
	"github.com/accreleus/quasar/control-plane/internal/session"
	"github.com/accreleus/quasar/control-plane/migrations"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testDB connects to TEST_DATABASE_URL, migrates, and truncates. Skips when the
// env var is absent (same pattern as the other packages).
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
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
		DELETE FROM sessions; DELETE FROM gpus; DELETE FROM hosts;
		DELETE FROM apps;     DELETE FROM auth_tokens; DELETE FROM users;
	`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// mintToken generates a random 32-byte hex token and its SHA-256 hash — mirrors
// session/token.go's newSignalingToken without touching unexported internals.
func mintToken(t *testing.T) (plain, hash string) {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	plain = hex.EncodeToString(b)
	h := sha256.Sum256([]byte(plain))
	hash = hex.EncodeToString(h[:])
	return
}

// seedAssignedSession inserts the minimum rows and returns an assigned session
// with a valid (unconsumed, non-expired) signaling token.
func seedAssignedSession(t *testing.T, pool *pgxpool.Pool) (sess session.Session, tokenPlain string) {
	t.Helper()
	ctx := context.Background()
	store := session.NewStore(pool)
	// Some callers seed multiple sessions under subtests that intentionally share
	// one database. Derive every fixture identity from the random token so those
	// calls exercise the signaling path rather than colliding on test-only UNIQUE
	// constraints.
	tokenPlain, tokenHash := mintToken(t)
	fixtureID := tokenPlain[:12]

	var userID string
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ($1, $2, 'x') RETURNING id::text`,
		"sig-"+fixtureID+"@t.local", "sig"+fixtureID).Scan(&userID))
	var appID string
	must(t, pool.QueryRow(ctx, `INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height, default_fps, default_bitrate_kbps)
		VALUES ($1, 256, 1, 1280, 720, 60, 6000) RETURNING id::text`,
		"sigtest-"+fixtureID).Scan(&appID))
	// Phase 2 (§6.4): an app with no entitlement is unlaunchable, and this fixture
	// INSERTs the app directly rather than through POST /v1/apps, so it must grant
	// the ('all') row the migration backfill / create path would have written.
	_, entErr := pool.Exec(ctx, `
		INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('all', NULL, $1::uuid, 'migration') ON CONFLICT DO NOTHING`, appID)
	must(t, entErr)
	var hostID string
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, capacity_detection)
		VALUES ($1, 'online', 'ok') RETURNING id::text`, "h-sig-"+fixtureID).Scan(&hostID))
	must(t, pool.QueryRow(ctx, `INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
		VALUES ($1, 0, 8192, 4) RETURNING id::text`, hostID).Scan(new(string)))

	sess, err := store.ScheduleAndCreate(ctx, session.CreateParams{
		UserID:          userID,
		AppID:           appID,
		Width:           1280,
		Height:          720,
		FPS:             60,
		BitrateKbps:     6000,
		H264Profile:     "constrained-baseline",
		NeedEncodeSlots: 1,
		TokenHash:       tokenHash,
		TokenExpires:    time.Now().Add(60 * time.Second),
	})
	if err != nil {
		t.Fatalf("schedule session: %v", err)
	}
	return sess, tokenPlain
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func newTestServer(t *testing.T, pool *pgxpool.Pool) *httptest.Server {
	return newTestServerWithOrigins(t, pool)
}

func newTestServerWithOrigins(t *testing.T, pool *pgxpool.Pool, allowedOrigins ...string) *httptest.Server {
	t.Helper()
	store := session.NewStore(pool)
	registry := agentws.NewRegistry(discardLog())
	relay := agentws.NewRelayBus(discardLog())
	// Tests construct the resolver explicitly, exactly as production does: there
	// is no implicit one, which is the point of the required parameter.
	h := NewHandler(store, registry, relay, discardLog(),
		origins.NewResolver(strings.Join(allowedOrigins, ","), len(allowedOrigins) > 0, nil, discardLog()))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func newTestServerWithLiveness(t *testing.T, pool *pgxpool.Pool, readTimeout, pingPeriod time.Duration) *httptest.Server {
	t.Helper()
	store := session.NewStore(pool)
	registry := agentws.NewRegistry(discardLog())
	relay := agentws.NewRelayBus(discardLog())
	h := NewHandler(store, registry, relay, discardLog(), origins.NewResolver("", false, nil, discardLog()))
	h.readTimeout = readTimeout
	h.pingPeriod = pingPeriod
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// newTestServerWithAuthSettled builds a server whose handler reports (via the
// returned channel) each time it has finished applying failure-limiter
// bookkeeping (Release + Failure/Forget) for one handshake. A client's Dial
// call returns as soon as the WS upgrade completes, but ConsumeSignalingToken
// — a DB round trip — and the limiter bookkeeping that follows it only run
// afterward; a test that immediately proceeds after a successful Dial can
// race that bookkeeping (#422). Draining this channel is the deterministic
// alternative to sleeping or racing.
func newTestServerWithAuthSettled(t *testing.T, pool *pgxpool.Pool) (*httptest.Server, chan string) {
	t.Helper()
	store := session.NewStore(pool)
	registry := agentws.NewRegistry(discardLog())
	relay := agentws.NewRelayBus(discardLog())
	h := NewHandler(store, registry, relay, discardLog(), origins.NewResolver("", false, nil, discardLog()))
	h.authSettled = make(chan string, 1)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, h.authSettled
}

func signalURL(srv *httptest.Server, token string) string {
	base := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/signal"
	if token != "" {
		return base + "?token=" + token
	}
	return base
}

// dialExpectClose dials the signal endpoint and reads until it gets a close
// frame, then returns the close code. Fails the test if no close is received
// within 3 seconds.
func dialExpectClose(t *testing.T, url string) int {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		// The server may have rejected the upgrade with a non-101 status (e.g.
		// 400/403/429) — not a WS close. Self-diagnose: log the actual status
		// and body so a future flake (#422) pins the cause without needing a
		// repro session (a "bad handshake" alone doesn't say which HTTP status
		// caused it).
		if resp != nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Logf("dial error (may be expected): %v; response status=%s body=%q", err, resp.Status, body)
		} else {
			t.Logf("dial error (may be expected): %v; no HTTP response available", err)
		}
		return 0
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				return ce.Code
			}
			t.Logf("non-close error: %v", err)
			return 0
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Token validation unit tests (pure — no DB)
// ─────────────────────────────────────────────────────────────────────────────

// TestMissingTokenHTTP400: no token → HTTP 400 before WS upgrade.
func TestMissingTokenHTTP400(t *testing.T) {
	pool := testDB(t)
	srv := newTestServer(t, pool)
	_, resp, err := websocket.DefaultDialer.Dial(signalURL(srv, ""), nil)
	if err == nil {
		t.Fatal("expected error for missing token")
	}
	if resp == nil {
		t.Skip("no HTTP response available")
	}
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

// TestInvalidTokenClose4401: an unrecognised token closes with 4401.
func TestInvalidTokenClose4401(t *testing.T) {
	pool := testDB(t)
	srv := newTestServer(t, pool)
	code := dialExpectClose(t, signalURL(srv, "notavalidtoken"))
	if code != wsCloseTokenInvalid {
		t.Fatalf("want close %d, got %d", wsCloseTokenInvalid, code)
	}
}

func TestInvalidSignalTokensAreRateLimitedPreUpgradeAndIgnoreXFF(t *testing.T) {
	pool := testDB(t)
	srv := newTestServer(t, pool)
	for i := 0; i < signalFailureLimit; i++ {
		h := make(http.Header)
		h.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i))
		conn, _, err := websocket.DefaultDialer.Dial(signalURL(srv, fmt.Sprintf("invalid-%d", i)), h)
		if err != nil {
			t.Fatalf("dial invalid token %d: %v", i, err)
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _, _ = conn.ReadMessage()
		conn.Close()
	}
	_, resp, err := websocket.DefaultDialer.Dial(signalURL(srv, "another-invalid"), nil)
	if err == nil {
		t.Fatal("blocked signaling handshake unexpectedly upgraded")
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%v, want 429", resp)
	}
}

func TestValidSignalTokenClearsPriorFailures(t *testing.T) {
	pool := testDB(t)
	srv, authSettled := newTestServerWithAuthSettled(t, pool)
	for i := 0; i < signalFailureLimit-1; i++ {
		if code := dialExpectClose(t, signalURL(srv, fmt.Sprintf("invalid-before-%d", i))); code != wsCloseTokenInvalid {
			t.Fatalf("invalid close=%d", code)
		}
		// dialExpectClose already waits for the server's close frame, which is
		// written after limiter bookkeeping — but also drain here so the
		// channel can never carry a stale entry into the next section.
		<-authSettled
	}
	_, token := seedAssignedSession(t, pool)
	conn, _, err := websocket.DefaultDialer.Dial(signalURL(srv, token), nil)
	if err != nil {
		t.Fatalf("valid token dial: %v", err)
	}
	conn.Close()
	// The client's Dial() returns as soon as the WS upgrade completes, but the
	// server only Forgets this IP's prior failures afterward, once
	// ConsumeSignalingToken's DB round trip finishes (#422: proceeding
	// immediately here raced that Forget against the invalid-after dials
	// below, which could accumulate enough Failure() calls to hit the limit
	// before the stale count was cleared — an intermittent 429/"bad
	// handshake" that only needed CPU/DB jitter to widen the window, so it
	// surfaced almost exclusively under full-suite load). Wait for the
	// deterministic signal instead of racing it.
	select {
	case settledIP := <-authSettled:
		if settledIP == "" {
			t.Fatalf("authSettled fired with empty clientIP")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for server to settle failure-limiter state after valid token")
	}
	// A cleared limiter admits a fresh full batch of failures.
	for i := 0; i < signalFailureLimit; i++ {
		if code := dialExpectClose(t, signalURL(srv, fmt.Sprintf("invalid-after-%d", i))); code != wsCloseTokenInvalid {
			t.Fatalf("invalid close after valid token=%d", code)
		}
		<-authSettled
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Token store integration tests (DB required)
// ─────────────────────────────────────────────────────────────────────────────

// TestConsumeSignalingTokenHappyPath: valid token → consumed, session returned.
func TestConsumeSignalingTokenHappyPath(t *testing.T) {
	pool := testDB(t)
	store := session.NewStore(pool)
	sess, plain := seedAssignedSession(t, pool)

	got, err := store.ConsumeSignalingToken(context.Background(), plain)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if got.ID != sess.ID {
		t.Fatalf("wrong session: got %s want %s", got.ID, sess.ID)
	}
}

// TestConsumeSignalingTokenSingleUse: second consume of the same token → error.
func TestConsumeSignalingTokenSingleUse(t *testing.T) {
	pool := testDB(t)
	store := session.NewStore(pool)
	_, plain := seedAssignedSession(t, pool)
	ctx := context.Background()

	if _, err := store.ConsumeSignalingToken(ctx, plain); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if _, err := store.ConsumeSignalingToken(ctx, plain); !errors.Is(err, session.ErrTokenInvalid) {
		t.Fatalf("second consume: want ErrTokenInvalid, got %v", err)
	}
}

// TestConsumeSignalingTokenExpired: an expired token is rejected.
func TestConsumeSignalingTokenExpired(t *testing.T) {
	pool := testDB(t)
	store := session.NewStore(pool)
	_, plain := seedAssignedSession(t, pool)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`UPDATE session_tokens SET expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}
	if _, err := store.ConsumeSignalingToken(ctx, plain); !errors.Is(err, session.ErrTokenInvalid) {
		t.Fatalf("expired token: want ErrTokenInvalid, got %v", err)
	}
}

func TestSignalingOriginPolicy(t *testing.T) {
	h := NewHandler(nil, nil, nil, slog.Default(),
		origins.NewResolver(" https://CONSOLE.example ,https://other.example ", true, nil, slog.Default()))
	for _, tc := range []struct {
		name, host, origin string
		want               bool
	}{
		{"non-browser", "quasar.local", "", true},
		{"same origin", "quasar.local", "https://quasar.local", true},
		{"configured", "quasar.local", "https://console.example", true},
		{"configured normalized", "quasar.local", "https://CONSOLE.example", true},
		{"foreign", "quasar.local", "https://evil.example", false},
		{"opaque", "quasar.local", "null", false},
		{"origin path", "quasar.local", "https://quasar.local/not-an-origin", false},
		{"origin query", "quasar.local", "https://quasar.local?not-an-origin", false},
		{"origin credentials", "quasar.local", "https://user@quasar.local", false},
		{"unsupported scheme", "quasar.local", "file://quasar.local", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/v1/signal", nil)
			r.Header.Set("Origin", tc.origin)
			if got := h.originAllowed(r); got != tc.want {
				t.Fatalf("originAllowed=%v want %v", got, tc.want)
			}
		})
	}
}

func TestSignalOriginEnforcedBeforeUpgrade(t *testing.T) {
	pool := testDB(t)
	_, token := seedAssignedSession(t, pool)
	srv := newTestServerWithOrigins(t, pool, "https://console.example")

	foreign := make(http.Header)
	foreign.Set("Origin", "https://evil.example")
	_, resp, err := websocket.DefaultDialer.Dial(signalURL(srv, token), foreign)
	if err == nil {
		t.Fatal("foreign origin upgraded")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin status=%v want 403", resp)
	}
	malformed := make(http.Header)
	malformed.Set("Origin", "https://console.example/not-an-origin")
	_, resp, err = websocket.DefaultDialer.Dial(signalURL(srv, token), malformed)
	if err == nil {
		t.Fatal("malformed origin upgraded")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("malformed origin status=%v want 403", resp)
	}

	// Missing Origin remains valid for browserless tooling. Both rejected
	// handshakes were before token consumption, so this same token still opens.
	conn, _, err := websocket.DefaultDialer.Dial(signalURL(srv, token), nil)
	if err != nil {
		t.Fatalf("missing Origin dial: %v", err)
	}
	conn.Close()
}

func TestSignalConfiguredOriginUpgrades(t *testing.T) {
	pool := testDB(t)
	_, token := seedAssignedSession(t, pool)
	srv := newTestServerWithOrigins(t, pool, "https://console.example")
	configured := make(http.Header)
	configured.Set("Origin", "https://console.example")
	conn, _, err := websocket.DefaultDialer.Dial(signalURL(srv, token), configured)
	if err != nil {
		t.Fatalf("configured origin dial: %v", err)
	}
	conn.Close()
}

func TestSignalRejectsBinaryFrame(t *testing.T) {
	pool := testDB(t)
	_, token := seedAssignedSession(t, pool)
	srv := newTestServer(t, pool)
	conn, _, err := websocket.DefaultDialer.Dial(signalURL(srv, token), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte(`{"type":"answer"}`)); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseUnsupportedData {
		t.Fatalf("close = %v, want code %d", err, websocket.CloseUnsupportedData)
	}
}

func TestSignalRejectsOversizedFrame(t *testing.T) {
	pool := testDB(t)
	_, token := seedAssignedSession(t, pool)
	srv := newTestServer(t, pool)
	conn, _, err := websocket.DefaultDialer.Dial(signalURL(srv, token), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, make([]byte, browserReadLimit+1)); err != nil {
		t.Fatalf("write oversized: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseMessageTooBig {
		t.Fatalf("close = %v, want code %d", err, websocket.CloseMessageTooBig)
	}
}

func TestSignalRejectsMalformedJSONFrame(t *testing.T) {
	pool := testDB(t)
	_, token := seedAssignedSession(t, pool)
	srv := newTestServer(t, pool)
	conn, _, err := websocket.DefaultDialer.Dial(signalURL(srv, token), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":`)); err != nil {
		t.Fatalf("write malformed JSON: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseUnsupportedData {
		t.Fatalf("close = %v, want code %d", err, websocket.CloseUnsupportedData)
	}
}

// TestBrowserFrameValidationCorpus is deterministic fuzz-style coverage for
// every browser frame boundary. It runs in normal `go test`, so its adversarial
// seeds remain exercised even where fuzzing is not part of CI.
func TestBrowserFrameValidationCorpus(t *testing.T) {
	tests := []struct {
		name        string
		messageType int
		frame       []byte
		wantCode    int
	}{
		{"valid object", websocket.TextMessage, []byte(`{"type":"answer","sdp":"v=0"}`), 0},
		{"valid scalar", websocket.TextMessage, []byte(`null`), 0},
		{"empty text", websocket.TextMessage, nil, websocket.CloseUnsupportedData},
		{"truncated JSON", websocket.TextMessage, []byte(`{"type":`), websocket.CloseUnsupportedData},
		{"invalid UTF-8", websocket.TextMessage, []byte{'"', 0xff, '"'}, websocket.CloseUnsupportedData},
		{"binary JSON-shaped", websocket.BinaryMessage, []byte(`{"type":"answer"}`), websocket.CloseUnsupportedData},
		{"oversize text", websocket.TextMessage, make([]byte, browserReadLimit+1), websocket.CloseMessageTooBig},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, err := validateBrowserFrame(tc.messageType, tc.frame)
			if tc.wantCode == 0 {
				if err != nil || code != 0 {
					t.Fatalf("validateBrowserFrame() = (%d, %v), want success", code, err)
				}
				return
			}
			if err == nil || code != tc.wantCode {
				t.Fatalf("validateBrowserFrame() = (%d, %v), want close %d", code, err, tc.wantCode)
			}
		})
	}
}

// FuzzBrowserFrameValidation protects the pure pre-relay boundary against
// future parser/limit regressions. F.Add seeds are also run by ordinary go test;
// do not run an unbounded fuzz campaign as part of this package's DB suite.
func FuzzBrowserFrameValidation(f *testing.F) {
	for _, seed := range [][]byte{
		nil,
		[]byte(`{"type":`),
		[]byte(`{"type":"answer","sdp":"v=0"}`),
		[]byte{'"', 0xff, '"'},
		make([]byte, browserReadLimit+1),
	} {
		f.Add(int(websocket.TextMessage), seed)
	}
	f.Add(int(websocket.BinaryMessage), []byte(`{"type":"answer"}`))

	f.Fuzz(func(t *testing.T, messageType int, frame []byte) {
		code, _, err := validateBrowserFrame(messageType, frame)
		if err == nil && code != 0 {
			t.Fatalf("successful validation returned close code %d", code)
		}
		if err != nil && code == 0 {
			t.Fatal("rejected validation returned no close code")
		}
		switch code {
		case 0, websocket.CloseUnsupportedData, websocket.CloseMessageTooBig:
		default:
			t.Fatalf("unexpected close code %d", code)
		}
	})
}

// TestSignalMalformedFramesConsumeOneToken proves actual WS upgrades reject
// malformed browser traffic and do not leave an accepted single-use token
// reusable. Token consumption occurs at authenticated upgrade by design; no
// malformed frame reaches an agent relay.
func TestSignalMalformedFramesConsumeOneToken(t *testing.T) {
	pool := testDB(t)
	store := session.NewStore(pool)
	srv := newTestServer(t, pool)
	cases := []struct {
		name        string
		messageType int
		frame       []byte
		wantCode    int
	}{
		{"binary", websocket.BinaryMessage, []byte(`{"type":"answer"}`), websocket.CloseUnsupportedData},
		{"truncated", websocket.TextMessage, []byte(`{"type":`), websocket.CloseUnsupportedData},
		{"oversize", websocket.TextMessage, make([]byte, browserReadLimit+1), websocket.CloseMessageTooBig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, token := seedAssignedSession(t, pool)
			conn, _, err := websocket.DefaultDialer.Dial(signalURL(srv, token), nil)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			if err := conn.WriteMessage(tc.messageType, tc.frame); err != nil {
				t.Fatalf("write: %v", err)
			}
			conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			_, _, err = conn.ReadMessage()
			var closeErr *websocket.CloseError
			if !errors.As(err, &closeErr) || closeErr.Code != tc.wantCode {
				t.Fatalf("close = %v, want code %d", err, tc.wantCode)
			}
			if _, err := store.ConsumeSignalingToken(context.Background(), token); !errors.Is(err, session.ErrTokenInvalid) {
				t.Fatalf("accepted token became reusable after malformed frame: %v", err)
			}
		})
	}
}

// TestQuietSignalingSocketStaysAlive proves an established session is not
// terminated merely because SDP/ICE exchange is complete and the signaling
// socket has no application messages. The client read loop processes the
// server's protocol Ping frames and Gorilla automatically replies with Pong.
func TestQuietSignalingSocketStaysAlive(t *testing.T) {
	pool := testDB(t)
	_, token := seedAssignedSession(t, pool)
	const readTimeout = 100 * time.Millisecond
	srv := newTestServerWithLiveness(t, pool, readTimeout, 20*time.Millisecond)
	conn, _, err := websocket.DefaultDialer.Dial(signalURL(srv, token), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readErr := make(chan error, 1)
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				readErr <- err
				return
			}
		}
	}()

	// Three original read-deadline windows would have deterministically closed
	// the pre-fix connection. Pong refreshes keep this one live.
	time.Sleep(3 * readTimeout)
	select {
	case err := <-readErr:
		t.Fatalf("quiet signaling socket closed before heartbeat proof: %v", err)
	default:
	}

	// A valid frame now reaches the relay. No test agent is registered, so the
	// expected 4500 close proves the connection survived until this deliberate
	// application-level action (rather than expiring at readTimeout).
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"answer","sdp":"v=0"}`)); err != nil {
		t.Fatalf("write after quiet period: %v", err)
	}
	select {
	case err := <-readErr:
		var closeErr *websocket.CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != wsCloseRelayUnavail {
			t.Fatalf("close = %v, want code %d", err, wsCloseRelayUnavail)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for deliberate relay-unavailable close")
	}
}

// TestUnresponsiveSignalingSocketTimesOut keeps processing Ping frames but
// deliberately suppresses Pong replies. This proves the refreshed deadline
// still reaps a dead/unresponsive peer instead of turning into an infinite WS.
func TestUnresponsiveSignalingSocketTimesOut(t *testing.T) {
	pool := testDB(t)
	_, token := seedAssignedSession(t, pool)
	const readTimeout = 100 * time.Millisecond
	srv := newTestServerWithLiveness(t, pool, readTimeout, 20*time.Millisecond)
	conn, _, err := websocket.DefaultDialer.Dial(signalURL(srv, token), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetPingHandler(func(string) error { return nil })

	readErr := make(chan error, 1)
	go func() {
		_, _, err := conn.ReadMessage()
		readErr <- err
	}()
	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("unresponsive signaling socket returned without an error")
		}
	case <-time.After(5 * readTimeout):
		t.Fatal("unresponsive signaling socket survived beyond its read deadline")
	}
}

// TestConsumeTerminalSessionClose4404: consuming a token for a terminal session
// via the WS endpoint closes with 4404.
func TestConsumeTerminalSessionClose4404(t *testing.T) {
	pool := testDB(t)
	store := session.NewStore(pool)
	sess, plain := seedAssignedSession(t, pool)
	ctx := context.Background()

	// Drive session to stopped through the full path.
	for _, st := range []session.State{
		session.StateStarting, session.StateRunning,
		session.StateStopping, session.StateStopped,
	} {
		if _, err := store.Transition(ctx, sess.ID, st, nil, nil); err != nil {
			t.Fatalf("transition → %s: %v", st, err)
		}
	}

	srv := newTestServer(t, pool)
	code := dialExpectClose(t, signalURL(srv, plain))
	if code != wsCloseNotFound {
		t.Fatalf("terminal session: want close %d, got %d", wsCloseNotFound, code)
	}
}
