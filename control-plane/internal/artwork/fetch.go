package artwork

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// DefaultMaxImageBytes caps a single fetched or uploaded image. Box art and
// hero banners are a few hundred KB; 8 MiB is generous and still bounds what a
// hostile or misconfigured source can write to disk per app.
const DefaultMaxImageBytes int64 = 8 << 20

// ErrBlockedAddress is returned when a URL resolves to an address the fetcher
// refuses to connect to.
var ErrBlockedAddress = errors.New("artwork: refusing to fetch from a non-public address")

// ErrFetchFailed wraps every outbound-fetch failure (bad URL, unreachable host,
// non-200, oversized body). It exists so the HTTP layer can answer 400 "that
// URL did not work" for an operator-supplied URL while still answering 500 for
// a genuine internal fault — without string-matching error text.
var ErrFetchFailed = errors.New("artwork: could not fetch the image")

// Fetcher performs the ONLY outbound HTTP this feature does. It is deliberately
// its own type rather than a bare http.Client because every request it makes is
// pointed at a URL that is, in the security sense, attacker-adjacent: a provider
// response chooses the CDN URL, and an operator pasting an override URL may be
// pasting something they were sent.
//
// Three properties it guarantees:
//
//  1. SSRF: the dialer inspects the RESOLVED IP of every connection — including
//     each hop of a redirect chain — and refuses loopback, private, link-local,
//     CGNAT, multicast and unspecified addresses. Checking the hostname instead
//     would be defeated by a DNS name that resolves to 169.254.169.254 or to a
//     container-network address like the Postgres service.
//  2. Redirects are followed at most maxRedirects times and each hop is
//     re-validated (scheme + address), so a public URL cannot bounce into the
//     deployment's own network.
//  3. The response body is size-capped and its type is verified by SNIFFING the
//     bytes, not by trusting Content-Type (see BlobStore.Put).
type Fetcher struct {
	client   *http.Client
	maxBytes int64
}

const maxRedirects = 3

// NewFetcher builds the guarded fetcher. timeout bounds the whole request
// including redirects and body read.
func NewFetcher(timeout time.Duration, maxBytes int64) *Fetcher {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxImageBytes
	}
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		// Control runs AFTER DNS resolution with the concrete address that is
		// about to be dialled — this is the only place a check is not racing
		// DNS rebinding, because it is the address the socket will actually use.
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("%w: unparseable address", ErrBlockedAddress)
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("%w: unparseable address", ErrBlockedAddress)
			}
			if !isPublicIP(ip) {
				return fmt.Errorf("%w: %s", ErrBlockedAddress, ip)
			}
			return nil
		},
	}
	return &Fetcher{
		maxBytes: maxBytes,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext:           dialer.DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 15 * time.Second,
				// A cache of idle connections to arbitrary remote hosts is not
				// worth holding for a feature that fetches twice per app, ever.
				MaxIdleConns:    4,
				IdleConnTimeout: 30 * time.Second,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return fmt.Errorf("artwork: too many redirects")
				}
				// The dialer re-checks the address of every hop, but the SCHEME
				// has to be re-checked here: a redirect to file:// or gopher://
				// never reaches the dialer.
				return validateURLScheme(req.URL)
			},
		},
	}
}

// Get fetches rawURL and returns the body bytes plus the declared content type.
// The URL is validated before the request and the body is capped at maxBytes.
func (f *Fetcher) Get(ctx context.Context, rawURL string) (data []byte, contentType string, err error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, "", fmt.Errorf("%w: invalid url: %v", ErrFetchFailed, err)
	}
	if err := validateURLScheme(u); err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "image/jpeg,image/png,image/webp")
	req.Header.Set("User-Agent", userAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrFetchFailed, unwrapBlocked(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%w: unexpected status %d", ErrFetchFailed, resp.StatusCode)
	}
	// Cheap pre-check: reject an oversized image before reading it. The real
	// enforcement is copyLimited, since Content-Length is advisory and absent
	// on a chunked response.
	if resp.ContentLength > f.maxBytes {
		return nil, "", fmt.Errorf("%w: image exceeds the %d byte limit", ErrFetchFailed, f.maxBytes)
	}
	body, err := copyLimited(resp.Body, f.maxBytes)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// validateURLScheme restricts fetching to http(s). Anything else — file://,
// ftp://, or a scheme-relative surprise — is rejected outright.
func validateURLScheme(u *url.URL) error {
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("%w: refusing url scheme %q", ErrFetchFailed, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: url has no host", ErrFetchFailed)
	}
	return nil
}

// unwrapBlocked surfaces ErrBlockedAddress through the http.Client's own error
// wrapping (*url.Error → *net.OpError → our dialer error). errors.Is walks that
// chain already; this keeps the sentinel reachable AFTER we re-wrap with
// ErrFetchFailed, so the handler can still tell "you pointed us at 10.0.0.5"
// apart from "that host was down".
func unwrapBlocked(err error) error {
	if errors.Is(err, ErrBlockedAddress) {
		return ErrBlockedAddress
	}
	return err
}

// isPublicIP reports whether ip is a globally routable unicast address.
//
// Deny-by-category, not by a list of "bad" addresses: the interesting targets
// on a self-hosted box are the container network (172.16/12), the host
// (127.0.0.1), the LAN (192.168/16, 10/8) and the cloud metadata service
// (169.254.169.254), and all four fall out of these rules. IPv6 mapped-v4 is
// unwrapped first so ::ffff:127.0.0.1 cannot slip past the v4 rules.
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsPrivate() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127: // 100.64/10 CGNAT
			return false
		case ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 0: // 192.0.0/24 IETF protocol assignments
			return false
		case ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 2: // TEST-NET-1
			return false
		case ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19): // benchmarking
			return false
		case ip4[0] == 198 && ip4[1] == 51 && ip4[2] == 100: // TEST-NET-2
			return false
		case ip4[0] == 203 && ip4[1] == 0 && ip4[2] == 113: // TEST-NET-3
			return false
		case ip4[0] >= 240: // reserved / broadcast
			return false
		}
		return true
	}
	// IPv6: unique-local (fc00::/7) is the v6 analogue of RFC1918.
	if len(ip) == net.IPv6len && ip[0]&0xfe == 0xfc {
		return false
	}
	return true
}
