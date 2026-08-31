// review_test.go — the properties added in response to Alice's review of PR #479.
// Kept in their own file so each one is traceable to the finding it closes.
package access

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- WARNING: leaf must be usable as a SERVER certificate --------------------

func TestValidateRejectsCALeaf(t *testing.T) {
	ca := issue(t, "Test CA", true, nil, []string{"ca.test"}, nil,
		time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	_, _, err := Validate(ca.certPEM, ca.keyPEM, SourceProvided)
	if err == nil {
		t.Fatal("a CA certificate was accepted as a server certificate and would have been hot-installed")
	}
	if !strings.Contains(err.Error(), "CA certificate") {
		t.Fatalf("message = %q, want it to name what was uploaded", err)
	}
}

func TestValidateRejectsClientAuthOnlyLeaf(t *testing.T) {
	leaf := issueWithEKU(t, "client.test", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	_, _, err := Validate(leaf.certPEM, leaf.keyPEM, SourceProvided)
	if err == nil {
		t.Fatal("a clientAuth-only certificate was accepted — every browser would refuse it")
	}
	if !strings.Contains(err.Error(), "Extended Key Usage") {
		t.Fatalf("message = %q, want it to name the EKU", err)
	}
}

// TestValidateAcceptsUnrestrictedAndServerAuthLeaves guards the other direction:
// "no EKU" means unrestricted and must NOT be mistaken for "wrong EKU". The
// batteries-included self-signed pair and plenty of real certificates rely on it.
func TestValidateAcceptsUnrestrictedAndServerAuthLeaves(t *testing.T) {
	for _, tc := range []struct {
		name string
		ekus []x509.ExtKeyUsage
	}{
		{"unrestricted", nil},
		{"serverAuth", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}},
		{"serverAuth + clientAuth", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}},
		{"any", []x509.ExtKeyUsage{x509.ExtKeyUsageAny}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaf := issueWithEKU(t, "ok.test", tc.ekus)
			if _, _, err := Validate(leaf.certPEM, leaf.keyPEM, SourceProvided); err != nil {
				t.Fatalf("rejected a usable server certificate: %v", err)
			}
		})
	}
}

// --- WARNING: strict PEM + canonical persisted bundle ------------------------

// TestParseChainRejectsBytesOutsideBlocks is the finding directly: pem.Decode
// silently skips unrecognised bytes, so a lenient parse lets arbitrary material
// ride along in a body that used to be persisted verbatim.
func TestParseChainRejectsBytesOutsideBlocks(t *testing.T) {
	leaf := selfSignedLeaf(t, "quasar.test")
	key := string(leaf.keyPEM)
	for _, tc := range []struct{ name, body string }{
		{"leading junk", "hello\n" + string(leaf.certPEM)},
		{"trailing junk", string(leaf.certPEM) + "\ntrailing garbage\n"},
		{"key smuggled after a certificate without a BEGIN of its own", string(leaf.certPEM) + "\n" + strings.TrimPrefix(key, "-----BEGIN PRIVATE KEY-----")},
		{"commented bundle", "# my cert\n" + string(leaf.certPEM)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseChain([]byte(tc.body)); err == nil {
				t.Fatal("non-certificate bytes were silently discarded instead of refused")
			}
		})
	}
	// Whitespace between blocks stays legal — a real fullchain.pem has it.
	ca := issue(t, "Test CA", true, nil, nil, nil, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	l2 := issue(t, "quasar.test", false, &ca, []string{"quasar.test"}, nil, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	bundle := string(l2.certPEM) + "\n\n  \n" + string(ca.certPEM) + "\n"
	if _, err := parseChain([]byte(bundle)); err != nil {
		t.Fatalf("a normal fullchain with blank lines was rejected: %v", err)
	}
}

// --- ARCHITECTURE: one resolver, so panel and socket cannot disagree ---------

// TestAccessCheckAgreesWithTheEnforcerOnDefaultPorts is the regression that
// motivated the shared resolver. The operator's real deployment is Caddy on 443,
// so `https://example.com:443` in the allow-list against a browser sending
// `Origin: https://example.com` is the LIKELY path, and it used to fail silently.
func TestAccessCheckAgreesWithTheEnforcerOnDefaultPorts(t *testing.T) {
	resolver := resolverFor([]string{"https://quasar.example.com:443"})
	svc := newCheckService(t, resolver)

	r := httptest.NewRequest(http.MethodGet, "/v1/admin/access-check", nil)
	r.Host = "quasar-internal.lan"                       // proxy rewrote Host: no same-origin exemption
	r.Header.Set("Origin", "https://quasar.example.com") // browser omits the default port
	r.Header.Set("X-Forwarded-Proto", "https")
	rr := httptest.NewRecorder()
	svc.handleAccessCheck(rr, r)

	got := decodeCheck(t, rr).Origins
	if got.RequestOriginAllowed == nil || !*got.RequestOriginAllowed {
		t.Fatal("an allow-list entry written with the explicit :443 did not match the browser's port-less Origin — " +
			"this is the silent failure in the exact topology the feature exists to support")
	}
	if got.SameOriginExemption {
		t.Error("this passed on the allow-list, not on same-origin")
	}

	// The ENFORCER, asked directly, must say the same thing. Same resolver
	// instance, so this asserts they are wired to one evaluation rather than two.
	d := resolver.Decide(context.Background(), "https://quasar.example.com", "quasar-internal.lan")
	if !d.Allowed || !d.Listed {
		t.Fatal("the resolver the socket enforces with disagrees with the panel")
	}
}

// TestSameOriginAgreesAcrossDefaultPortForms — Host may or may not carry :443
// depending on the proxy; both must match a port-less https Origin, and a
// genuinely different port must still not.
func TestSameOriginAgreesAcrossDefaultPortForms(t *testing.T) {
	r := resolverFor(nil)
	for _, tc := range []struct {
		origin, host string
		want         bool
	}{
		{"https://example.com", "example.com", true},
		{"https://example.com", "example.com:443", true},
		{"https://example.com:443", "example.com", true},
		{"http://example.com", "example.com:80", true},
		{"https://example.com", "example.com:8443", false},
		{"https://example.com:9999", "example.com:8443", false},
		{"http://example.com", "example.com:443", false},
	} {
		got := r.Decide(context.Background(), tc.origin, tc.host)
		if got.SameOrigin != tc.want {
			t.Errorf("Decide(%q, %q).SameOrigin = %v, want %v", tc.origin, tc.host, got.SameOrigin, tc.want)
		}
	}
}
