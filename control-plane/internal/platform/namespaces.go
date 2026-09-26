package platform

import "strings"

// The two image rules a developer apply checks up front (control-api.md
// §"Developer apply"), with the recovery actor's semantics: its allowlist
// (quasar-recovery trust::config, pinned by testdata/recovery/trust-vectors)
// stays the enforcement.

// DefaultAllowedNamespaces is the org's own namespace: the only place a
// platform image can come from unless an operator says otherwise.
var DefaultAllowedNamespaces = []string{"ghcr.io/accreleus/quasar"}

// ParseNamespaces splits QUASAR_UPDATER_ALLOWED_NAMESPACES, trimming spaces and
// any trailing slash. Blank yields the default, never an empty allowlist.
func ParseNamespaces(raw string) []string {
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimRight(strings.TrimSpace(part), "/")
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return append([]string(nil), DefaultAllowedNamespaces...)
	}
	return out
}

// NamespaceAllowed matches on a path-segment boundary, never a bare string
// prefix: `ghcr.io/accreleus/quasar` must not admit
// `ghcr.io/accreleus/quasar-evil/thing`.
func NamespaceAllowed(image string, allowed []string) bool {
	for _, ns := range allowed {
		if strings.HasPrefix(image, ns+"/") && len(image) > len(ns)+1 {
			return true
		}
	}
	return false
}

// ImageHasTagOrDigest reports whether a repository reference carries either.
// A tag is a `:` after the last `/` (so `registry:5000/repo` is a port); a
// digest is any `@`.
func ImageHasTagOrDigest(image string) bool {
	if strings.Contains(image, "@") {
		return true
	}
	last := strings.LastIndex(image, "/")
	return strings.Contains(image[last+1:], ":")
}
