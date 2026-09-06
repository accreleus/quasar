package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/db"
	"github.com/accreleus/quasar/control-plane/internal/ice"
	"github.com/accreleus/quasar/control-plane/internal/jobs"
	"github.com/accreleus/quasar/control-plane/internal/origins"
	"github.com/accreleus/quasar/control-plane/internal/semver"
	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// Config holds control-plane configuration read from the environment.
// Defaults and accepted values: docs/configuration.md.
type Config struct {
	ListenAddr string // e.g. ":8080"

	DatabaseURL string // postgres://user:pass@host/db?sslmode=...

	// Applied as Postgres RuntimeParams on every pooled connection, so a lock
	// pile-up parks one caller with an error rather than the whole pool (#416).
	DBStatementTimeout time.Duration // QUASAR_DB_STATEMENT_TIMEOUT (default 30s)
	DBLockTimeout      time.Duration // QUASAR_DB_LOCK_TIMEOUT (default 10s)

	LogLevel string // "debug"|"info"|"warn"|"error"

	AuthTokenTTL time.Duration // bearer-token lifetime (AUTH_TOKEN_TTL, e.g. "24h")

	// Fleet-wide fallback for node-agent enrollment (ENROLLMENT_TOKEN). Optional:
	// empty means only admin-minted per-host tokens enroll (#12).
	EnrollmentToken string

	// All three together provision the first admin at boot if none exists
	// (control-api.md §Authorization). Never "first to register wins".
	BootstrapAdminEmail    string // BOOTSTRAP_ADMIN_EMAIL
	BootstrapAdminUsername string // BOOTSTRAP_ADMIN_USERNAME
	BootstrapAdminPassword string // BOOTSTRAP_ADMIN_PASSWORD

	// Non-empty serves the built SPA on non-API paths: same-origin, no proxy.
	WebRoot string // QUASAR_WEB_ROOT (e.g. /app/web pointing at web/dist)

	// Empty / "spread" / "least-loaded" all select least-loaded (P3-02).
	PlacementPolicy string // QUASAR_PLACEMENT_POLICY

	// Advisory free-VRAM veto over the transactional encode-slot reservation
	// (#383 §4.3). Validated at startup: the veto SQL interpolates these into
	// `make_interval(secs => $n)`, where a bad value NULLs the HAVING clause
	// (silently fail-closed, inverting the intended fail-open). 0 omits the
	// clause: admission becomes slots-only.
	VramMinFreeMB int32 // QUASAR_VRAM_MIN_FREE_MB (default 1024)
	// Debit for launches no sample reflects yet; split from the floor.
	VramInflightEstimateMB int32 // QUASAR_VRAM_INFLIGHT_ESTIMATE_MB (default = floor)
	// Freshness window (older sample => the veto abstains) and the in-flight
	// debit's grace margin. 20s = 4x heartbeat, matching readDeadlineDur.
	VramStalenessSecs int32 // QUASAR_VRAM_STALENESS_SECS (default 20)

	// Seeds instance_settings.registration_mode on first boot only (LP-SEC-01).
	RegistrationMode string // REGISTRATION_MODE

	// Set => invite mint returns <base>/register?invite=<code>, else the UI does.
	PublicBaseURL string // PUBLIC_BASE_URL
	// Empty still enforces same-origin using the request Host.
	AllowedOrigins string // QUASAR_ALLOWED_ORIGINS
	// Presence, not non-emptiness. Since migration 0064 the list is an
	// admin-editable column and the env var overrides it, so set-but-empty must
	// mean "ignore the database" or an operator cannot pin the list off.
	AllowedOriginsSet bool

	// Handed to the client with each set of signaling coordinates
	// (control-api.md, SignalingCoords.ice_servers); nil means host candidates
	// only. Parsed at startup by internal/ice: a bad entry is otherwise
	// invisible until a peer connection fails to gather (#509).
	ICEServers []ice.Server // QUASAR_ICE_SERVERS (JSON array)

	// Optional second listener on the same handler, giving the browser a secure
	// context (required by Keyboard Lock / Gamepad). Plaintext ListenAddr is
	// unchanged: the node-agent enrolls and /health is checked over HTTP.
	TLS        string // QUASAR_TLS: "auto" (default) | "off"
	TLSAddr    string // QUASAR_TLS_ADDR, e.g. ":8443"
	TLSDir     string // QUASAR_TLS_DIR: where a generated self-signed pair is persisted
	TLSCert    string // QUASAR_TLS_CERT: operator-provided cert (PEM); used with TLSKey
	TLSKey     string // QUASAR_TLS_KEY: operator-provided key (PEM); used with TLSCert
	TLSHosts   string // QUASAR_TLS_HOSTS: extra comma-separated SAN hostnames/IPs
	PublicHost string // QUASAR_PUBLIC_HOST: added as a SAN on the self-signed cert when set

	// With HTTPS on, the plaintext listener 308s browser routes (426 for WS
	// upgrades). /health, /agent/ws and /v1/agent/* always stay plain HTTP.
	HTTPRedirect string // QUASAR_HTTP_REDIRECT: "auto" (default) | "off"
	// Redirect Location port; defaults to TLSAddr's, set when published elsewhere.
	TLSRedirectPort string // QUASAR_TLS_REDIRECT_PORT

	// The web UI is ~15 MB uncompressed on a cold load; off is for sitting
	// behind a compressing proxy, not a sensible default (#386).
	Compression bool // QUASAR_COMPRESSION (default true)

	// Independent of LOG_LEVEL; legacy booleans still parse. Default "errors"
	// because the #517 audit measured 60-120 routine lines/minute from one
	// session, drowning `docker logs` with no knob short of LOG_LEVEL, which
	// also silences wanted lifecycle lines.
	AccessLog string // QUASAR_ACCESS_LOG (default "errors"): "off" | "errors" | "all"

	// Comma-separated CIDRs of reverse proxies this deployment operates (#438).
	// ratelimit.ClientIP reads X-Forwarded-For only when the direct peer falls
	// inside one, then walks the chain right-to-left to the first that is not.
	// Empty means never read the header; trusting a network you do not operate
	// hands anyone on it unlimited rate-limit budgets, and a malformed entry
	// fails startup rather than silently sharing one budget across all clients
	// (the lockout-DoS #438 closes). Behind the hardened Caddy overlay this is
	// the compose project's own bridge subnet (`docker network inspect
	// <project>_default`), never the enclosing 172.16.0.0/12, which spans every
	// Docker bridge on the box.
	TrustedProxies []*net.IPNet // QUASAR_TRUSTED_PROXIES (default empty)

	// Parsed but probably not meant: logged WARN by main once the logger exists
	// (config runs before it). Never branched on.
	Warnings []string

	// Native-client version handshake (P9-08 / #236); empty is permissive and
	// clients sending no version always proceed. Min is a hard floor (426
	// client_too_old at login), Latest is advisory and client-enforced.
	MinClientVersion    string // QUASAR_MIN_CLIENT_VERSION
	LatestClientVersion string // QUASAR_LATEST_CLIENT_VERSION

	// Cover artwork (UI-P7) ships dark: an empty ArtworkAPIKey constructs no
	// provider, so no third-party request is made and every app keeps
	// cover_url = NULL. ArtworkDir is independent of the key: local caching,
	// admin upload and the override must work with no provider at all.
	ArtworkDir      string        // QUASAR_ARTWORK_DIR
	ArtworkProvider string        // QUASAR_ARTWORK_PROVIDER: "steamgriddb" (default) | "none"
	ArtworkAPIKey   string        // QUASAR_STEAMGRIDDB_API_KEY (secret; never logged)
	ArtworkMaxBytes int32         // QUASAR_ARTWORK_MAX_BYTES
	ArtworkInterval time.Duration // QUASAR_ARTWORK_SWEEP_INTERVAL

	// A non-terminal session's samples and trace events are trimmed to Rolling;
	// on a terminal state that window freezes and is kept for PostMortem, then
	// swept. Captures (diag.*) are exempt. PostMortem must be >= Rolling (a
	// shorter one deletes the evidence it exists to keep), checked at load.
	TelemetryRollingWindow       time.Duration // QUASAR_TELEMETRY_ROLLING_WINDOW (default 1h)
	TelemetryPostMortemRetention time.Duration // QUASAR_TELEMETRY_POSTMORTEM_RETENTION (default 24h)

	// Scan cadence per (user, provider app, host) (internal/library, spec
	// §11.2). Zero disables discovery regardless of the database flag: an
	// operator must be able to guarantee no third-party call without database
	// access. Since migration 0047 this overrides
	// instance_settings.library_discovery_interval_minutes; the Set flag lets
	// internal/library.NewResolver (it takes both) tell "pinned in the
	// environment" from "read the column".
	LibraryScanInterval    time.Duration // QUASAR_LIBRARY_SCAN_INTERVAL, meaningful only if Set
	LibraryScanIntervalSet bool          // whether QUASAR_LIBRARY_SCAN_INTERVAL was present in the environment

	// The §8.3 fifth rung: Valve's undocumented store `appdetails` endpoint,
	// consulted only for appids the denylist ladder would publish, where a
	// `type` other than "game" suppresses them. Off by default — it discloses
	// installed appids to a third party, the privacy class UI-P7 rejected. A
	// supplement to the denylist, never a replacement: an outage degrades to
	// the denylist alone. Same migration-0047 override as LibraryScanInterval.
	SteamAppDetailsLookup    bool // QUASAR_STEAM_APPDETAILS_LOOKUP, meaningful only if Set
	SteamAppDetailsLookupSet bool // whether QUASAR_STEAM_APPDETAILS_LOOKUP was present in the environment

	// Deployment-level knobs for internal/jobs; per-job enable/interval/window
	// live in the database. JobsEnabled is a kill switch, not a mode: false
	// means an adopted job does not run at all, since it no longer owns a
	// ticker. Per-job `enabled` is the way to stop one job.
	JobsEnabled bool // QUASAR_JOBS
	// Control-plane trigger latency; the agent uses QUASAR_JOB_POLL_SECS.
	JobsTickSecs int32 // QUASAR_JOBS_TICK_SECS
	// IANA zone seeding a new job's run window; never re-applied to an existing
	// row. Unknown fails startup — a silent UTC fallback fires the window at
	// the wrong hour with nothing saying why.
	JobsTimezone string // QUASAR_JOBS_TIMEZONE
	// Row cap, and the seed default for jobs.history_limit.
	JobsRunRetention int32 // QUASAR_JOBS_RUN_RETENTION
	// Age cap applied in addition to the row cap; 0 disables the age rule.
	JobsRunRetentionDays int32 // QUASAR_JOBS_RUN_RETENTION_DAYS
	// Silence before a claimed run is aborted and re-materialized (covers an
	// agent that died mid-run). Must exceed the longest expected job.
	JobsClaimTimeoutSecs int32 // QUASAR_JOBS_CLAIM_TIMEOUT_SECS

	// AES-256-GCM master key for instance_secrets (internal/secrets): base64 of
	// exactly 32 bytes, optionally prefixed "<version>:". Unset is supported —
	// the plane boots and secret-backed features report unavailable. Not
	// generated on first boot: a generated key diverges across a multi-node
	// deployment and makes a backup unrestorable. Previous is a comma-separated
	// list of decrypt-only predecessors ("<version>:<base64>") so a rotated
	// deployment still reads old rows. Never logged, never returned.
	SecretKey         string // QUASAR_SECRET_KEY
	SecretKeyPrevious string // QUASAR_SECRET_KEY_PREVIOUS

	// pprof ships enabled in production so a long-running box on someone else's
	// hardware is diagnosable without an image rebuild. The security posture is
	// the 127.0.0.1 default plus serving it from a second http.Server, not a
	// route on the application mux, so the loopback bind is a network-level
	// guarantee no routing mistake can defeat. Unlike every other knob, unset
	// means enabled and set-but-empty means disabled.
	PprofAddr string // QUASAR_PPROF_ADDR: "127.0.0.1:6060" (default); "" or "off" disables
}

