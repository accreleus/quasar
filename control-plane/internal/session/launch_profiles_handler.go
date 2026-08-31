package session

// launch_profiles_handler.go — the admin launch-profile surface (UI-P4).
// Every route is registered under the `admin` chain (RequireAuth →
// RequireAdmin), server-enforced never UI-gated (CLAUDE.md invariant #6); the
// disabled Delete button on a referenced profile is UX only, the 409 is the gate.

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// Write-warning codes. A warning is never a failure (control-api.md A4).
const (
	// warnH264FloorNotLast: rungs after the h264 rung are unreachable, since
	// h264 passes every clamp.
	warnH264FloorNotLast = "h264_floor_not_last"
	// warnFloorNotLeastDemanding: the h264 floor is harder to satisfy than a
	// rung above it, defeating its purpose as the rung that always works.
	warnFloorNotLeastDemanding = "floor_not_least_demanding"
)

type writeWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type launchProfileRungJSON struct {
	Position      int32       `json:"position"`
	StreamProfile profileJSON `json:"stream_profile"`
}

type launchProfileJSON struct {
	ID          string                  `json:"id"`
	DisplayName string                  `json:"display_name"`
	Description string                  `json:"description"`
	Visibility  string                  `json:"visibility"`
	SortOrder   int32                   `json:"sort_order"`
	Rungs       []launchProfileRungJSON `json:"rungs"`
	UsedBy      *LaunchProfileUsedBy    `json:"used_by,omitempty"`
	Warnings    []writeWarning          `json:"warnings"`
}

func toLaunchProfileJSON(lp profile.LaunchProfile) launchProfileJSON {
	out := launchProfileJSON{
		ID:          lp.ID,
		DisplayName: lp.DisplayName,
		Description: lp.Description,
		Visibility:  string(lp.Visibility),
		SortOrder:   lp.SortOrder,
		Rungs:       make([]launchProfileRungJSON, 0, len(lp.Rungs)),
		Warnings:    []writeWarning{},
	}
	for i, r := range lp.Rungs {
		out.Rungs = append(out.Rungs, launchProfileRungJSON{
			Position:      int32(i + 1),
			StreamProfile: toProfileJSON(r),
		})
	}
	out.Warnings = launchProfileWarnings(lp.Rungs)
	return out
}

// chainHasH264Floor reports whether a chain has the h264 floor rung (respec
// §2.7): at least one rung whose codec is h264. One rule, two call sites — do
// not duplicate it. A chain loses its floor either by editing the rung list
// (resolveRungWrite) or by changing a listed rung's codec out from under it
// (rungCodecChangeImpact, profiles_handler.go); both must call this.
func chainHasH264Floor(rungs []profile.Profile) bool {
	for _, r := range rungs {
		if r.Codec == profile.CodecH264 {
			return true
		}
	}
	return false
}

// launchProfileWarnings computes the two advisory warnings. Neither is ever a
// rejection — see resolveRungWrite for why "h264 must be LAST" cannot be one.
// Shared with the stream-profile codec-change path for the same reason
// chainHasH264Floor is: one rule, one implementation.
func launchProfileWarnings(rungs []profile.Profile) []writeWarning {
	out := []writeWarning{}
	lastH264 := -1
	for i, r := range rungs {
		if r.Codec == profile.CodecH264 {
			lastH264 = i
		}
	}
	if lastH264 < 0 {
		return out
	}
	if lastH264 != len(rungs)-1 {
		out = append(out, writeWarning{warnH264FloorNotLast,
			"the H.264 rung is not last: every rung after it is unreachable, because H.264 passes every clamp"})
	}
	floor := rungs[lastH264]
	for i, r := range rungs {
		if i == lastH264 {
			continue
		}
		harder := floor.MinOfferBandwidthKbps > r.MinOfferBandwidthKbps ||
			floor.MinDecodeHeight > r.MinDecodeHeight ||
			(floor.HardwareEncoderRequired && !r.HardwareEncoderRequired)
		if harder {
			out = append(out, writeWarning{warnFloorNotLeastDemanding,
				"the H.264 floor rung is harder to satisfy than rung " + r.ID +
					" (higher min_offer_bandwidth_kbps / min_decode_height, or it requires a hardware encoder while that rung does not)"})
			break
		}
	}
	return out
}

