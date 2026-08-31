package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/access"
	"github.com/accreleus/quasar/control-plane/internal/config"
	"github.com/accreleus/quasar/control-plane/internal/db"
	"github.com/accreleus/quasar/control-plane/internal/devauth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/internal/tlsx"
	"github.com/accreleus/quasar/control-plane/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	log := newLogger(cfg.LogLevel)
	log.Info("quasar control-plane starting")

	// Configuration that parsed but is probably not what the operator meant.
	// Emitted here because config.Load runs before a logger exists.
	for _, w := range cfg.Warnings {
		log.Warn(w)
	}

	// Dev-only agent auth (#399), gate 2: QUASAR_DEV_AGENT_AUTH=1 with
	// QUASAR_ENV=production refuses to boot, here before migrations/pool/
	// listener so a misconfigured production deploy serves nothing.
	devCfg := devauth.LoadConfig()
	if err := devCfg.Validate(); err != nil {
		return err
	}

	// Database preflight (#518), before migrations: a bad DATABASE_URL is
	// diagnosed as what it is instead of surfacing as a migration failure
	// wrapping a raw pgx dial error. Once this passes, migrate.Run's failures
	// are genuinely about migrations.
	preflightCtx, preflightCancel := context.WithTimeout(context.Background(), 10*time.Second)
	preflightErr := db.Preflight(preflightCtx, cfg.DatabaseURL)
	preflightCancel()
	if preflightErr != nil {
		return fmt.Errorf("database: %w", preflightErr)
	}

	// --- Migrations ----------------------------------------------------------
	log.Info("running migrations")
	if err := migrate.Run(migrations.FS, cfg.DatabaseURL); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	log.Info("migrations ok")

	// --- Database pool -------------------------------------------------------
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := db.Open(ctx, cfg.DatabaseURL, cfg.DBStatementTimeout, cfg.DBLockTimeout)
	cancel()
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()
	log.Info("database connected")

	// TLS certificate (#376; hot-reload per wizard v2 §S6d). Resolved before the
	// services so the access surface wraps the live certificate and a bad pair
	// on disk is fatal before any listener exists; the bind happens further down.
	var certManager *access.Manager
	if cfg.TLSEnabled() {
		certPath, keyPath, source, err := resolveTLSCert(cfg, log)
		if err != nil {
			return fmt.Errorf("tls: %w", err)
		}
		certManager, err = access.NewManager(certPath, keyPath, source, log)
		if err != nil {
			return fmt.Errorf("tls: %w", err)
		}
	}

	// --- Services ------------------------------------------------------------
	svc, err := NewServices(cfg, pool, log, certManager)
	if err != nil {
		return err
	}
	defer svc.Stop()

	// --- HTTP server ---------------------------------------------------------
	mux := http.NewServeMux()
	svc.RegisterRoutes(mux)

	// Dev-only agent auth (#399): here, never in Services.RegisterRoutes — that
	// is the production route table TestOpenAPIDrift records, and this route
	// must be absent from it under every configuration. Flag off ⇒ nothing is
	// wired and the path 404s from the mux. The devauth.reaper job (NewServices)
	// runs regardless of the flag: a once-enabled instance can still hold a
	// not-yet-expired ephemeral identity.
	if devCfg.Enabled {
		secret, err := devauth.MintSecret()
		if err != nil {
			return fmt.Errorf("dev agent auth: %w", err)
		}
		devSvc := devauth.NewService(svc.authSvc, secret, log)
		devauth.Register(mux, devCfg, devSvc, log)
		// The key goes to the log and the file: anyone who can read either
		// already has host access.
		wrote := devauth.WriteKeyFile(devCfg.KeyPath, secret, log)
		log.Warn("dev agent auth key (this boot only)",
			"key", secret,
			"key_file", devCfg.KeyPath,
			"key_file_written", wrote)
	}

	handler := httpx.SecurityHeaders(mux)
	// Compress before logging wraps it, so the logged bytes_out is the real
	// on-the-wire size (#386). Compress skips WebSocket upgrades entirely and
	// both wrappers forward Hijack, so the agent WS and signaling relay are
	// unaffected — see internal/httpx/compress.go.
	if cfg.Compression {
		handler = httpx.Compress(handler)
	} else {
		log.Warn("response compression disabled", "knob", "QUASAR_COMPRESSION")
	}
	// AccessLog is "off" | "errors" | "all" (#517); RequestLog itself decides
	// what a given level does with each request, including "off" being a
	// zero-overhead pass-through — see internal/httpx/requestlog.go.
	if cfg.AccessLog == "off" {
		log.Warn("request access log disabled", "knob", "QUASAR_ACCESS_LOG")
	}
	handler = httpx.RequestLog(handler, log, httpx.AccessLogLevel(cfg.AccessLog), cfg.TrustedProxies)
	// #438: say out loud which state the deployment is in. Both are legitimate
	// — the empty default is right for a direct-LAN install — but "behind a
	// proxy with nothing configured" is the silent failure the issue is about,
	// and this is the line that lets an operator notice it.
	if len(cfg.TrustedProxies) > 0 {
		nets := make([]string, 0, len(cfg.TrustedProxies))
		for _, n := range cfg.TrustedProxies {
			nets = append(nets, n.String())
		}
		log.Info("trusted reverse proxies configured; X-Forwarded-For will be honoured from these networks",
			"knob", "QUASAR_TRUSTED_PROXIES", "networks", strings.Join(nets, ","))
	} else {
		log.Info("no trusted reverse proxies configured; rate limits key on the direct peer address",
			"knob", "QUASAR_TRUSTED_PROXIES",
			"note", "set this to the proxy's network if this control plane sits behind one, or every client shares one budget")
	}
	newServer := func(addr string, h http.Handler) *http.Server {
		// NOTE: the WebSocket endpoints (agent WS, signaling relay) survive these
		// timeouts because gorilla/websocket's Upgrade hijacks the connection,
		// removing it from the server's deadline management. If the upgrader is
		// ever replaced with a non-hijacking one, the 10s WriteTimeout would
		// silently kill every long-lived WS (#148).
		return &http.Server{
			Addr:         addr,
			Handler:      h,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		}
	}
	// Browser routes on the plaintext listener 308-redirect to the HTTPS port
	// (WS upgrades get 426); agent routes (/health, /agent/ws, /v1/agent/*) stay
	// plain-HTTP. Plain HTTP is not a secure context, so a browser landing on it
	// gets silently broken Keyboard Lock / Gamepad APIs — better to never serve
	// it the SPA at all. QUASAR_HTTP_REDIRECT=off restores dual-serve.
	httpHandler := handler
	if cfg.HTTPRedirectEnabled() {
		httpHandler = httpx.RedirectToHTTPS(handler, cfg.TLSRedirectPort)
		log.Info("http→https redirect enabled", "redirect_port", cfg.TLSRedirectPort)
	}
	srv := newServer(cfg.ListenAddr, httpHandler)

	// Optional HTTPS listener (#376): the same handler over TLS so the browser
	// gets a secure context. Fatal on misconfiguration when QUASAR_TLS != off.
	var tlsSrv *http.Server
	var tlsListener net.Listener
	if cfg.TLSEnabled() {
		// Bind synchronously so a bad address / port-in-use is a loud fatal at
		// startup rather than a swallowed goroutine error.
		ln, err := net.Listen("tcp", cfg.TLSAddr)
		if err != nil {
			return fmt.Errorf("tls: listen %q: %w", cfg.TLSAddr, err)
		}
		tlsListener = ln
		tlsSrv = newServer(cfg.TLSAddr, handler)
		// The certificate comes from a callback, not a file pair — the indirection
		// that lets POST /v1/admin/tls/certificate take effect without a restart
		// (§S6d): the next handshake picks up the swapped pointer, no rebind.
		tlsSrv.TLSConfig = certManager.TLSConfig()
	}

	// Debug/profiling listener (PROF-01, #388): a third http.Server, loopback by
	// default (see newDebugServer for why it is not a /debug/* route). A bind
	// failure is not fatal — diagnostics must never take down the control
	// plane — but is logged at Error so it cannot pass unnoticed.
	var debugSrv *http.Server
	var debugListener net.Listener
	if cfg.PprofEnabled() {
		if ln, lnErr := net.Listen("tcp", cfg.PprofAddr); lnErr != nil {
			log.Error("debug listener unavailable — no pprof on this process",
				"addr", cfg.PprofAddr, "knob", "QUASAR_PPROF_ADDR", "err", lnErr)
		} else {
			debugListener = ln
			debugSrv = newDebugServer(cfg.PprofAddr, pool, log)
			log.Info("debug listener enabled",
				"addr", cfg.PprofAddr,
				"access", "docker exec <container> wget -qO- "+cfg.PprofAddr+"/debug/pprof/",
				"knob", "QUASAR_PPROF_ADDR")
		}
	} else {
		log.Info("debug listener disabled", "knob", "QUASAR_PPROF_ADDR")
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("listening", "addr", cfg.ListenAddr, "scheme", "http")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "err", err)
			stop()
		}
	}()

	if tlsSrv != nil {
		go func() {
			// Empty paths: the certificate is supplied by TLSConfig.GetCertificate
			// (see above), which is what ServeTLS falls back to when given none.
			if err := tlsSrv.ServeTLS(tlsListener, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("tls server error", "err", err)
				stop()
			}
		}()
	}

	if debugSrv != nil {
		go func() {
			// Deliberately does NOT stop() the process: a dead diagnostics
			// listener is not a reason to stop serving sessions.
			if err := debugSrv.Serve(debugListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("debug server error", "err", err)
			}
		}()
	}

	<-ctx.Done()
	log.Info("shutting down")

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()

	var shutErr error
	if debugSrv != nil {
		// Shut the debug listener first and swallow its error: a profile capture
		// still in flight must not be the reason the process reports a failed
		// shutdown.
		_ = debugSrv.Shutdown(shutCtx)
	}
	if tlsSrv != nil {
		if err := tlsSrv.Shutdown(shutCtx); err != nil {
			shutErr = err
		}
	}
	if err := srv.Shutdown(shutCtx); err != nil {
		shutErr = err
	}
	return shutErr
}

