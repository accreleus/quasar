package secrets

// Store + admin-surface integration tests. Require Postgres: run via
// `scripts/dev/dev.sh go-test-db` (which sets TEST_DATABASE_URL) against a FRESH
// database — setup truncates.
//
// NO TEST IN THIS FILE PRINTS A PLAINTEXT. Value comparisons go through
// sameSecret (constant-time, boolean result) and the response-body assertions
// check for ABSENCE, reporting only which field carried it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

// theSecret is the value under test. It is never printed: every assertion about
// it is a constant-time boolean or an absence check.
const theSecret = "sgdb-live-abcdefghijklmnop-9f2c"

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
	if _, err := pool.Exec(ctx, `TRUNCATE instance_secrets, auth_tokens, users CASCADE`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// testRegistry declares two names so the AAD/cross-row tests have somewhere to
// move a ciphertext TO.
func testRegistry() *Registry {
	return NewRegistry(
		Descriptor{Name: NameArtworkAPIKey, Label: "SteamGridDB API key", EnvVar: "QUASAR_STEAMGRIDDB_API_KEY"},
		Descriptor{Name: "test.other", Label: "Another secret"},
	)
}

func newStore(t *testing.T, pool *pgxpool.Pool, primary string) *Store {
	t.Helper()
	kr, err := ParseKeyring(primary, "")
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	return NewStore(pool, kr, testRegistry())
}

// --- round trip --------------------------------------------------------------

func TestSetGetDeleteRoundTrip(t *testing.T) {
	pool := testDB(t)
	s := newStore(t, pool, testKeyB64)
	ctx := context.Background()

	if err := s.Set(ctx, NameArtworkAPIKey, theSecret, ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(ctx, NameArtworkAPIKey)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !sameSecret(got, theSecret) {
		t.Fatal("Get did not return the value that was Set")
	}

	st, err := s.Status(ctx, NameArtworkAPIKey)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Configured || !st.Readable {
		t.Fatalf("Status: configured=%v readable=%v, want both true", st.Configured, st.Readable)
	}
	if st.Hint != Hint(theSecret) {
		t.Fatalf("Status.Hint = %q, want the masked tail", st.Hint)
	}
	if strings.Contains(theSecret, st.Hint) && len(st.Hint) > 4 {
		t.Fatalf("Status.Hint is longer than the 4-character mask: %d", len(st.Hint))
	}
	if st.KeyVersion != 1 {
		t.Fatalf("Status.KeyVersion = %d, want 1", st.KeyVersion)
	}

	if err := s.Delete(ctx, NameArtworkAPIKey); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, NameArtworkAPIKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete: want ErrNotFound, got %v", err)
	}
	// Deleting again is not an error: the caller asked for "nothing stored".
	if err := s.Delete(ctx, NameArtworkAPIKey); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	st, _ = s.Status(ctx, NameArtworkAPIKey)
	if st.Configured || st.Hint != "" {
		t.Fatal("Status after Delete must report nothing configured and no hint")
	}
}

// TestCiphertextOnDiskIsNotThePlaintext: what lands in Postgres must not be the
// value. Checked against the RAW bytes, so an accidental "store it plainly"
// regression cannot pass.
func TestCiphertextOnDiskIsNotThePlaintext(t *testing.T) {
	pool := testDB(t)
	s := newStore(t, pool, testKeyB64)
	ctx := context.Background()
	if err := s.Set(ctx, NameArtworkAPIKey, theSecret, ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var ct, nonce []byte
	if err := pool.QueryRow(ctx,
		`SELECT ciphertext, nonce FROM instance_secrets WHERE name = $1`, NameArtworkAPIKey).
		Scan(&ct, &nonce); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if bytes.Contains(ct, []byte(theSecret)) {
		t.Fatal("the stored ciphertext contains the plaintext")
	}
	if len(nonce) != 12 {
		t.Fatalf("stored nonce is %d bytes, want 12", len(nonce))
	}
}

// TestTwoWritesOfTheSameValueDifferOnDisk is the nonce-uniqueness property
// observed through the store, not just the crypto helpers.
func TestTwoWritesOfTheSameValueDifferOnDisk(t *testing.T) {
	pool := testDB(t)
	s := newStore(t, pool, testKeyB64)
	ctx := context.Background()

	read := func() (ct, nonce []byte) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT ciphertext, nonce FROM instance_secrets WHERE name = $1`, NameArtworkAPIKey).
			Scan(&ct, &nonce); err != nil {
			t.Fatalf("read row: %v", err)
		}
		return ct, nonce
	}
	if err := s.Set(ctx, NameArtworkAPIKey, theSecret, ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	ct1, n1 := read()
	if err := s.Set(ctx, NameArtworkAPIKey, theSecret, ""); err != nil {
		t.Fatalf("re-Set: %v", err)
	}
	ct2, n2 := read()

	if bytes.Equal(n1, n2) {
		t.Fatal("re-writing the same value reused the nonce")
	}
	if bytes.Equal(ct1, ct2) {
		t.Fatal("re-writing the same value produced identical ciphertext")
	}
}

// TestCiphertextMovedToAnotherNameFailsToDecrypt is the AAD binding, end to end
// through the database: an attacker with UPDATE on the table cannot swap a
// low-value credential onto a high-value name.
func TestCiphertextMovedToAnotherNameFailsToDecrypt(t *testing.T) {
	pool := testDB(t)
	s := newStore(t, pool, testKeyB64)
	ctx := context.Background()
	if err := s.Set(ctx, "test.other", theSecret, ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Copy the row verbatim onto the artwork name.
	if _, err := pool.Exec(ctx, `
		INSERT INTO instance_secrets (name, ciphertext, nonce, key_version, hint)
		SELECT $1, ciphertext, nonce, key_version, hint FROM instance_secrets WHERE name = 'test.other'
	`, NameArtworkAPIKey); err != nil {
		t.Fatalf("copy row: %v", err)
	}
	_, err := s.Get(ctx, NameArtworkAPIKey)
	if !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("a relocated ciphertext must not decrypt, got %v", err)
	}
	// AND it is the specific outcome, not the generic shrug. A relocated row is
	// well-formed — the AAD binding rejected it — so it must NOT be classified as
	// a malformed row, whose fix ("set the secret again") is different.
	if errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("a well-formed relocated row must not be reported as malformed, got %v", err)
	}
	if !strings.Contains(err.Error(), "copied from another secret") {
		t.Fatalf("the error must name the copied-row cause so an operator whose key is fine is not sent key-hunting, got %q", err.Error())
	}

	// Status, which is what an admin actually reads, must carry the same three
	// causes rather than asserting the master key.
	st, err := s.Status(ctx, NameArtworkAPIKey)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Readable {
		t.Fatal("a relocated row must not report as readable")
	}
	if !strings.Contains(st.Problem, "copied from another secret") {
		t.Fatalf("Status.Problem must offer the copied-row cause, got %q", st.Problem)
	}

	// The original still reads.
	if _, err := s.Get(ctx, "test.other"); err != nil {
		t.Fatalf("original row must still decrypt: %v", err)
	}
}

// TestAMalformedRowIsNotBlamedOnTheMasterKey: a truncated ciphertext in the
// database is NOT a key problem, and the operator-facing text must not send
// someone hunting for a QUASAR_SECRET_KEY that was never wrong. It still
// satisfies errors.Is(err, ErrKeyMismatch) so existing callers are unchanged.
func TestAMalformedRowIsNotBlamedOnTheMasterKey(t *testing.T) {
	pool := testDB(t)
	s := newStore(t, pool, testKeyB64)
	ctx := context.Background()
	if err := s.Set(ctx, NameArtworkAPIKey, theSecret, ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Truncate the stored ciphertext below the GCM tag length.
	if _, err := pool.Exec(ctx,
		`UPDATE instance_secrets SET ciphertext = substring(ciphertext from 1 for 4) WHERE name = $1`,
		NameArtworkAPIKey); err != nil {
		t.Fatalf("truncate ciphertext: %v", err)
	}

	_, err := s.Get(ctx, NameArtworkAPIKey)
	if !errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("want ErrCiphertextInvalid, got %v", err)
	}
	if !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("ErrCiphertextInvalid must still satisfy errors.Is(ErrKeyMismatch), got %v", err)
	}

	st, err := s.Status(ctx, NameArtworkAPIKey)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Configured || st.Readable {
		t.Fatalf("Status: configured=%v readable=%v, want true/false", st.Configured, st.Readable)
	}
	if !strings.Contains(st.Problem, "malformed") {
		t.Fatalf("Status.Problem must say the row is malformed, got %q", st.Problem)
	}
	if strings.Contains(st.Problem, "Restore the original QUASAR_SECRET_KEY") {
		t.Fatalf("a malformed row must not advise restoring a key, got %q", st.Problem)
	}
}

// --- key management ----------------------------------------------------------

// TestWrongMasterKeyProducesASpecificError is THE requirement: a changed master
// key must be distinguishable from "not configured", at the store level and in
// what the admin surface reports.
func TestWrongMasterKeyProducesASpecificError(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	if err := newStore(t, pool, testKeyB64).Set(ctx, NameArtworkAPIKey, theSecret, ""); err != nil {
		t.Fatalf("Set: %v", err)
	}

	wrong := newStore(t, pool, altKeyB64)
	_, err := wrong.Get(ctx, NameArtworkAPIKey)
	if !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("want ErrKeyMismatch, got %v", err)
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNoMasterKey) {
		t.Fatal("a wrong master key must not read as 'not found' or 'no key configured'")
	}

	st, err := wrong.Status(ctx, NameArtworkAPIKey)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Configured {
		t.Fatal("Status must still report the secret as configured — something IS stored")
	}
	if st.Readable {
		t.Fatal("Status must report the secret as unreadable under the wrong key")
	}
	if !strings.Contains(st.Problem, "master key") {
		t.Fatalf("Status.Problem must name the master key, got %q", st.Problem)
	}
	// The hint still works without a usable key — that is why it is stored.
	if st.Hint != Hint(theSecret) {
		t.Fatalf("Status.Hint should survive a key mismatch, got %q", st.Hint)
	}
	// And a wrong key does NOT silently fall through to the env fallback.
	if _, err := wrong.Resolve(ctx, NameArtworkAPIKey, "an-env-key"); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("Resolve must not fall back to the env under a key mismatch, got %v", err)
	}
}

// TestNoMasterKeyIsUsableNotFatal: with QUASAR_SECRET_KEY unset, nothing
// panics, writes are refused with a distinct error, and status reads still work.
func TestNoMasterKeyIsUsableNotFatal(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	// Something was stored earlier, by a control plane that had a key.
	if err := newStore(t, pool, testKeyB64).Set(ctx, NameArtworkAPIKey, theSecret, ""); err != nil {
		t.Fatalf("Set: %v", err)
	}

	none := NewStore(pool, nil, testRegistry())
	if none.Available() {
		t.Fatal("a store with no keyring must report unavailable")
	}
	if err := none.Set(ctx, "test.other", "anything", ""); !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("Set with no key: want ErrNoMasterKey, got %v", err)
	}
	if _, err := none.Get(ctx, NameArtworkAPIKey); !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("Get with no key: want ErrNoMasterKey, got %v", err)
	}
	st, err := none.Status(ctx, NameArtworkAPIKey)
	if err != nil {
		t.Fatalf("Status with no key must not error: %v", err)
	}
	if !st.Configured || st.Readable {
		t.Fatalf("Status: configured=%v readable=%v, want true/false", st.Configured, st.Readable)
	}
	if !strings.Contains(st.Problem, "QUASAR_SECRET_KEY") {
		t.Fatalf("Status.Problem must name the env var, got %q", st.Problem)
	}
	// Deleting still works with no key: clearing a value you cannot read is
	// exactly what an operator in this state needs to be able to do.
	if err := none.Delete(ctx, NameArtworkAPIKey); err != nil {
		t.Fatalf("Delete with no key: %v", err)
	}
}

// --- resolution precedence ----------------------------------------------------

func TestResolvePrefersTheDatabaseAndFallsBackToTheEnv(t *testing.T) {
	pool := testDB(t)
	s := newStore(t, pool, testKeyB64)
	ctx := context.Background()
	const envKey = "an-env-supplied-key"

	// 1. Nothing stored → the env fallback is in effect. THIS is the upgrade
	//    path: an operator who already set QUASAR_STEAMGRIDDB_API_KEY must not
	//    lose the feature the moment this facility ships.
	v, err := s.Resolve(ctx, NameArtworkAPIKey, envKey)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Origin != OriginEnvironment || !sameSecret(v.Secret, envKey) {
		t.Fatalf("origin = %q, want %q with the env value", v.Origin, OriginEnvironment)
	}

	// 2. Stored → the database wins. An admin typing a key into the UI must not
	//    be silently overridden by a stale env var.
	if err := s.Set(ctx, NameArtworkAPIKey, theSecret, ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err = s.Resolve(ctx, NameArtworkAPIKey, envKey)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Origin != OriginDatabase || !sameSecret(v.Secret, theSecret) {
		t.Fatalf("origin = %q, want %q with the stored value", v.Origin, OriginDatabase)
	}

	// 3. Cleared → back to the env, not off a cliff. An operator cannot lock
	//    themselves out by clearing in the UI.
	if err := s.Delete(ctx, NameArtworkAPIKey); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	v, err = s.Resolve(ctx, NameArtworkAPIKey, envKey)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Origin != OriginEnvironment || !sameSecret(v.Secret, envKey) {
		t.Fatalf("after clear: origin = %q, want %q", v.Origin, OriginEnvironment)
	}

	// 4. Nothing anywhere.
	v, err = s.Resolve(ctx, NameArtworkAPIKey, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Origin != OriginNone || v.Configured() {
		t.Fatalf("origin = %q, want %q", v.Origin, OriginNone)
	}
}

func TestUndeclaredNamesAreRejected(t *testing.T) {
	pool := testDB(t)
	s := newStore(t, pool, testKeyB64)
	ctx := context.Background()
	// Not an arbitrary key/value store: an undeclared name is refused rather
	// than silently creating a row nothing reads.
	if err := s.Set(ctx, "not.declared", "x", ""); !errors.Is(err, ErrUnknownSecret) {
		t.Fatalf("Set: want ErrUnknownSecret, got %v", err)
	}
	if _, err := s.Status(ctx, "not.declared"); !errors.Is(err, ErrUnknownSecret) {
		t.Fatalf("Status: want ErrUnknownSecret, got %v", err)
	}
	if err := s.Delete(ctx, "not.declared"); !errors.Is(err, ErrUnknownSecret) {
		t.Fatalf("Delete: want ErrUnknownSecret, got %v", err)
	}
	// "" is indistinguishable from unset at every later read, so it is refused.
	if err := s.Set(ctx, NameArtworkAPIKey, "", ""); !errors.Is(err, ErrEmptyValue) {
		t.Fatalf("empty Set: want ErrEmptyValue, got %v", err)
	}
}

// --- HTTP surface -------------------------------------------------------------

type httpHarness struct {
	srv        *httptest.Server
	store      *Store
	adminToken string
	userToken  string
}

func newHTTPHarness(t *testing.T, pool *pgxpool.Pool, primary string) *httpHarness {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := NewStore(pool, mustKeyring(t, primary), testRegistry())

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	// The gate under test is the REAL RequireAuth → RequireAdmin chain, composed
	// exactly as cmd/quasar-control composes it.
	admin := func(next http.Handler) http.Handler {
		return authHandler.RequireAuth(authHandler.RequireAdmin(next))
	}
	NewHandler(store, log).Register(mux, admin)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx := context.Background()
	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = $1`, "admin@test.local"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	adminTok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	if _, err := authSvc.Register(ctx, "user@test.local", "user", "quasar-fixture-pw-05"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	userTok, err := authSvc.Login(ctx, "user@test.local", "quasar-fixture-pw-05", "")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}
	return &httpHarness{srv: srv, store: store, adminToken: adminTok.Plaintext, userToken: userTok.Plaintext}
}

func mustKeyring(t *testing.T, primary string) *Keyring {
	t.Helper()
	kr, err := ParseKeyring(primary, "")
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	return kr
}

// do issues a request and returns the status plus the RAW body, so tests can
// assert on the exact bytes that went over the wire.
func do(t *testing.T, method, url, token string, body []byte) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// TestEveryRouteIsAdminGated. Server-enforced, per CLAUDE.md invariant #6: a
// VALID non-admin token is 403 on every route, and a missing token is 401.
// Hiding the panel in the UI is never the access control.
func TestEveryRouteIsAdminGated(t *testing.T) {
	pool := testDB(t)
	h := newHTTPHarness(t, pool, testKeyB64)

	routes := []struct {
		method, path string
		body         []byte
	}{
		{"GET", "/v1/admin/secrets", nil},
		{"PUT", "/v1/admin/secrets/" + NameArtworkAPIKey, []byte(`{"value":"x"}`)},
		{"DELETE", "/v1/admin/secrets/" + NameArtworkAPIKey, nil},
	}
	for _, r := range routes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			if code, _ := do(t, r.method, h.srv.URL+r.path, h.userToken, r.body); code != http.StatusForbidden {
				t.Errorf("non-admin token: status %d, want 403", code)
			}
			if code, _ := do(t, r.method, h.srv.URL+r.path, "", r.body); code != http.StatusUnauthorized {
				t.Errorf("no token: status %d, want 401", code)
			}
		})
	}
	// And nothing a non-admin sent got stored.
	st, err := h.store.Status(context.Background(), NameArtworkAPIKey)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Configured {
		t.Fatal("a 403'd PUT must not have written anything")
	}
}