func (h *Handler) handleAdminListLaunchProfiles(w http.ResponseWriter, r *http.Request) {
	lps, err := h.store.ListLaunchProfiles(r.Context(), false)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list launch profiles")
		return
	}
	items := make([]launchProfileJSON, 0, len(lps))
	for _, lp := range lps {
		j := toLaunchProfileJSON(lp)
		if usedBy, err := h.store.LaunchProfileUsedByFor(r.Context(), lp.ID); err == nil {
			u := usedBy
			j.UsedBy = &u
		}
		items = append(items, j)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) handleAdminGetLaunchProfile(w http.ResponseWriter, r *http.Request) {
	lp, err := h.store.GetLaunchProfile(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrLaunchProfileUnknown) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "launch profile not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load launch profile")
		return
	}
	j := toLaunchProfileJSON(lp)
	if usedBy, err := h.store.LaunchProfileUsedByFor(r.Context(), lp.ID); err == nil {
		u := usedBy
		j.UsedBy = &u
	}
	// Bare LaunchProfile, per openapi.yaml `/v1/admin/launch-profiles/{id}`.
	httpx.WriteJSON(w, http.StatusOK, j)
}

// launchProfileReq is the create/patch payload. Rungs is the ordered array of
// stream-profile ids; the server assigns `position` from that order. All-pointer:
// an absent field falls to the schema default on create, unchanged on patch,
// never a zero value clobbering a column.
type launchProfileReq struct {
	ID          *string   `json:"id"`
	DisplayName *string   `json:"display_name"`
	Description *string   `json:"description"`
	Visibility  *string   `json:"visibility"`
	SortOrder   *int32    `json:"sort_order"`
	Rungs       *[]string `json:"rungs"`
}

func (h *Handler) handleAdminCreateLaunchProfile(w http.ResponseWriter, r *http.Request) {
	var req launchProfileReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ID == nil || strings.TrimSpace(*req.ID) == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "id is required")
		return
	}
	if req.DisplayName == nil || strings.TrimSpace(*req.DisplayName) == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "display_name is required")
		return
	}
	if req.Rungs == nil || len(*req.Rungs) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "rungs must be a non-empty ordered array of stream profile ids")
		return
	}
	if req.Visibility != nil && !validProfileVisibility(*req.Visibility) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "visibility must be user, debug, or internal")
		return
	}

	rungs, err := h.resolveRungWrite(r, *req.Rungs)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
		return
	}

	name := strings.TrimSpace(*req.DisplayName)
	created, err := h.store.CreateLaunchProfile(r.Context(), LaunchProfileWrite{
		ID:          strings.TrimSpace(*req.ID),
		DisplayName: &name,
		Description: req.Description,
		Visibility:  req.Visibility,
		SortOrder:   req.SortOrder,
		Rungs:       rungIDs(rungs),
	})
	if errors.Is(err, ErrLaunchProfileExists) {
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "a launch profile with that id already exists")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not create launch profile")
		return
	}
	h.recordActivity(r.Context(), actorFromRequest(r), "launch_profile.create", "launch_profile", created.ID,
		map[string]any{"name": created.DisplayName})
	httpx.WriteJSON(w, http.StatusCreated, toLaunchProfileJSON(created))
}

func (h *Handler) handleAdminPatchLaunchProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req launchProfileReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.DisplayName != nil && strings.TrimSpace(*req.DisplayName) == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "display_name cannot be empty")
		return
	}
	if req.Visibility != nil && !validProfileVisibility(*req.Visibility) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "visibility must be user, debug, or internal")
		return
	}

	write := LaunchProfileWrite{
		Description: req.Description,
		Visibility:  req.Visibility,
		SortOrder:   req.SortOrder,
	}
	if req.DisplayName != nil {
		name := strings.TrimSpace(*req.DisplayName)
		write.DisplayName = &name
	}
	if req.Rungs != nil {
		if len(*req.Rungs) == 0 {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "rungs must be a non-empty ordered array of stream profile ids")
			return
		}
		rungs, err := h.resolveRungWrite(r, *req.Rungs)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
			return
		}
		write.Rungs = rungIDs(rungs)
	}

	updated, err := h.store.UpdateLaunchProfile(r.Context(), id, write)
	if errors.Is(err, ErrLaunchProfileUnknown) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "launch profile not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not save launch profile")
		return
	}
	// `id` excluded: the PATCH handler never applies it (the id comes from the path).
	h.recordActivity(r.Context(), actorFromRequest(r), "launch_profile.update", "launch_profile", updated.ID,
		map[string]any{"keys": audit.ChangedKeys(req, "id")})
	httpx.WriteJSON(w, http.StatusOK, toLaunchProfileJSON(updated))
}

