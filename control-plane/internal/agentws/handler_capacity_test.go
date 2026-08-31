package agentws

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeCapacityReason(t *testing.T) {
	reason := sanitizeCapacityReason("  probe\x00 failed\n" + strings.Repeat("界", 200))
	if !utf8.ValidString(reason) {
		t.Fatal("reason is not valid UTF-8")
	}
	if len(reason) > 512 {
		t.Fatalf("reason is %d bytes, want <= 512", len(reason))
	}
	if strings.ContainsAny(reason, "\x00\n") {
		t.Fatalf("reason contains control characters: %q", reason)
	}
}

func TestNormalizeCapacityReportCompatibilityAndFailClosed(t *testing.T) {
	gpu := GPUCapacity{Index: 0, VRAMMBTotal: 8192, EncodeSlotsTotal: 1}
	tests := []struct {
		name       string
		msg        CapacityMsg
		wantStatus string
		wantGPUs   int
		wantErr    bool
	}{
		{"legacy with gpu", CapacityMsg{GPUs: []GPUCapacity{gpu}}, "ok", 1, false},
		{"legacy empty", CapacityMsg{}, "unavailable", 0, false},
		{"ok empty", CapacityMsg{GPUDetection: "ok"}, "unavailable", 0, false},
		{"failed ignores gpu", CapacityMsg{GPUDetection: "failed", GPUs: []GPUCapacity{gpu}}, "failed", 0, false},
		{"invalid", CapacityMsg{GPUDetection: "maybe"}, "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, _, gpus, err := normalizeCapacityReport(tt.msg)
			if (err != nil) != tt.wantErr || status != tt.wantStatus || len(gpus) != tt.wantGPUs {
				t.Fatalf("status=%q gpus=%d err=%v", status, len(gpus), err)
			}
		})
	}
}
