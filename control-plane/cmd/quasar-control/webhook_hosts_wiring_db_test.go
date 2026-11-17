// webhook_hosts_wiring_db_test.go — PATCH /v1/admin/settings refuses a release
// webhook URL whose host QUASAR_PLATFORM_WEBHOOK_HOSTS excludes, through the
// one injection in NewServices (settingsHandler.WebhookHostAllowed, #156).
// Nothing else sets it, so deleting that line left every test green while the
// save-time refusal silently disappeared (delivery still refuses). This drives
// the real NewServices wiring and the real settings route.
//
// TEST_DATABASE_URL-gated like every other DB test in this package.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"
)

func TestNewServicesWiresTheWebhookHostNarrowingOnSettingsSave(t *testing.T) {
	t.Setenv("QUASAR_PLATFORM_WEBHOOK_HOSTS", "hooks.example.com")
	t.Setenv("QUASAR_JOBS", "0")

	pool := jobsTestDB(t)
	cfg := jobsTestConfig(t)
	svc, err := NewServices(cfg, pool, discardSlogLogger(), nil)
	if err != nil {
		t.Fatalf("NewServices: %v", err)
	}
	defer svc.Stop()

	tok, err := svc.authSvc.MintEphemeral(context.Background(), auth.RoleAdmin, time.Minute)
	if err != nil {
		t.Fatalf("mint admin identity: %v", err)
	}
	mux := http.NewServeMux()
	svc.RegisterRoutes(mux)

	patch := func(url string) (int, string) {
		body := `{"release_webhook_url":"` + url + `"}`
		req := httptest.NewRequest(http.MethodPatch, "/v1/admin/settings", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	if code, body := patch("https://other.example.net/a"); code != http.StatusBadRequest || !strings.Contains(body, "validation_failed") {
		t.Fatalf("a host outside QUASAR_PLATFORM_WEBHOOK_HOSTS: got %d %s, want 400 validation_failed", code, body)
	}
	if code, body := patch("https://hooks.example.com/a"); code != http.StatusOK {
		t.Fatalf("a host inside the list: got %d %s, want 200", code, body)
	}
}
