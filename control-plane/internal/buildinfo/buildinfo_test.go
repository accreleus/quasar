package buildinfo

import (
	"testing"
	"testing/fstest"

	"github.com/accreleus/quasar/control-plane/migrations"
)

// An unstamped build is the DEFAULT state of every developer `go build`, so
// this is the case that must be right: "dev" and two nulls, never a stale
// constant.
func TestUnstampedBuildReadsAsUnknown(t *testing.T) {
	id := Get()
	if id.Version != UnknownVersion {
		t.Errorf("version = %q, want %q", id.Version, UnknownVersion)
	}
	if id.SourceCommit != nil {
		t.Errorf("source_commit = %v, want nil", *id.SourceCommit)
	}
	if id.BuiltAt != nil {
		t.Errorf("built_at = %v, want nil", *id.BuiltAt)
	}
	if id.SchemaVersion <= 0 {
		t.Errorf("schema_version = %d, want > 0 — it is derived, never stamped", id.SchemaVersion)
	}
}

func TestNormalizeVersion(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "dev"},
		{"unknown", "dev"},  // the Dockerfile ARG default
		{"dev", "dev"},      // already the honest answer
		{"v0.2.0", "0.2.0"}, // the release lever stamps from a vX.Y.Z tag
		{"0.2.0", "0.2.0"},  // already stripped
		{"v0.2.0-rc.1", "0.2.0-rc.1"},
	} {
		if got := normalizeVersion(tc.in); got != tc.want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeCommitAcceptsOnlyAFullLowercaseSha(t *testing.T) {
	full := "1f0c1e0e0c5a9d1b7a2f3e4d5c6b7a8901234567"
	if got := normalizeCommit(full); got == nil || *got != full {
		t.Errorf("normalizeCommit(full) = %v, want %q", got, full)
	}
	// A short sha is a real identity for an AGENT (agent-api.md accepts 7-40),
	// but PlatformIdentity.source_commit promises a full one, so the control
	// plane reports null rather than a shape its own contract disallows.
	for _, bad := range []string{"", "unknown", "1f0c1e0", "1F0C1E0E0C5A9D1B7A2F3E4D5C6B7A8901234567", "zzzz"} {
		if got := normalizeCommit(bad); got != nil {
			t.Errorf("normalizeCommit(%q) = %q, want nil", bad, *got)
		}
	}
}

func TestNormalizeBuiltAtIsRFC3339UTCOrNil(t *testing.T) {
	if got := normalizeBuiltAt("2026-09-04T22:00:00+02:00"); got == nil || *got != "2026-09-04T20:00:00Z" {
		t.Errorf("normalizeBuiltAt(offset) = %v, want 2026-09-04T20:00:00Z", got)
	}
	for _, bad := range []string{"", "unknown", "yesterday", "2026-09-04"} {
		if got := normalizeBuiltAt(bad); got != nil {
			t.Errorf("normalizeBuiltAt(%q) = %q, want nil", bad, *got)
		}
	}
}

func TestHighestMigrationTakesTheLargestPrefix(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_initial.up.sql":    {},
		"0001_initial.down.sql":  {},
		"0074_identity.up.sql":   {},
		"0074_identity.down.sql": {},
		"0009_small.up.sql":      {},
		"embed.go":               {}, // not a migration
		"notes.md":               {},
	}
	got, err := HighestMigration(fsys)
	if err != nil {
		t.Fatalf("HighestMigration: %v", err)
	}
	if got != 74 {
		t.Errorf("HighestMigration = %d, want 74", got)
	}
}

// Ordering is numeric, not lexical: a three-digit file must not beat a
// four-digit one, and leading zeros must not be read as octal.
func TestHighestMigrationIsNumericNotLexical(t *testing.T) {
	fsys := fstest.MapFS{
		"0009_a.up.sql": {},
		"0100_b.up.sql": {},
		"0074_c.up.sql": {},
	}
	got, err := HighestMigration(fsys)
	if err != nil {
		t.Fatalf("HighestMigration: %v", err)
	}
	if got != 100 {
		t.Errorf("HighestMigration = %d, want 100", got)
	}
}

func TestHighestMigrationRefusesAnEmptySet(t *testing.T) {
	if _, err := HighestMigration(fstest.MapFS{"README.md": {}}); err == nil {
		t.Fatal("want an error for a migration set with no *.up.sql")
	}
}

// The derived value must track the real embedded set, so a migration landing
// without this test noticing is impossible.
func TestSchemaVersionMatchesTheEmbeddedSet(t *testing.T) {
	want, err := HighestMigration(migrations.FS)
	if err != nil {
		t.Fatalf("HighestMigration(embedded): %v", err)
	}
	if SchemaVersion() != want {
		t.Errorf("SchemaVersion() = %d, embedded set highest = %d", SchemaVersion(), want)
	}
}
