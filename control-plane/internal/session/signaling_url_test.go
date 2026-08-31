package session

import (
	"crypto/tls"
	"net/http"
	"testing"
)

func TestSignalingURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tls     bool
		xfproto string
		want    string
	}{
		{"plain http", false, "", "ws://play.lan:8080/v1/signal"},
		{"native tls", true, "", "wss://play.lan:8080/v1/signal"},
		{"behind https proxy", false, "https", "wss://play.lan:8080/v1/signal"},
		{"behind https proxy mixed case", false, "HTTPS", "wss://play.lan:8080/v1/signal"},
		{"proxy http stays ws", false, "http", "ws://play.lan:8080/v1/signal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodPost, "http://play.lan:8080/v1/sessions", nil)
			if err != nil {
				t.Fatal(err)
			}
			r.Host = "play.lan:8080"
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if tc.xfproto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.xfproto)
			}
			if got := (&Handler{}).signalingURL(r); got != tc.want {
				t.Fatalf("signalingURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSignalingURLBehindHostRewritingProxy is the regression for the defect this
// resolution exists for: a reverse proxy that rewrites Host to its upstream
// address made us hand the browser wss://<private-listener>/v1/signal. The client
// left the proxy, dialled the private listener directly, and — self-signed cert,
// or simply not reachable from where the client was — never connected, so the
// page loaded and signaling silently never started.
func TestSignalingURLBehindHostRewritingProxy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		publicBase string
		headers    map[string]string
		host       string
		want       string
	}{
		{
			// The live failure: Caddy terminating TLS for quasar.example.com but
			// forwarding Host as the upstream 192.0.2.10:18443.
			name:       "public base url wins over a rewritten host",
			publicBase: "https://play.example.com",
			headers: map[string]string{
				"X-Forwarded-Proto": "https",
				"X-Forwarded-Host":  "play.example.com",
				"X-Forwarded-For":   "203.0.113.7",
			},
			host: "192.0.2.10:18443",
			want: "wss://play.example.com/v1/signal",
		},
		{
			// No PUBLIC_BASE_URL: the header the proxy sets for this exact purpose.
			name: "x-forwarded-host when no public base url",
			headers: map[string]string{
				"X-Forwarded-Proto": "https",
				"X-Forwarded-Host":  "play.example.com",
			},
			host: "192.0.2.10:18443",
			want: "wss://play.example.com/v1/signal",
		},
		{
			// Proxy chain: the left-most entry is the one the client asked for.
			name: "x-forwarded-host chain takes the left-most",
			headers: map[string]string{
				"X-Forwarded-Proto": "https",
				"X-Forwarded-Host":  "play.example.com, inner.lan:8443",
			},
			host: "192.0.2.10:18443",
			want: "wss://play.example.com/v1/signal",
		},
		{
			// PUBLIC_BASE_URL carries a path for the invite magic link; only its
			// scheme + host are meaningful here.
			name:       "public base url path is ignored",
			publicBase: "https://play.example.com/quasar/",
			headers:    map[string]string{"X-Forwarded-For": "203.0.113.7"},
			host:       "192.0.2.10:18443",
			want:       "wss://play.example.com/v1/signal",
		},
		{
			// A plain-http proxy must not be upgraded to wss by the base URL's
			// mere presence — the base URL's own scheme decides.
			name:       "http public base url yields ws",
			publicBase: "http://play.example.com",
			headers:    map[string]string{"X-Forwarded-For": "203.0.113.7"},
			host:       "192.0.2.10:18443",
			want:       "ws://play.example.com/v1/signal",
		},
		{
			// THE GUARD: a PUBLIC_BASE_URL set for invite links must not hijack a
			// direct LAN client, whose public name may not resolve internally.
			// No X-Forwarded-* ⇒ not proxied ⇒ r.Host verbatim, as before.
			name:       "direct request ignores public base url",
			publicBase: "https://play.example.com",
			host:       "192.0.2.10:18443",
			want:       "ws://192.0.2.10:18443/v1/signal",
		},
		{
			// Garbage in the config must degrade to the old behaviour, never to a
			// malformed URL the client cannot dial.
			name:       "unparseable public base url falls through",
			publicBase: "not a url",
			headers:    map[string]string{"X-Forwarded-Proto": "https"},
			host:       "192.0.2.10:18443",
			want:       "wss://192.0.2.10:18443/v1/signal",
		},
		{
			name:       "public base url with no host falls through",
			publicBase: "https:///onlypath",
			headers:    map[string]string{"X-Forwarded-Proto": "https"},
			host:       "192.0.2.10:18443",
			want:       "wss://192.0.2.10:18443/v1/signal",
		},
		{
			name:    "blank x-forwarded-host falls through",
			headers: map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "  "},
			host:    "192.0.2.10:18443",
			want:    "wss://192.0.2.10:18443/v1/signal",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodPost, "http://ignored/v1/sessions", nil)
			if err != nil {
				t.Fatal(err)
			}
			r.Host = tc.host
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			h := (&Handler{}).WithPublicBaseURL(tc.publicBase)
			if got := h.signalingURL(r); got != tc.want {
				t.Fatalf("signalingURL = %q, want %q", got, tc.want)
			}
		})
	}
}
