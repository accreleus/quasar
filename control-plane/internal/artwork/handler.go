package artwork

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// Handler serves the artwork surface.
type Handler struct {
	svc     *Service
	log     *slog.Logger
	auditor interface {
		Record(context.Context, string, string, string, string, map[string]any) error
	}
}

// NewHandler builds the handler. svc may be nil only in route-registration
// tests; every request path checks it.
func NewHandler(svc *Service, log *slog.Logger, auditors ...interface {
	Record(context.Context, string, string, string, string, map[string]any) error
}) *Handler {
	h := &Handler{svc: svc, log: log}
	if len(auditors) > 0 {
		h.auditor = auditors[0]
	}
	return h
}

// Register wires the routes. Admin gating is server-side at the middleware on
// every mutating route (CLAUDE.md invariant #6). The asset GET is deliberately
// unauthenticated: a browser cannot attach an Authorization header to an
// <img src>, so the URL is a capability instead — the path is the SHA-256 of
// the image bytes (unguessable, reveals nothing), serving cached bytes only
// from a validated content-addressed name with nosniff and an immutable cache.
func (h *Handler) Register(mux httpx.Router, requireAuth, requireAdmin func(http.Handler) http.Handler) {
	admin := func(next http.Handler) http.Handler { return requireAuth(requireAdmin(next)) }

	mux.HandleFunc("GET /v1/artwork/{asset}", h.handleAsset)

	mux.Handle("GET /v1/admin/apps/{id}/artwork", admin(http.HandlerFunc(h.handleGet)))
	mux.Handle("PUT /v1/admin/apps/{id}/artwork", admin(http.HandlerFunc(h.handleApply)))
	mux.Handle("DELETE /v1/admin/apps/{id}/artwork", admin(http.HandlerFunc(h.handleClear)))
	mux.Handle("POST /v1/admin/apps/{id}/artwork/search", admin(http.HandlerFunc(h.handleSearch)))
	mux.Handle("POST /v1/admin/apps/{id}/artwork/upload", admin(http.HandlerFunc(h.handleUpload)))

	// Catalogue-wide, so it hangs off /v1/admin/artwork rather than a per-app
	// path: it names no app id, and pretending otherwise would need a fake one.
	mux.Handle("POST /v1/admin/artwork/reresolve", admin(http.HandlerFunc(h.handleReresolve)))
}

// --- wire shapes -------------------------------------------------------------

type artworkResp struct {
	AppID       string  `json:"app_id"`
	Source      string  `json:"source"`
	Provider    string  `json:"provider"`
	ProviderRef string  `json:"provider_ref"`
	MatchedName string  `json:"matched_name"`
	TileURL     *string `json:"tile_url"`
	HeroURL     *string `json:"hero_url"`
	Attribution string  `json:"attribution"`
	Locked      bool    `json:"locked"`
	UpdatedAt   string  `json:"updated_at"`
}

type artworkEnvelope struct {
	// Artwork is null when the app has none — the gradient-tile state.
	Artwork *artworkResp `json:"artwork"`
	// ProviderConfigured tells the admin UI whether the provider-backed
	// controls can do anything, so it can explain their absence instead of
	// offering a button that silently fails.
	ProviderConfigured bool   `json:"provider_configured"`
	ProviderName       string `json:"provider_name"`
	// ProviderOrigin is WHERE the credential in effect came from: "database"
	// (an admin set it through the UI), "environment" (the legacy
	// QUASAR_STEAMGRIDDB_API_KEY), "static" or "none". An operator upgrading a
	// deployment that already had the env var must be able to see, without
	// guessing, that it is still what is being used.
	ProviderOrigin string `json:"provider_origin"`
	// ProviderProblem explains why the provider is unavailable despite something
	// being configured (e.g. the master key does not match the stored key). ""
	// when there is nothing to explain. NEVER contains any part of a credential.
	ProviderProblem string `json:"provider_problem,omitempty"`
}

// envelopeFor builds the provider half of every response from one live
// resolution, so a single request cannot report two different provider states.
func (h *Handler) envelopeFor(r *http.Request) artworkEnvelope {
	info := h.svc.ProviderStatus(r.Context())
	return artworkEnvelope{
		ProviderConfigured: info.Configured,
		ProviderName:       info.Name,
		ProviderOrigin:     info.Origin,
		ProviderProblem:    info.Problem,
	}
}

type candidateResp struct {
	Ref      string `json:"ref"`
	Name     string `json:"name"`
	ThumbURL string `json:"thumb_url"`
}

