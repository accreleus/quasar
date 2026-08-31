package secrets

// BootWarning is pure — it reads only the keyring and the registry, never the
// database — so these tests build a *Store with a nil pool and run without
// Postgres (unlike the rest of this package's tests, which need
// TEST_DATABASE_URL). See #522.

import (
	"strings"
	"testing"
)

func TestBootWarningEmptyWhenKeyConfigured(t *testing.T) {
	kr, err := ParseKeyring(testKeyB64, "")
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	s := NewStore(nil, kr, testRegistry())
	if got := s.BootWarning(); got != "" {
		t.Fatalf("BootWarning with a configured key: want \"\", got %q", got)
	}
}

func TestBootWarningNamesTheEnvVarAndEveryDisabledFeature(t *testing.T) {
	s := NewStore(nil, nil, testRegistry())
	got := s.BootWarning()
	if got == "" {
		t.Fatal("BootWarning with no key configured must not be empty")
	}
	if !strings.Contains(got, "QUASAR_SECRET_KEY") {
		t.Fatalf("BootWarning must name the env var, got %q", got)
	}
	for _, want := range []string{"SteamGridDB API key", "Another secret"} {
		if !strings.Contains(got, want) {
			t.Fatalf("BootWarning must enumerate every declared secret by label; missing %q in %q", want, got)
		}
	}
}

func TestBootWarningExplainsTheFixAndKeyChangeConsequence(t *testing.T) {
	s := NewStore(nil, nil, testRegistry())
	got := s.BootWarning()
	if !strings.Contains(got, "openssl rand -base64 32") {
		t.Fatalf("BootWarning must say how to generate a key, got %q", got)
	}
	if !strings.Contains(got, "deploy/.env") {
		t.Fatalf("BootWarning must say where the key goes, got %q", got)
	}
	// Honesty about key rotation: changing the key does not silently discard
	// what is already stored, but it does become unreadable without the old
	// key preserved in QUASAR_SECRET_KEY_PREVIOUS. Neither claim ("lost
	// forever" or "just works") would be true.
	if !strings.Contains(got, "QUASAR_SECRET_KEY_PREVIOUS") {
		t.Fatalf("BootWarning must explain the key-rotation path, got %q", got)
	}
	if !strings.Contains(got, "unreadable") {
		t.Fatalf("BootWarning must be honest that a changed key makes old values unreadable, got %q", got)
	}
}

func TestBootWarningWithNoDeclaredSecretsStillNamesTheEnvVar(t *testing.T) {
	s := NewStore(nil, nil, NewRegistry())
	got := s.BootWarning()
	if got == "" {
		t.Fatal("BootWarning with no key configured must not be empty even with an empty registry")
	}
	if !strings.Contains(got, "QUASAR_SECRET_KEY") {
		t.Fatalf("BootWarning must name the env var, got %q", got)
	}
}
