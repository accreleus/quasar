package setup

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// unreadBody fails the test if the handler touches the request body.
type unreadBody struct{ t *testing.T }

func (b unreadBody) Read([]byte) (int, error) {
	b.t.Error("a refused claim must not read the body")
	return 0, http.ErrBodyReadAfterClose
}

// A claim on the plaintext listener is refused before anything looks at the
// token: no compare, no failure counted, no log line, no body read. The spoofed
// X-Forwarded-Proto is what used to carry it past the redirect (#593).
func TestClaimOverPlainHTTPIsRefusedBeforeTheTokenIsCompared(t *testing.T) {
	var logged bytes.Buffer
	c := &fakeClaimer{}
	svc := NewService(c, &fakeState{}, "the-real-token", slog.New(slog.NewTextHandler(&logged, nil)))

	plain := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/setup/claim", unreadBody{t})
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set(TokenHeader, token)
		rr := httptest.NewRecorder()
		svc.handleClaim(rr, req)
		return rr
	}

	right := plain("the-real-token")
	if right.Code != http.StatusUnauthorized {
		t.Fatalf("plaintext claim with the right token: status = %d, want 401 (%s)", right.Code, right.Body)
	}
	if c.claimCalled {
		t.Fatal("a plaintext claim reached ClaimSetup")
	}
	if !strings.Contains(right.Body.String(), "QUASAR_TRUSTED_PROXIES") || !strings.Contains(right.Body.String(), "HTTPS") {
		t.Fatalf("the refusal must say what to do; got %s", right.Body)
	}

	// More wrong tokens than the failure budget allows: none may be counted.
	for i := 0; i < claimFailureLimit+5; i++ {
		if wrong := plain("wrong-token"); wrong.Body.String() != right.Body.String() {
			t.Fatalf("refusal depends on the token:\n right=%s\n wrong=%s", right.Body, wrong.Body)
		}
	}
	if logged.Len() != 0 {
		t.Fatalf("a refused claim must not log; got %s", logged.String())
	}
	if rr := doClaim(t, svc, "wrong-token", goodBody); rr.Code != http.StatusUnauthorized {
		t.Fatalf("refused claims were counted as failures: status = %d, want 401", rr.Code)
	}
}

// claimStack is one deployment: the control plane's listeners as main.go wires
// them, and optionally a reverse proxy in front of the plaintext one. Real
// sockets, so the peer address and r.TLS are the server's own, not a test's.
type claimStack struct {
	claimer *fakeClaimer
	plain   *httptest.Server // the plaintext listener
	tls     *httptest.Server // the HTTPS listener; nil when QUASAR_TLS=off
}

func newClaimStack(t *testing.T, tlsOn bool, trusted []*net.IPNet) *claimStack {
	t.Helper()
	st := &claimStack{claimer: &fakeClaimer{}}
	mux := http.NewServeMux()
	NewService(st.claimer, &fakeState{}, "the-real-token", quietLog()).
		WithTrustedProxies(trusted).
		Register(mux, func(h http.Handler) http.Handler { return h })

	var plain http.Handler = mux
	if tlsOn {
		st.tls = httptest.NewTLSServer(mux)
		t.Cleanup(st.tls.Close)
		plain = httpx.RedirectToHTTPS(mux, "8443") // QUASAR_HTTP_REDIRECT=auto
	}
	st.plain = httptest.NewServer(plain)
	t.Cleanup(st.plain.Close)
	return st
}

// proxy fronts the plaintext listener the way Caddy does: it drops whatever
// X-Forwarded-* the client sent and states the protocol it was itself spoken to.
func (st *claimStack) proxy(t *testing.T, terminatesTLS bool) *httptest.Server {
	t.Helper()
	backend, err := url.Parse(st.plain.URL)
	if err != nil {
		t.Fatal(err)
	}
	rp := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(backend)
		pr.SetXForwarded()
	}}
	front := httptest.NewUnstartedServer(rp)
	if terminatesTLS {
		front.StartTLS()
	} else {
		front.Start()
	}
	t.Cleanup(front.Close)
	return front
}

