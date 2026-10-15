package agentws

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/ratelimit"
)

// #674: the pre-auth in-flight cap is per IPv6 /64, not per address, so one
// subscriber cannot multiply it by rotating addresses inside its prefix.
func TestServeHTTPCapsPreAuthSocketsPerIPv6Prefix(t *testing.T) {
	h := &Handler{failures: ratelimit.NewFailureLimiter(enrollmentFailureLimit, time.Minute, 16)}
	for i := 0; i < enrollmentMaxInFlightIP; i++ {
		if !h.failures.Reserve("2001:db8:1:2::/64", enrollmentMaxInFlightIP, enrollmentMaxInFlight) {
			t.Fatalf("reserve %d refused early", i)
		}
	}
	r := httptest.NewRequest("GET", "/agent/ws", nil)
	r.RemoteAddr = "[2001:db8:1:2:dead:beef::1]:5000" // same /64, new address
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", w.Code)
	}
}
