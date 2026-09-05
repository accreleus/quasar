// Package platform serves the platform-release admin read surface
// (control-api.md §"Platform releases", amendment 1): what this control plane
// is, what has been published, and what each target could be moved to.
//
// Every decision is PlanRelease (plan.go), which is pure; this file gathers the
// reads. The apply half's endpoints are apply_handler.go (#116); its reads join
// this one through Deps.OpenAttempts, so the page needs a single fetch.
package platform

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// Deps are the live reads the view is computed from. Function fields, not
// interfaces: internal/settings and internal/jobs sit above this package, and a
// test wants closures rather than fakes. Wired in cmd/quasar-control/app.go.
type Deps struct {
	// Read per request, so a channel switch needs no restart.
	Channel func(ctx context.Context) (channel, edgeBranch string, err error)
	// Hosts is the identity projection of the host list, in that list's order.
	Hosts    func(ctx context.Context) ([]HostIdentity, error)
	Releases func(ctx context.Context, channel string) ([]Release, error)
	// Detection reports when detection last SUCCEEDED and the last failure.
	Detection func(ctx context.Context) (DetectionStatus, error)
	// OpenAttempts is every non-terminal apply on the instance (amendment 2).
	// Optional: a build with no apply store wired leaves it nil, and the view
	// then reports nothing in flight rather than failing.
	OpenAttempts func(ctx context.Context) ([]Attempt, error)
	// ActiveRun is the fleet run that owns the fleet, or nil. Optional, as
	// OpenAttempts is.
	ActiveRun func(ctx context.Context) (*ApplyRun, error)
	// UpdaterPresent reports whether an updater sits beside THIS control plane;
	// false makes the control-plane target `updater_absent`. Nil reads as
	// absent, which refuses rather than attempts.
	UpdaterPresent func() bool
}

// errNoDeps is what a handler built with no dependencies answers with, rather
// than dereferencing nil.
var errNoDeps = errors.New("platform release view has no dependencies wired")

// Handler serves the platform-release read surface.
type Handler struct {
	deps *Deps
	log  *slog.Logger
}

// NewHandler builds the platform HTTP handler. nil deps leaves identity (a pure
// read of this binary's stamps) working and the release view answering 500.
func NewHandler(deps *Deps, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{deps: deps, log: log}
}

// Register wires the admin platform routes. admin must compose
// RequireAuth→RequireAdmin — the server-enforced gate, never UI hiding
// (control-api.md §Authorization).
func (h *Handler) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/admin/platform/identity", admin(http.HandlerFunc(h.handleIdentity)))
	mux.Handle("GET /v1/admin/platform/releases", admin(http.HandlerFunc(h.handleReleases)))
}

// The `{ "identity": … }` envelope openapi.yaml declares.
type identityResponse struct {
	Identity buildinfo.Identity `json:"identity"`
}

// No 404 shape: an unstamped build reports "dev" with two nulls, never fails.
func (h *Handler) handleIdentity(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, identityResponse{Identity: buildinfo.Get()})
}

// The whole Releases page in one read. READ ONLY: it writes nothing and never
// triggers detection — "check now" is POST /v1/admin/jobs/{job_id}/run.
func (h *Handler) handleReleases(w http.ResponseWriter, r *http.Request) {
	if h.deps == nil {
		h.log.Error("platform release view has no dependencies wired")
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read the release view")
		return
	}
	view, err := h.releaseView(r.Context())
	if err != nil {
		h.log.Error("build platform release view", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read the release view")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

// ReleaseView is the view the page reads, exposed so the apply endpoints
// evaluate eligibility against exactly it — one evaluation, one vocabulary.
func (h *Handler) ReleaseView(ctx context.Context) (View, error) {
	if h.deps == nil {
		return View{}, errNoDeps
	}
	return h.releaseView(ctx)
}

func (h *Handler) releaseView(ctx context.Context) (View, error) {
	channel, edgeBranch, err := h.deps.Channel(ctx)
	if err != nil {
		return View{}, err
	}
	if !ValidChannel(channel) {
		channel = ChannelStable
	}
	hosts, err := h.deps.Hosts(ctx)
	if err != nil {
		return View{}, err
	}
	releases, err := h.deps.Releases(ctx, channel)
	if err != nil {
		return View{}, err
	}
	status, err := h.deps.Detection(ctx)
	if err != nil {
		return View{}, err
	}
	var open []Attempt
	if h.deps.OpenAttempts != nil {
		open, err = h.deps.OpenAttempts(ctx)
		if err != nil {
			return View{}, err
		}
	}
	var run *ApplyRun
	if h.deps.ActiveRun != nil {
		run, err = h.deps.ActiveRun(ctx)
		if err != nil {
			return View{}, err
		}
	}
	return PlanRelease(PlanInputs{
		Channel:      channel,
		EdgeBranch:   edgeBranch,
		ControlPlane: buildinfo.Get(),
		Hosts:        hosts,
		Releases:     releases,
		CheckedAt:    status.CheckedAt,
		LastError:    status.LastError,
		OpenAttempts: open,
		ActiveRun:    run,

		UpdaterPresent: h.deps.UpdaterPresent != nil && h.deps.UpdaterPresent(),
	}), nil
}
