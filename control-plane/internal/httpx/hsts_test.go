package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Real listeners: net/http sets r.TLS on the TLS connection and not on the plain one.
func TestHSTSOnlyOnTLSListener(t *testing.T) {
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, tc := range []struct {
		name   string
		maxAge int32
		plain  string
		tls    string
	}{
		{"unset", 0, "", ""},
		{"set", 86400, "", "max-age=86400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := HSTS(ok, tc.maxAge)
			for _, l := range []struct {
				name string
				srv  *httptest.Server
				want string
			}{
				{"plain", httptest.NewServer(h), tc.plain},
				{"tls", httptest.NewTLSServer(h), tc.tls},
			} {
				defer l.srv.Close()
				resp, err := l.srv.Client().Get(l.srv.URL)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if got := resp.Header.Get("Strict-Transport-Security"); got != l.want {
					t.Fatalf("%s listener HSTS = %q, want %q", l.name, got, l.want)
				}
			}
		})
	}
}
