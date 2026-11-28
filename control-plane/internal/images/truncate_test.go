package images

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A byte cut through a multi-byte rune leaves invalid UTF-8, which Postgres refuses.
func TestStoredErrorCutsKeepValidUTF8(t *testing.T) {
	for name, c := range map[string]struct {
		cut func(string) string
		max int
	}{
		"host_images.error": {truncateImageError, maxStoredImageError},
		"image_sync_error":  {clampSyncError, maxSyncErrorLen},
	} {
		got := c.cut(strings.Repeat("a", c.max-1) + "é tail")
		if !utf8.ValidString(got) || got != strings.Repeat("a", c.max-1) {
			t.Errorf("%s: got %d bytes, valid=%v; want the %d bytes before the rune",
				name, len(got), utf8.ValidString(got), c.max-1)
		}
	}
}
