package artwork

// Consumer #1 of the encrypted-secrets facility: where the SteamGridDB key
// comes from, and the property that motivated the whole rework — a key set from
// the admin UI takes effect with NO control-plane restart.
//
// Requires Postgres (`scripts/dev/dev.sh go-test-db`). No test here makes a live
// third-party call: the provider is only ever constructed, never used to fetch.
//
// NO TEST HERE PRINTS A CREDENTIAL.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/secrets"
)

// A fixed test-only 32-byte master key. It protects nothing.
const testMasterKeyB64 = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

func secretStore(t *testing.T, pool *pgxpool.Pool) *secrets.Store {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DELETE FROM instance_secrets`); err != nil {
		t.Fatalf("clear instance_secrets: %v", err)
	}
	kr, err := secrets.ParseKeyring(testMasterKeyB64, "")
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	return secrets.NewStore(pool, kr, secrets.DefaultRegistry())
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestProviderPicksUpAUISetKeyWithoutARestart is THE acceptance property of the
// rework. One long-lived source, never rebuilt, observed across a UI-equivalent
// write and a clear.
func TestProviderPicksUpAUISetKeyWithoutARestart(t *testing.T) {
	pool := testDB(t)
	store := secretStore(t, pool)
	ctx := context.Background()

	// No env fallback: the deployment starts genuinely unconfigured.
	src := NewSecretProviderSource(store, "", false, discardLog())

	p, info := src.Provider(ctx)
	if p != nil || info.Configured {
		t.Fatal("an unconfigured deployment must resolve to no provider")
	}
	if info.Origin != OriginNone {
		t.Fatalf("origin = %q, want %q", info.Origin, OriginNone)
	}

	// An admin sets the key through /v1/admin/secrets. NOTHING is reconstructed.
	if err := store.Set(ctx, secrets.NameArtworkAPIKey, "a-provider-credential-9f2c", ""); err != nil {
		t.Fatalf("Set: %v", err)
	}

	p, info = src.Provider(ctx)
	if p == nil || !info.Configured {
		t.Fatal("the provider must become available with no restart")
	}
	if info.Name != "steamgriddb" {
		t.Fatalf("provider name = %q", info.Name)
	}
	if info.Origin != OriginDatabase {
		t.Fatalf("origin = %q, want %q", info.Origin, OriginDatabase)
	}

	// And clearing it turns the provider back off, again with no restart.
	if err := store.Delete(ctx, secrets.NameArtworkAPIKey); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if p, info = src.Provider(ctx); p != nil || info.Configured {
		t.Fatal("clearing the key must turn the provider off")
	}
}

// TestEnvFallbackKeepsWorkingForAnExistingDeployment: the upgrade path. An
// operator who already set QUASAR_STEAMGRIDDB_API_KEY must not lose artwork,
// and must be able to SEE that the env var is what is in effect.
func TestEnvFallbackKeepsWorkingForAnExistingDeployment(t *testing.T) {
	pool := testDB(t)
	store := secretStore(t, pool)
	ctx := context.Background()
	const envKey = "the-legacy-env-credential"

	src := NewSecretProviderSource(store, envKey, false, discardLog())

	p, info := src.Provider(ctx)
	if p == nil || !info.Configured {
		t.Fatal("an env-var deployment must keep working after the upgrade")
	}
	if info.Origin != OriginEnvironment {
		t.Fatalf("origin = %q, want %q", info.Origin, OriginEnvironment)
	}

	// The database wins once an admin sets one — a UI control that appears to
	// save and then does nothing is the failure this precedence avoids.
	if err := store.Set(ctx, secrets.NameArtworkAPIKey, "the-newer-ui-credential", ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, info = src.Provider(ctx); info.Origin != OriginDatabase {
		t.Fatalf("origin = %q, want %q — the database must outrank the env var", info.Origin, OriginDatabase)
	}

	// Clearing falls BACK to the env var rather than off a cliff.
	if err := store.Delete(ctx, secrets.NameArtworkAPIKey); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	p, info = src.Provider(ctx)
	if p == nil || info.Origin != OriginEnvironment {
		t.Fatalf("after clear: origin = %q, want %q", info.Origin, OriginEnvironment)
	}
}

// TestProviderOffSwitchOutranksAStoredKey: an operator who switched the
// provider off must not have it switched back on by someone typing a key into
// the admin UI.
func TestProviderOffSwitchOutranksAStoredKey(t *testing.T) {
	pool := testDB(t)
	store := secretStore(t, pool)
	ctx := context.Background()
	if err := store.Set(ctx, secrets.NameArtworkAPIKey, "a-provider-credential", ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	src := NewSecretProviderSource(store, "an-env-credential", true /* disabled */, discardLog())
	p, info := src.Provider(ctx)
	if p != nil || info.Configured {
		t.Fatal("QUASAR_ARTWORK_PROVIDER=none must win over any stored or env key")
	}
	if info.Problem == "" {
		t.Fatal("the off switch must explain itself rather than looking like 'no key set'")
	}
}

// TestAStoredKeyTheMasterKeyCannotOpenDoesNotSilentlyUseTheEnvVar. Falling back
// here would mean the server quietly uses a DIFFERENT credential than the one an
// admin configured — precisely the surprise the facility exists to prevent.
func TestAStoredKeyTheMasterKeyCannotOpenDoesNotSilentlyUseTheEnvVar(t *testing.T) {
	pool := testDB(t)
	store := secretStore(t, pool)
	ctx := context.Background()
	if err := store.Set(ctx, secrets.NameArtworkAPIKey, "a-provider-credential", ""); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// The master key changes underneath the stored row.
	wrongKr, err := secrets.ParseKeyring("f39/f39/f39/f39/f39/f39/f39/f39/f39/f39/f38=", "")
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	wrong := secrets.NewStore(pool, wrongKr, secrets.DefaultRegistry())
	src := NewSecretProviderSource(wrong, "an-env-credential", false, discardLog())

	p, info := src.Provider(ctx)
	if p != nil || info.Configured {
		t.Fatal("an undecryptable stored key must not fall through to the env var")
	}
	if info.Problem == "" {
		t.Fatal("the operator must be told the master key does not match, not left with silence")
	}
}

// TestTheClientIsReusedWhileTheCredentialIsUnchanged. The client owns the
// outbound throttle, so rebuilding it per call would let a sweep burst past the
// provider's rate limit.
func TestTheClientIsReusedWhileTheCredentialIsUnchanged(t *testing.T) {
	pool := testDB(t)
	store := secretStore(t, pool)
	ctx := context.Background()
	if err := store.Set(ctx, secrets.NameArtworkAPIKey, "credential-one", ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	src := NewSecretProviderSource(store, "", false, discardLog())

	first, _ := src.Provider(ctx)
	second, _ := src.Provider(ctx)
	if first != second {
		t.Fatal("an unchanged credential must reuse the client (the throttle lives on it)")
	}
	if err := store.Set(ctx, secrets.NameArtworkAPIKey, "credential-two", ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	third, _ := src.Provider(ctx)
	if third == first {
		t.Fatal("a changed credential must produce a new client")
	}
}

// TestTheSweeperDoesNothingWithoutAProvider: ship-dark survives the move to a
// per-use resolution. With no credential the sweep queries no apps, contacts no
// third party and writes no rows. (It DOES read instance_secrets looking for a
// credential — that lookup is what makes a UI-set key take effect without a
// restart, and the claim is deliberately about apps and outbound requests.)
func TestTheSweeperDoesNothingWithoutAProvider(t *testing.T) {
	pool := testDB(t)
	store := secretStore(t, pool)
	svc, err := New(NewStore(pool), t.TempDir(), Options{
		ProviderSource: NewSecretProviderSource(store, "", false, discardLog()),
		SweepInterval:  0,
	}, discardLog())
	if err != nil {
		t.Fatalf("artwork.New: %v", err)
	}
	ctx := context.Background()
	var appID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (name, description, kind, runtime_spec, enabled)
		 VALUES ('Portal 2', '', 'game', '{}'::jsonb, true) RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	svc.SweepOnce(ctx)

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app_artwork`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Fatalf("an unconfigured deployment wrote %d artwork rows; want 0", rows)
	}
}

