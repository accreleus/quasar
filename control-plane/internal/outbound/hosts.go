package outbound

import (
	"fmt"
	"net/url"
	"strings"
)

// ParseHostList parses the comma-separated host grammar every allowlist env
// knob shares (QUASAR_IMAGE_REGISTRY_HOSTS and friends): entries are trimmed,
// lowercased, and empty ones dropped. The result is never empty — an unset or
// all-blank value yields fallback, because an empty allowlist enforces nothing.
func ParseHostList(raw, fallback string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, h := range strings.Split(raw, ",") {
		if h = normalizeHost(h); h != "" {
			out[h] = struct{}{}
		}
	}
	if len(out) == 0 {
		out[normalizeHost(fallback)] = struct{}{}
	}
	return out
}

// HostAllowed reports whether host is in allow (case-insensitive).
//
// A nil allow map means "allow everything". That convention exists only for
// test seams that stand a local httptest server in for a real remote; New
// refuses to build a Client with an empty allowlist so it can never be the
// production behaviour.
func HostAllowed(allow map[string]struct{}, host string) bool {
	if allow == nil {
		return true
	}
	_, ok := allow[normalizeHost(host)]
	return ok
}

// CheckURL is the pre-flight every outbound request passes: https only, no
// userinfo, host on the allowlist. Callers that hold a URL from an untrusted
// source (a token realm, a catalog-supplied link) can run it before building a
// request; Client.Do runs it on every request regardless.
func CheckURL(u *url.URL, allow map[string]struct{}) error {
	if u == nil {
		return fmt.Errorf("outbound: no URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("outbound: %q is not https", u.Redacted())
	}
	if u.User != nil {
		return fmt.Errorf("outbound: %q carries userinfo", u.Redacted())
	}
	if !HostAllowed(allow, u.Hostname()) {
		return fmt.Errorf("outbound: host %q is not in the allowlist", u.Hostname())
	}
	return nil
}

func normalizeHost(h string) string { return strings.ToLower(strings.TrimSpace(h)) }
