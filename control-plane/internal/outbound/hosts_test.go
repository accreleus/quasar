package outbound

import "testing"

// TestParseHostList — the comma-separated env grammar every allowlist knob
// shares: trim, drop empties, lowercase, fall back when nothing is configured.
func TestParseHostList(t *testing.T) {
	if h := ParseHostList("", "ghcr.io"); len(h) != 1 || !HostAllowed(h, "ghcr.io") {
		t.Fatalf("empty raw: got %v want {ghcr.io}", h)
	}
	if h := ParseHostList("   ", "ghcr.io"); len(h) != 1 || !HostAllowed(h, "ghcr.io") {
		t.Fatalf("blank raw: got %v want {ghcr.io}", h)
	}
	h := ParseHostList(" ghcr.io , Registry.Example.COM ,, ", "ghcr.io")
	if len(h) != 2 {
		t.Fatalf("empty entries must be dropped: %v", h)
	}
	if !HostAllowed(h, "ghcr.io") || !HostAllowed(h, "registry.example.com") {
		t.Fatalf("parsed hosts: %v", h)
	}
	// A list of nothing but separators still yields the fallback, never an empty
	// (and therefore unenforceable) allowlist.
	if h := ParseHostList(" , , ", "ghcr.io"); len(h) != 1 || !HostAllowed(h, "ghcr.io") {
		t.Fatalf("separators only: got %v want {ghcr.io}", h)
	}
}

// TestHostAllowedNilIsAllowAll pins the test-only convention: a nil allowlist
// allows everything, which is why New refuses to build a production client with
// one.
func TestHostAllowedNilIsAllowAll(t *testing.T) {
	if !HostAllowed(nil, "anything.internal") {
		t.Fatal("a nil allowlist must allow everything (test-only convention)")
	}
	if HostAllowed(map[string]struct{}{}, "anything.internal") {
		t.Fatal("an empty (non-nil) allowlist must allow nothing")
	}
}
