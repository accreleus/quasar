// Package outbound is the control plane's one hardened egress HTTP client.
//
// Every outbound call the control plane makes on behalf of remote data — a
// registry host from a catalog manifest, a token realm from a registry's own
// WWW-Authenticate header, a GitHub Releases URL — is an SSRF vector unless it
// is contained. Containment used to live inside the registry digest resolver;
// it lives here so a second caller gets it by construction rather than by
// remembering to copy it (#105).
//
// What a Client enforces, before any network I/O:
//   - https only, no userinfo in the URL;
//   - the host is on this client's own allowlist (case-insensitive);
//   - no redirects are followed — a 3xx is returned to the caller as-is,
//     because a Location could point the next hop past these very checks;
//   - the dialer resolves the host itself and refuses every loopback, private,
//     link-local, multicast or unspecified address (DNS-rebind guard);
//   - the response body is bounded, so a hostile server cannot stream the
//     process out of memory.
//
// Every NEW outbound HTTP caller in the control plane uses this package. Each
// caller constructs its own Client with its own allowlist and timeout, so one
// caller's settings are never another's (the registry allowlist is
// QUASAR_IMAGE_REGISTRY_HOSTS; a GitHub client's is its own).
package outbound

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// DefaultTimeout bounds one whole request (dial + TLS + response). A slow
	// remote must not stretch an N-item loop into N x forever.
	DefaultTimeout = 5 * time.Second

	// DefaultMaxBodyBytes is the response-body ceiling when a Config leaves it
	// unset — the bound the registry token read has always used.
	DefaultMaxBodyBytes int64 = 1 << 20
)

// ErrBodyTooLarge is returned by a read of a response body that exceeds the
// client's MaxBodyBytes. The bound errors rather than truncating silently: a
// short body would otherwise surface far away as a confusing parse failure.
var ErrBodyTooLarge = errors.New("outbound: response body exceeds the configured limit")

// Config describes one caller's egress policy.
type Config struct {
	// AllowHosts is the set of hosts this client may contact, lowercased. It
	// must be non-empty: New refuses an empty one, because "nil means allow
	// everything" is a test-only convention (see HostAllowed) and must never be
	// reachable by accident in production. Build it with ParseHostList.
	AllowHosts map[string]struct{}

	// Timeout bounds one whole request; zero or negative means DefaultTimeout.
	Timeout time.Duration

	// MaxBodyBytes bounds every response body; zero or negative means DefaultMaxBodyBytes.
	MaxBodyBytes int64

	// LookupIP resolves a host for the DNS-rebind guard. Test seam: nil uses the
	// process resolver.
	LookupIP IPLookup

	// transport and dialContext are unexported test seams. Neither is settable
	// from outside the package, so no caller can quietly opt out of the guarded
	// transport it thinks it is getting.
	transport   http.RoundTripper
	dialContext dialFunc
}

// Client is a hardened HTTP client. Use New; the zero value is not usable.
type Client struct {
	http       *http.Client
	allowHosts map[string]struct{}
	maxBody    int64
}

// New builds a Client. It fails on an empty allowlist — a client that may
// contact anything is not a containment boundary.
func New(cfg Config) (*Client, error) {
	if len(cfg.AllowHosts) == 0 {
		return nil, fmt.Errorf("outbound: a client needs a non-empty host allowlist")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	allow := make(map[string]struct{}, len(cfg.AllowHosts))
	for h := range cfg.AllowHosts { // copied so a caller's later edit can't widen this client
		allow[normalizeHost(h)] = struct{}{}
	}

	hc := newGuardedHTTPClient(timeout, cfg.LookupIP, cfg.dialContext)
	if cfg.transport != nil {
		hc.Transport = cfg.transport
	}
	return &Client{http: hc, allowHosts: allow, maxBody: maxBody}, nil
}

// HostAllowed reports whether this client may contact host (case-insensitive).
// For callers that parse a host themselves — a registry ref, a token realm —
// and must refuse it before building any request.
func (c *Client) HostAllowed(host string) bool { return HostAllowed(c.allowHosts, host) }

// Do runs the request, enforcing scheme/userinfo/allowlist BEFORE any network
// I/O and bounding the response body afterwards.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, fmt.Errorf("outbound: request has no URL")
	}
	if err := CheckURL(req.URL, c.allowHosts); err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &boundedBody{r: resp.Body, limit: c.maxBody}
	return resp, nil
}

// boundedBody fails the read that would cross the limit instead of returning a
// silently truncated body. It reads at most limit+1 bytes: one byte past the
// limit is enough to know the body is too large, and reading further would be
// the very thing the bound exists to prevent.
type boundedBody struct {
	r     io.ReadCloser
	limit int64
	read  int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.read > b.limit {
		return 0, ErrBodyTooLarge
	}
	if room := b.limit + 1 - b.read; int64(len(p)) > room {
		p = p[:room]
	}
	n, err := b.r.Read(p)
	b.read += int64(n)
	if b.read > b.limit {
		return int(int64(n) - (b.read - b.limit)), ErrBodyTooLarge
	}
	return n, err
}

func (b *boundedBody) Close() error { return b.r.Close() }