// handleAdminDeleteLaunchProfile removes a launch profile. Refuse-if-referenced:
// 409 while any app, the global policy, or any user preference points at it
// (all three are FKs as of migration 0036; this 409 is the gate, the FKs the
// backstop). Allow-list membership (app_launch_profiles) is deliberately not in
// the refuse set: it's a restriction naming this profile, so its row cascades
// away with it (migration 0037). Since that can widen an app's menu (an emptied
// allow-list means unrestricted again), affected apps are captured before the
// delete and logged, so the widening is recorded rather than silent.
func (h *Handler) handleAdminDeleteLaunchProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Read failure degrades to no detail; it must not block the delete.
	var widened []AppRef
	if apps, err := h.store.AppsAllowListing(r.Context(), id); err != nil {
		slog.Warn("UI-P5: could not resolve apps allow-listing the launch profile", "profile_id", id, "err", err)
	} else {
		widened = apps
	}
	err := h.store.DeleteLaunchProfile(r.Context(), id)
	switch {
	case err == nil:
		var details map[string]any
		if len(widened) > 0 {
			names := make([]string, 0, len(widened))
			for _, a := range widened {
				names = append(names, a.Name)
			}
			details = map[string]any{"allow_list_widened_apps": names}
		}
		h.recordActivity(r.Context(), actorFromRequest(r), "launch_profile.delete", "launch_profile", id, details)
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrLaunchProfileUnknown):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "launch profile not found")
	case errors.Is(err, ErrLaunchProfileInUse):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict,
			"launch profile is referenced by an app, the global policy, or a user preference — point them elsewhere first")
	default:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not delete launch profile")
	}
}

// resolveRungWrite validates the ordered rung id array and returns the resolved
// stream profiles in order.
//
// A launch profile must contain at least one h264 rung: 400 otherwise, on
// create and patch, no grandfathering (migration 0036 guarantees every migrated
// chain satisfies this). "Must be last" is a warning, not a rejection: rejecting
// would make a migrated chain with h264 first (today's default order)
// permanently uneditable, and adds no safety since the resolver's unconditional
// floor dispatches the last h264 rung bypassing every clamp regardless of
// position — "not last" only makes later rungs unreachable, a usability defect.
func (h *Handler) resolveRungWrite(r *http.Request, ids []string) ([]profile.Profile, error) {
	seen := make(map[string]bool, len(ids))
	out := make([]profile.Profile, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, errProfileValidation("rungs[] entries must be non-empty stream profile ids")
		}
		if seen[id] {
			return nil, errProfileValidation("a stream profile may appear at most once in a launch profile: " + id)
		}
		seen[id] = true
		p, err := h.store.GetStreamProfile(r.Context(), id)
		if err != nil {
			// Caught at write time so it's never an FK error surfacing at launch.
			return nil, errProfileValidation("unknown stream profile id: " + id)
		}
		// GetStreamProfile also returns legacy (pre-0036, codec IS NULL) rows kept
		// only so a code-level revert finds its data; listing one as a rung would be
		// silently useless (recorded unknown_codec, skipped on every launch).
		if p.Codec == "" {
			return nil, errProfileValidation("stream profile is not a rung (it has no codec, i.e. it is a pre-UI-P4 legacy row): " + id)
		}
		out = append(out, p)
	}
	if !chainHasH264Floor(out) {
		return nil, errProfileValidation(errNoH264Floor)
	}
	return out, nil
}

// errNoH264Floor is the single wording of the floor rejection, shared by the two
// call sites so an operator gets the same sentence whichever way they broke it.
const errNoH264Floor = "a launch profile must contain at least one rung whose codec is h264 (the unconditional resolution floor)"

func rungIDs(ps []profile.Profile) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

func validProfileVisibility(v string) bool {
	switch profile.Visibility(v) {
	case profile.VisibilityUser, profile.VisibilityDebug, profile.VisibilityInternal:
		return true
	}
	return false
}
