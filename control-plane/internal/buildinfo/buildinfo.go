// Package buildinfo holds what this control-plane binary IS: the stamped
// semver, the source commit and build time it was built from, and the highest
// migration version it embeds.
//
// The first three are injected at link time (`go build -ldflags -X`) by every
// build path that produces the binary (deploy/Dockerfile.control,
// deploy/Dockerfile.control.prod, deploy/build-images.sh, the images
// workflow). An UNSTAMPED build must read as unknown — `"dev"` for the
// version, null for the commit and build time — and never as a stale package
// constant: a version baked into source drifts from the tree the moment a
// release is cut, and a wrong identity is worse than an absent one
// (control-api.md §Platform releases).
//
// schema_version is deliberately NOT a build flag. It is derived from the
// embedded migration set, so it is ALWAYS known — which is why it, and not
// semver or built_at, is the ordering key across the release surface: the
// control plane runs migrations forward at boot and crash-loops against a
// database ahead of it (ADR 0002).
package buildinfo

import (
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"time"

	"github.com/accreleus/quasar/control-plane/migrations"
)

// Linker-injected stamps. Set with, e.g.:
//
//	-ldflags "-X github.com/accreleus/quasar/control-plane/internal/buildinfo.version=0.2.0 ..."
//
// Left empty they read as unstamped. `var` (not `const`) and package-level:
// -X only writes to an initialized string variable.
var (
	version      = ""
	sourceCommit = ""
	builtAt      = ""
)

// UnknownVersion is what an unstamped build calls itself. Never null on the
// wire: a build always has some answer to "what are you", and this is the
// honest one (control-api.md `PlatformIdentity.version`).
const UnknownVersion = "dev"

// Identity is the wire shape of `PlatformIdentity` (openapi.yaml). The two
// pointer fields are null on an unstamped build.
type Identity struct {
	Version       string  `json:"version"`
	SourceCommit  *string `json:"source_commit"`
	BuiltAt       *string `json:"built_at"`
	SchemaVersion int     `json:"schema_version"`
}

// schemaVersion is derived once, at package init, from the embedded migration
// set. A failure here is a build defect (the embed pattern matched nothing),
// not a runtime condition, so it panics at init rather than serving a wrong
// ordering key.
var schemaVersion = mustHighestMigration(migrations.FS)

var migrationName = regexp.MustCompile(`^(\d+)_.*\.up\.sql$`)

// HighestMigration returns the largest `NNNN` prefix among the `*.up.sql`
// files in fsys. Exported for the test; production reads SchemaVersion.
func HighestMigration(fsys fs.FS) (int, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return 0, fmt.Errorf("read migrations: %w", err)
	}
	highest := 0
	for _, e := range entries {
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		// Atoi on the captured digits rather than a Sscanf over the name:
		// "0074" must be 74, and a name that is not `NNNN_…up.sql` must be
		// skipped outright rather than silently becoming 0.
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, fmt.Errorf("migration %q: %w", e.Name(), err)
		}
		if n > highest {
			highest = n
		}
	}
	if highest == 0 {
		return 0, fmt.Errorf("no migrations found: the embedded set is empty")
	}
	return highest, nil
}

func mustHighestMigration(fsys fs.FS) int {
	n, err := HighestMigration(fsys)
	if err != nil {
		panic("buildinfo: " + err.Error())
	}
	return n
}

// SchemaVersion is the highest migration version this binary embeds.
func SchemaVersion() int { return schemaVersion }

// Version is the stamped semver without a leading "v", or UnknownVersion.
func Version() string { return normalizeVersion(version) }

// Get returns this binary's identity.
func Get() Identity {
	return Identity{
		Version:       normalizeVersion(version),
		SourceCommit:  normalizeCommit(sourceCommit),
		BuiltAt:       normalizeBuiltAt(builtAt),
		SchemaVersion: schemaVersion,
	}
}

// normalizeVersion strips a leading "v" (the release lever stamps from a
// `vX.Y.Z` tag, the contract serves it without) and maps every unstamped or
// placeholder form onto UnknownVersion. "unknown" is included because the
// image build args default to that literal.
func normalizeVersion(v string) string {
	switch v {
	case "", "unknown", "dev":
		return UnknownVersion
	}
	if len(v) > 1 && v[0] == 'v' {
		return v[1:]
	}
	return v
}

var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// normalizeCommit returns the commit only when it is a full 40-character
// lowercase hex sha, which is what `PlatformIdentity.source_commit` promises.
// Anything else — empty, the `unknown` build-arg default, an abbreviated sha —
// is null: a field documented as a full sha must not sometimes be a short one.
func normalizeCommit(c string) *string {
	if !fullCommit.MatchString(c) {
		return nil
	}
	s := c
	return &s
}

// normalizeBuiltAt reparses and re-formats the stamp so the served value is
// RFC3339 UTC whatever the build path wrote, and an unparseable stamp (the
// `unknown` build-arg default included) is null rather than garbage a client
// would try to render as a date.
func normalizeBuiltAt(b string) *string {
	t, err := time.Parse(time.RFC3339, b)
	if err != nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// LogAttrs is what the startup line reports. Unstamped fields say "unknown"
// rather than being omitted — an operator reading logs needs to see that the
// binary does not know what it is.
func LogAttrs() []any {
	id := Get()
	commit, built := "unknown", "unknown"
	if id.SourceCommit != nil {
		commit = *id.SourceCommit
	}
	if id.BuiltAt != nil {
		built = *id.BuiltAt
	}
	return []any{
		"version", id.Version,
		"source_commit", commit,
		"built_at", built,
		"schema_version", id.SchemaVersion,
	}
}
