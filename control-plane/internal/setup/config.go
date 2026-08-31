// Package setup implements the first-run setup wizard's server side (Spec B W1:
// docs/design/plans/2026-08-07-first-run-wizard-spec.md). It is the ONE new
// privileged surface the wizard needs:
//
//   - GET  /v1/setup/status — unauthenticated routing booleans {admin_exists,
//     setup_completed}, so the SPA sends a virgin instance to /setup instead of
//     an unsatisfiable login screen. Leaks nothing else.
//   - POST /v1/setup/claim  — creates the FIRST admin, gated by a per-boot setup
//     token (NOT a bearer token). Login-shaped 201 so the operator is signed in.
//
// Both routes are registered unconditionally — claim self-disables via its 409
// gate once any admin exists, and registration-time gating would be wrong the
// moment it mattered: the first admin appears while the process runs. Token
// custody: 32 random bytes in a 0600 file only, never the log (unlike the
// dev-agent key #399, this token creates a production instance's first admin
// and log streams reach principals with no host access), never persisted
// across boots, constant-time compared, no missing-vs-wrong 401 distinction.
// The file write is mandatory: failure fails boot rather than degrading to
// log-only.
package setup

import (
	"os"
	"strings"
	"time"
)

// Environment variables. Documented in docs/configuration.md (the authority for
// every env var per CLAUDE.md) — keep that row in sync with any change here.
const (
	// EnvTokenPath overrides where the per-boot setup token file is written.
	EnvTokenPath = "QUASAR_SETUP_TOKEN_PATH"
)

// DefaultTokenPath is where the per-boot setup token is written for host-side
// retrieval (`docker exec <cp> cat /run/quasar/setup-token`). Anyone who can
// read it already has host access — the same trust model as the dev-agent key.
const DefaultTokenPath = "/run/quasar/setup-token"

// TokenHeader carries the per-boot setup token on POST /v1/setup/claim.
const TokenHeader = "X-Quasar-Setup-Token"

// Routes served by this package. Exported so main/app wiring and tests can
// assert them without restating the strings. Registered in
// Services.RegisterRoutes (the surface TestOpenAPIDrift records), so they MUST
// match protocol/openapi.yaml exactly.
const (
	RouteStatus   = "GET /v1/setup/status"
	RouteClaim    = "POST /v1/setup/claim"
	RouteComplete = "POST /v1/setup/complete"
)

// Claim failure rate-limiting (finding 6): every rejected claim WARNs with the
// caller's address, so without a bound an unauthenticated attacker floods the
// log stream. Mirrors the signaling limiter (internal/signal/handler.go) —
// generous enough that a fat-fingered token never locks the operator out.
const (
	claimFailureLimit  = 10
	claimFailureTTL    = time.Minute
	claimFailureMaxIPs = 4096
	// Reserve bounds: admission and accounting must be one atomic operation, or
	// a concurrent burst passes Allow before any Failure is recorded. Per-IP 10 /
	// global 256 mirror internal/signal and internal/agentws.
	claimMaxInFlightIP = 10
	claimMaxInFlight   = 256
)

// TokenPath returns the configured setup-token file path (env override or
// default).
func TokenPath() string {
	if v := strings.TrimSpace(os.Getenv(EnvTokenPath)); v != "" {
		return v
	}
	return DefaultTokenPath
}