func toResp(r Record) *artworkResp {
	out := &artworkResp{
		AppID:       r.AppID,
		Source:      r.Source,
		Provider:    r.Provider,
		ProviderRef: r.ProviderRef,
		MatchedName: r.MatchedName,
		Attribution: r.Attribution,
		Locked:      r.Locked,
		UpdatedAt:   r.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if r.TileAsset != "" {
		u := AssetURL(r.TileAsset)
		out.TileURL = &u
	}
	if r.HeroAsset != "" {
		u := AssetURL(r.HeroAsset)
		out.HeroURL = &u
	}
	return out
}

// --- handlers ----------------------------------------------------------------

// handleAsset serves a cached blob.
func (h *Handler) handleAsset(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		http.NotFound(w, r)
		return
	}
	data, contentType, err := h.svc.Blobs().Open(r.PathValue("asset"))
	if err != nil {
		// A bad name and a missing file are the same answer. Never 500: an
		// invalid name is an attempted traversal or a stale link.
		if errors.Is(err, ErrBadAsset) || errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		h.log.Warn("artwork: could not read asset", "err", err)
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	// nosniff matters here more than anywhere: this is the one route that
	// returns bytes a third party supplied. The bytes were already verified to
	// be a real JPEG/PNG/WebP, and this stops a browser second-guessing that.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Content-addressed name ⇒ the bytes at this URL can never change, so it is
	// safe to cache forever. This is what makes art survive a redeploy without
	// a single refetch at browse time.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	_, _ = w.Write(data)
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	rec, ok, err := h.svc.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeErr(w, err, "could not read artwork")
		return
	}
	env := h.envelopeFor(r)
	if ok {
		env.Artwork = toResp(rec)
	}
	httpx.WriteJSON(w, http.StatusOK, env)
}

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	var req struct {
		Query string `json:"query"`
	}
	// A body is optional: with none, the app's own name is the query.
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req)
	}
	cands, err := h.svc.Search(r.Context(), r.PathValue("id"), req.Query)
	if err != nil {
		h.writeErr(w, err, "could not search for artwork")
		return
	}
	out := make([]candidateResp, 0, len(cands))
	for _, c := range cands {
		out = append(out, candidateResp{Ref: c.Ref, Name: c.Name, ThumbURL: c.ThumbURL})
	}
	info := h.svc.ProviderStatus(r.Context())
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"candidates":          out,
		"provider_configured": info.Configured,
		"provider_name":       info.Name,
		"provider_origin":     info.Origin,
	})
}

