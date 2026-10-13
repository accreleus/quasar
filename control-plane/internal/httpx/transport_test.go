package httpx

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRefuseInsecure(t *testing.T) {
	_, edge, _ := net.ParseCIDR("172.18.0.0/16")
	_, local, _ := net.ParseCIDR("127.0.0.0/8")
	proxied := []*net.IPNet{edge}
	const (
		client = "203.0.113.7:40000"
		proxy  = "172.18.0.5:40000"
	)

	cases := []struct {
		name       string
		tls        bool
		peer       string
		trusted    []*net.IPNet
		headers    [][2]string
		loopbackOK bool
		accept     bool
	}{
		{name: "TLS listener", tls: true, peer: client, accept: true},
		{name: "TLS listener, QUASAR_TRUSTED_PROXIES set, peer is not one", tls: true, peer: client, trusted: proxied, accept: true},
		{name: "plain HTTP, no header", peer: client},
		// The bug: any peer could say it came through TLS.
		{name: "plain HTTP, spoofed https, no proxies configured", peer: client,
			headers: [][2]string{{"X-Forwarded-Proto", "https"}}},
		{name: "plain HTTP, spoofed https, peer outside the trusted network", peer: client, trusted: proxied,
			headers: [][2]string{{"X-Forwarded-Proto", "https"}}},
		{name: "plain HTTP, spoofed https, RFC 7239 form", peer: client,
			headers: [][2]string{{"Forwarded", "for=192.0.2.1;proto=https"}}},

		{name: "trusted proxy forwards https", peer: proxy, trusted: proxied, accept: true,
			headers: [][2]string{{"X-Forwarded-Proto", "https"}}},
		{name: "trusted proxy forwards HTTPS in capitals", peer: proxy, trusted: proxied, accept: true,
			headers: [][2]string{{"X-Forwarded-Proto", " HTTPS "}}},
		{name: "trusted proxy forwards http", peer: proxy, trusted: proxied,
			headers: [][2]string{{"X-Forwarded-Proto", "http"}}},
		{name: "trusted proxy says nothing", peer: proxy, trusted: proxied},
		{name: "trusted proxy sends only RFC 7239 Forwarded", peer: proxy, trusted: proxied,
			headers: [][2]string{{"Forwarded", "proto=https"}}},
		{name: "trusted proxy sends an empty value", peer: proxy, trusted: proxied,
			headers: [][2]string{{"X-Forwarded-Proto", ""}}},
		// An appending proxy leaves the client's own value in front of its own.
		{name: "trusted proxy appended http to a client's https", peer: proxy, trusted: proxied,
			headers: [][2]string{{"X-Forwarded-Proto", "https, http"}}},
		{name: "trusted proxy added a second header line", peer: proxy, trusted: proxied,
			headers: [][2]string{{"X-Forwarded-Proto", "https"}, {"X-Forwarded-Proto", "http"}}},
		{name: "two trusted hops, both https", peer: proxy, trusted: proxied, accept: true,
			headers: [][2]string{{"X-Forwarded-Proto", "https,https"}}},

		// Re-encrypting proxy: its word about the client's hop outranks our TLS.
		{name: "trusted proxy over TLS forwards http", tls: true, peer: proxy, trusted: proxied,
			headers: [][2]string{{"X-Forwarded-Proto", "http"}}},
		{name: "trusted proxy over TLS says nothing", tls: true, peer: proxy, trusted: proxied, accept: true},
		{name: "untrusted peer over TLS claims http", tls: true, peer: client, trusted: proxied, accept: true,
			headers: [][2]string{{"X-Forwarded-Proto", "http"}}},

		{name: "loopback peer, route allows it", peer: "127.0.0.1:40000", loopbackOK: true, accept: true},
		{name: "IPv6 loopback peer, route allows it", peer: "[::1]:40000", loopbackOK: true, accept: true},
		{name: "IPv4-mapped loopback peer, route allows it", peer: "[::ffff:127.0.0.1]:40000", loopbackOK: true, accept: true},
		{name: "loopback peer, route does not allow it", peer: "127.0.0.1:40000"},
		// What a published port looks like from inside the container.
		{name: "bridge gateway peer, route allows loopback", peer: "172.17.0.1:40000", loopbackOK: true},
		{name: "trusted loopback proxy forwards http", peer: "127.0.0.1:40000", trusted: []*net.IPNet{local}, loopbackOK: true,
			headers: [][2]string{{"X-Forwarded-Proto", "http"}}},
		{name: "peer address is not an IP", peer: "@", loopbackOK: true,
			headers: [][2]string{{"X-Forwarded-Proto", "https"}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target := "/v1/setup/claim"
			if c.tls {
				target = "https://quasar.test" + target
			}
			req := httptest.NewRequest(http.MethodPost, target, nil)
			req.RemoteAddr = c.peer
			for _, h := range c.headers {
				req.Header.Add(h[0], h[1])
			}
			rr := httptest.NewRecorder()

			refused := RefuseInsecure(rr, req, c.trusted, c.loopbackOK)
			if refused == c.accept {
				t.Fatalf("refused = %v, want %v", refused, !c.accept)
			}
			if c.accept {
				if rr.Body.Len() != 0 {
					t.Fatalf("an accepted request must be left untouched; wrote %s", rr.Body)
				}
				return
			}
			if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), `"code":"unauthorized"`) {
				t.Fatalf("refusal = %d %s, want 401 unauthorized", rr.Code, rr.Body)
			}
			for _, want := range []string{"HTTPS port", "QUASAR_TRUSTED_PROXIES", "X-Forwarded-Proto: https"} {
				if !strings.Contains(rr.Body.String(), want) {
					t.Fatalf("refusal must name %q; got %s", want, rr.Body)
				}
			}
		})
	}
}
