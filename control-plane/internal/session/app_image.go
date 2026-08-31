package session

import (
	"encoding/json"
	"strings"
)

// Image returns `runtime_spec.image` on the effective runtime app (post
// derived-tile/preset resolution by GetLaunchApp) — the key the image-management
// P2 scheduler filter matches against the catalog. Empty (malformed row, or an
// unmodeled spec shape) fails open: the filter is disabled for that launch,
// leaving a non-catalog launch exactly as it was.
func (a LaunchApp) Image() string {
	return runtimeSpecImage(a.RuntimeSpec)
}

// runtimeSpecImage pulls "image" from a runtime_spec document. A decode failure
// yields "" rather than an error: a launch must never fail because a
// preference-shaped filter field couldn't be read.
func runtimeSpecImage(spec json.RawMessage) string {
	if len(spec) == 0 {
		return ""
	}
	var doc struct {
		Image string `json:"image"`
	}
	if err := json.Unmarshal(spec, &doc); err != nil {
		return ""
	}
	return strings.TrimSpace(doc.Image)
}