// claim sends a well-formed claim with the right token, never following a redirect.
func claim(t *testing.T, srv *httptest.Server, spoofProto string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/setup/claim", strings.NewReader(goodBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(TokenHeader, "the-real-token")
	if spoofProto != "" {
		req.Header.Set("X-Forwarded-Proto", spoofProto)
	}
	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

// First-run setup still completes in every topology the install docs describe,
// and a claim that crossed a hop in clear creates no admin in any of them.
func TestFirstSetupByTopology(t *testing.T) {
	// The test proxy reaches the control plane from loopback; an operator names
	// their proxy's address the same way.
	loopbackProxy := []*net.IPNet{mustCIDR(t, "127.0.0.0/8")}

	cases := []struct {
		name    string
		tlsOn   bool
		trusted []*net.IPNet
		send    func(*testing.T, *claimStack) (int, string)
		want    int
	}{
		{name: "direct, on the HTTPS port (default and owned installs)", tlsOn: true,
			send: func(t *testing.T, st *claimStack) (int, string) { return claim(t, st.tls, "") },
			want: http.StatusCreated},
		{name: "direct, on the HTTP port: redirected as before", tlsOn: true,
			send: func(t *testing.T, st *claimStack) (int, string) { return claim(t, st.plain, "") },
			want: http.StatusPermanentRedirect},
		{name: "direct, on the HTTP port, claiming https: the bug", tlsOn: true,
			send: func(t *testing.T, st *claimStack) (int, string) { return claim(t, st.plain, "https") },
			want: http.StatusUnauthorized},
		{name: "direct, on the HTTP port, claiming https, with a proxy configured", tlsOn: true,
			trusted: []*net.IPNet{mustCIDR(t, "172.18.0.0/16")},
			send:    func(t *testing.T, st *claimStack) (int, string) { return claim(t, st.plain, "https") },
			want:    http.StatusUnauthorized},

		{name: "TLS-terminating trusted proxy to the HTTP port (hardened Caddy overlay, owned install behind a proxy)",
			tlsOn: true, trusted: loopbackProxy,
			send: func(t *testing.T, st *claimStack) (int, string) { return claim(t, st.proxy(t, true), "") },
			want: http.StatusCreated},
		{name: "QUASAR_TLS=off behind a TLS-terminating trusted proxy", trusted: loopbackProxy,
			send: func(t *testing.T, st *claimStack) (int, string) { return claim(t, st.proxy(t, true), "") },
			want: http.StatusCreated},
		{name: "the same proxy, never named in QUASAR_TRUSTED_PROXIES", tlsOn: true,
			send: func(t *testing.T, st *claimStack) (int, string) { return claim(t, st.proxy(t, true), "") },
			want: http.StatusUnauthorized},
		{name: "QUASAR_TLS=off, trusted proxy that served the client in clear", trusted: loopbackProxy,
			send: func(t *testing.T, st *claimStack) (int, string) { return claim(t, st.proxy(t, false), "https") },
			want: http.StatusUnauthorized},

		{name: "QUASAR_TLS=off, no proxy", send: func(t *testing.T, st *claimStack) (int, string) { return claim(t, st.plain, "") },
			want: http.StatusUnauthorized},
		{name: "QUASAR_TLS=off, no proxy, claiming https",
			send: func(t *testing.T, st *claimStack) (int, string) { return claim(t, st.plain, "https") },
			want: http.StatusUnauthorized},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newClaimStack(t, c.tlsOn, c.trusted)
			status, body := c.send(t, st)
			if status != c.want {
				t.Fatalf("status = %d, want %d (%s)", status, c.want, body)
			}
			if created := status == http.StatusCreated; st.claimer.claimCalled != created {
				t.Fatalf("ClaimSetup called = %v with status %d", st.claimer.claimCalled, status)
			}
			if status == http.StatusUnauthorized && !strings.Contains(body, "QUASAR_TRUSTED_PROXIES") {
				t.Fatalf("the refusal must name QUASAR_TRUSTED_PROXIES; got %s", body)
			}
		})
	}
}
