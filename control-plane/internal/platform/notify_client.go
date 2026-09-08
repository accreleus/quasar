package platform

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
	"github.com/accreleus/quasar/control-plane/internal/outbound"
)

// The outbound half. Containment is internal/outbound's; what is decided here
// is the allowlist: a webhook target is admin-supplied rather than remote data,
// so one delivery's allowlist is that URL's own host. Neither that nor
// QUASAR_PLATFORM_WEBHOOK_HOSTS relaxes the dial guard, so this can never reach
// the instance's own network.

const (
	// One request. A slow receiver must not hold a detection pass open.
	webhookRequestTimeout = 5 * time.Second
	// HTTP attempts within ONE pass. Bounded retries, then the row is failed
	// and the NEXT pass retries — up to MaxNotifyAttempts passes.
	webhookHTTPAttempts = 3
	// A receiver's body is read only to drain the connection.
	webhookMaxBodyBytes int64 = 64 << 10
)

// A var so the retry test does not sleep six seconds. Never reassigned in
// production.
var webhookRetryBackoff = 2 * time.Second

// Signature headers. Body signing is HMAC-SHA256 over "<timestamp>.<body>", so
// a captured delivery cannot be replayed under a new timestamp.
const (
	HeaderEvent     = "X-Quasar-Event"
	HeaderDelivery  = "X-Quasar-Delivery"
	HeaderTimestamp = "X-Quasar-Timestamp"
	HeaderSignature = "X-Quasar-Signature-256"
)

// WebhookHostsEnv narrows the destinations a webhook may be sent to. Unset =
// any public https host, which is the default for an admin-chosen URL.
const WebhookHostsEnv = "QUASAR_PLATFORM_WEBHOOK_HOSTS"

// SendWebhook POSTs one event. It never returns an error: every failure is a
// Delivery with OK=false, so a webhook problem cannot become a detection one.
func SendWebhook(ctx context.Context, cfg WebhookConfig, ev Event) Delivery {
	started := time.Now()
	done := func(d Delivery) Delivery {
		d.DurationMS = int(time.Since(started).Milliseconds())
		return d
	}

	body, err := json.Marshal(ev)
	if err != nil {
		return done(Delivery{Error: "could not encode the notification body"})
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || !u.IsAbs() || u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
		return done(Delivery{Error: "the configured webhook URL is not an absolute https URL without credentials"})
	}
	host := strings.ToLower(u.Hostname())
	if narrow := parseWebhookHosts(); narrow != nil && !outbound.HostAllowed(narrow, host) {
		return done(Delivery{Error: fmt.Sprintf("host %q is not in %s", host, WebhookHostsEnv)})
	}

	client, err := outbound.New(outbound.Config{
		AllowHosts:   map[string]struct{}{host: {}},
		Timeout:      webhookRequestTimeout,
		MaxBodyBytes: webhookMaxBodyBytes,
	})
	if err != nil {
		return done(Delivery{Error: "could not build the webhook client"})
	}

	return done(sendWith(ctx, client, cfg, ev.Event, body, u))
}

// sendWith is the retry policy over one Doer: bounded attempts, and only for a
// failure that could plausibly answer differently later. Split from SendWebhook
// so a test drives it without a live dial (outbound refuses loopback, so an
// httptest server is not reachable from here by construction).
func sendWith(ctx context.Context, d outbound.Doer, cfg WebhookConfig, event string, body []byte, u *url.URL) Delivery {
	var last Delivery
	for attempt := 1; attempt <= webhookHTTPAttempts; attempt++ {
		last = postOnce(ctx, d, cfg, event, body, u)
		if last.OK || !retryable(last.StatusCode) {
			return last
		}
		if attempt == webhookHTTPAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(time.Duration(attempt) * webhookRetryBackoff):
		}
	}
	return last
}

func postOnce(ctx context.Context, client outbound.Doer, cfg WebhookConfig, event string, body []byte, u *url.URL) Delivery {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return Delivery{Error: "could not build the webhook request"}
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "quasar-control-plane/"+buildinfo.Get().Version)
	req.Header.Set(HeaderEvent, event)
	req.Header.Set(HeaderDelivery, deliveryID())
	req.Header.Set(HeaderTimestamp, ts)
	if cfg.Secret != "" {
		req.Header.Set(HeaderSignature, Signature(cfg.Secret, ts, body))
	}

	resp, err := client.Do(req)
	if err != nil {
		return Delivery{Error: sanitizeErr(err, u)}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	code := resp.StatusCode
	if code >= 200 && code < 300 {
		return Delivery{OK: true, StatusCode: &code}
	}
	return Delivery{
		StatusCode: &code,
		Error:      fmt.Sprintf("the webhook receiver answered %d %s", code, http.StatusText(code)),
	}
}

// Signature is the X-Quasar-Signature-256 value: sha256=<hex HMAC over
// "<timestamp>.<body>">. Exported so the docs' verification recipe has one
// definition to match.
func Signature(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// retryable: a transport failure (no status) and the statuses that mean "later",
// never a 4xx the receiver will answer identically next week.
func retryable(status *int) bool {
	if status == nil {
		return true
	}
	switch *status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return *status >= 500
}

// sanitizeErr turns a transport error into prose that cannot carry the webhook
// URL — itself the credential — since this string is logged, stored in
// platform_release_notifications.last_error and served to the console.
func sanitizeErr(err error, u *url.URL) string {
	msg := err.Error()
	// *url.Error stringifies as `Post "<url>": <cause>`; the cause alone is the
	// part worth reporting.
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		msg = ue.Err.Error()
	}
	for _, secret := range []string{u.String(), u.RequestURI(), u.EscapedPath()} {
		if secret != "" && secret != "/" {
			msg = strings.ReplaceAll(msg, secret, "<webhook url>")
		}
	}
	return fmt.Sprintf("could not reach %s: %s", u.Hostname(), boundString(msg, 200))
}

// parseWebhookHosts reads the narrowing allowlist; nil when unset, which means
// "the configured host only", not "anything".
func parseWebhookHosts() map[string]struct{} {
	raw := strings.TrimSpace(os.Getenv(WebhookHostsEnv))
	if raw == "" {
		return nil
	}
	hosts := outbound.ParseHostList(raw, "")
	delete(hosts, "")
	if len(hosts) == 0 {
		return nil
	}
	return hosts
}

// deliveryID gives a receiver something to deduplicate on. Randomness failure
// is not worth failing a notification over — an empty id is still a valid header.
func deliveryID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
