package devauth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"
)

const testSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fakeMinter records what the handler asked for, so validation and defaulting can
// be asserted without a database.
type fakeMinter struct {
	calls   int
	gotRole string
	gotTTL  time.Duration
	token   auth.Token
	mintErr error
	// reapCalls is touched from the reaper goroutine, so it is atomic.
	reapCalls  atomic.Int64
	reapReport auth.ReapReport
}

func (f *fakeMinter) MintEphemeral(_ context.Context, role string, ttl time.Duration) (auth.Token, error) {
	f.calls++
	f.gotRole, f.gotTTL = role, ttl
	if f.mintErr != nil {
		return auth.Token{}, f.mintErr
	}
	return f.token, nil
}

func (f *fakeMinter) ReapEphemeral(context.Context) (auth.ReapReport, error) {
	f.reapCalls.Add(1)
	return f.reapReport, nil
}

func okMinter() *fakeMinter {
	return &fakeMinter{token: auth.Token{
		Plaintext: "tok-abc",
		ExpiresAt: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
		User: auth.User{
			ID: "11111111-2222-3333-4444-555555555555", Email: "agent-x@dev.invalid",
			Username: "agent-x", Role: "user",
			CreatedAt: time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC),
		},
	}}
}

// recordingRouter captures registered patterns (same shape as the drift test's).
type recordingRouter struct{ patterns []string }

func (r *recordingRouter) Handle(p string, _ http.Handler) { r.patterns = append(r.patterns, p) }
func (r *recordingRouter) HandleFunc(p string, _ func(http.ResponseWriter, *http.Request)) {
	r.patterns = append(r.patterns, p)
}

// --- gate 1: registration ----------------------------------------------------

func TestRegisterWithFlagOffRegistersNothing(t *testing.T) {
	rec := &recordingRouter{}
	Register(rec, Config{Enabled: false}, NewService(okMinter(), testSecret, testLogger()), testLogger())
	if len(rec.patterns) != 0 {
		t.Fatalf("flag off must register no routes; got %v", rec.patterns)
	}
}

func TestRegisterWithFlagOnRegistersTheRoute(t *testing.T) {
	rec := &recordingRouter{}
	Register(rec, Config{Enabled: true}, NewService(okMinter(), testSecret, testLogger()), testLogger())
	if len(rec.patterns) != 1 || rec.patterns[0] != Route {
		t.Fatalf("flag on must register exactly %q; got %v", Route, rec.patterns)
	}
}

// A real mux, not a recorder: with the flag off the path must 404 from routing
// itself — not 401, not 403. That difference is the whole gating design.
func TestMuxWithFlagOff404s(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, Config{Enabled: false}, NewService(okMinter(), testSecret, testLogger()), testLogger())

	req := httptest.NewRequest(http.MethodPost, "/v1/dev/agent-session", nil)
	req.Header.Set(KeyHeader, testSecret)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("flag off: want 404, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestMuxWithFlagOnServesTheRoute(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, Config{Enabled: true}, NewService(okMinter(), testSecret, testLogger()), testLogger())

	req := httptest.NewRequest(http.MethodPost, "/v1/dev/agent-session", nil)
	req.Header.Set(KeyHeader, testSecret)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("flag on: want 200, got %d (%s)", w.Code, w.Body.String())
	}
}

// --- gate 2: production refusal ----------------------------------------------

func TestValidateRefusesFlagInProduction(t *testing.T) {
	err := Config{Enabled: true, Env: "production"}.Validate()
	if err == nil {
		t.Fatal("QUASAR_DEV_AGENT_AUTH=1 + QUASAR_ENV=production must be a boot refusal")
	}
	if !strings.Contains(err.Error(), EnvFlag) || !strings.Contains(err.Error(), EnvName) {
		t.Fatalf("refusal must name both knobs; got %q", err)
	}
	// Case-insensitive: "Production" is the same mistake.
	if (Config{Enabled: true, Env: "Production"}).Validate() == nil {
		t.Fatal("QUASAR_ENV=Production must refuse too")
	}
}

func TestValidateAllowsEveryOtherCombination(t *testing.T) {
	for _, c := range []Config{
		{Enabled: false, Env: "production"},
		{Enabled: true, Env: "dev"},
		{Enabled: true, Env: ""},
		{Enabled: false, Env: ""},
	} {
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate(%+v) = %v, want nil", c, err)
		}
	}
}