// ArtworkProviderDisabled is separate from "is a key configured", which is
// resolved per use from the secrets store (ArtworkAPIKey as fallback).
func (c *Config) ArtworkProviderDisabled() bool {
	return strings.EqualFold(c.ArtworkProvider, "none")
}

// TLSEnabled: anything but an explicit "off".
func (c *Config) TLSEnabled() bool { return !strings.EqualFold(c.TLS, "off") }

// TLSProvided bypasses the self-signed generator.
func (c *Config) TLSProvided() bool { return c.TLSCert != "" && c.TLSKey != "" }

// PprofEnabled: anything but "off" or the empty string.
func (c *Config) PprofEnabled() bool {
	return c.PprofAddr != "" && !strings.EqualFold(c.PprofAddr, "off")
}

// HTTPRedirectEnabled requires the HTTPS listener to be running.
func (c *Config) HTTPRedirectEnabled() bool {
	return c.TLSEnabled() && !strings.EqualFold(c.HTTPRedirect, "off")
}

// Load populates a Config from the environment. Throughout, a malformed value
// fails startup rather than defaulting: the quiet outcome is a deployment that
// looks configured and is not.
func Load() (*Config, error) {
	dsn, err := databaseURL()
	if err != nil {
		return nil, err
	}
	c := &Config{
		ListenAddr:   envOr("LISTEN_ADDR", ":8080"),
		DatabaseURL:  dsn,
		LogLevel:     envOr("LOG_LEVEL", "info"),
		AuthTokenTTL: 24 * time.Hour,
	}

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	if _, err := parsePort(c.ListenAddr); err != nil {
		return nil, fmt.Errorf("LISTEN_ADDR %q: %w", c.ListenAddr, err)
	}

	// Running with no timeout is the failure these knobs close (#416).
	c.DBStatementTimeout = db.DefaultStatementTimeout
	if v := os.Getenv("QUASAR_DB_STATEMENT_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("QUASAR_DB_STATEMENT_TIMEOUT %q: must be a positive Go duration", v)
		}
		c.DBStatementTimeout = d
	}
	c.DBLockTimeout = db.DefaultLockTimeout
	if v := os.Getenv("QUASAR_DB_LOCK_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("QUASAR_DB_LOCK_TIMEOUT %q: must be a positive Go duration", v)
		}
		c.DBLockTimeout = d
	}

	// Optional since #12: a deployment can enroll entirely with admin-minted per-host
	// tokens, and requiring the fleet-wide static one would force every operator to keep
	// the credential the minted tokens exist to replace. Empty never matches any presented
	// token (agentws only compares when it is non-empty), so this disables the static path
	// rather than opening it. Contract: control-api.md §Host enrollment tokens.
	c.EnrollmentToken = os.Getenv("ENROLLMENT_TOKEN")
	if c.EnrollmentToken == "" {
		slog.Warn("no static ENROLLMENT_TOKEN: only minted per-host tokens can enroll")
	}

	c.BootstrapAdminEmail = os.Getenv("BOOTSTRAP_ADMIN_EMAIL")
	c.BootstrapAdminUsername = os.Getenv("BOOTSTRAP_ADMIN_USERNAME")
	c.BootstrapAdminPassword = os.Getenv("BOOTSTRAP_ADMIN_PASSWORD")
	c.WebRoot = os.Getenv("QUASAR_WEB_ROOT")
	c.PlacementPolicy = os.Getenv("QUASAR_PLACEMENT_POLICY")
	// These reach admission SQL; the veto must never brick the fleet (#383).
	minFree, err := envInt32("QUASAR_VRAM_MIN_FREE_MB", 1024, 0)
	if err != nil {
		return nil, err
	}
	c.VramMinFreeMB = minFree
	inflight, err := envInt32("QUASAR_VRAM_INFLIGHT_ESTIMATE_MB", minFree, 0)
	if err != nil {
		return nil, err
	}
	c.VramInflightEstimateMB = inflight
	// Minimum 1: secs => 0 makes every sample stale, negative makes all fresh.
	staleness, err := envInt32("QUASAR_VRAM_STALENESS_SECS", 20, 1)
	if err != nil {
		return nil, err
	}
	c.VramStalenessSecs = staleness

	c.RegistrationMode = os.Getenv("REGISTRATION_MODE")
	c.PublicBaseURL = os.Getenv("PUBLIC_BASE_URL")
	c.AllowedOrigins, c.AllowedOriginsSet = os.LookupEnv("QUASAR_ALLOWED_ORIGINS")
	if err := validateAllowedOrigins(c.AllowedOrigins); err != nil {
		return nil, err
	}

	// The failure this prevents is silent (negotiates, never gets media), so a
	// typo must not reach a client (#509).
	iceServers, err := ice.Parse(os.Getenv("QUASAR_ICE_SERVERS"))
	if err != nil {
		return nil, err
	}
	c.ICEServers = iceServers

	c.TLS = envOr("QUASAR_TLS", "auto")
	c.TLSAddr = envOr("QUASAR_TLS_ADDR", ":8443")
	c.TLSDir = envOr("QUASAR_TLS_DIR", "/var/lib/quasar-control/tls")
	c.TLSCert = os.Getenv("QUASAR_TLS_CERT")
	c.TLSKey = os.Getenv("QUASAR_TLS_KEY")
	c.TLSHosts = os.Getenv("QUASAR_TLS_HOSTS")
	c.PublicHost = os.Getenv("QUASAR_PUBLIC_HOST")
	if !strings.EqualFold(c.TLS, "auto") && !strings.EqualFold(c.TLS, "off") {
		return nil, fmt.Errorf("QUASAR_TLS %q: must be \"auto\" or \"off\"", c.TLS)
	}
	if c.TLSEnabled() {
		if _, err := parsePort(c.TLSAddr); err != nil {
			return nil, fmt.Errorf("QUASAR_TLS_ADDR %q: %w", c.TLSAddr, err)
		}
		// Supplying only one must not be papered over with a generated cert.
		if (c.TLSCert == "") != (c.TLSKey == "") {
			return nil, fmt.Errorf("QUASAR_TLS_CERT and QUASAR_TLS_KEY must be set together (got only one)")
		}
	}

	c.HTTPRedirect = envOr("QUASAR_HTTP_REDIRECT", "auto")
	if !strings.EqualFold(c.HTTPRedirect, "auto") && !strings.EqualFold(c.HTTPRedirect, "off") {
		return nil, fmt.Errorf("QUASAR_HTTP_REDIRECT %q: must be \"auto\" or \"off\"", c.HTTPRedirect)
	}
	c.TLSRedirectPort = os.Getenv("QUASAR_TLS_REDIRECT_PORT")
	if c.TLSRedirectPort == "" {
		p, err := parsePort(c.TLSAddr)
		if err == nil {
			c.TLSRedirectPort = strconv.Itoa(p)
		}
	} else if _, err := strconv.Atoi(c.TLSRedirectPort); err != nil {
		return nil, fmt.Errorf("QUASAR_TLS_REDIRECT_PORT %q: must be a port number", c.TLSRedirectPort)
	}

	compression, err := envBool("QUASAR_COMPRESSION", true)
	if err != nil {
		return nil, err
	}
	c.Compression = compression
	accessLog, err := envAccessLogLevel("QUASAR_ACCESS_LOG", "errors")
	if err != nil {
		return nil, err
	}
	c.AccessLog = accessLog

	trusted, trustedWarnings, err := envCIDRs("QUASAR_TRUSTED_PROXIES")
	if err != nil {
		return nil, err
	}
	c.TrustedProxies = trusted
	c.Warnings = append(c.Warnings, trustedWarnings...)

	c.MinClientVersion = os.Getenv("QUASAR_MIN_CLIENT_VERSION")
	c.LatestClientVersion = os.Getenv("QUASAR_LATEST_CLIENT_VERSION")
	// The login gate fails open on an unparseable floor, so an invalid
	// QUASAR_MIN_CLIENT_VERSION lets every client through unnoticed. Same
	// grammar (internal/semver) as the runtime gate, so what passes startup is
	// guaranteed to compare at login.
	if c.MinClientVersion != "" && !semver.Valid(c.MinClientVersion) {
		return nil, fmt.Errorf("QUASAR_MIN_CLIENT_VERSION %q: must be MAJOR.MINOR.PATCH semver", c.MinClientVersion)
	}
	if c.LatestClientVersion != "" && !semver.Valid(c.LatestClientVersion) {
		return nil, fmt.Errorf("QUASAR_LATEST_CLIENT_VERSION %q: must be MAJOR.MINOR.PATCH semver", c.LatestClientVersion)
	}

	c.ArtworkDir = envOr("QUASAR_ARTWORK_DIR", "/var/lib/quasar-control/artwork")
	c.ArtworkProvider = envOr("QUASAR_ARTWORK_PROVIDER", "steamgriddb")
	c.ArtworkAPIKey = os.Getenv("QUASAR_STEAMGRIDDB_API_KEY")
	switch strings.ToLower(c.ArtworkProvider) {
	case "steamgriddb", "none":
	default:
		// A misspelled provider alongside a set key would otherwise stay dark
		// with no explanation.
		return nil, fmt.Errorf("QUASAR_ARTWORK_PROVIDER %q: must be \"steamgriddb\" or \"none\"", c.ArtworkProvider)
	}
	artMax, err := envInt32("QUASAR_ARTWORK_MAX_BYTES", 8<<20, 1)
	if err != nil {
		return nil, err
	}
	c.ArtworkMaxBytes = artMax
	c.ArtworkInterval = 15 * time.Minute
	if v := os.Getenv("QUASAR_ARTWORK_SWEEP_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("QUASAR_ARTWORK_SWEEP_INTERVAL %q: must be a positive Go duration", v)
		}
		c.ArtworkInterval = d
	}

	c.TelemetryRollingWindow = telemetry.DefaultRolling
	c.TelemetryPostMortemRetention = telemetry.DefaultPostMortem
	for _, knob := range []struct {
		env string
		dst *time.Duration
	}{
		{"QUASAR_TELEMETRY_ROLLING_WINDOW", &c.TelemetryRollingWindow},
		{"QUASAR_TELEMETRY_POSTMORTEM_RETENTION", &c.TelemetryPostMortemRetention},
	} {
		v := os.Getenv(knob.env)
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("%s %q: must be a Go duration (e.g. \"90m\", \"48h\")", knob.env, v)
		}
		*knob.dst = d
	}
	// One check for the pair: a short post-mortem is a boot failure, not silent
	// data loss on the first sweep.
	if err := c.TelemetryPolicy().Validate(); err != nil {
		return nil, fmt.Errorf("QUASAR_TELEMETRY_ROLLING_WINDOW / QUASAR_TELEMETRY_POSTMORTEM_RETENTION: %w", err)
	}

	// Unset is distinct from any value: since migration 0047 the database column
	// decides unless the env var is present. The interval stays zero when unset,
	// so callers must check the Set flag first.
	if v := os.Getenv("QUASAR_LIBRARY_SCAN_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		// 0 is legal here, unlike QUASAR_ARTWORK_SWEEP_INTERVAL: it is the kill
		// switch forcing discovery dark regardless of the database flag.
		if err != nil || d < 0 {
			return nil, fmt.Errorf("QUASAR_LIBRARY_SCAN_INTERVAL %q: must be a non-negative Go duration (0 disables discovery entirely)", v)
		}
		c.LibraryScanInterval = d
		c.LibraryScanIntervalSet = true
	}
	if raw := strings.TrimSpace(os.Getenv("QUASAR_STEAM_APPDETAILS_LOOKUP")); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("QUASAR_STEAM_APPDETAILS_LOOKUP %q: must be a boolean (1/0, true/false)", raw)
		}
		c.SteamAppDetailsLookup = v
		c.SteamAppDetailsLookupSet = true
	}

	jobsEnabled, err := envBool("QUASAR_JOBS", true)
	if err != nil {
		return nil, err
	}
	c.JobsEnabled = jobsEnabled
	if c.JobsTickSecs, err = envInt32("QUASAR_JOBS_TICK_SECS", 10, 1); err != nil {
		return nil, err
	}
	if c.JobsRunRetention, err = envInt32("QUASAR_JOBS_RUN_RETENTION", 50, 1); err != nil {
		return nil, err
	}
	if c.JobsRunRetentionDays, err = envInt32("QUASAR_JOBS_RUN_RETENTION_DAYS", 30, 0); err != nil {
		return nil, err
	}
	if c.JobsClaimTimeoutSecs, err = envInt32("QUASAR_JOBS_CLAIM_TIMEOUT_SECS", 3600, 1); err != nil {
		return nil, err
	}
	// The tz database is embedded (internal/jobs imports time/tzdata), so this
	// validates identically on images without /usr/share/zoneinfo.
	c.JobsTimezone = envOr("QUASAR_JOBS_TIMEZONE", "UTC")
	if _, err := jobs.LoadLocation(c.JobsTimezone); err != nil {
		return nil, fmt.Errorf("QUASAR_JOBS_TIMEZONE: %w", err)
	}

	// internal/secrets.ParseKeyring owns the format and fails startup on a
	// malformed key rather than at the first write.
	c.SecretKey = os.Getenv("QUASAR_SECRET_KEY")
	c.SecretKeyPrevious = os.Getenv("QUASAR_SECRET_KEY_PREVIOUS")

	// LookupEnv, not envOr: "" is the off switch, so set-but-empty must not
	// collapse into the default the way it does elsewhere.
	c.PprofAddr = "127.0.0.1:6060"
	if v, ok := os.LookupEnv("QUASAR_PPROF_ADDR"); ok {
		c.PprofAddr = strings.TrimSpace(v)
	}
	if c.PprofEnabled() {
		if _, err := parsePort(c.PprofAddr); err != nil {
			return nil, fmt.Errorf("QUASAR_PPROF_ADDR %q: %w", c.PprofAddr, err)
		}
	}

	if v := os.Getenv("AUTH_TOKEN_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("AUTH_TOKEN_TTL %q: must be a positive Go duration", v)
		}
		c.AuthTokenTTL = d
	}

	return c, nil
}

