// quasar-updater is the per-host updater (CONTEXT.md "Updater"): the actor that
// pulls a platform release and recreates the containers it replaces, because a
// container cannot recreate itself.
//
// It ships as its own image (deploy/Dockerfile.updater) and runs as the compose
// service `quasar-updater` beside the stack it updates, with the docker socket
// and the stack directory mounted. It serves a unix socket in a named volume
// the stack's other containers share, and reports through one result file per
// request id in that same volume.
//
// IT IS NOT PART OF A PLATFORM RELEASE. The release manifest names the control
// plane and the node agent — the two images an apply moves by digest. The
// updater is what MOVES them, so it cannot be one of them; it updates by hand
// (`docker compose pull quasar-updater && docker compose up -d quasar-updater`,
// docs/upgrading.md).
//
// The logic lives in internal/updater and is stdlib-only, deliberately: this
// binary is in the control-plane module for one build and one test command, not
// because it shares anything with the control plane at run time.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/updater"
)

// Build stamps, set with -ldflags by deploy/Dockerfile.updater.
var (
	version      = "dev"
	sourceCommit = ""
)

const (
	defaultSocketPath = "/run/quasar-updater/updater.sock"
	defaultResultsDir = "/run/quasar-updater/results"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.Printf("quasar-updater %s (commit %s) starting", version, orDash(sourceCommit))

	socketPath := envOr("QUASAR_UPDATER_SOCKET", defaultSocketPath)
	resultsDir := envOr("QUASAR_UPDATER_RESULTS_DIR", defaultResultsDir)
	docker := updater.CLI{Bin: envOr("QUASAR_UPDATER_DOCKER_BIN", "docker")}

	// FAIL CLOSED. Without its own compose labels there is no correct compose
	// invocation to guess at, and a guess would act on the wrong project. Exit
	// loudly rather than serve something that might recreate the wrong
	// containers — `restart: unless-stopped` will retry, and the log line says
	// exactly what is missing.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	project, workingDir, configFiles, err := updater.Discover(ctx, docker)
	cancel()
	if err != nil {
		log.Fatalf("self-discovery failed: %v", err)
	}

	cfg := updater.Config{
		Project:           project,
		WorkingDir:        workingDir,
		ConfigFiles:       configFiles,
		AllowedNamespaces: updater.ParseNamespaces(os.Getenv("QUASAR_UPDATER_ALLOWED_NAMESPACES")),
		WaitTimeoutS:      envInt("QUASAR_UPDATER_WAIT_TIMEOUT_S", updater.DefaultWaitTimeoutS),
	}
	log.Printf("compose project %q, working dir %s, files %v", cfg.Project, cfg.WorkingDir, cfg.ConfigFiles)
	log.Printf("allowed namespaces: %v", cfg.AllowedNamespaces)

	store, err := updater.NewStore(resultsDir)
	if err != nil {
		log.Fatalf("results directory: %v", err)
	}

	srv := &updater.Server{
		Store:           store,
		Docker:          docker,
		Cfg:             cfg,
		EnvPath:         updater.EnvPathFor(cfg),
		PullTimeout:     time.Duration(envInt("QUASAR_UPDATER_PULL_TIMEOUT_S", 3600)) * time.Second,
		RecreateTimeout: time.Duration(envInt("QUASAR_UPDATER_RECREATE_TIMEOUT_S", 900)) * time.Second,
		Version:         version,
	}

	ln, err := updater.Listen(socketPath)
	if err != nil {
		log.Fatalf("listen on %s: %v", socketPath, err)
	}
	log.Printf("listening on %s (mode 0666), results in %s", socketPath, resultsDir)

	http := &http.Server{
		Handler: srv.Handler(),
		// The apply itself is detached, so no handler is long-running; these
		// bound a stuck peer, not the work.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		log.Printf("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = http.Shutdown(shutCtx)
	}()

	if err := http.Serve(ln); err != nil && !errors.Is(err, ErrClosed) {
		log.Printf("server stopped: %v", err)
	}
	_ = os.Remove(socketPath)
}

// ErrClosed is http.ErrServerClosed, aliased so the import block above stays
// about the things this file actually configures.
var ErrClosed = http.ErrServerClosed

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Printf("%s=%q is not a positive integer; using %d", name, v, def)
		return def
	}
	return n
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
