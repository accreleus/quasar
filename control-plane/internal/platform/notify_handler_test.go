package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func notifyHandler(t *testing.T, cfg WebhookConfig, out Delivery, sends *int) *NotifyHandler {
	t.Helper()
	return NewNotifyHandler(
		func(context.Context) (View, error) { return viewWithUpdate("aaaaaaa", "bbbbbbbbbb"), nil },
		func(context.Context) (WebhookConfig, error) { return cfg, nil },
		func(context.Context, WebhookConfig, Event) Delivery {
			*sends++
			return out
		},
		nil, quietLog(),
	)
}

func postTest(t *testing.T, h *NotifyHandler) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.handleTest(rec, httptest.NewRequest(http.MethodPost, "/v1/admin/platform/release-webhook/test", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	return rec, body
}

// TestTestSendRefusesWithNowhereToSend — the only 4xx on this route.
func TestTestSendRefusesWithNowhereToSend(t *testing.T) {
	sends := 0
	rec, body := postTest(t, notifyHandler(t, WebhookConfig{}, Delivery{OK: true}, &sends))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != CodeWebhookNotConfigured {
		t.Errorf("code = %v, want %s", errObj["code"], CodeWebhookNotConfigured)
	}
	if sends != 0 {
		t.Errorf("sent %d notifications with no URL, want 0", sends)
	}
}

// TestTestSendIgnoresTheEnabledSwitch — testing a URL before switching it on is
// the point of the button.
func TestTestSendIgnoresTheEnabledSwitch(t *testing.T) {
	sends := 0
	cfg := WebhookConfig{Enabled: false, URL: "https://hooks.example.com/abc"}
	rec, body := postTest(t, notifyHandler(t, cfg, Delivery{OK: true}, &sends))

	if rec.Code != http.StatusOK || sends != 1 {
		t.Fatalf("status=%d sends=%d, want 200 and one send", rec.Code, sends)
	}
	del, _ := body["delivery"].(map[string]any)
	if del["ok"] != true {
		t.Errorf("delivery = %v, want ok", del)
	}
}

// TestARefusedDeliveryIsTwoHundredWithOKFalse — the request succeeded and the
// receiver's answer is the payload. A 5xx here would say the control plane
// failed, which it did not.
func TestARefusedDeliveryIsTwoHundredWithOKFalse(t *testing.T) {
	sends := 0
	code := 404
	h := notifyHandler(t, WebhookConfig{Enabled: true, URL: "https://hooks.example.com/abc"},
		Delivery{StatusCode: &code, Error: "the webhook receiver answered 404 Not Found", DurationMS: 12}, &sends)
	rec, body := postTest(t, h)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	del, _ := body["delivery"].(map[string]any)
	if del["ok"] != false || del["status_code"] != float64(404) {
		t.Fatalf("delivery = %v, want ok:false with the receiver's status", del)
	}
	if del["error"] == nil || del["duration_ms"] != float64(12) {
		t.Errorf("delivery = %v, want the error and the duration reported", del)
	}
}

// TestRegisterWiresTheTestSendThroughTheAdminMiddleware — the gate is the
// middleware at registration; the handler never checks a role.
func TestRegisterWiresTheTestSendThroughTheAdminMiddleware(t *testing.T) {
	wrapped := 0
	admin := func(next http.Handler) http.Handler {
		wrapped++
		return next
	}
	NewNotifyHandler(nil, nil, nil, nil, quietLog()).Register(http.NewServeMux(), admin)
	if wrapped != 1 {
		t.Fatalf("admin middleware applied %d times, want 1", wrapped)
	}
}
