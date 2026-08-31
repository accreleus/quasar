package session

// profiles_handler.go — GET /v1/me/profiles and the admin stream-profile
// (encode rung) surface. Wire shape: control-api.md amendment B1/B3. A launch
// profile carries a `nominal` block plus `rungs[]`; a rung is a codec (no
// separate codecs[]/status enum).

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// profileJSON is the wire shape of a STREAM PROFILE (one encode rung).
type profileJSON struct {
	ID                            string       `json:"id"`
	DisplayName                   string       `json:"display_name"`
	Codec                         string       `json:"codec"`
	Width                         int32        `json:"width"`
	Height                        int32        `json:"height"`
	FPS                           int32        `json:"fps"`
	H264Profile                   string       `json:"h264_profile"`
	NominalBitrateKbps            int32        `json:"nominal_bitrate_kbps"`
	MinOfferBandwidthKbps         int32        `json:"min_offer_bandwidth_kbps"`
	RecommendedOfferBandwidthKbps int32        `json:"recommended_offer_bandwidth_kbps"`
	HeadroomFactor                float64      `json:"headroom_factor"`
	ABRFloorKbps                  int32        `json:"abr_floor_kbps"`
	MaxStartupRTTMs               int32        `json:"max_startup_rtt_ms"`
	MinDecodeHeight               int32        `json:"min_decode_height"`
	HighRefreshDisplay            string       `json:"high_refresh_display"`
	HardwareEncoderRequired       bool         `json:"hardware_encoder_required"`
	BrowserClient                 string       `json:"browser_client"`
	Playout0Ms                    int32        `json:"playout0_ms"`
	Visibility                    string       `json:"visibility"`
	UsedBy                        []ProfileRef `json:"used_by,omitempty"`
	// SessionCount: how many sessions resolved to this rung. sessions.stream_profile_id
	// is a NO ACTION FK, so any historical session blocks delete; omitted when zero.
	SessionCount int `json:"session_count,omitempty"`
	// Warnings reuses the launch-profile writeWarning shape (launch_profiles_handler.go),
	// same {code,message} key. omitempty: only the admin codec-change PATCH populates it.
	Warnings []writeWarning `json:"warnings,omitempty"`
}

// rungEvalJSON is one rung inside a launch profile's evaluation.
type rungEvalJSON struct {
	profileJSON
	Position    int32            `json:"position"`
	Eligibility string           `json:"eligibility"`
	Reasons     []profile.Reason `json:"reasons"`
}

// launchProfileEvalJSON is one entry of GET /v1/me/profiles.
type launchProfileEvalJSON struct {
	ID          string           `json:"id"`
	DisplayName string           `json:"display_name"`
	Description string           `json:"description"`
	Nominal     profile.Nominal  `json:"nominal"`
	Eligibility string           `json:"eligibility"`
	Reasons     []profile.Reason `json:"reasons"`
	Rungs       []rungEvalJSON   `json:"rungs"`
}

// profilesResponse is the GET /v1/me/profiles body.
type profilesResponse struct {
	RecommendedID string                  `json:"recommended_id"`
	Confidence    string                  `json:"confidence"`
	Notes         []profile.Reason        `json:"notes"`
	Profiles      []launchProfileEvalJSON `json:"profiles"`
}

