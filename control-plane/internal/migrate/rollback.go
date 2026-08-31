package migrate

import (
	"fmt"
	"regexp"
)

// versionNotFoundPattern matches golang-migrate's raw error for a database
// migration version that has no corresponding migration file in the source
// (control-plane/migrations). This happens when the database's applied
// schema_migrations.version is HIGHER than the highest version this binary
// was built with. The library's own text looks like:
//
//	no migration found for version 25: read down for version 25
//
// See github.com/golang-migrate/migrate/v4 migrate.go (versionExists) and
// source/iofs/iofs.go (ReadDown) for where that text comes from.
var versionNotFoundPattern = regexp.MustCompile(`no migration found for version (\d+)`)

// translateRollbackError inspects an error returned by (*migrate.Migrate).Up
// and, if it matches the "no migration found for version N" signature,
// replaces it with a message that names the cause (this binary is older
// than the database schema) and the fix (redeploy the ref that embeds the
// migration), instead of the raw golang-migrate text.
//
// It returns nil if err does not match the signature, so the caller falls
// back to its normal wrapping.
func translateRollbackError(err error) error {
	if err == nil {
		return nil
	}
	m := versionNotFoundPattern.FindStringSubmatch(err.Error())
	if m == nil {
		return nil
	}
	version := m[1]
	return fmt.Errorf(
		"this control-plane binary is older than the database schema: "+
			"the database has migration version %s applied, but this binary "+
			"does not embed a migration %s (cause: it was built from an older "+
			"commit or release, most likely after a `git checkout`/rollback to "+
			"an earlier ref without also rolling back the database). "+
			"fix: redeploy the ref or commit that embeds migration version %s "+
			"(for example `deploy/redeploy.sh <profile> <ref>`, or "+
			"`make redeploy-cp HOST=<host>` for a control-plane-only change) "+
			"rather than continuing to run this binary. "+
			"never roll a binary back below the database's applied migration "+
			"version; see docs/upgrading.md for the full recovery procedure. "+
			"(original error: %w)",
		version, version, version, err,
	)
}