// resolveTLSCert returns the cert/key file paths for the HTTPS listener and logs
// one line describing the mode. In "auto" with QUASAR_TLS_CERT+KEY set it uses
// them; otherwise it generates/reuses a persisted self-signed pair under
// QUASAR_TLS_DIR. An unwritable dir is a clear fatal telling the operator to
// mount a volume.
// source is the access.Source* constant naming which mode won, so
// GET /v1/admin/access-check can tell an operator whether they are looking at
// the batteries-included pair or their own mounted files.
func resolveTLSCert(cfg *config.Config, log *slog.Logger) (certPath, keyPath, source string, err error) {
	if cfg.TLSProvided() {
		fp, fpErr := tlsx.Fingerprint(cfg.TLSCert)
		if fpErr != nil {
			log.Warn("tls fingerprint", "err", fpErr)
			fp = "unknown"
		}
		log.Info("TLS enabled", "mode", "provided", "addr", cfg.TLSAddr, "cert", cfg.TLSCert, "fingerprint", fp)
		return cfg.TLSCert, cfg.TLSKey, access.SourceProvided, nil
	}

	sans := tlsx.GatherSANs(cfg.TLSHosts, cfg.PublicHost)
	certPath, keyPath, generated, err := tlsx.EnsureSelfSigned(cfg.TLSDir, sans)
	if err != nil {
		return "", "", "", err
	}
	fp, fpErr := tlsx.Fingerprint(certPath)
	if fpErr != nil {
		log.Warn("tls fingerprint", "err", fpErr)
		fp = "unknown"
	}
	action := "reused"
	if generated {
		action = "generated"
	}
	log.Info("TLS enabled",
		"mode", "self-signed",
		"cert_action", action,
		"addr", cfg.TLSAddr,
		"cert", certPath,
		"fingerprint", fp)
	log.Info("TLS self-signed: browsers show a one-time warning; accept the exception to reach the secure-context web client")
	log.Info("TLS: the certificate is downloadable at GET /v1/tls/certificate.pem — compare its fingerprint against the value logged above before trusting it")
	return certPath, keyPath, access.SourceSelfSigned, nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
