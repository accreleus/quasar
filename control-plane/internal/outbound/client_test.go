// client_test.go — the hardening rules, exercised through Client.Do rather than
// by calling the guard internals: a refusal that only holds when a test reaches
// past Do is not a refusal a caller can rely on.
package outbound

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// testConfig is the production shape (non-empty allowlist, real timeouts) with
// only the lookup swapped, so every case below goes through the same New().
func testConfig(hosts ...string) Config {
	allow := map[string]struct{}{}
	for _, h := range hosts {
		allow[h] = struct{}{}
	}
	return Config{AllowHosts: allow, Timeout: 2 * time.Second, MaxBodyBytes: 64}
}

func mustNew(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// roundTripperFunc is the transport seam used by the redirect case: the
// redirect target must be observably never requested, which needs a transport
// that records what it was asked for.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newRequest(t *testing.T, rawurl string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, rawurl, nil)
	if err != nil {
		t.Fatalf("build request %q: %v", rawurl, err)
	}
	return req
}

// TestNewRefusesEmptyAllowlist — "nil allowlist means allow everything" is a
// test-only convention; a production client must never be constructible that
// way by accident.
func TestNewRefusesEmptyAllowlist(t *testing.T) {
	for name, cfg := range map[string]Config{
		"nil":   {Timeout: time.Second},
		"empty": {AllowHosts: map[string]struct{}{}, Timeout: time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(cfg); err == nil {
				t.Fatal("New with an empty allowlist: got nil error, want a refusal")
			}
		})
	}
}

// TestDoRefusesNonPublicAddress — the DNS-rebind guard seen from Do: an
// allowlisted host that resolves to a loopback or private address is refused,
// and the dialer is never asked to connect.
func TestDoRefusesNonPublicAddress(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "192.168.4.5", "169.254.169.254", "::1"} {
		t.Run(ip, func(t *testing.T) {
			var dialed bool
			cfg := testConfig("registry.example.com")
			cfg.LookupIP = func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP(ip)}, nil
			}
			cfg.dialContext = func(context.Context, string, string) (net.Conn, error) {
				dialed = true
				return nil, errors.New("must not be reached")
			}
			c := mustNew(t, cfg)

			resp, err := c.Do(newRequest(t, "https://registry.example.com/v2/"))
			if err == nil {
				_ = resp.Body.Close()
				t.Fatalf("Do against a host resolving to %s: got nil error, want a refusal", ip)
			}
			if dialed {
				t.Fatalf("a connection was attempted to %s — the guard must refuse before any dial", ip)
			}
			if !strings.Contains(err.Error(), "non-public") {
				t.Fatalf("error %v does not name the non-public address refusal", err)
			}
		})
	}
}

// TestDoRefusesNonHTTPS — plaintext egress is refused before any network I/O.
func TestDoRefusesNonHTTPS(t *testing.T) {
	var tripped bool
	cfg := testConfig("registry.example.com")
	cfg.transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		tripped = true
		return nil, errors.New("must not be reached")
	})
	c := mustNew(t, cfg)

	if _, err := c.Do(newRequest(t, "http://registry.example.com/v2/")); err == nil {
		t.Fatal("Do on an http:// URL: got nil error, want a refusal")
	}
	if tripped {
		t.Fatal("the transport was reached — the scheme check must run before any network I/O")
	}
}

// TestDoRefusesUserinfo — credentials in the URL are a smuggling vector, not a
// supported way to authenticate.
func TestDoRefusesUserinfo(t *testing.T) {
	c := mustNew(t, testConfig("registry.example.com"))
	if _, err := c.Do(newRequest(t, "https://user:pass@registry.example.com/v2/")); err == nil {
		t.Fatal("Do on a URL carrying userinfo: got nil error, want a refusal")
	}
}

