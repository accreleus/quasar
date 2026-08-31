package access

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/origins"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// directTLSRequest builds a GET whose r.TLS is non-nil, simulating a request
// that arrived on THIS control plane's own TLS listener directly — as opposed
// to over plaintext HTTP (item 3: certificateSection must not report
// certificate.in_use=true for a request the listener's own TLS never touched)
// or via an upstream-terminating proxy signalled by X-Forwarded-Proto (§S6-0
// topology C, unaffected by this helper).
func directTLSRequest(target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.TLS = &tls.ConnectionState{}
	return r
}

// issued is a generated certificate plus the key that signed it.
type issued struct {
	certPEM []byte
	keyPEM  []byte
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
}

// issue mints a certificate. parent == nil makes it self-signed.
func issue(t *testing.T, cn string, isCA bool, parent *issued, dns []string, ips []net.IP, notBefore, notAfter time.Time) issued {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  isCA,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	signer := &tmpl
	signerKey := key
	if parent != nil {
		signer = parent.cert
		signerKey = parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return issued{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		cert:    parsed,
		key:     key,
	}
}

// issueWithEKU mints a self-signed leaf carrying exactly the given EKUs (nil =
// unrestricted), for the server-usability checks.
func issueWithEKU(t *testing.T, cn string, ekus []x509.ExtKeyUsage) issued {
	t.Helper()
	return issueCustom(t, cn, func(tmpl *x509.Certificate) { tmpl.ExtKeyUsage = ekus })
}

// issueCustom mints a self-signed leaf with SANs and a sane baseline, letting
// the caller adjust the template. One factory so a new field under test does not
// mean another near-copy of the x509 boilerplate.
func issueCustom(t *testing.T, cn string, mutate func(*x509.Certificate)) issued {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		DNSNames:              []string{cn},
	}
	if mutate != nil {
		mutate(&tmpl)
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return issued{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		cert:    parsed,
		key:     key,
	}
}

func selfSignedLeaf(t *testing.T, names ...string) issued {
	t.Helper()
	if len(names) == 0 {
		names = []string{"quasar.test"}
	}
	return issue(t, names[0], false, nil, names, []net.IP{net.ParseIP("192.0.2.10")},
		time.Now().Add(-time.Hour), time.Now().Add(365*24*time.Hour))
}

// --- §S6a: the one failure that would be catastrophic -------------------------

// TestCertificateDownloadNeverLeaksKey is the test §S6a demands by name: the
// public download route must not be able to emit private key material.
//
// It asserts the property from BOTH ends. The response is checked for any PEM
// block that is not a CERTIFICATE and for the literal "PRIVATE KEY" — but that
// alone would only prove this one input is safe, so it ALSO checks that the
// generated key's own body does not appear, and that what comes back decodes as
// exactly one certificate equal to the leaf. A handler that concatenated the key
// file, echoed the request, or served the wrong path fails all three.
func TestCertificateDownloadNeverLeaksKey(t *testing.T) {
	leaf := selfSignedLeaf(t, "quasar.test")
	pair, info, err := Validate(leaf.certPEM, leaf.keyPEM, SourceSelfSigned)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	svc := NewService(NewManagerFromPair(pair, info, testLogger()), nil, testLogger())

	rr := httptest.NewRecorder()
	svc.handleCertificateDownload(rr, httptest.NewRequest(http.MethodGet, "/v1/tls/certificate.pem", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.Bytes()

	if strings.Contains(string(body), "PRIVATE KEY") {
		t.Fatal("the certificate download contains a PRIVATE KEY block — this is the catastrophic failure §S6a exists to prevent")
	}
	// The key's own base64 body, independent of the header text.
	keyBlock, _ := pem.Decode(leaf.keyPEM)
	if keyBlock == nil {
		t.Fatal("test key did not decode")
	}
	if strings.Contains(string(body), strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBlock.Bytes})))) {
		t.Fatal("the certificate download contains the private key body")
	}

	// Exactly one CERTIFICATE block, and it is the leaf.
	block, rest := pem.Decode(body)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("response is not a single CERTIFICATE block: %q", string(body))
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		t.Fatalf("response carries trailing PEM data: %q", string(rest))
	}
	if string(block.Bytes) != string(leaf.cert.Raw) {
		t.Fatal("the served certificate is not the leaf in force")
	}
	if got := rr.Header().Get("X-Quasar-Certificate-Fingerprint"); got != Fingerprint(leaf.cert.Raw) {
		t.Fatalf("fingerprint header = %q, want %q", got, Fingerprint(leaf.cert.Raw))
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Error("the certificate must not be cacheable — it can be replaced by an upload at any moment")
	}
}

