package crud

// seeder_entitlement_test.go — steam-library-discovery Phase 2, §6.4's corollary
// applied to the RAW-SQL seeders, not just to POST /v1/apps.
//
// THE REGRESSION THIS CATCHES IS INVISIBLE ON EVERY EXISTING DEPLOYMENT. The
// 0043 backfill grants ('all') to every app that exists AT MIGRATION TIME, so a
// seeder that writes an apps row and no entitlement row is harmless on any box
// that was already seeded — and fatal on a fresh stack, a rebuilt dev box, or a
// restored-then-reseeded host, where the app arrives AFTER 0043 and the backfill
// can never reach it.
//
// Concretely: scripts/dev/seed-diagnostics-app.sh seeds the app that
// GetDiagnosticsAppID (internal/session/encoder_cert.go) resolves by name and
// that cert_handler.go launches through ScheduleAndCreate. Without an
// entitlement the SPT-06 certification bench dies with "not entitled to this
// app" and writes no cert row — on a box where nothing else looks wrong.
//
// This is a SOURCE test, not a DB test: it needs no Postgres and runs in the
// default `go test ./...`, which is the point. The behavioural half of the claim
// (an app with no entitlement really is unlaunchable) is proved by
// TestLaunchRejectsUnentitledApp in internal/session; this asserts the seeders
// hold up their end.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// insertAppsRe matches an apps INSERT in a shell heredoc, tolerating the
// whitespace and line breaks the seeders actually use.
var insertAppsRe = regexp.MustCompile(`(?is)INSERT\s+INTO\s+apps\b`)

// insertEntitlementsRe is the corresponding grant.
var insertEntitlementsRe = regexp.MustCompile(`(?is)INSERT\s+INTO\s+entitlements\b`)

// deployDir locates deploy/ relative to this test file
// (control-plane/internal/crud → ../../../deploy).
func deployDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "deploy")
}

// TestSeedersGrantAnEntitlement asserts that every script under deploy/ which
// INSERTs an apps row also INSERTs an entitlements row.
//
// It is a coarse instrument on purpose. A precise one would have to parse shell
// heredocs with variable interpolation, and the failure mode being guarded
// against is not subtle — it is "someone added a seeder and forgot entitlements
// exist". A new seeder that legitimately wants an unentitled app (a Phase 4
// discovered-tile fixture is the plausible case) should say so by naming itself
// in the exemption list below, with a reason, rather than by deleting this test.
func TestSeedersGrantAnEntitlement(t *testing.T) {
	// Scripts allowed to INSERT INTO apps with no entitlement. Empty today.
	// Phase 4's discovered tiles are created with NO 'all' row by design (§6.4),
	// so if a fixture for those ever lands here it belongs in this list.
	exempt := map[string]string{}

	dir := deployDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read deploy/: %v", err)
	}

	var checked []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sh") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if !insertAppsRe.Match(body) {
			continue
		}
		checked = append(checked, e.Name())
		if reason, ok := exempt[e.Name()]; ok {
			t.Logf("%s: exempt (%s)", e.Name(), reason)
			continue
		}
		if !insertEntitlementsRe.Match(body) {
			t.Errorf(`deploy/%s INSERTs an apps row but never INSERTs an entitlements row.

An app created after migration 0043 with no entitlement is INVISIBLE in
GET /v1/apps and UNLAUNCHABLE (POST /v1/sessions answers 403 "not entitled to
this app"). The 0043 backfill only covers apps that existed when it ran, so this
breaks fresh stacks and rebuilt boxes while looking fine on every box that is
already seeded.

Add, after the app INSERT:

    INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
    SELECT 'all', NULL, id, 'migration' FROM apps WHERE name = '<the app name>'
    ON CONFLICT DO NOTHING;

(steam-library-discovery spec §6.4. If the app is deliberately meant to be
invisible, add this script to the exemption list in this test with a reason.)`, e.Name())
		}
	}

	// Guard the guard: if the regex stops matching (a seeder switched to \copy, or
	// the scripts moved), this test would silently pass over an empty set.
	if len(checked) == 0 {
		t.Fatal("no deploy/*.sh script matched INSERT INTO apps — the detector is broken, " +
			"or the seeders moved; this test would pass vacuously")
	}
	sort.Strings(checked)
	t.Logf("checked %d app-seeding script(s): %s", len(checked), strings.Join(checked, ", "))
}
