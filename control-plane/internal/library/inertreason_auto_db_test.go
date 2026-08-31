package library

// inertreason_auto_db_test.go — #472: `inertReason`'s storage-driver branch
// must react to the RESOLVED driver, not the raw stored `storage_provider`
// setting. The bug this closes: a fresh install's storage_provider is 'auto'
// (never the literal string "volume"), so the original `provider ==
// settings.StorageVolume` compare never fired for the exact configuration it
// was written for — a rootless 'auto' instance. See handler.go's inertReason /
// noHostHasStorageRoot doc comments for the full reasoning, including why the
// fix is INSTANCE-WIDE ("no host has a root") rather than reporting a mix as
// inert.
//
// UPDATED 2026-08-10, when the per-host storage root became THE storage
// control. What a rootless-'auto' instance DOES changed — it no longer
// downgrades to the volume driver, it fails the launch — so these tests now
// assert the reason names the missing STORAGE ROOT rather than a fallback that
// no longer happens. What #472 was really about is untouched: the surfaces must
// not present that instance as healthy.
//
// TEST_DATABASE_URL-gated like every other DB test here: without it these
// SKIP. Use `scripts/dev/dev.sh go-test-db` or `make test-db`.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/settings"
	"github.com/accreleus/quasar/control-plane/internal/storage"
)

// fakeHomeRoots is a storage.HostRootResolver backed by a plain map, so a test
// can give two hosts different effective roots (or none) without going
// through hostcfg/agent reporting.
type fakeHomeRoots map[string]string

func (f fakeHomeRoots) HomeRoot(_ context.Context, hostID string) (string, error) {
	return f[hostID], nil
}

// newAutoServer wires the library handler with a REAL storage.Manager (not
// testStorageManager's fixed-root stub the rest of this package's tests use)
// so its resolveDriver actually varies per host — the thing #472's fix
// depends on. fakeSettings already satisfies storage.SettingsReader
// (StorageProvider), so the same `set` drives both the raw setting the
// handler reads AND the resolution the driver performs, exactly like
// production's homeProvider/settingsStore pairing (app.go).
func newAutoServer(t *testing.T, f fixture, set *fakeSettings, roots fakeHomeRoots) *httptest.Server {
	t.Helper()
	mgr := storage.New(f.pool, set, roots)
	h := NewHandler(f.store, mgr, set, NewAppDetails(false, quietLogger()), newTestResolver(set), quietLogger())
	mux := http.NewServeMux()
	h.Register(mux, func(next http.Handler) http.Handler { return next })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func addHost(t *testing.T, f fixture, nodeName string) string {
	t.Helper()
	h := sha256.Sum256([]byte(nodeName + "-secret"))
	var id string
	must(t, f.pool.QueryRow(context.Background(), `INSERT INTO hosts (node_name, status, node_secret_hash)
		VALUES ($1, 'online', $2) RETURNING id::text`, nodeName, hex.EncodeToString(h[:])).Scan(&id))
	return id
}

func inertReasonOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp, err := http.Get(srv.URL + "/v1/admin/library/status")
	must(t, err)
	defer resp.Body.Close()
	var body struct {
		InertReason string `json:"inert_reason"`
	}
	must(t, json.NewDecoder(resp.Body).Decode(&body))
	return body.InertReason
}

// TestInertReasonFreshInstallAutoNoHomeRootReportsNoRoot is #472's exact
// reproduction: storage_provider = 'auto' (the fresh-install default —
// migrations/0*_*.up.sql seeds `instance_settings.storage_provider = 'auto'`),
// and the one registered host has never had a home root set (no override, no
// agent-reported effective_settings, no QUASAR_HOME_ROOT). No host can hold a
// walkable home, and the status/force-scan surfaces must say so — this is the
// case the OLD literal-"volume" compare silently missed.
func TestInertReasonFreshInstallAutoNoHomeRootReportsNoRoot(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)

	set := &fakeSettings{enabled: true, provider: settings.StorageAuto}
	srv := newAutoServer(t, f, set, fakeHomeRoots{}) // no host has a root

	got := inertReasonOf(t, srv)
	if got == "" {
		t.Fatal("inert_reason is empty for auto+no-home-root; #472 requires a reason to fire here")
	}
	// It must name the missing thing in the operator's vocabulary AND where to
	// set it — this sentence is the only place they are told.
	for _, want := range []string{"storage root", "Admin → Hosts"} {
		if !strings.Contains(got, want) {
			t.Errorf("inert_reason = %q, want it to contain %q", got, want)
		}
	}

	// The force-scan surface shares the SAME function (handler.go's inertReason
	// doc: "ONE FUNCTION FOR TWO SURFACES ON PURPOSE") and must agree.
	code, res := postScan(t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("force scan status %d, want 200", code)
	}
	if res.Queued != 0 {
		t.Errorf("force scan queued %d while no host has a storage root, want 0", res.Queued)
	}
	if !strings.Contains(res.InertReason, "storage root") {
		t.Errorf("force scan inert_reason = %q, want it to name the missing storage root", res.InertReason)
	}
}

