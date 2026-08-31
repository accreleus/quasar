package agentws

import (
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
)

// TestSanitizeRegisterImages is the review-round #3 (Alice round-3)
// acceptance: register.images has no per-connection rate limit like
// image_state's token bucket, so this table proves every bound the sanitizer
// is supposed to enforce before a snapshot ever reaches ImageEvents.
func TestSanitizeRegisterImages(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	longID := strings.Repeat("a", maxImageIDLen+1)
	longVersion := strings.Repeat("v", maxVersionLen+1)
	maxLenID := strings.Repeat("a", maxImageIDLen)
	maxLenVersion := strings.Repeat("v", maxVersionLen)

	tests := []struct {
		name       string
		in         []RegisterImage
		wantOK     bool
		wantImages []RegisterImage
	}{
		{
			name:       "nil field means not reported",
			in:         nil,
			wantOK:     false,
			wantImages: nil,
		},
		{
			name:       "explicit empty array is a valid empty report",
			in:         []RegisterImage{},
			wantOK:     true,
			wantImages: []RegisterImage{},
		},
		{
			name: "well-formed entries pass through unchanged",
			in: []RegisterImage{
				{ImageID: "steam", Version: "1.0", State: "ready"},
				{ImageID: "epic", Version: "2.0", State: "absent"},
			},
			wantOK: true,
			wantImages: []RegisterImage{
				{ImageID: "steam", Version: "1.0", State: "ready"},
				{ImageID: "epic", Version: "2.0", State: "absent"},
			},
		},
		{
			name: "boundary-length image_id and version are kept",
			in: []RegisterImage{
				{ImageID: maxLenID, Version: maxLenVersion, State: "pulling"},
			},
			wantOK: true,
			wantImages: []RegisterImage{
				{ImageID: maxLenID, Version: maxLenVersion, State: "pulling"},
			},
		},
		{
			name: "empty image_id dropped",
			in: []RegisterImage{
				{ImageID: "", Version: "1.0", State: "ready"},
				{ImageID: "steam", Version: "1.0", State: "ready"},
			},
			wantOK: true,
			wantImages: []RegisterImage{
				{ImageID: "steam", Version: "1.0", State: "ready"},
			},
		},
		{
			name: "oversized image_id dropped",
			in: []RegisterImage{
				{ImageID: longID, Version: "1.0", State: "ready"},
				{ImageID: "steam", Version: "1.0", State: "ready"},
			},
			wantOK: true,
			wantImages: []RegisterImage{
				{ImageID: "steam", Version: "1.0", State: "ready"},
			},
		},
		{
			name: "oversized version dropped",
			in: []RegisterImage{
				{ImageID: "steam", Version: longVersion, State: "ready"},
				{ImageID: "epic", Version: "1.0", State: "ready"},
			},
			wantOK: true,
			wantImages: []RegisterImage{
				{ImageID: "epic", Version: "1.0", State: "ready"},
			},
		},
		{
			name: "invalid state dropped",
			in: []RegisterImage{
				{ImageID: "steam", Version: "1.0", State: "teleporting"},
				{ImageID: "epic", Version: "1.0", State: "ready"},
			},
			wantOK: true,
			wantImages: []RegisterImage{
				{ImageID: "epic", Version: "1.0", State: "ready"},
			},
		},
		{
			name: "building is not a valid register state (image_state only)",
			in: []RegisterImage{
				{ImageID: "steam", Version: "1.0", State: "building"},
			},
			wantOK:     true,
			wantImages: []RegisterImage{},
		},
		{
			name: "duplicate image_id keeps first occurrence",
			in: []RegisterImage{
				{ImageID: "steam", Version: "1.0", State: "ready"},
				{ImageID: "steam", Version: "2.0", State: "pulling"},
			},
			wantOK: true,
			wantImages: []RegisterImage{
				{ImageID: "steam", Version: "1.0", State: "ready"},
			},
		},
		{
			name:       "over the entry cap drops the WHOLE snapshot",
			in:         make([]RegisterImage, maxRegisterImages+1),
			wantOK:     false,
			wantImages: nil,
		},
		{
			name:       "exactly at the entry cap is accepted",
			in:         validEntries(maxRegisterImages),
			wantOK:     true,
			wantImages: validEntries(maxRegisterImages),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := sanitizeRegisterImages(tt.in, log, "test-host")
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if len(got) != len(tt.wantImages) {
				t.Fatalf("images = %+v, want %+v", got, tt.wantImages)
			}
			for i := range got {
				if got[i] != tt.wantImages[i] {
					t.Fatalf("images[%d] = %+v, want %+v", i, got[i], tt.wantImages[i])
				}
			}
		})
	}
}

// validEntries builds n distinct, well-formed RegisterImage entries for the
// entry-cap boundary cases above.
func validEntries(n int) []RegisterImage {
	out := make([]RegisterImage, n)
	for i := range out {
		out[i] = RegisterImage{ImageID: "img" + strconv.Itoa(i), Version: "1.0", State: "ready"}
	}
	return out
}
