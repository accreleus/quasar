package httpx

import (
	"net/http"
	"strconv"
)

// HSTS sends Strict-Transport-Security: max-age=<maxAge> on connections this
// process terminated itself (r.TLS set, i.e. the HTTPS listener). A request
// proxied to the plain listener carries none, and QUASAR_TLS=off has no HTTPS
// listener. maxAge <= 0 sends nothing. It pins the whole host name on every
// port, so it is opt-in and never adds includeSubDomains or preload.
func HSTS(next http.Handler, maxAge int32) http.Handler {
	if maxAge <= 0 {
		return next
	}
	value := "max-age=" + strconv.Itoa(int(maxAge))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", value)
		}
		next.ServeHTTP(w, r)
	})
}