// TestTheWireNeverCarriesThePlaintext asserts on the ACTUAL response bodies of
// every route, for both the write and the read.
func TestTheWireNeverCarriesThePlaintext(t *testing.T) {
	pool := testDB(t)
	h := newHTTPHarness(t, pool, testKeyB64)
	url := h.srv.URL + "/v1/admin/secrets/" + NameArtworkAPIKey

	body, err := json.Marshal(map[string]string{"value": theSecret})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	code, raw := do(t, "PUT", url, h.adminToken, body)
	if code != http.StatusOK {
		t.Fatalf("PUT: status %d, body %s", code, raw)
	}
	assertNoPlaintext(t, "PUT response", raw)

	code, raw = do(t, "GET", h.srv.URL+"/v1/admin/secrets", h.adminToken, nil)
	if code != http.StatusOK {
		t.Fatalf("GET: status %d", code)
	}
	assertNoPlaintext(t, "GET response", raw)

	// The GET is still USEFUL: it says one is configured, masked.
	var env struct {
		Secrets []struct {
			Name       string `json:"name"`
			Configured bool   `json:"configured"`
			Readable   bool   `json:"readable"`
			Hint       string `json:"hint"`
			Origin     string `json:"origin"`
		} `json:"secrets"`
		MasterKeyConfigured bool `json:"master_key_configured"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !env.MasterKeyConfigured {
		t.Fatal("master_key_configured should be true")
	}
	var found bool
	for _, s := range env.Secrets {
		if s.Name != NameArtworkAPIKey {
			continue
		}
		found = true
		if !s.Configured || !s.Readable {
			t.Errorf("configured=%v readable=%v, want both true", s.Configured, s.Readable)
		}
		if s.Hint != Hint(theSecret) {
			t.Errorf("hint is not the 4-character mask")
		}
		if s.Origin != OriginDatabase {
			t.Errorf("origin = %q, want %q", s.Origin, OriginDatabase)
		}
	}
	if !found {
		t.Fatal("the declared secret is missing from the list")
	}

	// DELETE returns 204 with no body at all.
	code, raw = do(t, "DELETE", url, h.adminToken, nil)
	if code != http.StatusNoContent {
		t.Fatalf("DELETE: status %d", code)
	}
	assertNoPlaintext(t, "DELETE response", raw)
}

// assertNoPlaintext fails WITHOUT printing the value or the body — a test
// failure message is a log line like any other.
func assertNoPlaintext(t *testing.T, what string, raw []byte) {
	t.Helper()
	if bytes.Contains(raw, []byte(theSecret)) {
		t.Fatalf("%s contains the secret value (body withheld)", what)
	}
	// Also catch a JSON-escaped copy.
	if enc, err := json.Marshal(theSecret); err == nil {
		if bytes.Contains(raw, bytes.Trim(enc, `"`)) {
			t.Fatalf("%s contains the JSON-escaped secret value (body withheld)", what)
		}
	}
}