// TestInertReasonAutoWithHomeRootIsLive — the same 'auto' setting is NOT inert
// once the host has an effective home root: auto resolves to local, discovery
// can actually walk it, and (the fixture already seeds a library-provider
// Steam app) nothing should block it.
func TestInertReasonAutoWithHomeRootIsLive(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)

	set := &fakeSettings{enabled: true, provider: settings.StorageAuto}
	srv := newAutoServer(t, f, set, fakeHomeRoots{f.host: "/data/quasar-homes"})

	if got := inertReasonOf(t, srv); got != "" {
		t.Errorf("inert_reason = %q, want empty — the host has an effective home root so auto resolves to local", got)
	}
}

// TestInertReasonAutoMixedHostsIsNotInert is the honesty check the spec calls
// for: with two hosts, one resolved to local and one to volume, discovery is
// NOT instance-wide blocked (the local host can still be scanned — the janitor
// and force-scan both work per (user, app, host) triple). Reporting this
// configuration as inert would be a false, imprecise per-host claim exactly of
// the kind #472's fix must not introduce in the other direction.
func TestInertReasonAutoMixedHostsIsNotInert(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	secondHost := addHost(t, f, "node-lib-2")

	set := &fakeSettings{enabled: true, provider: settings.StorageAuto}
	srv := newAutoServer(t, f, set, fakeHomeRoots{secondHost: "/data/quasar-homes"}) // f.host has none

	if got := inertReasonOf(t, srv); got != "" {
		t.Errorf("inert_reason = %q, want empty — one of two hosts resolves to local, so discovery is not instance-wide inert", got)
	}
}

// TestInertReasonAutoZeroHostsReportsNoRoot — no host registered at all is the
// degenerate case of "no host has a root": nothing can resolve to local, so it
// must report the same reason as the populated-but-rootless case.
func TestInertReasonAutoZeroHostsReportsNoRoot(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	set := &fakeSettings{enabled: true, provider: settings.StorageAuto}

	// A fixture-free fixture: no hosts, no apps, no users — just the store and
	// pool, which is all newAutoServer/inertReasonOf need.
	f := fixture{pool: pool, store: NewStore(pool)}
	_ = ctx
	srv := newAutoServer(t, f, set, fakeHomeRoots{})

	got := inertReasonOf(t, srv)
	if !strings.Contains(got, "storage root") {
		t.Errorf("inert_reason = %q, want the missing-storage-root reason for the zero-hosts case", got)
	}
}

// TestInertReasonExplicitLocalNoRootReportsNoRoot — the branch #472's fix did
// NOT cover, and which the 2026-08-10 change makes indistinguishable from the
// 'auto' case. Before, only 'auto' consulted the resolver, so a rootless
// 'local' instance — byte-for-byte as unable to scan anything — reported no
// reason at all. Now that 'auto' and 'local' are the same setting in different
// words, reporting one and not the other would be arbitrary.
func TestInertReasonExplicitLocalNoRootReportsNoRoot(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)

	set := &fakeSettings{enabled: true, provider: settings.StorageLocal}
	srv := newAutoServer(t, f, set, fakeHomeRoots{}) // no host has a root

	if got := inertReasonOf(t, srv); !strings.Contains(got, "storage root") {
		t.Errorf("inert_reason = %q, want the missing-storage-root reason for rootless 'local'", got)
	}
}

// TestInertReasonExplicitLocalWithRootIsLive — the mirror image, and the proof
// that the branch above reports a real condition rather than firing on the
// setting string.
func TestInertReasonExplicitLocalWithRootIsLive(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)

	set := &fakeSettings{enabled: true, provider: settings.StorageLocal}
	srv := newAutoServer(t, f, set, fakeHomeRoots{f.host: "/data/quasar-homes"})

	if got := inertReasonOf(t, srv); got != "" {
		t.Errorf("inert_reason = %q, want empty — 'local' with a root is the healthy configuration", got)
	}
}

// TestInertReasonExplicitVolumeStillReportsInert — #473 hard removal: a
// pre-upgrade database row that still carries the literal "volume" (settings
// validation rejects writing it going forward; migration 0069 coerces it, but
// this test simulates a row the migration somehow missed, or a fakeSettings
// double bypassing validation entirely) must still surface as inert, even
// WITH a host root — resolveDriver rejects "volume" outright now (its case
// returns ErrVolumeDriverRemoved before ever consulting the root), so
// noHostHasStorageRoot treats every host as unresolved and the instance is
// reported the same as a rootless one. There is no volume-specific reason
// text any more (reasonVolumeProvider was removed with it) — the point this
// test pins is that the removal FAILS CLOSED (still inert) rather than
// silently reading as healthy.
func TestInertReasonExplicitVolumeStillReportsInert(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)

	// "volume" is now a bare literal: internal/settings no longer exports a
	// constant for a value it rejects (#473) — fakeSettings is a test double
	// that bypasses that validation on purpose, to simulate a legacy row.
	set := &fakeSettings{enabled: true, provider: "volume"}
	srv := newAutoServer(t, f, set, fakeHomeRoots{f.host: "/data/quasar-homes"}) // even WITH a root

	got := inertReasonOf(t, srv)
	if !strings.Contains(got, "storage root") {
		t.Errorf("inert_reason = %q, want the missing-storage-root reason (volume never resolves) even though the host has a root", got)
	}
}
