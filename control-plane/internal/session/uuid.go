package session

// isValidUUID reports whether s is a canonical 8-4-4-4-12 hex UUID. A cheap
// format guard so a malformed/caller-supplied id becomes a clean "not found"
// (or no-op) instead of a Postgres cast error on `$1::uuid` (#414). Mirrors
// internal/crud/favourites.go's isValidUUID / internal/devices/handler.go's
// isUUID — every package that predicate-casts an id keeps its own copy since
// internal/session cannot import internal/crud (crud is imported BY session).
func isValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
