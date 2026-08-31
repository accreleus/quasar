// Package origins owns browser origins: entry validation, allow-list
// resolution from its two sources, and the per-request verdict.
//
// Resolver is the single owner of the decision: internal/signal asks it to
// enforce, internal/access asks it to explain. When those were separate
// implementations they disagreed on whether same-origin includes the port, so
// the diagnostic panel reported "allowed" for an origin the socket refused.
package origins

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxEntries bounds the admin-editable allow-list; every entry is compared
// linearly on a handshake. Not a security property.
const MaxEntries = 64

// MaxEntryLength bounds one entry (a hostname is at most 253 octets; scheme
// and port add a little).
const MaxEntryLength = 512

// Sources of the resolved allow-list, reported so a UI can grey out a control
// the environment has pinned.
const (
	SourceEnvironment = "environment"
	SourceDatabase    = "database"
)

// Normalize accepts only a serialized HTTP(S) origin and returns its canonical
// form: scheme, lowercased host, and the port only when it is not the scheme's
// default.
//
// Dropping the default port is load-bearing: a browser behind a proxy sends
// `Origin: https://example.com` while the operator types
// `https://example.com:443`; without canonicalisation the entry silently never
// matches, in exactly the topology this feature exists for. Rejecting path,
// credentials, query and fragment is what stops a merely host-shaped
// attacker URI ("https://evil.example/@trusted.host") passing as same-origin.
func Normalize(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > MaxEntryLength {
		return "", false
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" || u.User != nil ||
		u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	authority := canonicalAuthority(u.Host, scheme)
	if authority == "" {
		return "", false
	}
	return scheme + "://" + authority, true
}

// CanonicalAuthority normalizes a bare `host[:port]` authority against the
// scheme it was reached over. Exported because the same-origin comparison must
// canonicalise the request's Host with the origin's scheme — otherwise
// `Origin: https://example.com` would not match `Host: example.com:443`.
func CanonicalAuthority(hostPort, scheme string) string {
	return canonicalAuthority(hostPort, strings.ToLower(scheme))
}

func canonicalAuthority(hostPort, scheme string) string {
	hostPort = strings.TrimSpace(hostPort)
	if hostPort == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		// No port. SplitHostPort also errors on a bare IPv6 literal, but a legal
		// authority brackets those and Normalize already rejected the rest.
		host, port = hostPort, ""
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" {
		return ""
	}
	// Non-ASCII (IDN) hosts are rejected, not stored: a browser sends the
	// punycode A-label, so a stored U-label could never match. The proper fix is
	// idna.Lookup.ToASCII, but golang.org/x/net is not a dependency of this
	// module and adding one is an operator decision; until then ValidateList
	// turns this into a 400 telling the operator to enter the punycode form.
	if !isASCII(host) {
		return ""
	}
	if port != "" {
		// url.Parse accepts any digits, so ":65536" would otherwise be stored as
		// an entry no browser can ever send. Reject; the caller 400s it.
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return ""
		}
		port = strconv.Itoa(n)
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	// Canonicalise an IP literal, don't just re-bracket: [2001:0db8:0:0:0:0:0:1]
	// and [2001:db8::1] are one address, and a stored spelling that differs from
	// the browser's compressed form silently never matches.
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	// Re-bracket an IPv6 literal: a colon in a host is only legal inside brackets.
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// ValidateList normalizes an admin-supplied list, rejecting the first bad
// entry with a message naming its position.
//
// "*" is refused outright: a wildcard allow-list discards the layer entirely.
// Blank entries are dropped (textarea-backed UIs send trailing empty lines);
// duplicates collapse — including entries differing only by an explicit
// default port, which are duplicates after Normalize.
func ValidateList(entries []string) ([]string, error) {
	out := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for i, raw := range entries {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if trimmed == "*" {
			return nil, fmt.Errorf("allowed_origins entry %d is \"*\": a wildcard would discard the signaling origin check entirely; list each origin explicitly", i+1)
		}
		normalized, ok := Normalize(trimmed)
		if !ok {
			hint := ""
			if !isASCII(trimmed) {
				// Unguessable without being told: the browser sends punycode.
				hint = ". An internationalised domain must be entered in its punycode " +
					"(xn--) form — that is what the browser sends"
			}
			return nil, fmt.Errorf("allowed_origins entry %d is not a valid origin: expected scheme and host only, e.g. https://quasar.example.com:8443 (no path, query, credentials, or trailing slash)%s", i+1, hint)
		}
		if _, dup := seen[normalized]; dup {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	if len(out) > MaxEntries {
		return nil, fmt.Errorf("allowed_origins has %d entries; at most %d are accepted", len(out), MaxEntries)
	}
	return out, nil
}

// SplitList parses a comma-separated origin list (the QUASAR_ALLOWED_ORIGINS
// wire form) into raw, untrimmed entries.
func SplitList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

// Store is the seam onto the admin-editable allow-list column
// (instance_settings.allowed_origins). internal/settings.Store satisfies it.
type Store interface {
	AllowedOrigins(ctx context.Context) ([]string, error)
}

// cacheTTL bounds staleness on the handshake path. It exists so the two
// lookups per handshake (the pre-Upgrade check and gorilla's CheckOrigin) are
// one query that agrees with itself — not as a performance cache.
const cacheTTL = 2 * time.Second

// Resolver resolves the allow-list from its two sources and answers the one
// question both callers have: is this request's origin allowed?
type Resolver struct {
	envList []string
	envSet  bool
	store   Store
	log     *slog.Logger

	mu       sync.Mutex
	cached   []string
	cachedAt time.Time
	// fillGen sequences cache fills: concurrent misses race to write, and an
	// older snapshot landing last would pin a stale allow-list for a full TTL.
	// A fill only writes if no later fill (or Invalidate) started since.
	fillGen uint64
}

// NewResolver builds the resolver. envRaw is QUASAR_ALLOWED_ORIGINS; envSet
// reports whether the variable was present, which is not the same as
// non-empty — set-to-empty must mean "explicitly nothing, ignore the
// database". store may be nil (tests, the route recorder); the resolver then
// reports the environment alone.
func NewResolver(envRaw string, envSet bool, store Store, log *slog.Logger) *Resolver {
	r := &Resolver{envSet: envSet, store: store, log: log}
	for _, o := range SplitList(envRaw) {
		if normalized, ok := Normalize(o); ok {
			r.envList = append(r.envList, normalized)
		}
	}
	return r
}

// WithStore attaches the database source after construction.
func (r *Resolver) WithStore(s Store) *Resolver { r.store = s; return r }

// Resolve returns the allow-list actually in force and which source supplied it.
//
// The environment overrides the database — the mirror of internal/secrets, on
// purpose: this is a security control operators pin in compose, and an
// admin-UI edit must not widen it. access-check reports the winning source.
//
// A database read failure resolves to the empty list, never to "allow". Empty
// is not deny-all — Decide's same-origin exemption still applies — so a blip
// degrades to fresh-install behaviour.
func (r *Resolver) Resolve(ctx context.Context) (list []string, source string) {
	if r.envSet || r.store == nil {
		return r.envList, SourceEnvironment
	}
	r.mu.Lock()
	if r.cached != nil && time.Since(r.cachedAt) < cacheTTL {
		cached := r.cached
		r.mu.Unlock()
		return cached, SourceDatabase
	}
	r.fillGen++
	gen := r.fillGen
	r.mu.Unlock()

	stored, err := r.store.AllowedOrigins(ctx)
	if err != nil {
		if r.log != nil {
			r.log.Warn("signaling allow-list: database read failed; treating the list as empty "+
				"(same-origin requests are unaffected)", "err", err)
		}
		stored = nil
	}
	out := make([]string, 0, len(stored))
	for _, o := range stored {
		// Re-normalize stored values: a row edited directly in psql must not
		// compare differently from the way it was validated.
		if normalized, ok := Normalize(o); ok {
			out = append(out, normalized)
		}
	}
	r.mu.Lock()
	// Last fill to start wins, not last to finish; see fillGen.
	if gen == r.fillGen {
		r.cached, r.cachedAt = out, time.Now()
	}
	r.mu.Unlock()
	return out, SourceDatabase
}

// Invalidate drops the cached allow-list. Exported for dependent packages'
// tests (internal/signal); production relies on the TTL instead.
func (r *Resolver) Invalidate() {
	r.mu.Lock()
	r.cached, r.cachedAt = nil, time.Time{}
	// Bump so a fill already in flight cannot restore what was just dropped.
	r.fillGen++
	r.mu.Unlock()
}

// Decision is one origin check: the verdict the enforcer acts on plus what the
// diagnostic panel needs to explain it. One evaluation produces both, so they
// cannot disagree.
type Decision struct {
	// Present reports whether the request carried an Origin header at all.
	// Browserless tooling omits it and /v1/signal admits that (the single-use
	// token is the actual credential), so absence is not a refusal.
	Present bool
	// Parsed reports whether the header normalized to a legal origin.
	Parsed bool
	// Origin is the canonical form; "" when absent or unparseable.
	Origin string
	// Allowed is the verdict. internal/signal acts on this and nothing else.
	Allowed bool
	// Listed reports that an allow-list entry matched.
	Listed bool
	// SameOrigin reports that the origin's authority matches the request's Host
	// once both are canonicalised against the origin's scheme.
	SameOrigin bool
	// Exempt reports the request passed only via the same-origin rule — the
	// case that silently disappears when a reverse proxy rewrites Host.
	Exempt bool
	// Allowlist and Source describe what was resolved for this decision.
	Allowlist []string
	Source    string
}

// Decide evaluates one request: raw Origin header, raw HTTP Host.
//
// The same-origin comparison canonicalises both sides against the origin's
// scheme, so `https://example.com` matches Host `example.com:443`, while
// `https://example.com:9999` still fails against `example.com:8443`. Raw
// string comparison got both directions wrong.
func (r *Resolver) Decide(ctx context.Context, originHeader, requestHost string) Decision {
	list, source := r.Resolve(ctx)
	d := Decision{Allowlist: list, Source: source}

	if strings.TrimSpace(originHeader) == "" {
		// No Origin: not a browser. Allowed, and not reported as an exemption —
		// there was nothing to exempt.
		d.Allowed = true
		return d
	}
	d.Present = true

	normalized, ok := Normalize(originHeader)
	if !ok {
		return d // Parsed false, Allowed false
	}
	d.Parsed = true
	d.Origin = normalized

	for _, entry := range list {
		if entry == normalized {
			d.Listed = true
			break
		}
	}

	scheme, authority, _ := strings.Cut(normalized, "://")
	d.SameOrigin = authority != "" &&
		strings.EqualFold(authority, CanonicalAuthority(requestHost, scheme))

	d.Allowed = d.Listed || d.SameOrigin
	d.Exempt = !d.Listed && d.SameOrigin
	return d
}
