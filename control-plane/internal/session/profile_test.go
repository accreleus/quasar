package session

import "testing"

// TestValidH264Profile covers the schema.md legal set (allow) and rejects
// everything else (deny) — the gate the launch handler applies to a per-launch
// h264_profile override (P1-11). Pure; runs without a database.
func TestValidH264Profile(t *testing.T) {
	allow := []string{"constrained-baseline", "main", "high"}
	for _, p := range allow {
		if !ValidH264Profile(p) {
			t.Errorf("ValidH264Profile(%q) = false, want true", p)
		}
	}

	deny := []string{
		"",                 // absent/empty
		"baseline",         // legal H.264 profile but not a contract value
		"constrained-high", // ditto
		"Main", "HIGH",     // case-sensitive
		"high ", " main", // whitespace
		"ultra", "garbage", // nonsense
	}
	for _, p := range deny {
		if ValidH264Profile(p) {
			t.Errorf("ValidH264Profile(%q) = true, want false", p)
		}
	}
}

// TestPickProfile: an override wins; nil or empty falls back to the floor.
func TestPickProfile(t *testing.T) {
	if got := pickProfile(nil); got != "constrained-baseline" {
		t.Errorf("pickProfile(nil) = %q, want constrained-baseline", got)
	}
	empty := ""
	if got := pickProfile(&empty); got != "constrained-baseline" {
		t.Errorf("pickProfile(&\"\") = %q, want constrained-baseline", got)
	}
	main := "main"
	if got := pickProfile(&main); got != "main" {
		t.Errorf("pickProfile(&\"main\") = %q, want main", got)
	}
}
