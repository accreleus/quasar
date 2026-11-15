// guard_test.go — the dial-time guard, tested directly. These assertions moved
// verbatim from internal/images/digest_test.go with the code they cover (#105).
package outbound

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestGuardedDialRefusesNonPublicIP — the DNS-rebind guard: an allowlisted host
// that resolves to a non-public IP is refused at dial time, on every
// connection. The lookup is injected so no real DNS is needed.
func TestGuardedDialRefusesNonPublicIP(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "169.254.1.1", "::1", "0.0.0.0"} {
		lookup := func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP(ip)}, nil
		}
		dial := guardedDialContext(lookup, nil)
		if _, err := dial(context.Background(), "tcp", "registry.example.com:443"); err == nil {
			t.Fatalf("guardedDialContext must refuse %s", ip)
		}
	}
}

// TestDisallowedIP pins the classification the dial guard depends on.
func TestDisallowedIP(t *testing.T) {
	disallowed := []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.9.9", "169.254.0.1", "224.0.0.1", "0.0.0.0",
		"100.64.0.1", "100.127.255.254", // CGNAT
		"64:ff9b:1::1", // NAT64 local-use
		"192.0.0.9", "198.18.0.1", "198.19.255.255", "240.0.0.1", "255.255.255.255",
		"192.0.2.1", "198.51.100.1", "203.0.113.1", "2001:db8::1", "3fff::1",
		"2002:c000:204::1", "2001::1", // 6to4, Teredo
		"fc00::1", "fe80::1", "ff02::1", "::", "100::1",
		"fec0::1", "4000::1", "::ffff:0:a00:1", "5f00::1", // outside 2000::/3, incl. SIIT 10.0.0.1
		"64:ff9b::a00:1", "64:ff9b::7f00:1", "64:ff9b::6440:1", // DNS64 of 10.0.0.1, 127.0.0.1, 100.64.0.1
		"::ffff:10.1.2.3", "::ffff:100.64.0.1", "::ffff:127.0.0.1", // IPv4-mapped
	}
	for _, s := range disallowed {
		if !DisallowedIP(net.ParseIP(s)) {
			t.Errorf("DisallowedIP(%s) = false, want true", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "140.82.112.3", "100.63.255.255", "100.128.0.1", "2606:4700::1111", "::ffff:8.8.8.8", "64:ff9b::808:808", "2001:4860:4860::8888"} {
		if DisallowedIP(net.ParseIP(s)) {
			t.Errorf("DisallowedIP(%s) = true, want false", s)
		}
	}
}

func TestDisallowedIPRefusesNil(t *testing.T) {
	if !DisallowedIP(nil) {
		t.Error("DisallowedIP(nil) = false, want true")
	}
}

// TestGuardedClientIgnoresProxyEnv: with HTTPS_PROXY set the transport must not
// dial the proxy; it dials (and checks) the target itself.
func TestGuardedClientIgnoresProxyEnv(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.example.com:3128")
	t.Setenv("HTTP_PROXY", "http://proxy.example.com:3128")
	t.Setenv("NO_PROXY", "")
	lookup := func(_ context.Context, host string) ([]net.IP, error) {
		if host == "proxy.example.com" {
			return []net.IP{net.ParseIP("8.8.8.8")}, nil // a public proxy
		}
		return []net.IP{net.ParseIP("10.0.0.7")}, nil // the private target
	}
	var dialled []string
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialled = append(dialled, addr)
		return nil, errors.New("test dial")
	}
	c := newGuardedHTTPClient(time.Second, lookup, dial)
	// ProxyFromEnvironment reads the env once per process, so an earlier test can
	// hide a set Proxy from the dial check below; pin the field too.
	if tr, ok := c.Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Fatalf("guarded transport must have Proxy == nil, got %#v", c.Transport)
	}
	for _, u := range []string{"https://internal.example.com/", "http://internal.example.com/"} {
		_, err := c.Get(u)
		if err == nil || !strings.Contains(err.Error(), "non-public address") {
			t.Fatalf("GET %s: err = %v, want a non-public refusal", u, err)
		}
	}
	if len(dialled) != 0 {
		t.Fatalf("dialled %v; the guard must refuse before any connect", dialled)
	}
}

// TestGuardedClientChecksEveryRedirectHop: a client that follows redirects
// still refuses a hop whose host resolves to a private address.
func TestGuardedClientChecksEveryRedirectHop(t *testing.T) {
	lookup := func(_ context.Context, host string) ([]net.IP, error) {
		if host == "public.example.com" {
			return []net.IP{net.ParseIP("8.8.8.8")}, nil
		}
		return []net.IP{net.ParseIP("100.64.0.9")}, nil
	}
	var dialled []string
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialled = append(dialled, addr)
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			_, _ = http.ReadRequest(bufio.NewReader(server))
			_, _ = server.Write([]byte("HTTP/1.1 302 Found\r\nLocation: http://cgnat.example.com/\r\nContent-Length: 0\r\n\r\n"))
		}()
		return client, nil
	}
	c := newGuardedHTTPClient(2*time.Second, lookup, dial)
	c.CheckRedirect = nil // follow, as a client with a different policy would
	_, err := c.Get("http://public.example.com/")
	if err == nil || !strings.Contains(err.Error(), "non-public address") {
		t.Fatalf("err = %v, want the redirect hop refused", err)
	}
	if len(dialled) != 1 || dialled[0] != "8.8.8.8:80" {
		t.Fatalf("dialled %v, want only the public first hop", dialled)
	}
}
