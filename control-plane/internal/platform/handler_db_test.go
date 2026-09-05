// The release view behind the REAL RequireAuth→RequireAdmin chain: hiding admin
// UI is never the access control (CLAUDE.md invariant #6), so the gate is
// exercised here rather than asserted about.
package platform

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

func newViewHarness(t *testing.T, pool *pgxpool.Pool, deps *Deps) (adminToken, userToken, url string) {
	t.Helper()
	ctx := context.Background()

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	authHandler := auth.NewHandler(authSvc)
	for _, u := range []struct{ email, name string }{
		{"release-admin@t.local", "releaseadmin"},
		{"release-user@t.local", "releaseuser"},
	} {
		if _, err := authSvc.Register(ctx, u.email, u.name, "password12345"); err != nil {
			t.Fatalf("register %s: %v", u.email, err)
		}
	}
	mustExec(t, pool, `UPDATE users SET role='admin' WHERE email='release-admin@t.local'`)
	adminTok, err := authSvc.Login(ctx, "release-admin@t.local", "password12345", "test")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	userTok, err := authSvc.Login(ctx, "release-user@t.local", "password12345", "test")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}
	adminToken, userToken = adminTok.Plaintext, userTok.Plaintext

	mux := http.NewServeMux()
	NewHandler(deps, nil).Register(mux, func(next http.Handler) http.Handler {
		return authHandler.RequireAuth(authHandler.RequireAdmin(next))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return adminToken, userToken, srv.URL + "/v1/admin/platform/releases"
}

func get(t *testing.T, url, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func TestReleaseViewIsAdminOnlyAndServesThePlan(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)

	mustExec(t, pool, `INSERT INTO hosts (node_name, status, source_commit, built_at, install_mode, updater_present)
		VALUES ('gpu-01', 'online', $1, now(), 'registry', true)`, commitA)
	checked := time.Date(2026, 9, 4, 2, 7, 11, 0, time.UTC)
	if _, err := store.UpsertRelease(ctx, Release{
		Channel: ChannelStable, Version: str("0.2.0"), SourceCommit: commitB,
		// At the running binary's own schema: a release below it is never
		// listed (ADR 0002), which would make this a test of the wrong rule.
		BuiltAt: at(4), SchemaVersion: buildinfo.Get().SchemaVersion, Manifest: validManifest, Notes: "### Fixed\n",
	}); err != nil {
		t.Fatalf("seed release: %v", err)
	}

	deps := &Deps{
		Channel: func(context.Context) (string, string, error) { return ChannelStable, "develop", nil },
		Hosts:   store.Hosts,
		Releases: func(ctx context.Context, channel string) ([]Release, error) {
			return store.Releases(ctx, channel)
		},
		Detection: func(context.Context) (DetectionStatus, error) {
			return DetectionStatus{CheckedAt: &checked}, nil
		},
	}
	adminToken, userToken, url := newViewHarness(t, pool, deps)

	if code, _ := get(t, url, ""); code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", code)
	}
	if code, _ := get(t, url, userToken); code != http.StatusForbidden {
		t.Errorf("non-admin = %d, want 403 — the gate is the server's, not the UI's", code)
	}

	code, body := get(t, url, adminToken)
	if code != http.StatusOK {
		t.Fatalf("admin = %d (%s), want 200", code, body)
	}
	var view struct {
		Channel    string `json:"channel"`
		SourceRepo string `json:"source_repo"`
		EdgeBranch string `json:"edge_branch"`
		CheckedAt  string `json:"checked_at"`
		Installed  struct {
			Hosts []struct {
				NodeName      string `json:"node_name"`
				IdentityKnown bool   `json:"identity_known"`
			} `json:"hosts"`
		} `json:"installed"`
		Available []struct {
			ID      string          `json:"id"`
			Version string          `json:"version"`
			Notes   string          `json:"notes"`
			Man     json.RawMessage `json:"manifest"`
		} `json:"available"`
		Targets []struct {
			Kind string `json:"kind"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if view.Channel != ChannelStable || view.EdgeBranch != "develop" {
		t.Errorf("channel/edge_branch = %q/%q", view.Channel, view.EdgeBranch)
	}
	// Served from the detection config, not hard-coded by the client (#104).
	if view.SourceRepo != DefaultReleaseRepo {
		t.Errorf("source_repo = %q, want %q", view.SourceRepo, DefaultReleaseRepo)
	}
	if view.CheckedAt != "2026-09-04T02:07:11Z" {
		t.Errorf("checked_at = %q, want the last successful detection", view.CheckedAt)
	}
	if len(view.Available) != 1 || view.Available[0].Version != "0.2.0" || len(view.Available[0].Man) == 0 {
		t.Errorf("available = %+v, want the seeded release with its manifest", view.Available)
	}
	if view.Available[0].ID == "" {
		t.Error("a release must carry the row id amendment 2's apply will name")
	}
	if len(view.Targets) != 2 || view.Targets[0].Kind != TargetControlPlane {
		t.Errorf("targets = %+v, want the control plane then one host", view.Targets)
	}
	if len(view.Installed.Hosts) != 1 || !view.Installed.Hosts[0].IdentityKnown {
		t.Errorf("installed.hosts = %+v", view.Installed.Hosts)
	}
}