// handleApply is the admin override. Exactly one of three intents:
//   - {"provider_ref": "..."} — accept a candidate from the search results
//   - {"tile_url"/"hero_url"} — fetch operator-supplied art (SSRF-guarded)
//   - {"rematch": true}       — re-run automatic matching now
//
// `force` only qualifies `rematch`: a rematch refuses a locked row (409) unless
// the caller says it means it. The other two intents SET locked themselves, so
// they were never blocked by it.
func (h *Handler) handleApply(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	appID := r.PathValue("id")
	var req struct {
		ProviderRef string `json:"provider_ref"`
		TileURL     string `json:"tile_url"`
		HeroURL     string `json:"hero_url"`
		Rematch     bool   `json:"rematch"`
		Force       bool   `json:"force"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid JSON body")
		return
	}

	var (
		rec Record
		err error
	)
	switch {
	case req.Rematch:
		rec, err = h.svc.ResolveByID(r.Context(), appID, req.Force)
	case strings.TrimSpace(req.ProviderRef) != "":
		rec, err = h.svc.ApplyCandidate(r.Context(), appID, req.ProviderRef)
	case strings.TrimSpace(req.TileURL) != "" || strings.TrimSpace(req.HeroURL) != "":
		rec, err = h.svc.ApplyURLs(r.Context(), appID, req.TileURL, req.HeroURL)
	default:
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"supply provider_ref, tile_url/hero_url, or rematch")
		return
	}
	if err != nil {
		h.writeErr(w, err, "could not apply artwork")
		return
	}
	h.record(r, "app.artwork.set", appID)
	env := h.envelopeFor(r)
	env.Artwork = toResp(rec)
	httpx.WriteJSON(w, http.StatusOK, env)
}

func (h *Handler) handleUpload(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	appID := r.PathValue("id")
	crop := r.URL.Query().Get("crop")
	if crop == "" {
		crop = CropTile
	}
	// MaxBytesReader is the hard stop: it caps what the SERVER will read at all,
	// independent of Content-Length, which a client controls and can lie about.
	limit := h.svc.fetcher.maxBytes
	r.Body = http.MaxBytesReader(w, r.Body, limit+1)
	data, err := copyLimited(r.Body, limit)
	if err != nil {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, httpx.CodeValidationFailed,
			"image is too large")
		return
	}
	rec, err := h.svc.Upload(r.Context(), appID, crop, r.Header.Get("Content-Type"), data)
	if err != nil {
		h.writeErr(w, err, "could not store the uploaded image")
		return
	}
	h.record(r, "app.artwork.upload", appID)
	env := h.envelopeFor(r)
	env.Artwork = toResp(rec)
	httpx.WriteJSON(w, http.StatusOK, env)
}

// handleReresolve re-runs automatic matching across the whole catalogue —
// needed because a provider-query change (#385) cannot reach apps that already
// have a row (Resolve returns early for those). An explicit admin action, not
// a boot-time migration, because it spends third-party requests. Locked rows
// are skipped and counted; `{"force": true}` is the only way to overwrite an
// operator's manual correction.
func (h *Handler) handleReresolve(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	var req struct {
		Force bool `json:"force"`
	}
	// A body is optional; absent means force=false, the safe default.
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req)
	}
	res, err := h.svc.ReresolveAll(r.Context(), req.Force)
	if err != nil {
		h.writeErr(w, err, "could not re-resolve artwork")
		return
	}
	// Details are counts, never app names or ids: a bounded, non-secret payload
	// well under admin_activity's 4096-byte CHECK however large the catalogue is.
	h.recordDetails(r, "app.artwork.reresolve", "", map[string]any{
		"count":          res.Resolved,
		"skipped_locked": res.SkippedLocked,
		"failed":         res.Failed,
		"total":          res.Total,
		"force":          req.Force,
	})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"total":          res.Total,
		"resolved":       res.Resolved,
		"skipped_locked": res.SkippedLocked,
		"failed":         res.Failed,
	})
}

func (h *Handler) handleClear(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	appID := r.PathValue("id")
	if err := h.svc.Clear(r.Context(), appID); err != nil {
		h.writeErr(w, err, "could not clear artwork")
		return
	}
	h.record(r, "app.artwork.cleared", appID)
	w.WriteHeader(http.StatusNoContent)
}

// --- plumbing ----------------------------------------------------------------

// ready guards the routes when the service could not be constructed (e.g. the
// cache directory is not writable). 503 rather than 500: the deployment is
// otherwise healthy and the library still renders gradients.
func (h *Handler) ready(w http.ResponseWriter) bool {
	if h.svc == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, httpx.CodeInternal,
			"the artwork service is not available on this deployment")
		return false
	}
	return true
}

// writeErr maps the package's sentinel errors to contract status codes. Anything
// unrecognised is a 500 with a generic message — never the raw error, which can
// carry an internal URL or path.
func (h *Handler) writeErr(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, ErrAppNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "app not found")
	case errors.Is(err, ErrProviderNotConfigured):
		// 409, not 500: not opting in to a provider is the documented default.
		// The reason comes from the typed error's ProviderInfo (fixed strings),
		// never by slicing err.Error() — an internal error string has no path
		// into this response body by construction.
		msg := "no artwork provider is configured on this deployment"
		var unavailable *ProviderUnavailableError
		if errors.As(err, &unavailable) && unavailable.Info.Problem != "" {
			msg = unavailable.Info.Problem
		}
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, msg)
	case errors.Is(err, ErrUnsupportedType):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"the image must be a JPEG, PNG or WebP")
	case errors.Is(err, ErrBlockedAddress):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"that URL resolves to a non-public address and will not be fetched")
	case errors.Is(err, ErrFetchFailed):
		// The URL, not the server, is what failed — 400 so an operator fixes the
		// input rather than filing a control-plane bug. The underlying error is
		// logged, never returned: it can carry a resolved internal address.
		h.log.Info("artwork: fetch failed", "err", err)
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"could not fetch an image from that URL")
	case errors.Is(err, ErrLocked):
		// 409, not 403: the caller is allowed to do this, the RESOURCE is in a
		// state that refuses it — and `force` is the documented way through.
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict,
			"this app's artwork is locked by an admin override; send force to replace it")
	case errors.Is(err, ErrInvalidRequest):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
	default:
		h.log.Warn("artwork: request failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, fallback)
	}
}

func (h *Handler) record(r *http.Request, action, appID string) {
	h.recordDetails(r, action, appID, nil)
}

// recordDetails is record with an allowlisted details payload. appID may be ""
// for a catalogue-wide action — the store NULLIFs it, and target_type stays
// "app" so the audit list still filters coherently.
func (h *Handler) recordDetails(r *http.Request, action, appID string, details map[string]any) {
	if h.auditor == nil {
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	if err := h.auditor.Record(r.Context(), user.ID, action, "app", appID, details); err != nil {
		h.log.Warn("artwork: record admin activity failed", "action", action, "err", err)
	}
}
