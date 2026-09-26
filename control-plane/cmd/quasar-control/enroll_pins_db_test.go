// Add host's images on an owned install, through /enroll-host.sh (#365): the operator's
// override, else the installed release's images, else the machine's install-time
// fallback. TEST_DATABASE_URL-gated; `make test-db` runs it.
package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/enrollscript"
	"github.com/accreleus/quasar/control-plane/internal/platform"
)

func servedPins(t *testing.T, pins enrollscript.PinSource) string {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "enroll-host.sh"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "enroll-host.sh"), script, 0o644); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	rr := httptest.NewRecorder()
	enrollscript.HandlerFrom(root, pins, quiet).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/enroll-host.sh", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

func TestAddHostOnAnOwnedInstallPrefersTheInstalledReleaseOverItsInstallTimeImages(t *testing.T) {
	pool := jobsTestDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE platform_releases CASCADE`); err != nil {
		t.Fatal(err)
	}
	store := platform.NewStore(pool)

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "release", "platform-release-manifest.v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := platform.ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertRelease(ctx, platform.Release{
		Channel: platform.ChannelStable, Version: &m.Version, SourceCommit: m.SourceCommit,
		BuiltAt: m.BuiltAtTime(), SchemaVersion: m.SchemaVersion, Manifest: json.RawMessage(raw),
	}); err != nil {
		t.Fatal(err)
	}
	releaseSeed, releaseAgent, _ := platform.EnrollImagesOf(m)

	// What a revision-2 control-plane recipe gives an owned machine that installed an
	// older release and set no override: only the install-time fallbacks.
	installTime := enrollscript.Pins{
		SeedImage:  "ghcr.io/accreleus/quasar/quasar-recovery@sha256:" + strings.Repeat("1", 64),
		AgentImage: "ghcr.io/accreleus/quasar/quasar-node-agent@sha256:" + strings.Repeat("2", 64),
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	// This control plane's release trust rides every answer, whichever images win.
	trust := enrollscript.Pins{
		AllowedNamespaces:  "registry.example.invalid/quasar",
		InsecureRegistries: "registry.example.invalid:5000",
	}
	trusted := func(body string) {
		t.Helper()
		if !strings.Contains(body, "\nPINNED_ALLOWED_NAMESPACES='"+trust.AllowedNamespaces+"'\n") ||
			!strings.Contains(body, "\nPINNED_INSECURE_REGISTRIES='"+trust.InsecureRegistries+"'\n") {
			t.Fatalf("the release trust was dropped:\n%s", body)
		}
	}

	before := strings.Repeat("0", 40)
	body := servedPins(t, installedEnrollPins(trust, installTime, store, &before, quiet))
	trusted(body)
	if !strings.Contains(body, "\nPINNED_SEED_IMAGE='"+installTime.SeedImage+"'\n") ||
		!strings.Contains(body, "\nPINNED_AGENT_IMAGE='"+installTime.AgentImage+"'\n") {
		t.Fatal("before any release is installed, the install-time images must be served")
	}

	// The control plane now runs the detected release: its images win.
	after := m.SourceCommit
	body = servedPins(t, installedEnrollPins(trust, installTime, store, &after, quiet))
	trusted(body)
	if !strings.Contains(body, "\nPINNED_SEED_IMAGE='"+releaseSeed+"'\n") ||
		!strings.Contains(body, "\nPINNED_AGENT_IMAGE='"+releaseAgent+"'\n") {
		t.Fatalf("after the update the installed release's images must win:\n%s", body)
	}

	// An operator override still wins over the release, one field at a time.
	override := trust
	override.AgentImage = "registry.example.invalid/dev/quasar-node-agent@sha256:" + strings.Repeat("e", 64)
	body = servedPins(t, installedEnrollPins(override, installTime, store, &after, quiet))
	trusted(body)
	if !strings.Contains(body, "\nPINNED_AGENT_IMAGE='"+override.AgentImage+"'\n") ||
		!strings.Contains(body, "\nPINNED_SEED_IMAGE='"+releaseSeed+"'\n") {
		t.Fatalf("the override must win for its field only:\n%s", body)
	}
}