// TestHTTPDistinguishesNoKeyFromWrongKey: the two key failures must produce
// different, actionable answers on the wire, not one shrug.
func TestHTTPDistinguishesNoKeyFromWrongKey(t *testing.T) {
	pool := testDB(t)

	// (a) No master key at all: storing is refused with SETUP guidance, and the
	//     envelope says so, but the endpoint still answers.
	noKey := newHTTPHarness(t, pool, "")
	code, raw := do(t, "PUT", noKey.srv.URL+"/v1/admin/secrets/"+NameArtworkAPIKey,
		noKey.adminToken, []byte(`{"value":"`+theSecret+`"}`))
	if code != http.StatusConflict {
		t.Fatalf("PUT with no master key: status %d, want 409", code)
	}
	if !strings.Contains(string(raw), "QUASAR_SECRET_KEY") {
		t.Fatalf("the 409 must name the env var to set, got %s", raw)
	}
	code, raw = do(t, "GET", noKey.srv.URL+"/v1/admin/secrets", noKey.adminToken, nil)
	if code != http.StatusOK {
		t.Fatalf("GET with no master key: status %d, want 200 (the surface must still work)", code)
	}
	if !strings.Contains(string(raw), `"master_key_configured":false`) {
		t.Fatalf("GET must report master_key_configured=false, got %s", raw)
	}

	// (b) A key IS configured and something is stored — then the key changes.
	if err := newStore(t, pool, testKeyB64).Set(context.Background(), NameArtworkAPIKey, theSecret, ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	wrong := NewStore(pool, mustKeyring(t, altKeyB64), testRegistry())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	NewHandler(wrong, log).Register(mux, func(next http.Handler) http.Handler { return next })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	code, raw = do(t, "GET", srv.URL+"/v1/admin/secrets", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET under a wrong key: status %d", code)
	}
	s := string(raw)
	if !strings.Contains(s, `"configured":true`) || !strings.Contains(s, `"readable":false`) {
		t.Fatalf("want configured=true readable=false, got %s", s)
	}
	if !strings.Contains(s, "master key does not match") {
		t.Fatalf("the problem text must name the master-key mismatch specifically, got %s", s)
	}
	if strings.Contains(s, "no master key is configured") {
		t.Fatal("a WRONG key must not be reported as NO key")
	}
	assertNoPlaintext(t, "wrong-key GET", raw)
}

func TestUndeclaredNameIs404OverHTTP(t *testing.T) {
	pool := testDB(t)
	h := newHTTPHarness(t, pool, testKeyB64)
	code, _ := do(t, "PUT", h.srv.URL+"/v1/admin/secrets/nope.not.declared",
		h.adminToken, []byte(`{"value":"x"}`))
	if code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", code)
	}
	code, _ = do(t, "PUT", h.srv.URL+"/v1/admin/secrets/"+NameArtworkAPIKey,
		h.adminToken, []byte(`{"value":""}`))
	if code != http.StatusBadRequest {
		t.Fatalf("empty value: status %d, want 400", code)
	}
}
