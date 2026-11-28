package access

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateKeepsValidUTF8(t *testing.T) {
	got := truncate(strings.Repeat("a", maxReflectedLength-1) + "é tail")
	if want := strings.Repeat("a", maxReflectedLength-1) + "…(truncated)"; got != want {
		t.Fatalf("cut through a rune: valid=%v", utf8.ValidString(got))
	}
}
