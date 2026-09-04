package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/access"
	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/artwork"
	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/config"
	"github.com/accreleus/quasar/control-plane/internal/console"
	"github.com/accreleus/quasar/control-plane/internal/crud"
	"github.com/accreleus/quasar/control-plane/internal/devauth"
	"github.com/accreleus/quasar/control-plane/internal/devices"
	"github.com/accreleus/quasar/control-plane/internal/health"
	"github.com/accreleus/quasar/control-plane/internal/hostcfg"
	"github.com/accreleus/quasar/control-plane/internal/hostenroll"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/ice"
	"github.com/accreleus/quasar/control-plane/internal/images"
	"github.com/accreleus/quasar/control-plane/internal/invites"
	"github.com/accreleus/quasar/control-plane/internal/jobs"
	"github.com/accreleus/quasar/control-plane/internal/library"
	"github.com/accreleus/quasar/control-plane/internal/origins"
	"github.com/accreleus/quasar/control-plane/internal/secrets"
	"github.com/accreleus/quasar/control-plane/internal/session"
	"github.com/accreleus/quasar/control-plane/internal/settings"
	"github.com/accreleus/quasar/control-plane/internal/setup"
	signalpkg "github.com/accreleus/quasar/control-plane/internal/signal"
	"github.com/accreleus/quasar/control-plane/internal/storage"
	"github.com/accreleus/quasar/control-plane/internal/telemetry"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Adapts the settings + invites stores to auth.Registration (LP-SEC-01 SEC-03). Must live
// in main: auth importing settings/invites would be an import cycle. RedeemInvite runs
// inside the register transaction, so the invite consume is atomic with the account insert.
type registrationGate struct {
	settings *settings.Store
}

func (g registrationGate) Mode(ctx context.Context) (string, error) {
	return g.settings.RegistrationMode(ctx)
}

func (g registrationGate) RedeemInvite(ctx context.Context, tx pgx.Tx, code string) (string, bool, error) {
	role, err := invites.Redeem(ctx, tx, code)
	if errors.Is(err, invites.ErrInvalidInvite) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return role, true, nil
}

type Services struct {
	cfg *config.Config
	log *slog.Logger

	authHandler     *auth.Handler
	crudHandler     *crud.Handler
	sessionHandler  *session.Handler
	deviceHandler   *devices.Handler
	signalHandler   *signalpkg.Handler
	agentHandler    *agentws.Handler
	storageHandler  *storage.Handler
	cfgHandler      *hostcfg.Handler
	settingsHandler *settings.Handler
	invitesHandler  *invites.Handler
	enrollHandler   *hostenroll.Handler
	consoleHandler  *console.Handler
	auditHandler    *audit.Handler
	artworkHandler  *artwork.Handler
	secretsHandler  *secrets.Handler
	libraryHandler  *library.Handler
	imagesHandler   *images.Handler
	setupHandler    *setup.Service
	accessHandler   *access.Service
	jobsHandler     *jobs.Handler
	// Registered unconditionally; with no agent-side runner it returns an empty
	// claim list.
	jobsAgentHandler *jobs.AgentHandler
	pool             *pgxpool.Pool

	// authSvc: main.go wires the dev-only agent-auth endpoint (#399) to this same
	// service so there is one hashing and token-issuance path. Unused by RegisterRoutes.
	authSvc *auth.Service

	// janitorStop cancels the background goroutines; Stop() calls it.
	janitorStop context.CancelFunc
	// The jobs framework's single ticker; Stop() waits on it so an in-flight run
	// records its outcome rather than leaving a `running` row for the claim reaper.
	// Design: docs/design/plans/2026-08-12-jobs-framework-and-viewer.md §8.
	jobsDispatcher *jobs.Dispatcher
	// Stop() cancels its lifecycle context, ending in-flight watchStartToRunning.
	coordinator *session.Coordinator
}

// Adapts the dispatcher onto the narrower images.JobEnqueuer, so internal/images
// need not import the jobs framework for a best-effort trigger.
type jobEnqueuer struct{ d *jobs.Dispatcher }

func (q jobEnqueuer) EnqueueJob(ctx context.Context, jobID, hostID string, params any) error {
	_, err := q.d.Enqueue(ctx, jobID, hostID, params)
	return err
}

// An unmanaged job's Description is also the API's unmanaged_note (openapi.yaml
// Job). detail is the symbol inside file; empty when the file alone identifies it.
func unmanagedDescription(desc, file, detail string) string {
	if detail == "" {
		return fmt.Sprintf("%s Hard-coded in %s.", desc, file)
	}
	return fmt.Sprintf("%s Hard-coded in %s (%s).", desc, file, detail)
}

