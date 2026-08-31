package signal

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"

	"github.com/accreleus/quasar/control-plane/internal/origins"
	"testing"
)

type stubOriginStore struct {
	list  []string
	err   error
	calls atomic.Int32
}

func (s *stubOriginStore) AllowedOrigins(context.Context) ([]string, error) {
	s.calls.Add(1)
	return s.list, s.err
}

// handlerWith builds a signal handler over a resolver constructed from the same
// inputs production uses. It goes through internal/origins deliberately: these
// tests assert what the ENFORCER does, and the enforcer's rules now live in the
// shared resolver, so a test that reimplemented them would be testing itself.
func handlerWith(env string, envSet bool, store *stubOriginStore) *Handler {
	var st origins.Store
	if store != nil {
		st = store
	}
	return NewHandler(nil, nil, nil, quietLogger(), origins.NewResolver(env, envSet, st, quietLogger()))
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func request(origin, host string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/signal", nil)
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

// TestDatabaseAllowListIsHonouredWithoutRestart is the point of §S6e: an origin
// an admin adds to the column is accepted with no process restart.
func TestDatabaseAllowListIsHonouredWithoutRestart(t *testing.T) {
	store := &stubOriginStore{}
	// No env argument at all → the environment is UNSET, so the column governs.
	h := handlerWith("", false, store)

	if h.originAllowed(request("https://proxy.example", "internal.local")) {
		t.Fatal("an unlisted cross-host origin was allowed")
	}
	store.list = []string{"https://proxy.example"}
	// Defeat the TTL cache: it exists to make one handshake one query, not to
	// delay an admin's edit beyond a couple of seconds.
	h.resolver.Invalidate()
	if !h.originAllowed(request("https://proxy.example", "internal.local")) {
		t.Fatal("an origin added to the database column was still refused — the whole point of §S6e is that this needs no restart")
	}
}

// TestEnvOverridesDatabase pins the precedence rule that makes the migration a
// no-op on upgrade. The ENVIRONMENT wins — the mirror of internal/secrets, and
// deliberately so: this is a security control some deployments pin in their
// compose file, and a UI edit must not widen a list an operator hardened there.
func TestEnvOverridesDatabase(t *testing.T) {
	store := &stubOriginStore{list: []string{"https://from-db.example"}}
	h := handlerWith("https://from-env.example", true, store)

	if !h.originAllowed(request("https://from-env.example", "internal.local")) {
		t.Error("the env-configured origin was refused")
	}
	if h.originAllowed(request("https://from-db.example", "internal.local")) {
		t.Error("a database origin was honoured while the environment pinned the list")
	}
	if store.calls.Load() != 0 {
		t.Errorf("the database was queried %d times while the env override was in force", store.calls.Load())
	}
}

// TestEnvSetButEmptyPinsTheListOff covers the case a presence check exists for:
// QUASAR_ALLOWED_ORIGINS="" is "explicitly nothing", not "unset".
func TestEnvSetButEmptyPinsTheListOff(t *testing.T) {
	store := &stubOriginStore{list: []string{"https://from-db.example"}}
	h := handlerWith("", true, store)

	if h.originAllowed(request("https://from-db.example", "internal.local")) {
		t.Fatal("the database column was consulted although the environment explicitly set an empty list")
	}
	// Same-origin still passes — an empty list is not deny-all.
	if !h.originAllowed(request("https://internal.local", "internal.local")) {
		t.Fatal("a same-origin request was refused; an empty allow-list must not mean deny-all")
	}
}

// TestDatabaseFailureDegradesToEmptyNotToAllow is the fail-closed property: a
// database blip must land on the behaviour a fresh install has, never a wider one.
func TestDatabaseFailureDegradesToEmptyNotToAllow(t *testing.T) {
	store := &stubOriginStore{err: errors.New("connection refused")}
	h := handlerWith("", false, store)

	if h.originAllowed(request("https://anything.example", "internal.local")) {
		t.Fatal("a database read failure widened the allow-list")
	}
	if !h.originAllowed(request("https://internal.local", "internal.local")) {
		t.Error("same-origin must still pass during a database outage")
	}
	if !h.originAllowed(request("", "internal.local")) {
		t.Error("a request with no Origin (a non-browser client) must still pass")
	}
}

// TestSameOriginComparisonIncludesPort guards the assumption internal/access
// mirrors. If this rule ever changes, access-check's report must change with it.
func TestSameOriginComparisonIncludesPort(t *testing.T) {
	h := handlerWith("", false, &stubOriginStore{})
	if !h.originAllowed(request("https://quasar.test:8443", "quasar.test:8443")) {
		t.Error("identical authority should pass")
	}
	if h.originAllowed(request("https://quasar.test:9999", "quasar.test:8443")) {
		t.Error("a differing PORT is a different origin and must not pass same-origin")
	}
}

// TestAllowListIsCachedWithinOneHandshake covers why the TTL exists: originAllowed
// is called twice per handshake (the explicit pre-Upgrade check and gorilla's
// CheckOrigin), and those two must agree and cost one query, not two.
func TestAllowListIsCachedWithinOneHandshake(t *testing.T) {
	store := &stubOriginStore{list: []string{"https://a.example"}}
	h := handlerWith("", false, store)
	for i := 0; i < 5; i++ {
		h.originAllowed(request("https://a.example", "internal.local"))
	}
	if got := store.calls.Load(); got != 1 {
		t.Errorf("database queried %d times for 5 checks in the same TTL window; want 1", got)
	}
}

// TestStoredOriginIsRenormalized: a row edited directly in psql must not be able
// to introduce a value the socket compares differently from the way the PATCH
// handler validated it.
func TestStoredOriginIsRenormalized(t *testing.T) {
	store := &stubOriginStore{list: []string{"https://EVIL.example/path", "HTTPS://Good.Example"}}
	h := handlerWith("", false, store)

	if h.originAllowed(request("https://evil.example", "internal.local")) {
		t.Error("a malformed stored entry was honoured")
	}
	if !h.originAllowed(request("https://good.example", "internal.local")) {
		t.Error("a stored entry differing only in case should normalize and match")
	}
}
