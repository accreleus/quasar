package platform

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The webhook target is public by construction (outbound refuses to dial a
// loopback address), so an httptest server is unreachable from this path on
// purpose. These drive sendWith over a stub Doer instead, which is what makes
// the headers, the signature and the retry policy observable.

type stubDoer struct {
	calls    []*http.Request
	bodies   []string
	statuses []int
	err      error
}

func (s *stubDoer) Do(req *http.Request) (*http.Response, error) {
	s.calls = append(s.calls, req)
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		s.bodies = append(s.bodies, string(b))
	}
	if s.err != nil {
		return nil, &url.Error{Op: "Post", URL: req.URL.String(), Err: s.err}
	}
	status := 200
	if n := len(s.calls) - 1; n < len(s.statuses) {
		status = s.statuses[n]
	} else if len(s.statuses) > 0 {
		status = s.statuses[len(s.statuses)-1]
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// noBackoff shrinks the retry wait for the duration of one test.
func noBackoff(t *testing.T) {
	t.Helper()
	prev := webhookRetryBackoff
	webhookRetryBackoff = time.Millisecond
	t.Cleanup(func() { webhookRetryBackoff = prev })
}

// TestSendRefusesAnUnusableURLBeforeAnyRequest — http, a credential in the URL
// and a relative reference never reach the network.
func TestSendRefusesAnUnusableURLBeforeAnyRequest(t *testing.T) {
	for name, raw := range map[string]string{
		"http":     "http://hooks.example.com/abc",
		"userinfo": "https://user:pass@hooks.example.com/abc",
		"relative": "/hooks/abc",
		"no host":  "https:///abc",
		"empty":    "",
	} {
		t.Run(name, func(t *testing.T) {
			got := SendWebhook(context.Background(), WebhookConfig{Enabled: true, URL: raw}, Event{})
			if got.OK || got.Error == "" {
				t.Fatalf("SendWebhook(%q) = %+v, want a refusal", raw, got)
			}
		})
	}
}

// TestSendRefusesAHostOutsideTheNarrowingAllowlist — QUASAR_PLATFORM_WEBHOOK_HOSTS
// is the operator's pin, checked before any client is built.
func TestSendRefusesAHostOutsideTheNarrowingAllowlist(t *testing.T) {
	t.Setenv(WebhookHostsEnv, "hooks.example.com, ntfy.example.com")

	got := SendWebhook(context.Background(),
		WebhookConfig{Enabled: true, URL: "https://elsewhere.example.net/abc"}, Event{})
	if got.OK || !strings.Contains(got.Error, WebhookHostsEnv) {
		t.Fatalf("SendWebhook = %+v, want a refusal naming %s", got, WebhookHostsEnv)
	}
	// A listed host passes this gate; the assertion stops here rather than
	// letting the call reach a real dial.
	if _, ok := parseWebhookHosts()["hooks.example.com"]; !ok {
		t.Fatal("a listed host was not in the parsed allowlist")
	}
}

// TestPostSignsTheBodyWhenASecretIsConfigured — and sends no signature header
// when there is none, because Slack, Discord and ntfy authenticate by URL.
func TestPostSignsTheBodyWhenASecretIsConfigured(t *testing.T) {
	u := mustURL(t, "https://hooks.example.com/abc")
	body := []byte(`{"event":"platform.release.detected"}`)

	d := &stubDoer{statuses: []int{200}}
	got := postOnce(context.Background(), d, WebhookConfig{URL: u.String(), Secret: "s3cret"},
		NotifyEventRelease, deliveryID(), body, u)
	if !got.OK {
		t.Fatalf("postOnce = %+v, want ok", got)
	}
	req := d.calls[0]
	if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("request = %s %s, content-type %q", req.Method, req.URL, req.Header.Get("Content-Type"))
	}
	if req.Header.Get(HeaderEvent) != NotifyEventRelease {
		t.Errorf("%s = %q", HeaderEvent, req.Header.Get(HeaderEvent))
	}
	if len(req.Header.Get(HeaderDelivery)) != 32 {
		t.Errorf("%s = %q, want 32 hex", HeaderDelivery, req.Header.Get(HeaderDelivery))
	}
	ts := req.Header.Get(HeaderTimestamp)
	want := Signature("s3cret", ts, body)
	if sig := req.Header.Get(HeaderSignature); sig != want {
		t.Errorf("%s = %q, want %q", HeaderSignature, sig, want)
	}

	unsigned := &stubDoer{statuses: []int{200}}
	postOnce(context.Background(), unsigned, WebhookConfig{URL: u.String()}, NotifyEventRelease, deliveryID(), body, u)
	if sig := unsigned.calls[0].Header.Get(HeaderSignature); sig != "" {
		t.Errorf("%s = %q with no secret, want none", HeaderSignature, sig)
	}
}

// TestSignatureCoversTheTimestamp — the timestamp is inside the signed material,
// so a captured delivery cannot be replayed under a new one.
func TestSignatureCoversTheTimestamp(t *testing.T) {
	body := []byte(`{"a":1}`)
	if Signature("k", "100", body) == Signature("k", "101", body) {
		t.Fatal("two timestamps produced the same signature; a replay would verify")
	}
	if Signature("k", "100", body) == Signature("other", "100", body) {
		t.Fatal("two secrets produced the same signature")
	}
	if !strings.HasPrefix(Signature("k", "100", body), "sha256=") {
		t.Error("signature should carry its algorithm prefix")
	}
}

// TestRetryPolicy — a 5xx and a transport failure are retried, a 4xx is not.
func TestRetryPolicy(t *testing.T) {
	noBackoff(t)
	u := mustURL(t, "https://hooks.example.com/abc")
	cfg := WebhookConfig{URL: u.String()}

	t.Run("a 4xx is answered once", func(t *testing.T) {
		d := &stubDoer{statuses: []int{404}}
		got := sendWith(context.Background(), d, cfg, NotifyEventRelease, deliveryID(), []byte("{}"), u)
		if got.OK || len(d.calls) != 1 {
			t.Fatalf("calls=%d ok=%v, want one attempt and a failure", len(d.calls), got.OK)
		}
		if got.StatusCode == nil || *got.StatusCode != 404 {
			t.Errorf("status = %v, want 404", got.StatusCode)
		}
	})

	t.Run("a 5xx is retried to the attempt bound", func(t *testing.T) {
		d := &stubDoer{statuses: []int{500}}
		got := sendWith(context.Background(), d, cfg, NotifyEventRelease, deliveryID(), []byte("{}"), u)
		if got.OK || len(d.calls) != webhookHTTPAttempts {
			t.Fatalf("calls=%d, want %d", len(d.calls), webhookHTTPAttempts)
		}
	})

	t.Run("a 429 that then succeeds is delivered", func(t *testing.T) {
		d := &stubDoer{statuses: []int{429, 204}}
		got := sendWith(context.Background(), d, cfg, NotifyEventRelease, deliveryID(), []byte("{}"), u)
		if !got.OK || len(d.calls) != 2 {
			t.Fatalf("calls=%d ok=%v, want two attempts ending delivered", len(d.calls), got.OK)
		}
	})

	t.Run("a transport failure is retried", func(t *testing.T) {
		d := &stubDoer{err: errors.New("dial tcp: connection refused")}
		got := sendWith(context.Background(), d, cfg, NotifyEventRelease, deliveryID(), []byte("{}"), u)
		if got.OK || len(d.calls) != webhookHTTPAttempts || got.StatusCode != nil {
			t.Fatalf("calls=%d status=%v, want %d attempts and no status", len(d.calls), got.StatusCode, webhookHTTPAttempts)
		}
	})
}

// TestErrorNeverCarriesTheWebhookURL — a Slack or Discord webhook URL
// authenticates by being known, and this string is logged, stored and served.
func TestErrorNeverCarriesTheWebhookURL(t *testing.T) {
	noBackoff(t)
	secretPath := "/services/T000/B000/SUPERSECRETTOKEN"
	u := mustURL(t, "https://hooks.example.com"+secretPath)

	d := &stubDoer{err: errors.New("dial tcp 93.184.216.34:443: i/o timeout")}
	got := sendWith(context.Background(), d, WebhookConfig{URL: u.String()}, NotifyEventRelease, deliveryID(), []byte("{}"), u)

	if strings.Contains(got.Error, secretPath) || strings.Contains(got.Error, "SUPERSECRETTOKEN") {
		t.Fatalf("delivery error leaked the webhook URL: %q", got.Error)
	}
	if !strings.Contains(got.Error, "hooks.example.com") {
		t.Errorf("error %q should still name the host, which is not the secret part", got.Error)
	}
}

// TestParseWebhookHostsUnsetMeansNoNarrowing — nil, not an empty set that would
// refuse everything.
func TestParseWebhookHostsUnsetMeansNoNarrowing(t *testing.T) {
	t.Setenv(WebhookHostsEnv, "")
	if got := parseWebhookHosts(); got != nil {
		t.Fatalf("unset %s = %v, want nil (no narrowing)", WebhookHostsEnv, got)
	}
	t.Setenv(WebhookHostsEnv, " , , ")
	if got := parseWebhookHosts(); got != nil {
		t.Fatalf("all-blank %s = %v, want nil", WebhookHostsEnv, got)
	}
	t.Setenv(WebhookHostsEnv, "Hooks.Example.COM")
	got := parseWebhookHosts()
	if _, ok := got["hooks.example.com"]; !ok || len(got) != 1 {
		t.Fatalf("%s = %v, want one lowercased host", WebhookHostsEnv, got)
	}
}

// TestRetriesReuseOneDeliveryID — the header a receiver deduplicates on must
// name the SEND, not the attempt, or a 5xx that later succeeds is delivered
// twice under two ids and the deduplication does nothing.
func TestRetriesReuseOneDeliveryID(t *testing.T) {
	noBackoff(t)
	u := mustURL(t, "https://hooks.example.com/abc")

	d := &stubDoer{statuses: []int{500, 204}}
	if out := sendWith(context.Background(), d, WebhookConfig{URL: u.String()},
		NotifyEventRelease, deliveryID(), []byte("{}"), u); !out.OK {
		t.Fatalf("sendWith = %+v, want the second attempt delivered", out)
	}
	if len(d.calls) != 2 {
		t.Fatalf("calls = %d, want two attempts", len(d.calls))
	}
	first := d.calls[0].Header.Get(HeaderDelivery)
	second := d.calls[1].Header.Get(HeaderDelivery)
	if first == "" || len(first) != 32 {
		t.Fatalf("%s = %q, want 32 hex", HeaderDelivery, first)
	}
	if first != second {
		t.Errorf("%s changed across a retry: %q then %q", HeaderDelivery, first, second)
	}
	// The signature material is per-attempt even so, which is what keeps a
	// captured delivery from being replayable.
	if d.calls[0].Header.Get(HeaderTimestamp) == "" {
		t.Error("no timestamp header on the first attempt")
	}
}