// NewServices wires every subsystem; call Stop() for the goroutines it starts.
// certManager (internal/access) must be built by main.go before this call: a bad
// cert/key on disk must be fatal before any listener or goroutine exists. Nil when
// QUASAR_TLS=off, which the access routes report rather than crashing on.
func NewServices(cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger, certManager *access.Manager) (*Services, error) {
	// Parsed before any database work or goroutine: a malformed key must fail startup
	// rather than surface later as an unreadable secret. An absent key is not an
	// error - nil keyring, still bootable, secret-backed features report unavailable.
	keyring, err := secrets.ParseKeyring(cfg.SecretKey, cfg.SecretKeyPrevious)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}

	// LP-SEC-01 §A.0: seeds registration_mode from the env on first boot only.
	settingsStore := settings.NewStore(pool)
	seedCtx, seedCancel := context.WithTimeout(context.Background(), 10*time.Second)
	seedErr := settingsStore.Seed(seedCtx, cfg.RegistrationMode)
	seedCancel()
	if seedErr != nil {
		return nil, fmt.Errorf("seed instance settings: %w", seedErr)
	}

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), cfg.AuthTokenTTL,
		auth.WithRegistration(registrationGate{settings: settingsStore}))
	if err != nil {
		return nil, fmt.Errorf("auth service: %w", err)
	}

	janitorCtx, janitorStop := context.WithCancel(context.Background())

	// The registry is built before any subsystem so each can register its Definition
	// where it is wired; it is synced and the dispatcher started at the end of this
	// function, once every subsystem has had its chance.
	jobRegistry := jobs.NewRegistry()
	jobStore := jobs.NewStore(pool)
	// Nil until the end of this function. Closures below capture the variable, not
	// a value; none of them runs synchronously during NewServices.
	var jobsDispatcher *jobs.Dispatcher

	jobRegistry.MustRegister(jobs.Definition{
		ID:          "auth.token_janitor",
		Name:        "Auth token janitor",
		Description: "Deletes expired or revoked auth_tokens rows.",
		Plane:       jobs.PlaneControl,
		Scope:       jobs.ScopeInstance,
		Managed:     true,
		Default:     jobs.Schedule{Kind: jobs.KindInterval, IntervalSecs: 6 * 3600},
		Run: func(ctx context.Context, rc jobs.RunContext) (jobs.Outcome, error) {
			n, err := authSvc.SweepTokens(ctx, log)
			if err != nil {
				return jobs.Outcome{}, err
			}
			return jobs.Succeeded(jobs.Summary{"deleted": n}), nil
		},
	})

	// First-admin bootstrap (control-api.md §Authorization). Idempotent, so every boot.
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 10*time.Second)
	bootRes, err := authSvc.EnsureBootstrapAdmin(bootCtx, cfg.BootstrapAdminEmail, cfg.BootstrapAdminUsername, cfg.BootstrapAdminPassword)
	bootCancel()
	if err != nil {
		janitorStop()
		return nil, fmt.Errorf("bootstrap admin: %w", err)
	}
	if bootRes != auth.BootstrapSkipped {
		log.Info("bootstrap admin", "action", bootRes.String(), "email", cfg.BootstrapAdminEmail)
	}

	// First-run setup wizard (Spec B W1). Mint the per-boot token only when no admin
	// exists, so a fresh instance can be claimed via POST /v1/setup/claim; otherwise
	// remove any stale token file. Token custody: a 0600 file only, never the log (a
	// logged token is readable by log-aggregation principals who could then claim the
	// instance) and never persisted across boots. The file write must therefore be
	// fatal on failure rather than degrading to a log-only token.
	setupTokenPath := setup.TokenPath()
	var setupSecret string
	adminCtx, adminCancel := context.WithTimeout(context.Background(), 10*time.Second)
	adminExists, adminErr := authSvc.AdminExists(adminCtx)
	adminCancel()
	if adminErr != nil {
		janitorStop()
		return nil, fmt.Errorf("setup: check admin exists: %w", adminErr)
	}
	if adminExists {
		setup.RemoveTokenFile(setupTokenPath, log)
	} else {
		secret, mErr := setup.MintToken()
		if mErr != nil {
			janitorStop()
			return nil, fmt.Errorf("setup: mint token: %w", mErr)
		}
		setupSecret = secret
		if wErr := setup.WriteTokenFile(setupTokenPath, secret); wErr != nil {
			janitorStop()
			return nil, fmt.Errorf("setup: the per-boot setup token file could not be written (it is the ONLY place the token exists — set QUASAR_SETUP_TOKEN_PATH to a writable location): %w", wErr)
		}
		// Never log the token itself, only where to find it.
		log.Warn("FIRST-RUN SETUP: no admin exists — claim the instance at POST /v1/setup/claim with header X-Quasar-Setup-Token",
			"token_file", setupTokenPath,
			"note", "read the token from the file (e.g. docker exec <control-plane> cat "+setupTokenPath+"); it is per-boot — a restart before claim rotates it")
	}
	setupHandler := setup.NewService(authSvc, settingsStore, setupSecret, log).
		WithTrustedProxies(cfg.TrustedProxies)

	placementPolicy, err := session.ParsePlacementPolicy(cfg.PlacementPolicy)
	if err != nil {
		janitorStop()
		return nil, fmt.Errorf("placement policy: %w", err)
	}
	log.Info("scheduler placement policy", "policy", placementPolicy.String())

	// #383: encode slots are the transactional reservation, live free VRAM only an
	// advisory veto. config.Load validates these, so nothing unparseable reaches the
	// admission SQL. MinFreeMB 0 is the kill switch.
	vramAdmission := session.VramAdmission{
		MinFreeMB:     cfg.VramMinFreeMB,
		InflightMB:    cfg.VramInflightEstimateMB,
		StalenessSecs: cfg.VramStalenessSecs,
	}
	log.Info("scheduler vram veto",
		"min_free_mb", vramAdmission.MinFreeMB,
		"inflight_estimate_mb", vramAdmission.InflightMB,
		"staleness_secs", vramAdmission.StalenessSecs,
		"enabled", vramAdmission.MinFreeMB > 0)

	sessionStore := session.NewStore(pool,
		session.WithPlacementPolicy(placementPolicy),
		session.WithVramAdmission(vramAdmission))

	// The only thing that deletes session telemetry. On the dispatcher rather than
	// its own ticker so it is single-flight across control-plane instances.
	telemetryPolicy := cfg.TelemetryPolicy()
	log.Info("session telemetry retention",
		"rolling_window", telemetryPolicy.Rolling.String(),
		"post_mortem_retention", telemetryPolicy.PostMortem.String(),
		"knobs", "QUASAR_TELEMETRY_ROLLING_WINDOW / QUASAR_TELEMETRY_POSTMORTEM_RETENTION")
	telemetryStore := sessionStore.Telemetry()
	jobRegistry.MustRegister(jobs.Definition{
		ID:   "telemetry.retain",
		Name: "Session telemetry retention",
		Description: "Trims live sessions to the rolling window and sweeps terminal sessions " +
			"past their post-mortem retention. Captures (diag.*) are exempt.",
		Plane:       jobs.PlaneControl,
		Scope:       jobs.ScopeInstance,
		Managed:     true,
		Default:     jobs.Schedule{Kind: jobs.KindInterval, IntervalSecs: 300},
		EnvOverride: "QUASAR_TELEMETRY_RETAIN_INTERVAL",
		Run: func(ctx context.Context, rc jobs.RunContext) (jobs.Outcome, error) {
			rep, err := telemetry.RunRetention(ctx, telemetryStore, telemetryPolicy, log)
			if err != nil {
				return jobs.Outcome{}, err
			}
			return jobs.Succeeded(jobs.Summary{
				"rolling_samples":    rep.RollingSamples,
				"rolling_events":     rep.RollingEvents,
				"postmortem_samples": rep.PostMortemSamples,
				"postmortem_events":  rep.PostMortemEvents,
				"postmortem_clocks":  rep.PostMortemClocks,
				"total":              rep.Total(),
				"truncated":          rep.Truncated,
				"took_ms":            rep.Duration.Milliseconds(),
			}), nil
		},
	})
	cfgStore := hostcfg.NewStore(pool)
	consoleStore := console.NewStore(pool)
	auditStore := audit.NewStore(pool)
	agentRegistry := agentws.NewRegistry(log)
	relayBus := agentws.NewRelayBus(log)

	// P5-02: the provider setting and each host's effective home root (hostcfg
	// override → agent effective_settings → QUASAR_HOME_ROOT) resolve fresh per
	// launch, so a UI flip needs no restart.
	homeRootEnv := strings.TrimSpace(os.Getenv("QUASAR_HOME_ROOT"))
	homeProvider := storage.New(pool, settingsStore,
		storage.HostRootResolverFunc(func(ctx context.Context, hostID string) (string, error) {
			return cfgStore.HomeRoot(ctx, hostID, homeRootEnv)
		}))
	jobRegistry.MustRegister(jobs.Definition{
		ID:          "storage.home_janitor",
		Name:        "Home janitor",
		Description: "Hard-deletes tombstoned user_homes rows past grace whose host is gone.",
		Plane:       jobs.PlaneControl,
		Scope:       jobs.ScopeInstance,
		Managed:     true,
		Default:     jobs.Schedule{Kind: jobs.KindInterval, IntervalSecs: 6 * 3600},
		Run: func(ctx context.Context, rc jobs.RunContext) (jobs.Outcome, error) {
			n, err := homeProvider.SweepHomes(ctx, log)
			if err != nil {
				return jobs.Outcome{}, err
			}
			return jobs.Succeeded(jobs.Summary{"deleted_unreapable": n}), nil
		},
	})

	coordinator := session.NewCoordinator(sessionStore, agentRegistry, log,
		session.WithHomeProvider(homeProvider),
		// The terminal-failure edge has no acting admin, so it is recorded here
		// rather than from a handler.
		session.WithAuditor(auditStore),
		// Mic capture (spec §3.5): the launcher reads mic_capture_enabled per launch.
		session.WithMicSettings(settingsStore),
		// #11: uncordon must check the live connection, not just hosts.status — a
		// control-plane restart leaves a draining row whose agent has not reconnected.
		session.WithAgentConnectivity(agentRegistry),
		// #402: the relay buffers agent signaling frames per session while no browser
		// is attached. Register/Unregister are browser-driven, so a headless session
		// would leak its buffer; the coordinator evicts at every terminal transition.
		session.WithSessionForgetter(relayBus),
		// #492: a (re)registering agent is not running the job runs its previous
		// process claimed and will never report them. Closing them here frees the
		// job_runs_open_per_target single-flight slot now instead of an hour later.
		session.WithJobReclaimer(func(ctx context.Context, hostID, reason string) (int, error) {
			if jobsDispatcher == nil {
				return 0, nil
			}
			return jobsDispatcher.ReclaimHostRuns(ctx, hostID, reason)
		}))

	authHandler := auth.NewHandler(authSvc, auditStore).
		WithVersionPolicy(cfg.MinClientVersion, cfg.LatestClientVersion).
		WithTrustedProxies(cfg.TrustedProxies)
	crudHandler := crud.NewHandler(pool, auditStore)
	crudHandler.SetRegistry(agentRegistry) // lets DELETE /v1/hosts/{id} check live connectivity
	sessionHandler := session.NewHandler(coordinator, sessionStore, auditStore).
		WithPublicBaseURL(cfg.PublicBaseURL).
		WithICEServers(cfg.ICEServers)
	// #509: log the state either way - "none configured" is the default and also the
	// whole explanation for a WAN session that negotiates but never gets media.
	// Credentials must stay redacted.
	if len(cfg.ICEServers) == 0 {
		log.Info("ice servers: none configured; clients gather host candidates only " +
			"(LAN or VPN reachability required — deploy/README.md, QUASAR_ICE_SERVERS)")
	} else {
		log.Info("ice servers configured", "count", len(cfg.ICEServers),
			"servers", ice.Redact(cfg.ICEServers))
	}
	// Device revoke reuses the coordinator's teardown (LP-SEC-01 §B.6, no wire change).
	deviceStopper := func(ctx context.Context, sessionID, reason string) error {
		_, err := coordinator.Stop(ctx, sessionID, reason)
		return err
	}
	deviceHandler := devices.NewHandler(devices.NewStore(pool), deviceStopper)
	// One shared origin resolver (wizard v2 §S6e, migration 0064). internal/signal
	// enforces with it, internal/access reports from it, and they must get the same
	// instance: two copies drifted, and the diagnostic said "allowed" for an origin
	// the socket refused. AllowedOriginsSet is not redundant with the value - without
	// the presence flag an unset var looks "set to empty" and pins the column off.
	originResolver := origins.NewResolver(cfg.AllowedOrigins, cfg.AllowedOriginsSet, settingsStore, log)
	signalHandler := signalpkg.NewHandler(sessionStore, agentRegistry, relayBus, log, originResolver).
		WithTrustedProxies(cfg.TrustedProxies)
	agentHandler := agentws.NewHandler(pool, cfg.EnrollmentToken, log, agentRegistry, coordinator, relayBus, cfgStore, consoleStore).
		WithTrustedProxies(cfg.TrustedProxies)
	// CM-09 item 2: console re-eval hook, set after both exist. A plain func value
	// because session must not import agentws.Handler, only its agentws.Events subset.
	coordinator.ConsoleReeval = agentHandler.ConsoleSessionTerminated
	storageHandler := storage.NewHandler(homeProvider, auditStore)
	cfgHandler := hostcfg.NewHandler(cfgStore, agentRegistry, sessionStore, auditStore)
	settingsHandler := settings.NewHandler(settingsStore, auditStore)
	invitesHandler := invites.NewHandler(invites.NewStore(pool), cfg.PublicBaseURL, auditStore)
	enrollHandler := hostenroll.NewHandler(hostenroll.NewStore(pool), auditStore)
	consoleHandler := console.NewHandler(consoleStore, agentRegistry, auditStore)
	auditHandler := audit.NewHandler(auditStore)

	secretStore := secrets.NewStore(pool, keyring, secrets.DefaultRegistry())
	// Log the state, never a key or any part of one.
	log.Info("encrypted secrets", "master_key_configured", secretStore.Available(),
		"key_versions", secretStore.KeyVersions())
	// #522: an unset master key degrades a feature class, and the INFO line above
	// is invisible to an operator who never reads logs. Warn every boot while the
	// deployment stays in that state.
	if w := secretStore.BootWarning(); w != "" {
		log.Warn(w)
	}
	secretsHandler := secrets.NewHandler(secretStore, log, auditStore)

	// §S6a/S6b. Reports on certManager (the cert the listener serves, not a re-read
	// file) and on the shared origin resolver (what the socket enforces, not what the
	// database says). POST /v1/admin/tls/certificate (§S6d) is not here: a
	// header-based transport gate on the shared router was holed repeatedly.
	accessHandler := access.NewService(certManager, originResolver, log)

	// Cover artwork (UI-P7). Fail-open: a failure here never fails startup, the
	// library degrades to the gradient tile and the routes answer 503. The API key
	// is resolved per use from the secrets store (falling back to
	// QUASAR_STEAMGRIDDB_API_KEY), not read here, so a UI change needs no restart.
	var artworkHandler *artwork.Handler
	artworkOpts := artwork.Options{
		ProviderSource: artwork.NewSecretProviderSource(
			secretStore, cfg.ArtworkAPIKey, cfg.ArtworkProviderDisabled(), log),
		MaxImageBytes: int64(cfg.ArtworkMaxBytes),
		SweepInterval: cfg.ArtworkInterval,
	}
	artworkSvc, artErr := artwork.New(artwork.NewStore(pool), cfg.ArtworkDir, artworkOpts, log)
	if artErr != nil {
		log.Warn("artwork service unavailable — the library will render its gradient fallback",
			"err", artErr, "dir", cfg.ArtworkDir)
	} else {
		// Log the state, never the key. Unconfigured is the default, not a warning.
		artInfo := artworkSvc.ProviderStatus(context.Background())
		log.Info("cover artwork", "provider_configured", artInfo.Configured,
			"provider", artInfo.Name, "credential_origin", artInfo.Origin,
			"cache_dir", cfg.ArtworkDir)
		// The artwork.prune_orphans job below does not replace this boot-time pass.
		if n, err := artworkSvc.PruneOrphans(context.Background()); err != nil {
			log.Warn("artwork: orphan prune failed", "err", err)
		} else if n > 0 {
			log.Info("artwork: pruned unreferenced cached images", "count", n)
		}

		// Default matches cfg.ArtworkInterval's own default (900s);
		// QUASAR_ARTWORK_SWEEP_INTERVAL stays authoritative via EnvOverride.
		svc := artworkSvc
		jobRegistry.MustRegister(jobs.Definition{
			ID:          "artwork.sweep",
			Name:        "Artwork grabber",
			Description: "Resolves cover and hero art for apps that have no artwork record.",
			Plane:       jobs.PlaneControl,
			Scope:       jobs.ScopeInstance,
			Managed:     true,
			Default: jobs.Schedule{
				Kind:         jobs.KindInterval,
				IntervalSecs: 900,
			},
			EnvOverride: "QUASAR_ARTWORK_SWEEP_INTERVAL",
			Run: func(ctx context.Context, rc jobs.RunContext) (jobs.Outcome, error) {
				res := svc.SweepOnce(ctx)
				if !res.ProviderConfigured {
					return jobs.Skipped("no artwork provider configured"), nil
				}
				return jobs.Succeeded(jobs.Summary{
					"apps_considered":  res.AppsConsidered,
					"artwork_resolved": res.ArtworkResolved,
					"no_match":         res.NoMatch,
				}), nil
			},
		})
		jobRegistry.MustRegister(jobs.Definition{
			ID:          "artwork.prune_orphans",
			Name:        "Artwork orphan prune",
			Description: "Deletes cached artwork blobs no artwork row references.",
			Plane:       jobs.PlaneControl,
			Scope:       jobs.ScopeInstance,
			Managed:     true,
			Default: jobs.Schedule{
				Kind: jobs.KindManual,
			},
			Run: func(ctx context.Context, rc jobs.RunContext) (jobs.Outcome, error) {
				n, err := svc.PruneOrphans(ctx)
				if err != nil {
					return jobs.Outcome{}, err
				}
				return jobs.Succeeded(jobs.Summary{"pruned": n}), nil
			},
		})
	}
	artworkHandler = artwork.NewHandler(artworkSvc, log, auditStore)

	// Steam library discovery (Phase 4, spec §7/§8/§11; migration 0047). Off by
	// default two independent ways: the instance_settings flag (migration 0045), and
	// QUASAR_LIBRARY_SCAN_INTERVAL, which forces the janitor off regardless of the
	// database. The janitor reads settings per pass, never here, so a UI toggle needs
	// no restart.
	//
	// libraryResolver is the one env-override-else-database resolver (resolve.go),
	// handed to both janitor and handler so the admin status panel cannot disagree
	// with what the scheduler did. appDetails is built enabled=true unconditionally;
	// the real gate is libraryResolver.AppDetailsEnabled, checked in handler.go
	// before Classify (its static switch survives only for
	// TestAppDetailsDisabledMakesNoRequest).
	libraryStore := library.NewStore(pool)
	appDetails := library.NewAppDetails(true, log)
	libraryResolver := library.NewResolver(settingsStore,
		cfg.LibraryScanIntervalSet, cfg.LibraryScanInterval,
		cfg.SteamAppDetailsLookupSet, cfg.SteamAppDetailsLookup)
	log.Info("library discovery",
		"scan_interval_env_set", cfg.LibraryScanIntervalSet, "scan_interval_env", cfg.LibraryScanInterval,
		"appdetails_lookup_env_set", cfg.SteamAppDetailsLookupSet, "appdetails_lookup_env", cfg.SteamAppDetailsLookup)
	libraryJanitor := library.NewJanitor(libraryStore, settingsStore, libraryResolver, log)
	libraryHandler := library.NewHandler(libraryStore, homeProvider, settingsStore,
		appDetails, libraryResolver, log, auditStore)

	// jobs.interval_secs is seeded once from instance_settings.
	// library_discovery_interval_minutes (design §8.2); after this boot an admin owns
	// it by PATCH and that column is no longer read for the schedule.
	// QUASAR_LIBRARY_SCAN_INTERVAL stays authoritative via EnvOverride.
	libDiscoverySeedCtx, libDiscoverySeedCancel := context.WithTimeout(context.Background(), 10*time.Second)
	libDiscoverySeedMinutes, libDiscoverySeedErr := settingsStore.LibraryDiscoveryIntervalMinutes(libDiscoverySeedCtx)
	libDiscoverySeedCancel()
	if libDiscoverySeedErr != nil || libDiscoverySeedMinutes <= 0 {
		// Fail-open to the documented default (360 min): this only seeds a new
		// row's starting point, which an admin PATCH can fix.
		libDiscoverySeedMinutes = 360
	}
	jobRegistry.MustRegister(jobs.Definition{
		ID:          "library.discovery",
		Name:        "Steam library discovery",
		Description: "Enqueues and reconciles per-(user, app, host) library scans.",
		Plane:       jobs.PlaneControl,
		Scope:       jobs.ScopeInstance,
		Managed:     true,
		Default: jobs.Schedule{
			Kind:         jobs.KindInterval,
			IntervalSecs: int32(libDiscoverySeedMinutes) * 60,
		},
		EnvOverride: "QUASAR_LIBRARY_SCAN_INTERVAL",
		Run: func(ctx context.Context, rc jobs.RunContext) (jobs.Outcome, error) {
			res, skip := libraryJanitor.RunOnce(ctx)
			if skip != "" {
				return jobs.Skipped(skip), nil
			}
			return jobs.Succeeded(jobs.Summary{
				"enqueued":           res.Enqueued,
				"returned_abandoned": res.ReturnedAbandoned,
				"expired":            res.Expired,
				"pruned":             res.Pruned,
			}), nil
		},
	})

	// Enabling discovery must not wait for the next scheduled pass: a pass that read
	// `false` already scheduled its successor a full interval out, so without this
	// nudge switching the feature on could take most of a day to produce one tile.
	// A plain func value because internal/library imports internal/settings, so
	// settings must never import library back. Enqueue runs on its own goroutine so
	// it can never stall an admin's PATCH.
	settingsHandler.OnLibraryDiscoveryEnabled = func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := jobsDispatcher.Enqueue(ctx, "library.discovery", "", nil); err != nil {
				log.Warn("library discovery: could not nudge a pass via the jobs framework", "err", err)
			}
		}()
	}

	// App-image catalog + ensure. semantics: control-api.md §"App-image catalog +
	// management" (sync is fail-open - the cached catalog keeps serving and the
	// failure surfaces as sync_error). The ensurer is wired as agentws' ImageEvents,
	// not the coordinator's Events: image presence has a separate owner
	// (agentws/images.go). The Store gets the Ensurer after both exist, so actions
	// and the auto update policy dispatch through one seam and cannot diverge.
	imagesStore := images.NewStore(pool)
	imagesStore.SetLogger(log)
	// Trust boundary: only provider names the operator listed in
	// QUASAR_LIBRARY_PROVIDERS are auto-installed, never one a compromised catalog
	// claims. Must be set before the reconciler goroutine below can read it.
	imagesStore.SetProviderAllowlist(os.Getenv("QUASAR_LIBRARY_PROVIDERS"))
	imagesHandler := images.NewHandler(imagesStore, auditStore)
	imagesEnsurer := images.NewEnsurer(pool, agentRegistry, log)
	imagesStore.SetEnsurer(imagesEnsurer)
	agentHandler.SetImageEvents(imagesEnsurer)

	// Provider reconciliation. semantics: control-api.md §"P5 side effect".
	//
	// One level-triggered, coalescing reconciler: every trigger pokes the same worker,
	// which reads the current library_discovery_enabled and drives the world to match.
	// Level, not edge: a goroutine per transition is a race with no ordering - an
	// enable descheduled behind a later disable lands after it, leaving apps enabled
	// with discovery off. A worker, not a mutex: with a goroutine per trigger each
	// timeout starts before the lock is acquired, so a disable queued behind a slow
	// ensure can expire and die with no future trigger to fix it.
	//
	// providerReconcileC is cap-1 with a non-blocking send: poking a full channel is a
	// no-op, since the queued pass reads the level itself, and goroutine growth is
	// bounded at one. Migration 0060's library_discovery_suspended carries "only an
	// explicit enable may re-enable apps" in the data, so any enabled-level pass
	// restores exactly the apps this reconciler suspended.
	providerReconcileC := make(chan struct{}, 1)
	reconcileProviders := func() {
		select {
		case providerReconcileC <- struct{}{}:
		default: // a pass is already queued; it will read the level for us
		}
	}
	// Own context, so it is never inside another operation's spent budget.
	convergeProviderApps := func(enabled bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if !enabled {
			// Suspend only: never uninstall the image and never delete an app,
			// which would cascade the discovered tiles.
			if _, err := imagesStore.SuspendProviderApps(ctx); err != nil {
				log.Error("library provider suspend failed", "err", err)
			}
			return
		}
		if _, err := imagesStore.RestoreProviderApps(ctx); err != nil {
			log.Error("library provider restore failed", "err", err)
		}
	}
	go func() {
		for range providerReconcileC {
			func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()

				enabled, err := settingsStore.LibraryDiscoveryEnabled(ctx)
				if err != nil {
					log.Error("library provider reconcile: read discovery switch failed", "err", err)
					return
				}
				if !enabled {
					convergeProviderApps(false)
					return
				}
				if err := imagesStore.EnsureProviders(ctx); err != nil {
					log.Error("library provider auto-ensure failed", "err", err)
				}
				// Re-read on a fresh context: EnsureProviders is long and may have
				// exhausted ctx. A disable committed while it ran must win.
				rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
				enabled, err = settingsStore.LibraryDiscoveryEnabled(rctx)
				rcancel()
				if err != nil {
					log.Error("library provider reconcile: re-read discovery switch failed", "err", err)
					return
				}
				convergeProviderApps(enabled)
			}()
		}
	}()
	// All four triggers are the same call: both settings flips, every successful
	// catalog sync (a provider unresolved at enable can finally resolve), and startup
	// (converges an upgrade already enabled, or a process that exited mid-pass).
	settingsHandler.EnsureLibraryProviders = reconcileProviders
	settingsHandler.DisableLibraryProviders = reconcileProviders
	imagesStore.SetOnSyncSuccess(reconcileProviders)
	reconcileProviders()

	// Registered here, unconditionally, not next to devauth.Register in main.go:
	// registering a job after jobRegistry.Sync has run keeps it out of the `jobs`
	// table until the next boot, and this reaper must run flag or no flag.
	jobRegistry.MustRegister(jobs.Definition{
		ID:          "devauth.reaper",
		Name:        "Dev-agent-auth identity reaper",
		Description: "Deletes expired throwaway (#399 dev-agent-auth) identities.",
		Plane:       jobs.PlaneControl,
		Scope:       jobs.ScopeInstance,
		Managed:     true,
		Default:     jobs.Schedule{Kind: jobs.KindInterval, IntervalSecs: int32(devauth.ReapInterval.Seconds())},
		Run: func(ctx context.Context, rc jobs.RunContext) (jobs.Outcome, error) {
			rep, err := devauth.ReapOnce(ctx, authSvc, log)
			if err != nil && rep == (auth.ReapReport{}) {
				// The listing query failed. Per-row failures ReapOnce swallows.
				return jobs.Outcome{}, err
			}
			return jobs.Succeeded(jobs.Summary{
				"deleted":    rep.Deleted,
				"in_session": rep.InSession,
				"failed":     rep.Failed,
			}), nil
		},
	})

	// Agent-plane jobs. No Run func: the control plane owns the schedule and the run
	// record, the host owns the work. Materialized here, claimed over
	// GET /v1/agent/jobs/pending, run by node-agent/src/agent.rs, closed by
	// POST /v1/agent/jobs/report; protocol/agent-api.md is untouched. Kill switches
	// widen: each job's `enabled` flag (disabled never runs, not even manually) ->
	// QUASAR_JOBS -> host-side knobs, which report `skipped` with a reason.
	jobRegistry.MustRegister(jobs.Definition{
		ID:          "template.warmup",
		Name:        "Golden-home template warm-up",
		Description: "Builds the per-image golden-home template by booting the app once into a throwaway scratch home. Host-side knob: QUASAR_TEMPLATE_WARMUP is opt-in (default off) and a host without it reports the run as skipped, naming the knob.",
		Plane:       jobs.PlaneAgent,
		Scope:       jobs.ScopeHost,
		Managed:     true,
		// Event, not interval: trigger is images.Ensurer.AgentImageState ->
		// Dispatcher.Enqueue. No default window; an operator PATCHes one (design §8.3).
		Default: jobs.Schedule{Kind: jobs.KindEvent},
		// A host refusing to build is a deferral reported by the runner; the
		// dispatcher's persisted backoff (30s doubling to 15min) replaces the agent's
		// in-memory ladder, which evaporated on every reconnect. An event run's params
		// come from the image that reached `ready`; a manual run has none, so it must
		// resolve them here or the host fails the run with `params incomplete`.
		ResolveParams: imagesEnsurer.WarmupParamsForHost,
	})
	jobRegistry.MustRegister(jobs.Definition{
		ID:          "home.gc",
		Name:        "Home backing-store GC",
		Description: "Reaps the docker volume or directory behind each user home the control plane has tombstoned past its 24 h grace period (#175).",
		Plane:       jobs.PlaneAgent,
		Scope:       jobs.ScopeHost,
		Managed:     true,
		// Reproduces the agent ticker exactly: adoption must not change its timing.
		Default: jobs.Schedule{Kind: jobs.KindInterval, IntervalSecs: 6 * 3600},
	})

	// Unmanaged legacy jobs (§8.6). Never claimed, never scheduled, "run now"
	// refuses with job_unmanaged (protocol/openapi.yaml unmanaged_note,
	// control-api.md "Unmanaged jobs are listed on purpose").
	for _, u := range []struct {
		id, name, desc, plane, scope, file, detail string
	}{
		{"images.provider_reconcile", "Library-provider reconciler", "Suspends provider apps while library discovery is off, and restores them when it is back on.", "control", "instance", "cmd/quasar-control/app.go", "the level-triggered providerReconcileC worker"},
		{"images.ensure_retry", "Image-ensure retry", "Retries a failed image pull or build, waiting longer after each attempt.", "control", "instance", "internal/images/ensure.go", "scheduleRetry"},
		{"images.registration_reconcile", "Image registration reconciler", "Records which images a reconnecting agent reports it already has.", "control", "instance", "internal/images/ensure.go", "AgentImagesRegistered"},
		{"session.start_watchdog", "Session start watchdog", "Fails a session that never reaches running in time.", "control", "instance", "internal/session/launcher.go", "watchStartToRunning"},
		{"console.selfheal", "Console self-heal backoff", "Backs off, then stops retrying, when a console session keeps dying at start.", "control", "host", "internal/agentws/handler.go", "consoleAuto backoff"},
		{"session.cert_bench", "Encoder cert benchmark", "Benchmarks a host's encoders when an admin asks for it.", "control", "instance", "internal/session/cert_handler.go", ""},
		{"library.scanner", "Steam library ACF scanner", "Polls the control plane for a Steam library scan to run, and reports the result. Its cadence comes from the Steam library discovery job.", "agent", "host", "node-agent/src/agent.rs", "spawn_library_scanner"},
		{"console.hotplug_watcher", "Device hotplug watcher", "Watches for console and storage devices being plugged in or removed.", "agent", "host", "node-agent/src/session/console_hotplug.rs", ""},
		{"images.workers", "Image workers", "Runs the image pull, build and remove requests the control plane sends this host.", "agent", "host", "node-agent/src/images/mod.rs", "spawn_worker"},
	} {
		jobRegistry.MustRegister(jobs.Definition{
			ID:          u.id,
			Name:        u.name,
			Description: unmanagedDescription(u.desc, u.file, u.detail),
			Plane:       jobs.Plane(u.plane),
			Scope:       jobs.Scope(u.scope),
			Managed:     false,
			Default:     jobs.Schedule{Kind: jobs.KindEvent},
		})
	}

	// Sync runs after migrations and before the dispatcher starts. A sync failure must
	// be fatal, or the dispatcher schedules against a table not describing this build.
	jobsCfg := jobs.Config{
		Enabled:       cfg.JobsEnabled,
		TickInterval:  time.Duration(cfg.JobsTickSecs) * time.Second,
		Timezone:      cfg.JobsTimezone,
		HistoryLimit:  int(cfg.JobsRunRetention),
		RetentionDays: int(cfg.JobsRunRetentionDays),
		ClaimTimeout:  time.Duration(cfg.JobsClaimTimeoutSecs) * time.Second,
	}
	syncCtx, syncCancel := context.WithTimeout(context.Background(), 15*time.Second)
	_, syncErr := jobRegistry.Sync(syncCtx, jobStore, jobsCfg.Timezone, jobsCfg.HistoryLimit, log)
	syncCancel()
	if syncErr != nil {
		janitorStop()
		return nil, fmt.Errorf("jobs registry sync: %w", syncErr)
	}
	jobsDispatcher = jobs.New(jobStore, jobRegistry, jobsCfg, log)
	jobsDispatcher.Start(janitorCtx)
	// The #488 warm-up's event trigger: the one place the control plane learns an
	// image reached `ready`. Wired after the dispatcher exists; until then, and when
	// QUASAR_JOBS leaves it inert, an image_state is ingested as before.
	imagesEnsurer.SetJobEnqueuer(jobEnqueuer{jobsDispatcher})
	jobsHandler := jobs.NewHandler(jobStore, jobRegistry, jobsDispatcher, log, auditStore)

	// Agent auth is homeProvider, the same storage.Manager passed to the storage and
	// library handlers: one node_secret verification across all /v1/agent/* surfaces.
	jobsAgentHandler := jobs.NewAgentHandler(jobStore, jobsDispatcher, homeProvider, log)

	return &Services{
		cfg:              cfg,
		log:              log,
		authHandler:      authHandler,
		crudHandler:      crudHandler,
		sessionHandler:   sessionHandler,
		deviceHandler:    deviceHandler,
		signalHandler:    signalHandler,
		agentHandler:     agentHandler,
		storageHandler:   storageHandler,
		cfgHandler:       cfgHandler,
		settingsHandler:  settingsHandler,
		invitesHandler:   invitesHandler,
		enrollHandler:    enrollHandler,
		consoleHandler:   consoleHandler,
		auditHandler:     auditHandler,
		artworkHandler:   artworkHandler,
		secretsHandler:   secretsHandler,
		libraryHandler:   libraryHandler,
		imagesHandler:    imagesHandler,
		setupHandler:     setupHandler,
		accessHandler:    accessHandler,
		jobsHandler:      jobsHandler,
		jobsAgentHandler: jobsAgentHandler,

		pool:           pool,
		authSvc:        authSvc,
		janitorStop:    janitorStop,
		jobsDispatcher: jobsDispatcher,
		coordinator:    coordinator,
	}, nil
}