// TestCertificateDownloadServesTheActiveLeaf pins that the download reports the
// certificate the LISTENER is serving, read through the same Manager the TLS
// stack calls — not a file re-read that could drift from it.
func TestCertificateDownloadServesTheActiveLeaf(t *testing.T) {
	leaf := selfSignedLeaf(t, "active.test")
	pair, info, err := Validate(leaf.certPEM, leaf.keyPEM, SourceSelfSigned)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	mgr := NewManagerFromPair(pair, info, testLogger())

	served, err := mgr.GetCertificate(nil)
	if err != nil || served.Leaf.Subject.CommonName != "active.test" {
		t.Fatalf("GetCertificate: %v / %v", err, served)
	}
	rr := httptest.NewRecorder()
	NewService(mgr, nil, testLogger()).
		handleCertificateDownload(rr, httptest.NewRequest(http.MethodGet, "/v1/tls/certificate.pem", nil))
	block, _ := pem.Decode(rr.Body.Bytes())
	if block == nil || string(block.Bytes) != string(served.Leaf.Raw) {
		t.Fatal("the download is not the certificate the TLS stack serves")
	}
	if strings.Contains(rr.Body.String(), "PRIVATE KEY") {
		t.Fatal("PRIVATE KEY block in the download")
	}
}

// --- validation ----------------------------------------------------------------

func TestValidateRejectsMismatchedKey(t *testing.T) {
	leaf := selfSignedLeaf(t, "quasar.test")
	other := selfSignedLeaf(t, "other.test")
	_, _, err := Validate(leaf.certPEM, other.keyPEM, SourceProvided)
	if err == nil {
		t.Fatal("a key from a different certificate was accepted — tls.X509KeyPair is the proof the pair matches and it did not run")
	}
	if !strings.Contains(err.Error(), "does not match the certificate") {
		t.Fatalf("message = %q, want the key-mismatch sentence", err)
	}
}

// TestValidateRejectsReversedChainWithItsOwnMessage is the §S6d requirement that
// a wrongly-ordered chain gets a message saying so. The regression it guards is
// specific: X509KeyPair also fails on a reversed chain, with "key does not
// match", which sends an operator hunting the wrong file. If the order of checks
// is ever swapped, this test fails on the MESSAGE, not on acceptance.
func TestValidateRejectsReversedChainWithItsOwnMessage(t *testing.T) {
	ca := issue(t, "Test CA", true, nil, nil, nil, time.Now().Add(-time.Hour), time.Now().Add(365*24*time.Hour))
	leaf := issue(t, "quasar.test", false, &ca, []string{"quasar.test"}, nil,
		time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))

	correct := append(append([]byte{}, leaf.certPEM...), ca.certPEM...)
	if _, _, err := Validate(correct, leaf.keyPEM, SourceProvided); err != nil {
		t.Fatalf("a correctly ordered leaf+CA chain was rejected: %v", err)
	}

	reversed := append(append([]byte{}, ca.certPEM...), leaf.certPEM...)
	_, _, err := Validate(reversed, leaf.keyPEM, SourceProvided)
	if err == nil {
		t.Fatal("a reversed chain was accepted")
	}
	if !strings.Contains(err.Error(), "wrong order") {
		t.Fatalf("message = %q, want it to name the wrong ORDER — X509KeyPair's own error would misdiagnose this as a key mismatch", err)
	}
}

