package migrate

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// rawGolangMigrateErr reproduces the exact text golang-migrate/migrate v4
// produces for a database version with no corresponding migration file
// (migrate.go versionExists wrapping source/iofs.ReadDown's fs.PathError).
// Kept inline rather than importing the library so this test does not
// depend on the library's error text staying importable/constructible from
// this package; it pins the literal string the ticket (#514) was filed
// against.
func rawGolangMigrateErr(version string) error {
	inner := fmt.Errorf("read down for version %s: file does not exist", version)
	return fmt.Errorf("no migration found for version %s: %w", version, inner)
}

func TestTranslateRollbackError_DetectsVersionMismatch(t *testing.T) {
	raw := rawGolangMigrateErr("25")

	got := translateRollbackError(raw)
	if got == nil {
		t.Fatalf("translateRollbackError(%v) = nil, want a translated error", raw)
	}

	msg := got.Error()
	for _, want := range []string{
		"older than the database schema",
		"migration version 25",
		"redeploy",
		"docs/upgrading.md",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("translated error missing %q\ngot: %s", want, msg)
		}
	}

	// The raw library error must still be reachable via errors.Is/Unwrap so
	// callers (and tests) that inspect the chain don't lose it.
	if !errors.Is(got, raw) {
		t.Errorf("translated error does not wrap the original: %v", got)
	}
}

func TestTranslateRollbackError_ExtractsVersionNumber(t *testing.T) {
	got := translateRollbackError(rawGolangMigrateErr("42"))
	if got == nil {
		t.Fatal("expected a translated error")
	}
	if !strings.Contains(got.Error(), "42") {
		t.Errorf("expected translated error to name version 42, got: %s", got.Error())
	}
	if strings.Contains(got.Error(), "version 25") {
		t.Errorf("translated error leaked an unrelated version number: %s", got.Error())
	}
}

func TestTranslateRollbackError_PassesThroughUnrelatedErrors(t *testing.T) {
	for _, err := range []error{
		nil,
		errors.New("connection refused"),
		errors.New("dirty database version 5. Fix and force version."),
		fmt.Errorf("open migrations db: %w", errors.New("dial tcp: timeout")),
	} {
		if got := translateRollbackError(err); got != nil {
			t.Errorf("translateRollbackError(%v) = %v, want nil (not a version-mismatch error)", err, got)
		}
	}
}
