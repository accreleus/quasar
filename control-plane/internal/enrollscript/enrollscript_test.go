package enrollscript_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/enrollscript"
)

type fixture struct {
	SeedImage  string   `json:"seed_image"`
	AgentImage string   `json:"agent_image"`
	Lines      []string `json:"lines"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "enroll-host", "pins.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func realScript(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "enroll-host.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func webRoot(t *testing.T, script []byte) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "enroll-host.sh"), script, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func get(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/enroll-host.sh", nil))
	return rr
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// The lines the console reads back (testdata/enroll-host/pins.json) are exactly what the
// served script carries, and the rendered script still parses.
func TestServedScriptCarriesThePinnedImagesTheConsoleReads(t *testing.T) {
	f := loadFixture(t)
	h := enrollscript.Handler(webRoot(t, realScript(t)), enrollscript.Pins{SeedImage: f.SeedImage, AgentImage: f.AgentImage}, quiet)
	rr := get(t, h)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	lines := strings.Split(body, "\n")
	for _, want := range f.Lines {
		n := 0
		for _, l := range lines {
			if l == want {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d lines %q, want 1", n, want)
		}
	}
	if strings.Contains(body, "PINNED_SEED_IMAGE=''") || strings.Contains(body, "PINNED_AGENT_IMAGE=''") {
		t.Error("a placeholder survived the render")
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q: the pins follow configuration and must not be cached", cc)
	}
	if !strings.HasPrefix(body, "#!/bin/sh\n") {
		t.Errorf("not the script: %q", body[:min(len(body), 40)])
	}
	check := exec.Command("sh", "-n")
	check.Stdin = strings.NewReader(body)
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("the rendered script does not parse: %v\n%s", err, out)
	}
}

func TestUnsetPinsServeTheScriptWithEmptyPins(t *testing.T) {
	rr := get(t, enrollscript.Handler(webRoot(t, realScript(t)), enrollscript.Pins{}, quiet))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "\nPINNED_SEED_IMAGE=''\n") {
		t.Fatal("an unset pin must stay empty, so the script can say what is not configured")
	}
}

func TestAScriptWithoutItsPlaceholdersIsRefusedNotServedUnpinned(t *testing.T) {
	f := loadFixture(t)
	pins := enrollscript.Pins{SeedImage: f.SeedImage, AgentImage: f.AgentImage}
	for name, script := range map[string]string{
		"missing":   "#!/bin/sh\necho old\n",
		"duplicate": "#!/bin/sh\nPINNED_SEED_IMAGE=''\nPINNED_SEED_IMAGE=''\nPINNED_AGENT_IMAGE=''\n",
	} {
		rr := get(t, enrollscript.Handler(webRoot(t, []byte(script)), pins, quiet))
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("%s: status %d, want 500", name, rr.Code)
		}
		if strings.Contains(rr.Body.String(), "#!/bin/sh") {
			t.Errorf("%s: served the script anyway", name)
		}
	}
}

func TestNoScriptInTheWebRootIsNotFound(t *testing.T) {
	rr := get(t, enrollscript.Handler(t.TempDir(), enrollscript.Pins{}, quiet))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rr.Code)
	}
}

func TestValidImageAcceptsOnlyADigestPin(t *testing.T) {
	for ref, want := range map[string]bool{
		"ghcr.io/accreleus/quasar/quasar-recovery@sha256:" + strings.Repeat("a", 64): true,
		"registry.example:5000/quasar-recovery@sha256:" + strings.Repeat("0", 64):    true,
		"ghcr.io/accreleus/quasar/quasar-recovery:0.6.0":                             false,
		"ghcr.io/x/quasar-recovery:0.6.0@sha256:" + strings.Repeat("a", 64):          false,
		"ghcr.io/x/quasar-recovery@sha256:" + strings.Repeat("A", 64):                false,
		"ghcr.io/x/quasar-recovery@sha256:abc":                                       false,
		"ghcr.io/x/q'; rm -rf /@sha256:" + strings.Repeat("a", 64):                   false,
		"": false,
	} {
		if got := enrollscript.ValidImage(ref); got != want {
			t.Errorf("ValidImage(%q) = %v, want %v", ref, got, want)
		}
	}
}
