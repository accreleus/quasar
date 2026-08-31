// review5_test.go — the round-5 findings that are NOT upload-specific and so
// stayed on this branch when POST /v1/admin/tls/certificate was split out.
package access

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestForwardedProtoIsAuthoritativeForTheBrowserHop is the round-5 finding.
// Backend TLS was treated as proof even when the proxy explicitly said the
// browser hop was http, so scheme=https + X-Forwarded-Proto=http reported a
// secure context. secure_context is the microphone-availability field, so a
// false positive actively misleads.
func TestForwardedProtoIsAuthoritativeForTheBrowserHop(t *testing.T) {
	if secureContext("https", "http", "http://public.example", "quasar.internal:8443") {
		t.Fatal("backend TLS overrode the proxy's explicit statement that the BROWSER hop was http — " +
			"the browser never sees the backend hop")
	}
	// The proxy saying https is what makes it secure, whatever our own hop was.
	if !secureContext("http", "https", "https://public.example", "quasar.internal:8080") {
		t.Error("a proxy asserting https was not honoured")
	}
	if !secureContext("https", "https", "https://public.example", "quasar.internal:8443") {
		t.Error("https on both hops should be secure")
	}
	// A loopback ORIGIN is still a secure context even when the proxy says http:
	// http://localhost is one by browser rule.
	if !secureContext("https", "http", "http://localhost:3000", "quasar.internal:8443") {
		t.Error("a loopback browser origin should remain a secure context")
	}
	// ...but an absent Origin with a proxy in play is no evidence at all, and
	// must not fall back to our backend Host.
	if secureContext("https", "http", "", "127.0.0.1:8443") {
		t.Fatal("an absent Origin behind a proxy fell back to the BACKEND Host")
	}
}

// TestAccessCheckSecureContextThroughAnHTTPSBackendProxy wires it end to end:
// the shape where Quasar is reached over TLS by a proxy that serves the browser
// in the clear.
func TestAccessCheckSecureContextThroughAnHTTPSBackendProxy(t *testing.T) {
	svc := newCheckService(t, resolverFor(nil))
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/access-check", nil)
	r.TLS = nil // handleAccessCheck reads r.TLS for the observed scheme
	r.Host = "quasar.internal:8443"
	r.Header.Set("Origin", "http://public.example")
	r.Header.Set("X-Forwarded-Proto", "http")
	rr := httptest.NewRecorder()
	svc.handleAccessCheck(rr, r)

	got := decodeCheck(t, rr).Request
	if got.SecureContext {
		t.Fatal("an insecure browser page was reported as a secure context; the operator would be told the " +
			"microphone should work and sent hunting a bug that is not there")
	}
	if !got.TLSTerminatedUpstream {
		t.Error("topology C detection should still fire — it is presentation only and stays")
	}
}

// TestNotYetValidCertificateAdviceComesFirst — a post-dated certificate is
// rejected on every handshake, so expiry, SAN and trust advice are all useless
// until it is fixed. Reachable because the on-disk compatibility path accepts
// such a pair rather than refusing to boot.
func TestNotYetValidCertificateAdviceComesFirst(t *testing.T) {
	future := issue(t, "quasar.test", false, nil, []string{"quasar.test"}, nil,
		time.Now().Add(48*time.Hour), time.Now().Add(90*24*time.Hour))
	chain, err := parseChain(future.certPEM)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	info := describe(SourceSelfSigned, chain)
	if !info.SelfSigned {
		t.Fatal("precondition: the fixture should be self-signed and covered, i.e. the branches that used to win")
	}
	svc := NewService(NewManagerFromPair(nil, info, testLogger()), resolverFor(nil), testLogger())

	r := directTLSRequest("/v1/admin/access-check")
	r.Host = "quasar.test:8443"
	rr := httptest.NewRecorder()
	svc.handleAccessCheck(rr, r)

	advice := decodeCheck(t, rr).Certificate.Advice
	if !strings.Contains(advice, "NOT VALID UNTIL") {
		t.Fatalf("advice = %q, want it to lead with the not-yet-valid window", advice)
	}
	if !strings.Contains(advice, "clock") {
		t.Errorf("advice = %q, want it to name the usual cause; a wrong clock looks nothing like a certificate problem", advice)
	}
	if strings.Contains(advice, "add it to your OS trust store") {
		t.Fatalf("a post-dated certificate was given download-and-trust advice: %q", advice)
	}
}