// handleListProfiles serves GET /v1/me/profiles.
func (h *Handler) handleListProfiles(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}

	// (nil, nil) means no usable probe (absent or stale); a read error degrades to no-probe.
	var pr *profile.Probe
	dp, err := h.store.LatestProbe(r.Context(), user.ID)
	if err != nil {
		slog.Warn("AS10-02: probe load failed, evaluating without probe", "user_id", user.ID, "err", err)
	} else if dp != nil {
		pr = &profile.Probe{
			BandwidthKbps:    dp.BandwidthKbps,
			RTTMs:            dp.RTTMs,
			MaxDecodeHeight:  dp.MaxDecodeHeight,
			DisplayRefreshHz: dp.DisplayRefreshHz,
			// h264 decode is always allowed; hevc/av1 come from the probe. nil/stale
			// probe leaves Codecs nil (unknown → allow). Feeds eligibility's
			// codec_not_supported gate only; never touches HostCaps or launch clamps.
			Codecs: map[profile.Codec]bool{
				profile.CodecH264: true,
				profile.CodecHEVC: dp.HEVC,
				profile.CodecAV1:  dp.AV1,
			},
		}
	}

	// Historical failures at launch-profile grain; rung-level decode failures feed
	// the launch resolver's clamp 4 instead.
	var historical map[string]bool
	deviceKey, _ := h.store.LatestDeviceKey(r.Context(), user.ID)
	if hf, err := h.store.ProfileFailures(r.Context(), user.ID, deviceKey); err != nil {
		slog.Warn("AS10-11: profile failures load failed, evaluating without history", "user_id", user.ID, "err", err)
	} else {
		historical = hf
	}

	// No in-code fallback catalog: guessing one would answer an eligibility
	// question with data that does not describe this deployment.
	catalog, err := h.store.ListLaunchProfiles(r.Context(), true)
	if err != nil {
		slog.Error("UI-P4: launch profile load failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load launch profiles")
		return
	}

	// ?app_id narrows the catalogue for the launch menu; convenience only — POST
	// /v1/sessions enforces the same allow-list independently in launcher.go.
	if appID := strings.TrimSpace(r.URL.Query().Get("app_id")); appID != "" {
		restriction, err := h.store.AppProfileRestrictionByID(r.Context(), user.ID, appID)
		switch {
		case errors.Is(err, ErrNotFound):
			// Same rule as GET /v1/apps/{id}: absent, disabled, or not entitled all
			// return 404 (Phase 2 §6.3) — indistinguishable so existence isn't leaked.
			httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "app not found")
			return
		case err != nil:
			slog.Error("UI-P5: app allow-list load failed", "app_id", appID, "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not load the app's launch profiles")
			return
		}
		catalog = restriction.Filter(catalog)
	}

	ev := profile.EvaluateLaunchProfiles(catalog, profile.EvalInput{Probe: pr, HistoricalFailures: historical})
	httpx.WriteJSON(w, http.StatusOK, toProfilesResponse(ev))
}

// toProfilesResponse maps the engine result to the wire shape.
func toProfilesResponse(ev profile.LaunchEvaluation) profilesResponse {
	resp := profilesResponse{
		RecommendedID: ev.RecommendedID,
		Confidence:    string(ev.Confidence),
		Notes:         nonNilReasons(ev.Notes),
		Profiles:      make([]launchProfileEvalJSON, 0, len(ev.Profiles)),
	}
	for _, pe := range ev.Profiles {
		entry := launchProfileEvalJSON{
			ID:          pe.LaunchProfile.ID,
			DisplayName: pe.LaunchProfile.DisplayName,
			Description: pe.LaunchProfile.Description,
			Nominal:     pe.Nominal,
			Eligibility: string(pe.Eligibility),
			Reasons:     nonNilReasons(pe.Reasons),
			Rungs:       make([]rungEvalJSON, 0, len(pe.Rungs)),
		}
		for _, re := range pe.Rungs {
			entry.Rungs = append(entry.Rungs, rungEvalJSON{
				profileJSON: toProfileJSON(re.Profile),
				Position:    re.Position,
				Eligibility: string(re.Eligibility),
				Reasons:     nonNilReasons(re.Reasons),
			})
		}
		resp.Profiles = append(resp.Profiles, entry)
	}
	return resp
}

func toProfileJSON(p profile.Profile) profileJSON {
	return profileJSON{
		ID:                            p.ID,
		DisplayName:                   p.DisplayName,
		Codec:                         string(p.Codec),
		Width:                         p.Width,
		Height:                        p.Height,
		FPS:                           p.FPS,
		H264Profile:                   p.H264Profile,
		NominalBitrateKbps:            p.NominalBitrateKbps,
		MinOfferBandwidthKbps:         p.MinOfferBandwidthKbps,
		RecommendedOfferBandwidthKbps: p.RecommendedOfferBandwidthKbps,
		HeadroomFactor:                p.HeadroomFactor,
		ABRFloorKbps:                  p.ABRFloorKbps,
		MaxStartupRTTMs:               p.MaxStartupRTTMs,
		MinDecodeHeight:               p.MinDecodeHeight,
		HighRefreshDisplay:            string(p.HighRefreshDisplay),
		HardwareEncoderRequired:       p.HardwareEncoderRequired,
		BrowserClient:                 string(p.BrowserClient),
		Playout0Ms:                    p.Playout0Ms,
		Visibility:                    string(p.Visibility),
	}
}

// nonNilReasons returns a non-nil slice so the JSON encodes [] rather than null.
func nonNilReasons(rs []profile.Reason) []profile.Reason {
	if rs == nil {
		return []profile.Reason{}
	}
	return rs
}

// --- admin: stream profiles (= encode rungs) --------------------------------