// RegisterRoutes attaches all HTTP handlers to mux. The RequireAuth → RequireAdmin
// chain is load-bearing (control-api.md §Authorization): which endpoints are
// admin-gated must not change without explicit human sign-off.
func (s *Services) RegisterRoutes(mux httpx.Router) {
	mux.HandleFunc("GET /health", health.Handler(s.pool))
	s.authHandler.Register(mux)
	s.crudHandler.Register(mux, s.authHandler.RequireAuth, s.authHandler.RequireAdmin)
	s.sessionHandler.Register(mux, s.authHandler.RequireAuth, s.authHandler.RequireAdmin)
	s.deviceHandler.Register(mux, s.authHandler.RequireAuth)
	s.signalHandler.Register(mux)
	s.agentHandler.Register(mux)
	s.storageHandler.Register(mux, s.authHandler.RequireAuth, s.authHandler.RequireAdmin)
	s.cfgHandler.Register(mux, s.authHandler.RequireAuth, s.authHandler.RequireAdmin)

	// The server-enforced gate. Access is never UI-gated.
	admin := func(next http.Handler) http.Handler {
		return s.authHandler.RequireAuth(s.authHandler.RequireAdmin(next))
	}
	s.settingsHandler.Register(mux, admin)
	s.invitesHandler.Register(mux, admin)
	s.enrollHandler.Register(mux, admin)
	s.consoleHandler.Register(mux, admin)
	s.auditHandler.Register(mux, admin)
	s.secretsHandler.Register(mux, admin)
	s.artworkHandler.Register(mux, s.authHandler.RequireAuth, s.authHandler.RequireAdmin)
	// Its two /v1/agent/library/* routes authenticate by node_secret, not a bearer.
	s.libraryHandler.Register(mux, admin)
	s.imagesHandler.Register(mux, admin)

	// Not behind `admin`: /v1/agent/jobs/* authenticates by node_secret.
	s.jobsAgentHandler.Register(mux)

	// Registered unconditionally; the auth split is inside Register. status + claim
	// are unauthenticated because they must work before any admin, and so any bearer
	// token, exists; claim has its own per-boot-token gate and self-disables with 409.
	// POST /v1/setup/complete takes the admin gate.
	s.setupHandler.Register(mux, admin)

	// Auth split inside Register: GET /v1/tls/certificate.pem is unauthenticated
	// because a client that does not yet trust the cert often cannot log in to fetch
	// it, and it discloses only the public half. access-check takes the admin gate.
	s.accessHandler.Register(mux, admin)

	s.jobsHandler.Register(mux, admin)

	// QUASAR_WEB_ROOT. Non-API paths fall through to index.html for client-side
	// routing; in dev the Vite proxy does this instead (vite.config.ts).
	if s.cfg.WebRoot != "" {
		s.log.Info("serving SPA", "root", s.cfg.WebRoot)
		mux.Handle("/", httpx.SPAHandler(s.cfg.WebRoot))
	}
}

// Stop cancels the background goroutines and the coordinator's lifecycle context.
func (s *Services) Stop() {
	s.janitorStop()
	// Wait for an in-flight job to record its outcome; abandoning it leaves a
	// `running` row the claim reaper only aborts an hour later.
	if s.jobsDispatcher != nil {
		s.jobsDispatcher.Wait()
	}
	s.coordinator.Close()
}
