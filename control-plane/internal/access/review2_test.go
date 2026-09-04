// review2_test.go — the properties added in response to Alice's ROUND 2 review
// of PR #479. One test per finding, named to it, as in review_test.go.
package access

import (
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- WARNING: chain-wide validity --------------------------------------------

// TestValidateRejectsExpiredIntermediate — a valid leaf with an expired
// intermediate installs cleanly and then breaks every client that builds a path
// through it, which looks nothing like a certificate problem from the operator's
// side.
func TestValidateRejectsExpiredIntermediate(t *testing.T) {
	expiredCA := issue(t, "Stale CA", true, nil, nil, nil,
		time.Now().Add(-72*time.Hour), time.Now().Add(-time.Hour))
	leaf := issue(t, "quasar.test", false, &expiredCA, []string{"quasar.test"}, nil,
		time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
	chain := append(append([]byte{}, leaf.certPEM...), expiredCA.certPEM...)

	_, _, err := Validate(chain, leaf.keyPEM, SourceProvided)
	if err == nil {
		t.Fatal("a chain containing an expired intermediate was accepted")
	}
	if !strings.Contains(err.Error(), "intermediate certificate 1") {
		t.Fatalf("message = %q, want it to identify WHICH certificate is expired", err)
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("message = %q, want it to say expired", err)
	}
	// The leaf alone is fine — proving the rejection came from the chain walk.
	if _, _, err := Validate(leaf.certPEM, leaf.keyPEM, SourceProvided); err != nil {
		t.Fatalf("the leaf alone should validate: %v", err)
	}
}

// --- WARNING: KeyUsage --------------------------------------------------------

func TestValidateRejectsLeafWithoutDigitalSignatureKeyUsage(t *testing.T) {
	leaf := issueWithKeyUsage(t, "nosig.test", x509.KeyUsageKeyEncipherment)
	_, _, err := Validate(leaf.certPEM, leaf.keyPEM, SourceProvided)
	if err == nil {
		t.Fatal("a certificate whose Key Usage forbids digital signatures was accepted")
	}
	if !strings.Contains(err.Error(), "Key Usage") {
		t.Fatalf("message = %q, want it to name Key Usage", err)
	}
}

// TestValidateAcceptsAbsentKeyUsage — absent extension means unrestricted and
// must not be mistaken for "forbidden". Guarding the permissive direction is
// what stops this check refusing valid certificates.
func TestValidateAcceptsAbsentKeyUsage(t *testing.T) {
	leaf := issueWithKeyUsage(t, "noku.test", 0)
	if _, _, err := Validate(leaf.certPEM, leaf.keyPEM, SourceProvided); err != nil {
		t.Fatalf("a certificate with no Key Usage extension was rejected: %v", err)
	}
}

// --- WARNING (high): the upgrade regression ----------------------------------

// TestNewManagerAcceptsAnExistingLooseOnDiskPair is the upgrade path. Real
// cert.pem files carry OpenSSL bag attributes, comments or a text dump above the
// PEM block; ServeTLS accepted them, so they are in production today. Applying
// the upload-path strictness to them would turn an upgrade into an outage on a
// deployment that changed nothing.
func TestNewManagerAcceptsAnExistingLooseOnDiskPair(t *testing.T) {
	leaf := selfSignedLeaf(t, "legacy.test")
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	// The shape OpenSSL emits and that our strict UPLOAD parser refuses.
	loose := "Bag Attributes\n    friendlyName: legacy\nsubject=CN = legacy.test\nissuer=CN = legacy.test\n" +
		string(leaf.certPEM) + "\n# trailing note\n"
	if _, err := parseChain([]byte(loose)); err == nil {
		t.Fatal("precondition failed: this fixture should be refused by the strict upload parser")
	}
	if err := os.WriteFile(certPath, []byte(loose), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, leaf.keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	mgr, err := NewManager(certPath, keyPath, SourceSelfSigned, testLogger())
	if err != nil {
		t.Fatalf("an on-disk pair that crypto/tls accepts must still boot: %v", err)
	}
	got, err := mgr.GetCertificate(nil)
	if err != nil || got == nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	info := mgr.Current()
	if want := SPKIPin(leaf.cert.RawSubjectPublicKeyInfo); info.SPKISHA256 != want {
		t.Errorf("spki_sha256 = %q, want the leaf's %q", info.SPKISHA256, want)
	}
	if info.FingerprintSHA256 != Fingerprint(leaf.cert.Raw) {
		t.Errorf("fingerprint = %q, want the on-disk leaf's %q", info.FingerprintSHA256, Fingerprint(leaf.cert.Raw))
	}
	if len(info.DNSNames) != 1 || info.DNSNames[0] != "legacy.test" {
		t.Errorf("dns_names = %v, want the metadata to survive the fallback path", info.DNSNames)
	}
	// And the download still works off that fallback metadata, keys excluded.
	svc := NewService(mgr, nil, testLogger())
	rr := httptest.NewRecorder()
	svc.handleCertificateDownload(rr, httptest.NewRequest(http.MethodGet, "/v1/tls/certificate.pem", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("download status = %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "PRIVATE KEY") {
		t.Fatal("PRIVATE KEY block in the download built from the fallback path")
	}
	if strings.Contains(rr.Body.String(), "Bag Attributes") {
		t.Error("the download echoed the file's non-PEM text; it must be re-encoded from the parsed DER")
	}
}

// TestNewManagerStillFailsOnAGenuinelyBrokenPair — the fallback must not become
// "accept anything".
func TestNewManagerStillFailsOnAGenuinelyBrokenPair(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := NewManager(certPath, keyPath, SourceSelfSigned, testLogger()); err == nil {
		t.Fatal("a pair crypto/tls cannot load must still be fatal")
	}
}

// --- helper -------------------------------------------------------------------

// issueWithKeyUsage mints a self-signed serverAuth leaf with exactly the given
// KeyUsage bits (0 = the extension is absent).
func issueWithKeyUsage(t *testing.T, cn string, ku x509.KeyUsage) issued {
	t.Helper()
	return issueCustom(t, cn, func(tmpl *x509.Certificate) {
		tmpl.KeyUsage = ku
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	})
}