func (h *Handler) handleAdminListStreamProfiles(w http.ResponseWriter, r *http.Request) {
	profiles, err := h.store.ListRungs(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list stream profiles")
		return
	}
	items := make([]profileJSON, 0, len(profiles))
	for _, p := range profiles {
		j := toProfileJSON(p)
		// Shown in the editor too: editing a shared rung changes every consumer.
		if usedBy, sessions, err := h.store.StreamProfileUsedBy(r.Context(), p.ID); err == nil {
			j.UsedBy = usedBy
			j.SessionCount = sessions
		}
		items = append(items, j)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// streamProfileReq is the admin create/patch payload. All-pointer: an absent
// field means default-on-create / unchanged-on-patch, never a zero that clobbers
// a column.
type streamProfileReq struct {
	ID                            *string  `json:"id"`
	DisplayName                   *string  `json:"display_name"`
	Codec                         *string  `json:"codec"`
	Width                         *int32   `json:"width"`
	Height                        *int32   `json:"height"`
	FPS                           *int32   `json:"fps"`
	H264Profile                   *string  `json:"h264_profile"`
	NominalBitrateKbps            *int32   `json:"nominal_bitrate_kbps"`
	MinOfferBandwidthKbps         *int32   `json:"min_offer_bandwidth_kbps"`
	RecommendedOfferBandwidthKbps *int32   `json:"recommended_offer_bandwidth_kbps"`
	HeadroomFactor                *float64 `json:"headroom_factor"`
	ABRFloorKbps                  *int32   `json:"abr_floor_kbps"`
	MaxStartupRTTMs               *int32   `json:"max_startup_rtt_ms"`
	MinDecodeHeight               *int32   `json:"min_decode_height"`
	HighRefreshDisplay            *string  `json:"high_refresh_display"`
	HardwareEncoderRequired       *bool    `json:"hardware_encoder_required"`
	BrowserClient                 *string  `json:"browser_client"`
	Playout0Ms                    *int32   `json:"playout0_ms"`
	Visibility                    *string  `json:"visibility"`
}

func (req streamProfileReq) applyTo(p profile.Profile) profile.Profile {
	if req.DisplayName != nil {
		p.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if req.Codec != nil {
		p.Codec = profile.Codec(strings.TrimSpace(*req.Codec))
	}
	if req.Width != nil {
		p.Width = *req.Width
	}
	if req.Height != nil {
		p.Height = *req.Height
	}
	if req.FPS != nil {
		p.FPS = *req.FPS
	}
	if req.H264Profile != nil {
		p.H264Profile = *req.H264Profile
	}
	if req.NominalBitrateKbps != nil {
		p.NominalBitrateKbps = *req.NominalBitrateKbps
	}
	if req.MinOfferBandwidthKbps != nil {
		p.MinOfferBandwidthKbps = *req.MinOfferBandwidthKbps
	}
	if req.RecommendedOfferBandwidthKbps != nil {
		p.RecommendedOfferBandwidthKbps = *req.RecommendedOfferBandwidthKbps
	}
	if req.HeadroomFactor != nil {
		p.HeadroomFactor = *req.HeadroomFactor
	}
	if req.ABRFloorKbps != nil {
		p.ABRFloorKbps = *req.ABRFloorKbps
	}
	if req.MaxStartupRTTMs != nil {
		p.MaxStartupRTTMs = *req.MaxStartupRTTMs
	}
	if req.MinDecodeHeight != nil {
		p.MinDecodeHeight = *req.MinDecodeHeight
	}
	if req.HighRefreshDisplay != nil {
		p.HighRefreshDisplay = profile.DisplayReq(*req.HighRefreshDisplay)
	}
	if req.HardwareEncoderRequired != nil {
		p.HardwareEncoderRequired = *req.HardwareEncoderRequired
	}
	if req.BrowserClient != nil {
		p.BrowserClient = profile.BrowserSupport(*req.BrowserClient)
	}
	if req.Playout0Ms != nil {
		p.Playout0Ms = *req.Playout0Ms
	}
	if req.Visibility != nil {
		p.Visibility = profile.Visibility(*req.Visibility)
	}
	return p
}

// newRungDefaults is the base a POST starts from, so an omitted optional field
// lands on a sane value rather than a zero that fails validation.
func newRungDefaults() profile.Profile {
	return profile.Profile{
		H264Profile:        "high",
		HeadroomFactor:     1.5,
		HighRefreshDisplay: profile.DisplayNone,
		BrowserClient:      profile.BrowserSupported,
		Playout0Ms:         50,
		// A rung is never offered standalone, only via the launch profile that
		// lists it.
		Visibility: profile.VisibilityInternal,
	}
}

// handleAdminCreateStreamProfile inserts a rung. No chain guard here: a new rung
// is listed by nothing yet. The h264 floor is checked when a chain adds it
// (resolveRungWrite) or when its codec later changes (patch below).
func (h *Handler) handleAdminCreateStreamProfile(w http.ResponseWriter, r *http.Request) {
	var req streamProfileReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ID == nil || strings.TrimSpace(*req.ID) == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "id is required")
		return
	}
	p := req.applyTo(newRungDefaults())
	p.ID = strings.TrimSpace(*req.ID)
	if err := validateAdminStreamProfile(p); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
		return
	}
	created, err := h.store.CreateStreamProfile(r.Context(), p)
	if errors.Is(err, ErrProfileExists) {
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "a stream profile with that id already exists")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not create stream profile")
		return
	}
	h.recordActivity(r.Context(), actorFromRequest(r), "stream_profile.create", "stream_profile", created.ID,
		map[string]any{"name": created.DisplayName})
	// Bare StreamProfile response per openapi.yaml, not a {"profile": ...} envelope.
	httpx.WriteJSON(w, http.StatusCreated, toProfileJSON(created))
}

func (h *Handler) handleAdminPatchStreamProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	current, err := h.store.GetStreamProfile(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "stream profile not found")
		return
	}

	var req streamProfileReq
	if !decodeJSON(w, r, &req) {
		return
	}
	storedCodec := current.Codec
	current = req.applyTo(current)

	if err := validateAdminStreamProfile(current); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
		return
	}

	// Codec-change chain guard: only needed here, and only when the codec moves.
	// Create can't break a chain (nothing lists a new rung yet); delete is already
	// refuse-if-in-use; a non-codec edit can't change the h264 floor. Do not
	// duplicate this check on those paths.
	var warnings []writeWarning
	if current.Codec != storedCodec {
		broken, ws, err := h.rungCodecChangeImpact(r.Context(), current)
		if err != nil {
			slog.Error("could not evaluate the rung codec change against its launch profiles",
				"stream_profile_id", current.ID, "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal,
				"could not check which launch profiles list this rung")
			return
		}
		if len(broken) > 0 {
			httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, brokenFloorMessage(storedCodec, current.Codec, broken))
			return
		}
		warnings = ws
	}

	updated, err := h.store.UpdateStreamProfile(r.Context(), current)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not save stream profile")
		return
	}
	// id excluded: applyTo never reads it (comes from the path), so listing it
	// would report a change that didn't happen.
	h.recordActivity(r.Context(), actorFromRequest(r), "stream_profile.update", "stream_profile", updated.ID,
		map[string]any{"keys": audit.ChangedKeys(req, "id")})
	j := toProfileJSON(updated)
	j.Warnings = warnings
	httpx.WriteJSON(w, http.StatusOK, j)
}

// rungCodecChangeImpact replays a pending codec change against every launch
// profile that lists this rung and reports the fallout.
//
// Editing a rung's codec reaches the same chains editing its list position does,
// so the h264-floor rule (chainHasH264Floor) must hold here too: the resolver's
// floor dispatches the terminal rung bypassing every clamp, so a chain left with
// no h264 rung fails at dispatch on any host missing that codec.
//
// `broken` lists chains the change would leave with zero h264 rungs (rejected).
// `warnings` are chain-prefixed advisories from the shared launchProfileWarnings,
// describing the resulting chain rather than a diff against the current one.
func (h *Handler) rungCodecChangeImpact(ctx context.Context, updated profile.Profile) ([]ProfileRef, []writeWarning, error) {
	chains, err := h.store.LaunchProfilesContainingRung(ctx, updated.ID)
	if err != nil {
		return nil, nil, err
	}
	var broken []ProfileRef
	warnings := []writeWarning{}
	for _, lp := range chains {
		rungs := make([]profile.Profile, len(lp.Rungs))
		copy(rungs, lp.Rungs)
		for i := range rungs {
			if rungs[i].ID == updated.ID {
				rungs[i] = updated
			}
		}
		if !chainHasH264Floor(rungs) {
			broken = append(broken, ProfileRef{ID: lp.ID, DisplayName: lp.DisplayName})
			continue
		}
		for _, wn := range launchProfileWarnings(rungs) {
			wn.Message = "launch profile " + describeLaunchProfile(lp.ID, lp.DisplayName) + ": " + wn.Message
			warnings = append(warnings, wn)
		}
	}
	if len(warnings) == 0 {
		warnings = nil
	}
	return broken, warnings, nil
}