func TestLoadConfigReadsTheEnvironment(t *testing.T) {
	t.Setenv(EnvFlag, "1")
	t.Setenv(EnvName, "dev")
	cfg := LoadConfig()
	if !cfg.Enabled || cfg.Env != "dev" || cfg.KeyPath != DefaultKeyPath {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	// Only the exact string "1" enables — "true"/"yes" must not.
	t.Setenv(EnvFlag, "true")
	if LoadConfig().Enabled {
		t.Fatal(`QUASAR_DEV_AGENT_AUTH="true" must NOT enable the feature (only "1")`)
	}
	t.Setenv(EnvKeyPath, "/tmp/quasar-dev-key")
	if got := LoadConfig().KeyPath; got != "/tmp/quasar-dev-key" {
		t.Fatalf("key path override ignored: %q", got)
	}
}

// --- gate 3/5: the key -------------------------------------------------------

func post(t *testing.T, svc *Service, key string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/dev/agent-session", r)
	if key != "" {
		req.Header.Set(KeyHeader, key)
	}
	w := httptest.NewRecorder()
	svc.Handle(w, req)
	return w
}

func TestWrongAndMissingKeyAreIdentical401s(t *testing.T) {
	m := okMinter()
	svc := NewService(m, testSecret, testLogger())

	missing := post(t, svc, "", "")
	wrong := post(t, svc, "not-the-key", "")
	wrongSameLen := post(t, svc, strings.Repeat("a", len(testSecret)), "")

	for name, w := range map[string]*httptest.ResponseRecorder{
		"missing": missing, "wrong": wrong, "wrong-same-length": wrongSameLen,
	} {
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s key: want 401, got %d", name, w.Code)
		}
	}
	if missing.Body.String() != wrong.Body.String() || wrong.Body.String() != wrongSameLen.Body.String() {
		t.Fatalf("401 bodies must be byte-identical:\n missing=%s wrong=%s samelen=%s",
			missing.Body, wrong.Body, wrongSameLen.Body)
	}
	if m.calls != 0 {
		t.Fatalf("a rejected key must never reach the minter; calls=%d", m.calls)
	}

	var env struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(wrong.Body.Bytes(), &env); err != nil {
		t.Fatalf("401 body is not the standard error envelope: %v (%s)", err, wrong.Body)
	}
	if env.Error.Code != "unauthorized" {
		t.Fatalf("401 code = %q, want unauthorized", env.Error.Code)
	}
}

func TestEmptyConfiguredSecretRejectsEverything(t *testing.T) {
	svc := NewService(okMinter(), "", testLogger())
	if w := post(t, svc, "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("empty secret + empty header must still 401; got %d", w.Code)
	}
}

// --- request validation ------------------------------------------------------

func TestAbsentBodyUsesDefaults(t *testing.T) {
	m := okMinter()
	w := post(t, NewService(m, testSecret, testLogger()), testSecret, "")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body)
	}
	if m.gotRole != auth.RoleUser || m.gotTTL != DefaultTTL {
		t.Fatalf("defaults: got role=%q ttl=%v, want user/%v", m.gotRole, m.gotTTL, DefaultTTL)
	}
}

