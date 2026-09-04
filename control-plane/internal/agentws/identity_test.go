package agentws

import (
	"encoding/json"
	"testing"
	"time"
)

func TestIdentityFromRegisterAcceptsTheContractsShapes(t *testing.T) {
	// 7-40 lowercase hex, both ends, stored exactly as sent — a short commit is
	// a real identity, only a less specific one.
	for _, commit := range []string{"1f0c1e0", "1f0c1e0e0c5a9d1b7a2f3e4d5c6b7a8901234567"} {
		id, dropped := identityFromRegister(RegisterMsg{SourceCommit: &commit})
		if len(dropped) != 0 {
			t.Errorf("%q was dropped: %v", commit, dropped)
		}
		if id.SourceCommit == nil || *id.SourceCommit != commit {
			t.Errorf("%q round-trip = %v", commit, id.SourceCommit)
		}
	}
}

func TestIdentityFromRegisterTreatsMalformedValuesAsAbsent(t *testing.T) {
	bad := RegisterMsg{
		SourceCommit: strPtr("1F0C1E0"), // uppercase
		BuiltAt:      strPtr("last tuesday"),
		InstallMode:  strPtr("kubernetes"),
	}
	id, dropped := identityFromRegister(bad)
	if id.SourceCommit != nil || id.BuiltAt != nil || id.InstallMode != nil {
		t.Fatalf("malformed values were kept: %+v", id)
	}
	if len(dropped) != 3 {
		t.Errorf("dropped = %v, want all three named so an operator can act on it", dropped)
	}
}

func TestIdentityFromRegisterKeepsFalseDistinctFromAbsent(t *testing.T) {
	no := false
	present, _ := identityFromRegister(RegisterMsg{UpdaterPresent: &no})
	if present.UpdaterPresent == nil || *present.UpdaterPresent {
		t.Errorf("an explicit false became %v — NULL is 'nobody has said', false is 'none found'",
			present.UpdaterPresent)
	}
	absent, _ := identityFromRegister(RegisterMsg{})
	if absent.UpdaterPresent != nil {
		t.Errorf("an absent field became %v", *absent.UpdaterPresent)
	}
}

func TestIdentityFromRegisterNormalizesBuiltAtToUTC(t *testing.T) {
	id, dropped := identityFromRegister(RegisterMsg{BuiltAt: strPtr("2026-09-04T22:00:00+02:00")})
	if len(dropped) != 0 {
		t.Fatalf("an RFC3339 offset time was dropped: %v", dropped)
	}
	want := time.Date(2026, 9, 4, 20, 0, 0, 0, time.UTC)
	if id.BuiltAt == nil || !id.BuiltAt.Equal(want) {
		t.Errorf("built_at = %v, want %v", id.BuiltAt, want)
	}
}

// A host with ANY of the four absent is identity-unknown and never eligible
// for a platform-release apply.
func TestIdentityKnownNeedsAllFour(t *testing.T) {
	full := RegisterMsg{
		SourceCommit:   strPtr("1f0c1e0"),
		BuiltAt:        strPtr("2026-09-04T12:00:00Z"),
		InstallMode:    strPtr("registry"),
		UpdaterPresent: boolPtr(true),
	}
	if id, _ := identityFromRegister(full); !id.Known() {
		t.Fatalf("a fully reported identity read as unknown: %+v", id)
	}
	for name, mutate := range map[string]func(*RegisterMsg){
		"source_commit":   func(m *RegisterMsg) { m.SourceCommit = nil },
		"built_at":        func(m *RegisterMsg) { m.BuiltAt = nil },
		"install_mode":    func(m *RegisterMsg) { m.InstallMode = nil },
		"updater_present": func(m *RegisterMsg) { m.UpdaterPresent = nil },
	} {
		msg := full
		mutate(&msg)
		if id, _ := identityFromRegister(msg); id.Known() {
			t.Errorf("identity read as known with %s absent", name)
		}
	}
}

// An older agent's register decodes with every identity field absent — the
// byte-identical guarantee, checked from the control plane's side.
func TestPreAmendmentRegisterDecodesWithNoIdentity(t *testing.T) {
	var reg RegisterMsg
	body := `{"type":"register","node_name":"gpu-host-01","agent_version":"0.1.0",
	          "auth":{"node_secret":"s"},"images":[]}`
	if err := json.Unmarshal([]byte(body), &reg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id, dropped := identityFromRegister(reg)
	if len(dropped) != 0 {
		t.Errorf("an older agent produced drop warnings: %v", dropped)
	}
	if id.Known() {
		t.Error("an older agent read as identity-known")
	}
}

// strPtr lives in store_test.go.
func boolPtr(b bool) *bool { return &b }