func TestValidateRejectsPrivateKeyInCertificateField(t *testing.T) {
	leaf := selfSignedLeaf(t, "quasar.test")
	bundled := append(append([]byte{}, leaf.certPEM...), leaf.keyPEM...)
	_, _, err := Validate(bundled, leaf.keyPEM, SourceProvided)
	if err == nil {
		t.Fatal("a private key pasted into certificate_pem was accepted")
	}
	if !strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatalf("message = %q, want it to name the block by type rather than silently skipping it", err)
	}
}

func TestValidateRejectsExpiredAndSANlessCertificates(t *testing.T) {
	expired := issue(t, "old.test", false, nil, []string{"old.test"}, nil,
		time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
	if _, _, err := Validate(expired.certPEM, expired.keyPEM, SourceProvided); err == nil ||
		!strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired certificate: err = %v, want an expiry-specific refusal", err)
	}

	noSAN := issue(t, "cn-only.test", false, nil, nil, nil,
		time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	if _, _, err := Validate(noSAN.certPEM, noSAN.keyPEM, SourceProvided); err == nil ||
		!strings.Contains(err.Error(), "Subject Alternative Names") {
		t.Fatalf("SAN-less certificate: err = %v, want a SAN-specific refusal", err)
	}
}

func TestValidateRejectsSwappedFields(t *testing.T) {
	leaf := selfSignedLeaf(t, "quasar.test")
	_, _, err := Validate(leaf.certPEM, leaf.certPEM, SourceProvided)
	if err == nil || !strings.Contains(err.Error(), "other way round") {
		t.Fatalf("err = %v, want the fields-swapped sentence", err)
	}
}

func TestCoversHost(t *testing.T) {
	leaf := selfSignedLeaf(t, "quasar.test")
	_, info, err := Validate(leaf.certPEM, leaf.keyPEM, SourceSelfSigned)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"quasar.test", true},
		{"quasar.test:8443", true},
		{"QUASAR.TEST:8443", true},
		{"192.0.2.10:8443", true},
		{"host-a.local:8443", false},
		{"", false},
	} {
		if got := info.CoversHost(tc.host); got != tc.want {
			t.Errorf("CoversHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// --- §S6b / §S6-0: access check ----------------------------------------------

type fakeSettings struct {
	list []string
	err  error
}

func (f fakeSettings) AllowedOrigins(context.Context) ([]string, error) { return f.list, f.err }

// resolverFor builds the shared resolver the way production does, so these
// tests exercise the same object internal/signal enforces with.
func resolverFor(list []string) *origins.Resolver {
	return origins.NewResolver("", false, fakeSettings{list: list}, testLogger())
}

func decodeCheck(t *testing.T, rr *httptest.ResponseRecorder) accessCheckResponse {
	t.Helper()
	var out accessCheckResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode access-check: %v (%s)", err, rr.Body.String())
	}
	return out
}

func newCheckService(t *testing.T, resolver *origins.Resolver) *Service {
	t.Helper()
	leaf := selfSignedLeaf(t, "quasar.test")
	pair, info, err := Validate(leaf.certPEM, leaf.keyPEM, SourceSelfSigned)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return NewService(NewManagerFromPair(pair, info, testLogger()), resolver, testLogger())
}

// TestAccessCheckTopologyCReportsCertificateNotInUse is the §S6-0 requirement.
// A fleet host runs exactly this shape (Caddy terminating TLS),
// and the panel telling them their internal self-signed certificate is broken
// is the failure mode this endpoint exists to prevent.
func TestAccessCheckTopologyCReportsCertificateNotInUse(t *testing.T) {
	svc := newCheckService(t, resolverFor(nil))
	for _, tc := range []struct{ name, header, value string }{
		{"x-forwarded-proto", "X-Forwarded-Proto", "https"},
		{"x-forwarded-proto chain", "X-Forwarded-Proto", "https, http"},
		{"rfc 7239 forwarded", "Forwarded", `for=192.0.2.1;proto=https;by=203.0.113.1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A hostname the certificate does NOT cover: without topology
			// detection this is exactly the request that would produce a false
			// "your certificate is wrong".
			r := httptest.NewRequest(http.MethodGet, "/v1/admin/access-check", nil)
			r.Host = "quasar.example.com"
			r.Header.Set(tc.header, tc.value)
			rr := httptest.NewRecorder()
			svc.handleAccessCheck(rr, r)

			got := decodeCheck(t, rr)
			if !got.Request.TLSTerminatedUpstream {
				t.Fatal("topology C not detected")
			}
			if got.Certificate.InUse {
				t.Fatal("certificate reported IN USE behind an upstream terminator — the browser never sees it")
			}
			if got.Certificate.HostCovered != nil {
				t.Error("host_covered must be absent when the certificate is not in use; reporting it invites a UI to complain about it")
			}
			if got.Certificate.Advice != "" {
				t.Errorf("advice = %q, want none: there is nothing for the operator to fix", got.Certificate.Advice)
			}
			if !strings.Contains(got.Certificate.NotInUseReason, "supported") {
				t.Errorf("reason = %q, want it to say the setup is supported", got.Certificate.NotInUseReason)
			}
			if !got.Request.SecureContext {
				t.Error("secure_context must be true when the browser spoke https to the proxy")
			}
		})
	}
}

// TestAccessCheckTopologyAReportsSANGap is the mirror: with no proxy in front,
// an uncovered host MUST be named, along with the fact that the self-signed
// certificate never regenerates on its own.
func TestAccessCheckTopologyAReportsSANGap(t *testing.T) {
	svc := newCheckService(t, resolverFor(nil))
	r := directTLSRequest("/v1/admin/access-check")
	r.Host = "host-a.local:8443"
	rr := httptest.NewRecorder()
	svc.handleAccessCheck(rr, r)

	got := decodeCheck(t, rr)
	if got.Request.TLSTerminatedUpstream {
		t.Fatal("topology C detected with no forwarded header")
	}
	if !got.Certificate.InUse {
		t.Fatal("certificate should be in use")
	}
	if got.Certificate.HostCovered == nil || *got.Certificate.HostCovered {
		t.Fatal("host_covered should be false for a name outside the SANs")
	}
	if !strings.Contains(got.Certificate.Advice, "QUASAR_TLS_HOSTS") {
		t.Errorf("advice = %q, want the exact variable to set", got.Certificate.Advice)
	}
	if !strings.Contains(got.Certificate.Advice, "NEVER regenerated") {
		t.Errorf("advice = %q, want it to state that the certificate does not regenerate on its own — "+
			"setting the variable alone does not fix it", got.Certificate.Advice)
	}
}

// TestAccessCheckForwardedHeaderAuthorisesNothing pins the §S6-0 rule that a
// forwarded header may only soften advice. A spoofed header must not change any
// origin verdict.
func TestAccessCheckForwardedHeaderAuthorisesNothing(t *testing.T) {
	svc := newCheckService(t, resolverFor([]string{"https://listed.example"}))

	verdict := func(spoof bool) accessCheckOrigins {
		r := httptest.NewRequest(http.MethodGet, "/v1/admin/access-check", nil)
		r.Host = "internal.local"
		r.Header.Set("Origin", "https://evil.example")
		if spoof {
			r.Header.Set("X-Forwarded-Proto", "https")
		}
		rr := httptest.NewRecorder()
		svc.handleAccessCheck(rr, r)
		return decodeCheck(t, rr).Origins
	}
	plain, spoofed := verdict(false), verdict(true)
	if plain.RequestOriginAllowed == nil || *plain.RequestOriginAllowed {
		t.Fatal("an unlisted, cross-host origin must not be allowed")
	}
	if spoofed.RequestOriginAllowed == nil || *spoofed.RequestOriginAllowed {
		t.Fatal("a forwarded header changed an origin verdict — forwarded headers must authorise NOTHING")
	}
}

func TestAccessCheckLengthCapsReflectedHeaders(t *testing.T) {
	svc := newCheckService(t, resolverFor(nil))
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/access-check", nil)
	r.Host = strings.Repeat("a", 5000) + ".test"
	r.Header.Set("Origin", "https://"+strings.Repeat("b", 5000))
	rr := httptest.NewRecorder()
	svc.handleAccessCheck(rr, r)

	got := decodeCheck(t, rr)
	if len(got.Request.Host) > maxReflectedLength+32 {
		t.Errorf("host reflected at %d chars — §S6b requires a cap", len(got.Request.Host))
	}
	if len(got.Request.Origin) > maxReflectedLength+32 {
		t.Errorf("origin reflected at %d chars — §S6b requires a cap", len(got.Request.Origin))
	}
}

// TestAccessCheckSameOriginExemptionIsNamed covers the §S6e correction: a
// same-origin request passes with NO configuration, so the panel must say the
// exemption is what carried it rather than implying the list is doing work.
func TestAccessCheckSameOriginExemptionIsNamed(t *testing.T) {
	svc := newCheckService(t, resolverFor(nil))
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/access-check", nil)
	r.Host = "quasar.test:8443"
	r.Header.Set("Origin", "https://quasar.test:8443")
	rr := httptest.NewRecorder()
	svc.handleAccessCheck(rr, r)

	got := decodeCheck(t, rr).Origins
	if got.RequestOriginAllowed == nil || !*got.RequestOriginAllowed {
		t.Fatal("a same-origin request must pass with no configuration")
	}
	if !got.SameOriginExemption {
		t.Fatal("same_origin_exemption must be reported — it is what silently disappears behind a Host-rewriting proxy")
	}
	if !strings.Contains(got.Advice, "rewrites Host") {
		t.Errorf("advice = %q, want it to name the proxy case", got.Advice)
	}
}

// TestAccessCheckSuggestsTheCurrentOriginUnlessPinned covers "detection plus the
// fix" — and the fact that suggesting a UI edit is wrong when the environment
// has pinned the list, because the edit would do nothing.
func TestAccessCheckSuggestsTheCurrentOriginUnlessPinned(t *testing.T) {
	dbSvc := newCheckService(t, resolverFor(nil))
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/access-check", nil)
	r.Host = "internal.local"
	r.Header.Set("Origin", "https://quasar.example.com")
	rr := httptest.NewRecorder()
	dbSvc.handleAccessCheck(rr, r)
	got := decodeCheck(t, rr).Origins
	if got.Source != origins.SourceDatabase {
		t.Errorf("source = %q, want database", got.Source)
	}
	if got.SuggestedEntry != "https://quasar.example.com" {
		t.Errorf("suggested_entry = %q, want the current origin ready to add", got.SuggestedEntry)
	}

	pinned := NewService(dbSvc.manager,
		origins.NewResolver("https://other.example", true, nil, testLogger()), testLogger())
	rr2 := httptest.NewRecorder()
	pinned.handleAccessCheck(rr2, r)
	got2 := decodeCheck(t, rr2).Origins
	if got2.Source != origins.SourceEnvironment {
		t.Errorf("source = %q, want environment", got2.Source)
	}
	if got2.SuggestedEntry != "" {
		t.Errorf("suggested_entry = %q, want empty: adding it in the UI would do nothing while the env pins the list", got2.SuggestedEntry)
	}
	if !strings.Contains(got2.Advice, "QUASAR_ALLOWED_ORIGINS") {
		t.Errorf("advice = %q, want it to name the variable that is actually in charge", got2.Advice)
	}
}

func TestAccessCheckNoOriginHeaderIsNotAFailure(t *testing.T) {
	svc := newCheckService(t, resolverFor(nil))
	rr := httptest.NewRecorder()
	svc.handleAccessCheck(rr, httptest.NewRequest(http.MethodGet, "/v1/admin/access-check", nil))
	got := decodeCheck(t, rr).Origins
	if got.RequestOriginAllowed != nil {
		t.Errorf("request_origin_allowed = %v, want null when no Origin was sent", *got.RequestOriginAllowed)
	}
}