// validateAllowedOrigins rejects a bad entry rather than dropping it, and never
// echoes the value. The predicate lives in internal/origins as ONE definition
// shared with internal/signal (Origin header per handshake) and
// internal/settings (the admin-editable column): a divergence between them
// surfaces only as an unexplainable 403.
func validateAllowedOrigins(raw string) error {
	for i, origin := range strings.Split(raw, ",") {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		if _, ok := origins.Normalize(origin); !ok {
			return fmt.Errorf("QUASAR_ALLOWED_ORIGINS contains invalid origin at position %d", i+1)
		}
	}
	return nil
}

// envInt32 rejects anything that is not a plain integer at or above min. Strict
// because these values are interpolated into admission SQL: defaulting on
// garbage hides a misconfiguration behind behaviour nobody asked for.
func envInt32(key string, def, min int32) (int32, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s %q: must be an integer", key, raw)
	}
	if int32(v) < min {
		return 0, fmt.Errorf("%s %q: must be >= %d", key, raw, min)
	}
	return int32(v), nil
}

// envBool rejects anything strconv.ParseBool does not accept: a typo in a knob
// gating a third-party call must fail startup, or a bare
// QUASAR_STEAM_APPDETAILS_LOOKUP=yes silently means "off".
func envBool(key string, def bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s %q: must be a boolean (1/0, true/false)", key, raw)
	}
	return v, nil
}

