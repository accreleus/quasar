package setup

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mustCIDR(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", cidr, err)
	}
	return n
}

// claimFrom drives one setup claim with an explicit peer address and forwarded
// chain, which is the only thing that varies across these tests.
func claimFrom(t *testing.T, svc *Service, peer, xff, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/setup/claim", strings.NewReader(goodBody))
	req.RemoteAddr = peer
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	if token != "" {
		req.Header.Set(TokenHeader, token)
	}
	rr := httptest.NewRecorder()
	svc.handleClaim(rr, req)
	return rr
}

// burnBudget spends the whole failure allowance for one forwarded client and
// asserts it ends locked out, so the follow-up assertion about a DIFFERENT
// client is meaningful.
func burnBudget(t *testing.T, svc *Service, peer, xff string) {
	t.Helper()
	for i := 0; i < claimFailureLimit; i++ {
		if rr := claimFrom(t, svc, peer, xff, "wrong-token"); rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, rr.Code)
		}
	}
	if rr := claimFrom(t, svc, peer, xff, "wrong-token"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d failures: status = %d, want 429", claimFailureLimit, rr.Code)
	}
}

// TestClaimBudgetsArePerForwardedClientBehindATrustedProxy is the #438
// headline: with the proxy's network trusted, one attacker burning the claim
// budget must not lock the legitimate operator out of first-run setup. Both
// arrive from the SAME peer address (the proxy container) and are told apart
// only by X-Forwarded-For.
func TestClaimBudgetsArePerForwardedClientBehindATrustedProxy(t *testing.T) {
	svc := NewService(&fakeClaimer{}, &fakeState{}, "the-real-token", quietLog()).
		WithTrustedProxies([]*net.IPNet{mustCIDR(t, "172.18.0.0/16")})

	burnBudget(t, svc, "172.18.0.5:40000", "203.0.113.9")

	// The operator, behind the same proxy, is untouched — and a correct token
	// still works, which is the whole point of the fix.
	if rr := claimFrom(t, svc, "172.18.0.5:40001", "198.51.100.4", "wrong-token"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("a second client behind the same proxy: status = %d, want 401 (its own budget)", rr.Code)
	}
	if rr := claimFrom(t, svc, "172.18.0.5:40002", "198.51.100.4", "the-real-token"); rr.Code != http.StatusCreated {
		t.Fatalf("legitimate claim: status = %d, want 201", rr.Code)
	}
}

// TestClaimIgnoresForwardedHeaderWithoutConfiguration pins the default: with no
// QUASAR_TRUSTED_PROXIES, the header is not an input at all. Rotating it must
// NOT mint a fresh budget — that would be the opposite defect, an unbounded
// bypass of the limiter.
func TestClaimIgnoresForwardedHeaderWithoutConfiguration(t *testing.T) {
	svc := NewService(&fakeClaimer{}, &fakeState{}, "the-real-token", quietLog())

	burnBudget(t, svc, "172.18.0.5:40000", "203.0.113.9")

	if rr := claimFrom(t, svc, "172.18.0.5:40001", "198.51.100.4", "wrong-token"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("rotating X-Forwarded-For minted a fresh budget: status = %d, want 429", rr.Code)
	}
}

// TestClaimIgnoresForwardedHeaderFromAnUntrustedPeer covers the case where
// proxies ARE configured but the request did not come through one — a direct
// hit on an exposed port. The header is attacker-supplied there and must not
// key anything.
func TestClaimIgnoresForwardedHeaderFromAnUntrustedPeer(t *testing.T) {
	svc := NewService(&fakeClaimer{}, &fakeState{}, "the-real-token", quietLog()).
		WithTrustedProxies([]*net.IPNet{mustCIDR(t, "172.18.0.0/16")})

	burnBudget(t, svc, "192.0.2.10:40000", "203.0.113.9")

	if rr := claimFrom(t, svc, "192.0.2.10:40001", "198.51.100.4", "wrong-token"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("an untrusted peer rotated its own key: status = %d, want 429", rr.Code)
	}
}