// TestDoRefusesHostOffAllowlist — the allowlist is enforced by the client, so a
// caller that forgot to pre-check a host it parsed still cannot reach it.
func TestDoRefusesHostOffAllowlist(t *testing.T) {
	// A public lookup plus a dialer that always fails: nothing here touches the
	// network, and an allowed host fails with a distinguishable error.
	cfg := testConfig("ghcr.io")
	cfg.LookupIP = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("140.82.112.3")}, nil
	}
	cfg.dialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("dial disabled in this test")
	}
	c := mustNew(t, cfg)

	if _, err := c.Do(newRequest(t, "https://evil.example.com/token")); err == nil {
		t.Fatal("Do on an off-allowlist host: got nil error, want a refusal")
	}
	// Case-insensitive match: an uppercase spelling of an allowlisted host must
	// get past the allowlist and fail at the (disabled) dial instead, never be
	// refused as off-allowlist.
	_, err := c.Do(newRequest(t, "https://GHCR.IO/token"))
	if err == nil {
		t.Fatal("uppercase host: expected the request to proceed past the allowlist and then fail to connect")
	}
	if strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("uppercase host was refused as off-allowlist: %v", err)
	}
}

// TestDoDoesNotFollowRedirects — a 3xx Location from an untrusted server could
// point the next request past the allowlist and the dial guard, so the redirect
// is surfaced to the caller and never followed.
func TestDoDoesNotFollowRedirects(t *testing.T) {
	var asked []string
	cfg := testConfig("registry.example.com", "evil.example.com")
	cfg.transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		asked = append(asked, r.URL.String())
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://evil.example.com/elsewhere"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    r,
		}, nil
	})
	c := mustNew(t, cfg)

	resp, err := c.Do(newRequest(t, "https://registry.example.com/v2/x/manifests/1"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status: got %d want %d (the 3xx must be returned, not followed)", resp.StatusCode, http.StatusFound)
	}
	if len(asked) != 1 || asked[0] != "https://registry.example.com/v2/x/manifests/1" {
		t.Fatalf("requests made: %v — the redirect target must never be contacted", asked)
	}
}

// TestDoBoundsResponseBody — a hostile server must not stream the control plane
// out of memory. The bound errors rather than truncating: a silently short body
// becomes a confusing parse failure somewhere else.
func TestDoBoundsResponseBody(t *testing.T) {
	cfg := testConfig("registry.example.com")
	cfg.MaxBodyBytes = 16
	cfg.transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", 1024))),
			Request:    r,
		}, nil
	})
	c := mustNew(t, cfg)

	resp, err := c.Do(newRequest(t, "https://registry.example.com/token"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("reading an oversized body: got (%d bytes, %v), want ErrBodyTooLarge", len(got), err)
	}
}

// TestDoPassesBodyUnderTheBound — the bound must not corrupt or truncate a
// response that fits.
func TestDoPassesBodyUnderTheBound(t *testing.T) {
	const payload = `{"token":"t"}`
	cfg := testConfig("registry.example.com")
	cfg.transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(payload)),
			Request:    r,
		}, nil
	})
	c := mustNew(t, cfg)

	resp, err := c.Do(newRequest(t, "https://registry.example.com/token"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != payload {
		t.Fatalf("body: got %q want %q", body, payload)
	}
}

// TestHostAllowed — the pre-flight check callers use on a host they parsed
// themselves (a registry ref, a token realm) before building a request.
func TestHostAllowed(t *testing.T) {
	c := mustNew(t, testConfig("ghcr.io"))
	if !c.HostAllowed("ghcr.io") || !c.HostAllowed("GHCR.io") {
		t.Fatal("an allowlisted host must be allowed, case-insensitively")
	}
	if c.HostAllowed("registry.internal") {
		t.Fatal("an off-allowlist host must be refused")
	}
}

// TestCheckURL — the shared pre-flight, including the nil-allowlist (test-only)
// convention that keeps existing injected-client callers working.
func TestCheckURL(t *testing.T) {
	allow := map[string]struct{}{"ghcr.io": {}}
	for _, raw := range []string{"http://ghcr.io/token", "https://user:pass@ghcr.io/token", "https://evil.internal/token"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if err := CheckURL(u, allow); err == nil {
			t.Fatalf("CheckURL(%q): got nil error, want a refusal", raw)
		}
	}
	u, err := url.Parse("https://evil.internal/token")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := CheckURL(u, nil); err != nil {
		t.Fatalf("a nil allowlist is the test-only allow-everything case: %v", err)
	}
}