// envAccessLogLevel accepts booleans for compatibility with the knob's original
// form: an existing QUASAR_ACCESS_LOG=false must keep working (#517).
func envAccessLogLevel(key, def string) (string, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	switch strings.ToLower(raw) {
	case "off", "errors", "all":
		return strings.ToLower(raw), nil
	}
	if b, err := strconv.ParseBool(raw); err == nil {
		if b {
			return "all", nil
		}
		return "off", nil
	}
	return "", fmt.Errorf("%s %q: must be \"off\", \"errors\", or \"all\" (booleans accepted for backward compatibility: true -> \"all\", false -> \"off\")", key, raw)
}

// envCIDRs reads a comma-separated CIDR list (#438). A /0 is rejected, not
// warned about: it lets every caller pick its own limiter key by writing a
// header, and a mistake that silently disables a security control must not
// boot. Prefixes shorter than /8 are legal but usually wider than meant, so
// they warn.
func envCIDRs(key string) ([]*net.IPNet, []string, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil, nil, nil
	}
	var (
		out      []*net.IPNet
		warnings []string
	)
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		n, err := parseCIDROrIP(field)
		if err != nil {
			return nil, nil, fmt.Errorf("%s %q: %w", key, field, err)
		}
		ones, bits := n.Mask.Size()
		if ones == 0 {
			return nil, nil, fmt.Errorf("%s %q: a /0 trusts every address there is, which disables the rate limiters entirely — name the proxy's own network instead", key, field)
		}
		if ones < 8 {
			warnings = append(warnings, fmt.Sprintf(
				"%s entry %q is a /%d of a %d-bit address space — anyone inside it can choose their own rate-limit key; name only the network your proxy runs on",
				key, n.String(), ones, bits))
		}
		out = append(out, n)
	}
	return out, warnings, nil
}

// parseCIDROrIP widens a bare IP to its own single-host network (/32 or /128);
// guessing the other way (10.0.0.5 meaning 10.0.0.0/8) is a security defect.
func parseCIDROrIP(field string) (*net.IPNet, error) {
	if _, n, err := net.ParseCIDR(field); err == nil {
		return n, nil
	}
	ip := net.ParseIP(field)
	if ip == nil {
		return nil, fmt.Errorf("each entry must be a CIDR (172.18.0.0/16, fd00::/8) or a bare IP address")
	}
	bits := 128
	if v4 := ip.To4(); v4 != nil {
		ip, bits = v4, 32
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parsePort(addr string) (int, error) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return strconv.Atoi(addr[i+1:])
		}
	}
	return 0, fmt.Errorf("no port in address")
}

// TelemetryPolicy is the only thing that should read the retention pair: the
// janitor takes a Policy, so "post-mortem >= rolling" is stated once.
func (c *Config) TelemetryPolicy() telemetry.Policy {
	return telemetry.Policy{
		Rolling:    c.TelemetryRollingWindow,
		PostMortem: c.TelemetryPostMortemRetention,
		Batch:      telemetry.DefaultBatch,
	}
}