// TestTheArtworkEndpointReflectsAUISetKeyLive is the same acceptance property
// observed through the HTTP surface an admin actually uses: one running server,
// one GET before and one after the secret is written.
func TestTheArtworkEndpointReflectsAUISetKeyLive(t *testing.T) {
	pool := testDB(t)
	store := secretStore(t, pool)
	log := discardLog()
	ctx := context.Background()

	svc, err := New(NewStore(pool), t.TempDir(), Options{
		ProviderSource: NewSecretProviderSource(store, "", false, log),
		SweepInterval:  0,
	}, log)
	if err != nil {
		t.Fatalf("artwork.New: %v", err)
	}
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	NewHandler(svc, log).Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = $1`, "admin@test.local"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	tok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	var appID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (name, description, kind, runtime_spec, enabled)
		 VALUES ('Portal 2', '', 'game', '{}'::jsonb, true) RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	get := func() (configured bool, origin string) {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+"/v1/admin/apps/"+appID+"/artwork", nil)
		req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET: status %d", resp.StatusCode)
		}
		var env struct {
			ProviderConfigured bool   `json:"provider_configured"`
			ProviderOrigin     string `json:"provider_origin"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return env.ProviderConfigured, env.ProviderOrigin
	}

	if configured, origin := get(); configured || origin != OriginNone {
		t.Fatalf("before: configured=%v origin=%q, want false/none", configured, origin)
	}
	if err := store.Set(ctx, secrets.NameArtworkAPIKey, "a-provider-credential", ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Same server, same service, no restart.
	if configured, origin := get(); !configured || origin != OriginDatabase {
		t.Fatalf("after: configured=%v origin=%q, want true/database", configured, origin)
	}
}
