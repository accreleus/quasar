package agentws

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Both strings reach a Postgres text column, which refuses half a rune.
func TestWireCutsKeepValidUTF8(t *testing.T) {
	img := ImageStateMsg{ImageID: "i", Error: strings.Repeat("a", maxImageErrLen-1) + "é tail"}
	reason := strings.Repeat("a", maxReleaseReasonLen-1) + "é tail"
	rel := ReleaseStateMsg{RequestID: "r", State: "failed", Reason: &reason}
	if !validateImageState(&img) || !validateReleaseState(&rel) {
		t.Fatal("a boundable message was dropped")
	}
	for name, got := range map[string]string{"image_state.error": img.Error, "release_state.reason": *rel.Reason} {
		if !utf8.ValidString(got) || !strings.HasSuffix(got, "a") {
			t.Errorf("%s: cut through a rune: valid=%v", name, utf8.ValidString(got))
		}
	}
}
