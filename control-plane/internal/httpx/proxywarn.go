package httpx

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/ratelimit"
)

// WarnUnlistedProxy logs one WARN per peer per interval when a request carries
// X-Forwarded-* from a direct peer outside QUASAR_TRUSTED_PROXIES. Warn-only:
// the request is served as before. Put it on the plain HTTP listener, outside
// any redirect.
func WarnUnlistedProxy(next http.Handler, log *slog.Logger, trusted []*net.IPNet) http.Handler {
	seen := ratelimit.NewFailureLimiter(1, 10*time.Minute, 4096)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hasForwardedHeader(r) {
			if ip, listed := ratelimit.DirectPeer(r, trusted); !listed {
				// Limiter key is the /64 for IPv6; the log names the peer itself, since
				// operators copy it into QUASAR_TRUSTED_PROXIES.
				key := ratelimit.PrefixKey(r, nil)
				peer := key
				if ip != nil {
					peer = ip.String()
				}
				if seen.Allow(key) {
					seen.Failure(key)
					log.Warn("forwarded-from-unlisted-peer", "peer", peer,
						"note", "this peer sent X-Forwarded-* to the plain HTTP port but is not in QUASAR_TRUSTED_PROXIES; a later release refuses credential routes from it: if this is your reverse proxy, name its address in QUASAR_TRUSTED_PROXIES")
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Any header with the X-Forwarded- prefix counts, known suffix or not.
func hasForwardedHeader(r *http.Request) bool {
	for name := range r.Header {
		if strings.HasPrefix(name, "X-Forwarded-") {
			return true
		}
	}
	return false
}