// brokenFloorMessage names the affected chains: an operator editing a rung only
// sees the rung, not what lists it.
func brokenFloorMessage(from, to profile.Codec, broken []ProfileRef) string {
	names := make([]string, 0, len(broken))
	for _, b := range broken {
		names = append(names, describeLaunchProfile(b.ID, b.DisplayName))
	}
	return "changing this rung's codec from " + string(from) + " to " + string(to) +
		" would leave " + plural(len(broken), "launch profile", "launch profiles") +
		" with no H.264 rung: " + strings.Join(names, ", ") + ". " + errNoH264Floor +
		" — if the chain has no h264 rung the resolver's floor dispatches the terminal rung anyway, " +
		"bypassing every clamp, and the launch fails at dispatch on any host that cannot encode that codec. " +
		"Add an h264 rung to " + plural(len(broken), "that launch profile", "those launch profiles") + " first."
}

// describeLaunchProfile renders `"Display Name" (id)`, or just the id when the two
// are the same (which the seeded ladder's rows are).
func describeLaunchProfile(id, displayName string) string {
	if displayName == "" || displayName == id {
		return id
	}
	return `"` + displayName + `" (` + id + `)`
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// handleAdminDeleteStreamProfile removes a rung. Refuse-if-in-use: 409 while any
// launch profile lists it or any session resolved to it. The disabled UI button
// is UX only; the FK backstop (launch_profile_rungs RESTRICT, sessions NO ACTION)
// turns a miss here into a 500, not this 409.
func (h *Handler) handleAdminDeleteStreamProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Read the name before delete (nothing left to name after); a failed read just
	// drops the detail. Same pattern as handleAdminDeleteLaunchProfile.
	var name string
	if existing, getErr := h.store.GetStreamProfile(r.Context(), id); getErr == nil {
		name = existing.DisplayName
	}
	err := h.store.DeleteStreamProfile(r.Context(), id)
	switch {
	case err == nil:
		var details map[string]any
		if name != "" {
			details = map[string]any{"name": name}
		}
		h.recordActivity(r.Context(), actorFromRequest(r), "stream_profile.delete", "stream_profile", id, details)
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrProfileUnknown):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "stream profile not found")
	case errors.Is(err, ErrProfileInUse):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict,
			"stream profile is still referenced: it is listed by a launch profile, or past sessions "+
				"recorded it as the rung they resolved to. Remove it from every launch profile; a rung "+
				"named by session history can never be deleted, because that history records what each "+
				"session actually got.")
	default:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not delete stream profile")
	}
}

func validateAdminStreamProfile(p profile.Profile) error {
	if strings.TrimSpace(p.DisplayName) == "" {
		return errProfileValidation("display_name is required")
	}
	switch p.Codec {
	case profile.CodecH264, profile.CodecHEVC, profile.CodecAV1:
	default:
		// A rung is a codec (UI-P4 B3): enabled/disabled via launch-profile
		// membership, not a status enum.
		return errProfileValidation("codec must be one of: h264, hevc, av1")
	}
	if p.Width <= 0 || p.Height <= 0 || p.FPS <= 0 {
		return errProfileValidation("width, height and fps must be positive")
	}
	if p.NominalBitrateKbps <= 0 {
		return errProfileValidation("nominal_bitrate_kbps must be positive")
	}
	if p.MinOfferBandwidthKbps < 0 || p.RecommendedOfferBandwidthKbps < 0 || p.ABRFloorKbps < 0 || p.MaxStartupRTTMs < 0 || p.MinDecodeHeight < 0 || p.Playout0Ms < 0 {
		return errProfileValidation("profile numeric limits must be non-negative")
	}
	if p.HeadroomFactor <= 0 {
		return errProfileValidation("headroom_factor must be positive")
	}
	if !ValidH264Profile(p.H264Profile) {
		return errProfileValidation("h264_profile must be constrained-baseline, main, or high")
	}
	if p.HighRefreshDisplay != profile.DisplayNone && p.HighRefreshDisplay != profile.DisplayRecommended && p.HighRefreshDisplay != profile.DisplayRequired {
		return errProfileValidation("high_refresh_display must be none, recommended, or required")
	}
	if p.BrowserClient != profile.BrowserRecommended && p.BrowserClient != profile.BrowserSupported && p.BrowserClient != profile.BrowserRisky {
		return errProfileValidation("browser_client must be recommended, supported, or risky")
	}
	if p.Visibility != profile.VisibilityUser && p.Visibility != profile.VisibilityDebug && p.Visibility != profile.VisibilityInternal {
		return errProfileValidation("visibility must be user, debug, or internal")
	}
	return nil
}

type errProfileValidation string

func (e errProfileValidation) Error() string { return string(e) }

func actorFromRequest(r *http.Request) string {
	user, _ := auth.UserFromContext(r.Context())
	return user.ID
}