func TestExplicitRoleAndTTLArePassedThrough(t *testing.T) {
	m := okMinter()
	w := post(t, NewService(m, testSecret, testLogger()), testSecret, `{"role":"admin","ttl_seconds":600}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body)
	}
	if m.gotRole != auth.RoleAdmin || m.gotTTL != 10*time.Minute {
		t.Fatalf("got role=%q ttl=%v, want admin/10m", m.gotRole, m.gotTTL)
	}
}

func TestTTLAndRoleValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		// The cap is a REJECTION, not a clamp (see resolveTTL's comment).
		{"one second over the 8h cap", `{"ttl_seconds":28801}`},
		{"one second under the 60s floor", `{"ttl_seconds":59}`},
		{"zero", `{"ttl_seconds":0}`},
		{"negative", `{"ttl_seconds":-1}`},
		{"unknown role", `{"role":"superuser"}`},
		{"empty-ish role typo", `{"role":"Admin"}`},
		{"malformed json", `{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := okMinter()
			w := post(t, NewService(m, testSecret, testLogger()), testSecret, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d (%s)", w.Code, w.Body)
			}
			var env struct {
				Error struct{ Code string } `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("not an error envelope: %v (%s)", err, w.Body)
			}
			if env.Error.Code != "validation_failed" {
				t.Fatalf("code = %q, want validation_failed", env.Error.Code)
			}
			if m.calls != 0 {
				t.Fatal("an invalid request must never reach the minter")
			}
		})
	}
}

func TestTTLBoundariesAreAccepted(t *testing.T) {
	for _, secs := range []int{60, 28800} {
		m := okMinter()
		w := post(t, NewService(m, testSecret, testLogger()), testSecret,
			`{"ttl_seconds":`+strconv.Itoa(secs)+`}`)
		if w.Code != http.StatusOK {
			t.Fatalf("ttl_seconds=%d: want 200, got %d (%s)", secs, w.Code, w.Body)
		}
		if m.gotTTL != time.Duration(secs)*time.Second {
			t.Fatalf("ttl_seconds=%d: minter got %v", secs, m.gotTTL)
		}
	}
}

// --- response shape ----------------------------------------------------------

func TestResponseCarriesLoginShapePlusStorageKeys(t *testing.T) {
	m := okMinter()
	w := post(t, NewService(m, testSecret, testLogger()), testSecret, "")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body)
	}

	var resp struct {
		AccessToken string            `json:"access_token"`
		TokenType   string            `json:"token_type"`
		ExpiresAt   string            `json:"expires_at"`
		User        auth.User         `json:"user"`
		StorageKeys map[string]string `json:"storage_keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, w.Body)
	}
	if resp.AccessToken != "tok-abc" || resp.TokenType != "Bearer" {
		t.Fatalf("token fields wrong: %+v", resp)
	}
	if resp.ExpiresAt != "2026-08-07T12:00:00Z" {
		t.Fatalf("expires_at = %q, want RFC3339 UTC", resp.ExpiresAt)
	}
	if resp.User.ID != m.token.User.ID || resp.User.Role != "user" {
		t.Fatalf("user echoed wrong: %+v", resp.User)
	}

	// The three keys web/src/auth/storage.ts reads, ready to inject.
	if got := resp.StorageKeys["quasar.auth.token"]; got != "tok-abc" {
		t.Fatalf("storage token = %q", got)
	}
	if got := resp.StorageKeys["quasar.auth.expires_at"]; got != resp.ExpiresAt {
		t.Fatalf("storage expires_at = %q, want %q", got, resp.ExpiresAt)
	}
	var storedUser auth.User
	if err := json.Unmarshal([]byte(resp.StorageKeys["quasar.auth.user"]), &storedUser); err != nil {
		t.Fatalf("storage user is not JSON: %v (%q)", err, resp.StorageKeys["quasar.auth.user"])
	}
	if storedUser.ID != resp.User.ID || storedUser.Email != resp.User.Email ||
		storedUser.Username != resp.User.Username || storedUser.Role != resp.User.Role {
		t.Fatalf("storage user != response user: %+v vs %+v", storedUser, resp.User)
	}
	if len(resp.StorageKeys) != 3 {
		t.Fatalf("storage_keys must be exactly the three SPA keys; got %v", resp.StorageKeys)
	}
}

// --- secret + key file -------------------------------------------------------

func TestMintSecretIsLongAndUnique(t *testing.T) {
	a, err := MintSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := MintSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 2*secretBytes {
		t.Fatalf("secret is %d hex chars, want %d (≥32 bytes)", len(a), 2*secretBytes)
	}
	if a == b {
		t.Fatal("two mints returned the same secret")
	}
}

func TestWriteKeyFileCreatesA0600File(t *testing.T) {
	path := t.TempDir() + "/nested/dir/dev-agent-key"
	if !WriteKeyFile(path, testSecret, testLogger()) {
		t.Fatal("WriteKeyFile reported failure")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != testSecret {
		t.Fatalf("file contents = %q", data)
	}
}

// A key-file failure is logged, not fatal: the control plane keeps serving with a
// log-only key.
func TestWriteKeyFileFailureIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	blocker := dir + "/blocker"
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if WriteKeyFile(blocker+"/key", testSecret, testLogger()) {
		t.Fatal("writing under a regular file must report failure")
	}
	// The service is unaffected and still serves.
	if w := post(t, NewService(okMinter(), testSecret, testLogger()), testSecret, ""); w.Code != http.StatusOK {
		t.Fatalf("service must keep serving after a key-file failure; got %d", w.Code)
	}
}

// --- reaper pass ---------------------------------------------------------
//
// The 10ms-ticker lifecycle test this used to be (StartReaper) no longer
// applies: the ticker is gone, and jobs.go/devauth.reaper (internal/jobs) is
// now the trigger, calling ReapOnce once per claimed run. What remains
// testable at this layer is the pass itself.

func TestReapOnceCallsTheMinterOnce(t *testing.T) {
	m := okMinter()
	if _, err := ReapOnce(context.Background(), m, testLogger()); err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}
	if got := m.reapCalls.Load(); got != 1 {
		t.Fatalf("reapCalls = %d, want 1", got)
	}
}

func TestReapOnceReturnsTheReport(t *testing.T) {
	m := okMinter()
	m.reapReport = auth.ReapReport{Deleted: 2, InSession: 1, Failed: 0}
	rep, err := ReapOnce(context.Background(), m, testLogger())
	if err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}
	if rep != m.reapReport {
		t.Fatalf("report = %+v, want %+v", rep, m.reapReport)
	}
}
