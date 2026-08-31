package storage

// Pure unit tests — driver ref shapes and mount-string validation. The
// DB-backed EnsureHome/TouchUsed paths are exercised by the session package's
// home_test.go integration tests.

import (
	"context"
	"errors"
	"path"
	"strings"
	"testing"
)

const (
	u = "11111111-2222-3333-4444-555555555555"
	a = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

func TestLocalDriverRef(t *testing.T) {
	got := localDriver{root: "/data/quasar/homes"}.ref(homeKey{userID: u, appID: a, userSlug: "michael", appSlug: "steam-dev-" + a[:8]})
	want := "/data/quasar/homes/michael/steam-dev-" + a[:8]
	if got != want {
		t.Errorf("ref = %q, want %q", got, want)
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct{ name, id, want string }{
		{"michael", u, "michael"},                // lossless: used as-is
		{"admin", u, "admin"},                    // lossless
		{"qses_harness-2", u, "qses_harness-2"},  // _- allowed
		{"Steam (Dev)", a, "steam-dev-" + a[:8]}, // altered → short-id suffix
		{"Mike!", u, "mike-" + u[:8]},            // altered → suffix (collision-proof)
		{"", u, u},                               // empty → uuid fallback
		{"???", u, u},                            // fully stripped → uuid fallback
	}
	for _, c := range cases {
		if got := slugify(c.name, c.id); got != c.want {
			t.Errorf("slugify(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestValidateMount(t *testing.T) {
	valid := []string{
		// mountPattern's shape (source:containerPath:rw) is provider-agnostic —
		// this string is only exercising the pattern, not the (removed) volume
		// driver; validateMount has no notion of which driver produced it.
		"quasar-home-" + u + "-" + a + ":/home/quasar:rw",
		"/data/quasar/homes/" + u + "/" + a + ":/home/quasar:rw",
	}
	for _, m := range valid {
		if err := validateMount(m); err != nil {
			t.Errorf("validateMount(%q) = %v, want nil", m, err)
		}
	}
	invalid := []string{
		"vol:/home/quasar:ro",         // wrong mode
		"vol:/home/quasar",            // missing mode
		"vol:relative/path:rw",        // container path not absolute
		"vol:/home/../etc:rw",         // traversal
		"vol name:/home/quasar:rw",    // space
		"vol:/home/quasar:rw; rm -rf", // injection junk
		"",                            // empty
	}
	for _, m := range invalid {
		if err := validateMount(m); err == nil {
			t.Errorf("validateMount(%q) = nil, want error", m)
		}
	}
}

// driverName resolves the per-call driver for a Manager (no host root) and returns
// its provider name, failing the test on a resolution error.
func driverName(t *testing.T, m *Manager) string {
	t.Helper()
	drv, err := m.resolveDriver(context.Background(), u /*hostID, unused by fixed resolvers*/)
	if err != nil {
		t.Fatalf("resolveDriver: %v", err)
	}
	return drv.name()
}

func TestNewFromEnvAutoDefault(t *testing.T) {
	// Unset provider + no home root → auto → ErrNoHomeRoot. This assertion used
	// to read "→ volume": it is the whole 2026-08-10 change, in one line.
	t.Setenv("QUASAR_STORAGE_PROVIDER", "")
	t.Setenv("QUASAR_HOME_ROOT", "")
	m, err := NewFromEnv(nil)
	if err != nil {
		t.Fatalf("NewFromEnv (no env): %v", err)
	}
	if _, err := m.resolveDriver(context.Background(), u); !errors.Is(err, ErrNoHomeRoot) {
		t.Errorf("default resolution: err = %v, want ErrNoHomeRoot", err)
	}

	// Unset provider + home root set → auto → local.
	t.Setenv("QUASAR_HOME_ROOT", "/data/quasar/homes")
	m, err = NewFromEnv(nil)
	if err != nil {
		t.Fatalf("NewFromEnv (home root set): %v", err)
	}
	if got := driverName(t, m); got != "local" {
		t.Errorf("driver with QUASAR_HOME_ROOT = %q, want local", got)
	}

	// Explicit volume is REJECTED at construction (#473 hard removal) — it
	// never even gets to layer over the home root.
	t.Setenv("QUASAR_STORAGE_PROVIDER", "volume")
	t.Setenv("QUASAR_HOME_ROOT", "")
	if _, err := NewFromEnv(nil); !errors.Is(err, ErrVolumeDriverRemoved) {
		t.Errorf("NewFromEnv (explicit volume): err = %v, want ErrVolumeDriverRemoved", err)
	}

	// A relative QUASAR_HOME_ROOT is rejected at construction.
	t.Setenv("QUASAR_STORAGE_PROVIDER", "")
	t.Setenv("QUASAR_HOME_ROOT", "relative/root")
	if _, err := NewFromEnv(nil); err == nil {
		t.Error("NewFromEnv with relative QUASAR_HOME_ROOT: want error, got nil")
	}
}

// TestResolveDriverMatrix exercises the storage-config resolution table: the
// instance-wide provider layered over the session host's effective root. resolveDriver
// reads only the injected settings + root resolvers (no pool), so nil is safe here.
//
// THE TABLE IS THE SPEC, across two operator decisions: 'auto' with no root
// used to be "volume" and is now ErrNoHomeRoot (2026-08-10); explicit 'volume'
// used to keep working as a legacy opt-in and is now REJECTED outright (#473,
// 2026-08-25 hard removal — Quasar is unreleased, no back-compat owed).
func TestResolveDriverMatrix(t *testing.T) {
	cases := []struct {
		name        string
		provider    string
		root        string
		wantDriver  string // "" ⇒ expect an error
		wantNoRoot  bool   // and that error must be ErrNoHomeRoot
		wantRemoved bool   // and that error must be ErrVolumeDriverRemoved
	}{
		{name: "auto with root → local", provider: "auto", root: "/data/homes", wantDriver: "local"},
		{name: "auto empty provider with root → local", provider: "", root: "/data/homes", wantDriver: "local"},
		{name: "auto no root → loud error, never volume", provider: "auto", wantNoRoot: true},
		{name: "auto empty provider no root → loud error", provider: "", wantNoRoot: true},
		{name: "explicit local with root → local", provider: "local", root: "/data/homes", wantDriver: "local"},
		{name: "explicit local no root → loud error", provider: "local", wantNoRoot: true},
		{name: "explicit volume with root → removed", provider: "volume", root: "/data/homes", wantRemoved: true},
		{name: "explicit volume no root → removed", provider: "volume", wantRemoved: true},
		{name: "local with relative root → error", provider: "local", root: "relative/root"},
		{name: "auto with relative root → error", provider: "auto", root: "relative/root"},
		{name: "unknown provider → error", provider: "nfs", root: "/data/homes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := New(nil, fixedProvider(c.provider), fixedRoot(c.root))
			drv, err := m.resolveDriver(context.Background(), u)
			if c.wantDriver == "" {
				if err == nil {
					t.Fatalf("resolveDriver(%q,%q) = %q, want error", c.provider, c.root, drv.name())
				}
				// A no-root failure must be distinguishable from a malformed
				// one: library.Handler's inertReason branches on exactly this.
				if got := errors.Is(err, ErrNoHomeRoot); got != c.wantNoRoot {
					t.Fatalf("resolveDriver(%q,%q): errors.Is(ErrNoHomeRoot) = %v, want %v (err = %v)",
						c.provider, c.root, got, c.wantNoRoot, err)
				}
				if got := errors.Is(err, ErrVolumeDriverRemoved); got != c.wantRemoved {
					t.Fatalf("resolveDriver(%q,%q): errors.Is(ErrVolumeDriverRemoved) = %v, want %v (err = %v)",
						c.provider, c.root, got, c.wantRemoved, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveDriver(%q,%q): unexpected error %v", c.provider, c.root, err)
			}
			if drv.name() != c.wantDriver {
				t.Errorf("resolveDriver(%q,%q) = %q, want %q", c.provider, c.root, drv.name(), c.wantDriver)
			}
			// The local driver must carry a cleaned root.
			if c.wantDriver == "local" {
				if ld, ok := drv.(localDriver); !ok || ld.root != path.Clean(c.root) {
					t.Errorf("local driver root = %+v, want %q", drv, path.Clean(c.root))
				}
			}
		})
	}
}

// TestNoHomeRootErrorIsOperatorFacing pins the WORDING, not just the failure.
// The text an operator reads is half the fix here: the message this replaced
// ("storage_provider=local but host <uuid> has no effective home root (set the
// home_root host knob or QUASAR_HOME_ROOT)") named two internal settings, an env
// var and a UUID, and told nobody where to click. A future edit that quietly
// reverts it to developer-speak should fail a test, not a support conversation.
func TestNoHomeRootErrorIsOperatorFacing(t *testing.T) {
	// nil pool → hostLabel falls back to the raw id without touching a database.
	m := New(nil, fixedProvider("auto"), fixedRoot(""))
	_, err := m.resolveDriver(context.Background(), u)
	if err == nil {
		t.Fatal("resolveDriver with no root: want error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"storage root",  // names the thing that is missing, in the UI's vocabulary
		u,               // names WHICH host
		"cannot start",  // says what will not happen
		"Admin → Hosts", // says where to fix it
		"setup wizard",  // and the other place it can be fixed
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("no-root error is missing %q\ngot: %s", want, msg)
		}
	}
	// It must NOT reintroduce the internal vocabulary it replaced.
	for _, unwanted := range []string{"storage_provider=", "home_root host knob", "QUASAR_HOME_ROOT"} {
		if strings.Contains(msg, unwanted) {
			t.Errorf("no-root error leaks developer-speak %q\ngot: %s", unwanted, msg)
		}
	}
}

func TestEnsureHomeRejectsRelativeContainerPath(t *testing.T) {
	m := NewLocal(nil, "/data/quasar/homes") // pool untouched on this error path
	if _, err := m.EnsureHome(context.Background(), u, a, u, "relative/home"); err == nil ||
		!strings.Contains(err.Error(), "not absolute") {
		t.Errorf("EnsureHome with relative path: err = %v, want 'not absolute'", err)
	}
}
