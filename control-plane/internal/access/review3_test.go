// review3_test.go — properties added in response to Alice's ROUND 3 review of
// PR #479. One test per finding, as before.
package access

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- WARNING: SelfSigned misclassification -----------------------------------

// TestOrdinarySelfSignedServerLeafIsClassifiedSelfSigned is the finding: a
// self-signed SERVER certificate has IsCA=false, and CheckSignatureFrom enforces
// CA constraints on the purported parent, so it returned ConstraintViolationError
// and the certificate was reported as NOT self-signed. That is the most common
// certificate on a fresh install, and misclassifying it suppresses the
// download-and-trust guidance for exactly the operators who need it.
func TestOrdinarySelfSignedServerLeafIsClassifiedSelfSigned(t *testing.T) {
	leaf := selfSignedLeaf(t, "quasar.test")
	if leaf.cert.IsCA {
		t.Fatal("precondition: the fixture must be a non-CA server leaf")
	}
	// The old implementation's mistake, pinned so the regression is visible.
	if leaf.cert.CheckSignatureFrom(leaf.cert) == nil {
		t.Fatal("precondition: CheckSignatureFrom is expected to reject a non-CA self-issuer")
	}

	_, info, err := Validate(leaf.certPEM, leaf.keyPEM, SourceSelfSigned)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !info.SelfSigned {
		t.Fatal("an ordinary self-signed server certificate was reported as NOT self-signed")
	}
}

// TestCAIssuedLeafIsNotClassifiedSelfSigned guards the other direction, so the
// looser check cannot start calling everything self-signed.
func TestCAIssuedLeafIsNotClassifiedSelfSigned(t *testing.T) {
	ca := issue(t, "Test CA", true, nil, nil, nil, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	leaf := issue(t, "quasar.test", false, &ca, []string{"quasar.test"}, nil,
		time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	chain := append(append([]byte{}, leaf.certPEM...), ca.certPEM...)

	_, info, err := Validate(chain, leaf.keyPEM, SourceProvided)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if info.SelfSigned {
		t.Fatal("a CA-issued leaf was reported as self-signed")
	}
}

// TestSelfSignedGuidanceReachesTheOperator ties the classification to the visible
// consequence: the access panel must offer download-and-verify advice for a
// covered self-signed certificate.
func TestSelfSignedGuidanceReachesTheOperator(t *testing.T) {
	svc := newCheckService(t, resolverFor(nil))
	r := directTLSRequest("/v1/admin/access-check")
	r.Host = "quasar.test:8443" // covered by the fixture's SANs
	rr := httptest.NewRecorder()
	svc.handleAccessCheck(rr, r)

	got := decodeCheck(t, rr).Certificate
	if got.Info == nil || !got.Info.SelfSigned {
		t.Fatal("the served self-signed certificate was not reported as self-signed")
	}
	if !strings.Contains(got.Advice, "fingerprint") {
		t.Fatalf("advice = %q, want the download-and-verify guidance", got.Advice)
	}
}

// --- WARNING: SAN advice must match the certificate's source ------------------

// TestSANMismatchAdviceBranchesOnSource is the finding: one remedy for three
// sources sends operators of two supported topologies down steps that cannot
// work. QUASAR_TLS_HOSTS regenerates nothing for a mounted file; deleting the pem
// files does nothing for an uploaded certificate that is reloaded from the
// database at every restart.
func TestSANMismatchAdviceBranchesOnSource(t *testing.T) {
	for _, tc := range []struct {
		source      string
		mustContain []string
		mustNotSay  []string
	}{
		{
			source:      SourceSelfSigned,
			mustContain: []string{"QUASAR_TLS_HOSTS", "NEVER regenerated", "QUASAR_TLS_DIR"},
		},
		{
			source:      SourceProvided,
			mustContain: []string{"QUASAR_TLS_CERT", "does not apply"},
			// Telling a mounted-file operator to set QUASAR_TLS_HOSTS or delete
			// files in QUASAR_TLS_DIR is advice that cannot work.
			mustNotSay: []string{"Add that name to QUASAR_TLS_HOSTS", "delete cert.pem"},
		},
	} {
		t.Run(tc.source, func(t *testing.T) {
			advice := sanMismatchAdvice(tc.source, "host-a.local")
			if !strings.Contains(advice, "host-a.local") {
				t.Errorf("advice does not name the host: %q", advice)
			}
			for _, want := range tc.mustContain {
				if !strings.Contains(advice, want) {
					t.Errorf("advice for %s missing %q: %q", tc.source, want, advice)
				}
			}
			for _, bad := range tc.mustNotSay {
				if strings.Contains(advice, bad) {
					t.Errorf("advice for %s wrongly says %q (that step cannot fix their problem): %q",
						tc.source, bad, advice)
				}
			}
		})
	}
}
