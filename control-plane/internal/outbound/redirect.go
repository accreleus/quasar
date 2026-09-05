package outbound

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// One validated redirect hop, for the two endpoints that need it: a GHCR blob
// (307 to pkg-containers.githubusercontent.com) and a GitHub release asset (302
// to objects.githubusercontent.com). The client follows nothing by default —
// a Location is remote-supplied and could point anywhere — so a caller that
// must follow one asks for it here rather than relaxing its transport.
//
// The hop is https-only, allowlisted, single, and carries NO Authorization: the
// target is a presigned URL, which rejects a bearer token and would be handed
// the credential for a host that is not the one it was issued to.

var (
	ErrNoLocation     = errors.New("outbound: redirect carried no Location")
	ErrRedirectScheme = errors.New("outbound: redirect is not an absolute https URL")
	ErrRedirectUser   = errors.New("outbound: redirect carries userinfo")
	ErrRedirectHost   = errors.New("outbound: redirect host is not allowed")
	ErrRedirectSecond = errors.New("outbound: a second redirect is not followed")
)

// Doer is the request seam GetOneRedirect needs: *Client in production, a
// caller's own transport in a test.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// IsRedirect reports whether a status is one this helper will follow.
func IsRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// CheckRedirectTarget validates a Location: absolute https, no userinfo, host
// in allow (nil allows everything — the test-seam convention HostAllowed
// documents) and, when extra is non-empty, in extra as well.
func CheckRedirectTarget(location string, allow map[string]struct{}, extra []string) (*url.URL, error) {
	if location == "" {
		return nil, ErrNoLocation
	}
	u, err := url.Parse(location)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %v", ErrRedirectScheme, location, err)
	}
	if !u.IsAbs() || u.Scheme != "https" {
		return nil, fmt.Errorf("%w: %q", ErrRedirectScheme, location)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: %q", ErrRedirectUser, u.Redacted())
	}
	if !HostAllowed(allow, u.Hostname()) {
		return nil, fmt.Errorf("%w: %q", ErrRedirectHost, u.Hostname())
	}
	if len(extra) > 0 && !HostAllowed(hostSet(extra), u.Hostname()) {
		return nil, fmt.Errorf("%w: %q", ErrRedirectHost, u.Hostname())
	}
	return u, nil
}

func hostSet(hosts []string) map[string]struct{} {
	out := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		if h = normalizeHost(h); h != "" {
			out[h] = struct{}{}
		}
	}
	return out
}

// GetOneRedirect runs req and, on a 3xx, takes exactly one validated hop. The
// second request keeps req's Accept and drops every other header, Authorization
// included. A non-redirect response is returned untouched, so this is a
// drop-in for a plain Do.
func GetOneRedirect(d Doer, req *http.Request, allow map[string]struct{}, extra []string) (*http.Response, error) {
	resp, err := d.Do(req)
	if err != nil {
		return nil, err
	}
	if !IsRedirect(resp.StatusCode) {
		return resp, nil
	}
	location := resp.Header.Get("Location")
	drain(resp)

	u, err := CheckRedirectTarget(location, allow, extra)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", req.URL.Redacted(), err)
	}
	hop, err := http.NewRequestWithContext(req.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build redirected request: %w", err)
	}
	if accept := req.Header.Get("Accept"); accept != "" {
		hop.Header.Set("Accept", accept)
	}
	resp, err = d.Do(hop)
	if err != nil {
		return nil, err
	}
	if IsRedirect(resp.StatusCode) {
		drain(resp)
		return nil, fmt.Errorf("%s: %w", req.URL.Redacted(), ErrRedirectSecond)
	}
	return resp, nil
}

// GetFollowingOneRedirect GETs url, following at most one redirect under the
// rules above. allowedHosts narrows the hop further than this client's own
// allowlist; empty means the client's allowlist alone decides.
func (c *Client) GetFollowingOneRedirect(ctx context.Context, url string, allowedHosts []string) (*http.Response, error) {
	return c.GetFollowingOneRedirectWithHeader(ctx, url, nil, allowedHosts)
}

// GetFollowingOneRedirectWithHeader is the same with headers on the FIRST
// request only — an Authorization for the origin never reaches the hop.
func (c *Client) GetFollowingOneRedirectWithHeader(ctx context.Context, rawURL string, header http.Header, allowedHosts []string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request %s: %w", rawURL, err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return GetOneRedirect(c, req, c.allowHosts, allowedHosts)
}

func drain(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}
