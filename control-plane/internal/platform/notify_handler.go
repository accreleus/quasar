package platform

import (
	"context"
	"net/http"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// The test-send endpoint. Configuring the webhook is PATCH /v1/admin/settings
// and the signing secret is PUT /v1/admin/secrets/{name}; only the test needs a
// route. semantics: control-api.md §"Release notifications"

// CodeWebhookNotConfigured is the 400 for a test with nowhere to send.
const CodeWebhookNotConfigured = "webhook_not_configured"

// NotifyHandler serves the release-notification test send.
type NotifyHandler struct {
	view    func(ctx context.Context) (View, error)
	config  func(ctx context.Context) (WebhookConfig, error)
	send    func(ctx context.Context, cfg WebhookConfig, ev Event) Delivery
	auditor audit.Recorder
	log     logger
	now     func() time.Time
}

// NewNotifyHandler builds the endpoint. A nil send falls back to the production
// sender; a nil now to time.Now.
func NewNotifyHandler(
	view func(ctx context.Context) (View, error),
	config func(ctx context.Context) (WebhookConfig, error),
	send func(ctx context.Context, cfg WebhookConfig, ev Event) Delivery,
	auditor audit.Recorder,
	log logger,
) *NotifyHandler {
	if send == nil {
		send = SendWebhook
	}
	return &NotifyHandler{view: view, config: config, send: send, auditor: auditor, log: log, now: time.Now}
}

// Register wires the route. admin must compose RequireAuth→RequireAdmin.
func (h *NotifyHandler) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/admin/platform/release-webhook/test", admin(http.HandlerFunc(h.handleTest)))
}

// deliveryResponse is the `{ "delivery": … }` envelope.
type deliveryResponse struct {
	Delivery deliveryBody `json:"delivery"`
}

type deliveryBody struct {
	OK         bool    `json:"ok"`
	StatusCode *int    `json:"status_code"`
	Error      *string `json:"error"`
	DurationMS int     `json:"duration_ms"`
}

// handleTest sends one test notification.
//
// A refused delivery is 200 with ok=false, not a 5xx: the request succeeded and
// the receiver's answer is the payload. It ignores release_webhook_enabled, and
// writes no platform_release_notifications row, so a test can never suppress
// the real notification for a release.
//
// The send runs the full retry ladder synchronously inside the request, so an
// unresponsive receiver holds it for up to ~21 s. That is deliberate: the admin
// pressed "Send test" to find out whether the webhook works, and the whole
// answer — including "it only worked on the third try" — is what they asked
// for. Returning 202 and making them poll would tell them less.
func (h *NotifyHandler) handleTest(w http.ResponseWriter, r *http.Request) {
	if h.view == nil || h.config == nil {
		h.log.Error("release webhook test has no dependencies wired")
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "release notifications are not available on this control plane")
		return
	}
	cfg, err := h.config(r.Context())
	if err != nil {
		h.log.Error("read the release webhook setting", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read the webhook setting")
		return
	}
	if !cfg.Configured() {
		httpx.WriteError(w, http.StatusBadRequest, CodeWebhookNotConfigured,
			"set a webhook URL before sending a test notification")
		return
	}
	view, err := h.view(r.Context())
	if err != nil {
		h.log.Error("build platform release view", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read the release view")
		return
	}

	del := h.send(r.Context(), cfg, TestEvent(view, h.now()))

	if u, ok := auth.UserFromContext(r.Context()); ok {
		// The URL is a credential on every receiver that authenticates by URL,
		// so the audit row records the outcome and never the destination.
		audit.TryRecord(r.Context(), h.auditor, u.ID, "platform.release_webhook.tested", "instance", "",
			map[string]any{"ok": del.OK, "status_code": del.StatusCode})
	}

	body := deliveryBody{OK: del.OK, StatusCode: del.StatusCode, DurationMS: del.DurationMS}
	if !del.OK && del.Error != "" {
		msg := del.Error
		body.Error = &msg
	}
	httpx.WriteJSON(w, http.StatusOK, deliveryResponse{Delivery: body})
}
