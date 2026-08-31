// Package devauth implements the dev-only agent-auth endpoint (#399): a
// throwaway, auto-reaped identity minted for automated UI-validation agents so no
// shared, long-lived credential has to exist in a repo.
//
// Read the gating order before changing anything here — it is fail-closed and the
// order matters (spec: docs/design/plans/2026-08-07-dev-agent-auth-spec.md §2):
//
//  1. Off unless QUASAR_DEV_AGENT_AUTH=1. Absent the flag the route is not
//     registered at all — a 404 from the mux, never a 403 guard a routing
//     mistake could widen. Hence registration from cmd/quasar-control/main.go,
//     never Services.RegisterRoutes (the surface TestOpenAPIDrift records).
//  2. Flag set while QUASAR_ENV=production ⇒ the process refuses to boot.
//  3. The key is per-boot and random. Nothing persists it across boots.
//  4. Every enabled boot emits a WARN banner naming the flag.
//  5. Wrong or missing key ⇒ 401 with no message or timing distinction.
//
// The endpoint is provisioning, not a bypass: it creates a real users row and a
// real token, and RequireAuth → RequireAdmin is untouched.
package devauth

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Environment variables (documented in docs/configuration.md).
const (
	EnvFlag    = "QUASAR_DEV_AGENT_AUTH" // "1" enables; anything else leaves the route unregistered
	EnvName    = "QUASAR_ENV"            // "production" makes the flag fatal
	EnvKeyPath = "QUASAR_DEV_AGENT_KEY_PATH"
)

// DefaultKeyPath is where the per-boot key is written for host-side tooling
// (`docker exec <cp> cat /run/quasar/dev-agent-key`). A caller who can read it
// already has host access.
const DefaultKeyPath = "/run/quasar/dev-agent-key"

// Route is the single path this package serves. Exported so tests can assert its
// presence/absence without restating the string.
const Route = "POST /v1/dev/agent-session"

// KeyHeader carries the per-boot secret.
const KeyHeader = "X-Quasar-Dev-Key"

// TTL policy (protocol/control-api.md §"Dev-only agent auth").
const (
	DefaultTTL = 30 * time.Minute
	MinTTL     = 60 * time.Second
	MaxTTL     = 8 * time.Hour
)

// ReapInterval is how often the reaper sweeps expired identities. It also runs
// once at boot, so a crashed run's leftovers are gone before the first request.
const ReapInterval = time.Minute

// Config is the parsed environment for this feature.
type Config struct {
	Enabled bool
	// Env is the raw QUASAR_ENV value, kept for the production refusal below.
	Env string
	// KeyPath is where the per-boot key file is written.
	KeyPath string
}

// LoadConfig reads the environment. It performs no validation — call Validate.
func LoadConfig() Config {
	return Config{
		Enabled: strings.TrimSpace(os.Getenv(EnvFlag)) == "1",
		Env:     strings.TrimSpace(os.Getenv(EnvName)),
		KeyPath: envOr(EnvKeyPath, DefaultKeyPath),
	}
}

// Validate is gate 2: a hard boot refusal, not "enabled=false with a warning" —
// a production deployment carrying the flag must be seen and fixed.
func (c Config) Validate() error {
	if c.Enabled && strings.EqualFold(c.Env, "production") {
		return fmt.Errorf("%s=1 is refused while %s=production — dev-only agent auth must never run in production",
			EnvFlag, EnvName)
	}
	return nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
