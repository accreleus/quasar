package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// SPT-06 — admin encoder certification endpoints.
//
// Certification measures real encode performance per (profile × bitrate) cell,
// which requires a live WebRTC peer (webrtcbin gates frame flow until one
// connects — without it fps≈0 and every cell falsely reads `unsafe`). The peer
// (Chrome-for-Testing + playwright) lives on the deploy host, so certification
// is script-orchestrated: `scripts/harness/run-spt06-certify.sh` drives a CFT
// peer per cell while the control plane does launch + measurement + verdict +
// cap.
//
// GET  .../encoder-certification                       list cert rows for a host.
// POST .../encoder-certification/runs                   open a run, return the cell plan.
// POST .../encoder-certification/cells                  launch one bench cell, no verdict yet.
// POST .../cells/{session_id}/finalize                  measure, verdict, cap, teardown.
// POST .../runs/{run_id}/complete                       close a run.
// GET  .../runs/{run_id}                                poll run status.
//
// All admin-gated via the requireAdmin wrapper in Register.

// defaultCertProfiles: bench profile IDs when the POST body omits them. Must
// reference stream_profiles rows; unknown IDs are silently skipped.
var defaultCertProfiles = []string{"1080p60", "720p60", "720p30"}

var defaultCertBitratesKbps = []int{4000, 6000, 8000, 12000}

// benchWindowSec: default per-cell measurement window; the script drives the
// CFT peer for this long before calling /finalize.
const benchWindowSec = 25

// certWarmupMs: leading warmup dropped from agent samples at finalize, since
// the session reaches `running` before the CFT peer connects and webrtcbin
// gates frames until then (~0 fps). Agent heartbeat is ~5s, so 6s clears it.
const certWarmupMs int64 = 6000

type certResp struct {
	HostID         string        `json:"host_id"`
	Certifications []certRowResp `json:"certifications"`
}

type certRowResp struct {
	ID       string `json:"id"`
	GPUIndex int    `json:"gpu_index"`
	Encoder  string `json:"encoder"`
	// ProfileID is context; StreamProfileID is what the scheduler cap keys on
	// (migration 0041).
	ProfileID       string    `json:"profile_id"`
	StreamProfileID string    `json:"stream_profile_id"`
	Width           int       `json:"width"`
	Height          int       `json:"height"`
	FPS             int       `json:"fps"`
	BitrateKbps     int       `json:"bitrate_kbps"`
	Verdict         string    `json:"verdict"`
	EncodeP50       float64   `json:"encode_ms_p50"`
	EncodeP95       float64   `json:"encode_ms_p95"`
	EncodeMax       float64   `json:"encode_ms_max"`
	OutputFPS       float64   `json:"output_fps"`
	DropRate        float64   `json:"drop_rate"`
	LiveWriteStable bool      `json:"live_write_stable"`
	SampleWindowMs  int       `json:"sample_window_ms"`
	SampleCount     int       `json:"sample_count"`
	AgentVersion    *string   `json:"agent_version,omitempty"`
	MeasuredAt      time.Time `json:"measured_at"`
}

func (h *Handler) handleGetEncoderCerts(w http.ResponseWriter, r *http.Request) {
	hostID := r.PathValue("id")
	if hostID == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "missing host id")
		return
	}

	if _, err := h.store.GetHost(r.Context(), hostID); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "host lookup failed")
		return
	}

	var filter CertFilter
	if v := r.URL.Query().Get("gpu_index"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid gpu_index")
			return
		}
		filter.GPUIndex = &n
	}
	if v := r.URL.Query().Get("encoder"); v != "" {
		filter.Encoder = &v
	}
	if v := r.URL.Query().Get("profile_id"); v != "" {
		filter.ProfileID = &v
	}
	if v := r.URL.Query().Get("stream_profile_id"); v != "" {
		filter.StreamProfileID = &v
	}
	if v := r.URL.Query().Get("max_age_s"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid max_age_s")
			return
		}
		d := time.Duration(n) * time.Second
		filter.MaxAge = &d
	}

	rows, err := h.store.GetEncoderCerts(r.Context(), hostID, filter)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "cert query failed")
		return
	}

	resp := certResp{HostID: hostID, Certifications: make([]certRowResp, 0, len(rows))}
	for _, row := range rows {
		resp.Certifications = append(resp.Certifications, toCertRowResp(row))
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

type certRunReq struct {
	GPUIndex     int      `json:"gpu_index"`
	Encoder      string   `json:"encoder"`
	Profiles     []string `json:"profiles"`
	BitratesKbps []int    `json:"bitrates_kbps"`
}

// certCellPlan: one bench cell, a (rung × bitrate) point since migration 0041.
// ProfileID is echoed for harness grouping; StreamProfileID is what /cells and
// /finalize should carry.
type certCellPlan struct {
	ProfileID       string `json:"profile_id"`
	StreamProfileID string `json:"stream_profile_id"`
	Codec           string `json:"codec"`
	BitrateKbps     int    `json:"bitrate_kbps"`
}

type certRunResp struct {
	RunID     string         `json:"run_id"`
	HostID    string         `json:"host_id"`
	Status    string         `json:"status"`
	StartedAt time.Time      `json:"started_at"`
	TotalPts  int            `json:"total_pts"`
	Encoder   string         `json:"encoder"`
	GPUIndex  int            `json:"gpu_index"`
	Cells     []certCellPlan `json:"cells"`
}

// handleTriggerCertRun validates host + encoder, computes the (profile ×
// bitrate) cell plan, reserves the one-per-host run lock, and returns the plan.
// It does not launch any bench session — the script drives each cell via
// /cells and finalizes it.
func (h *Handler) handleTriggerCertRun(w http.ResponseWriter, r *http.Request) {
	hostID := r.PathValue("id")
	if hostID == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "missing host id")
		return
	}

	host, err := h.store.GetHost(r.Context(), hostID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "host lookup failed")
		return
	}
	if host.Status != "online" {
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "host is not online")
		return
	}

	var req certRunReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Encoder == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "encoder is required (va, nvenc, openh264, or vulkan)")
		return
	}
	switch req.Encoder {
	case "va", "nvenc", "openh264", "vulkan":
	default:
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "encoder must be va, nvenc, openh264, or vulkan")
		return
	}

	profiles := req.Profiles
	if len(profiles) == 0 {
		profiles = defaultCertProfiles
	}
	bitrates := req.BitratesKbps
	if len(bitrates) == 0 {
		bitrates = defaultCertBitratesKbps
	}

	// Expand each launch profile into its rungs (0041): a cert is a statement
	// about one rung, so a multi-codec chain must visit every rung or leave
	// others uncertified. On today's single-rung chains the plan is unchanged,
	// one field richer. Unknown ids are skipped; a rung id may be named
	// directly to certify one codec of a chain.
	cells := make([]certCellPlan, 0, len(profiles)*len(bitrates))
	for _, p := range profiles {
		for _, target := range h.expandCertTargets(r.Context(), p) {
			wire, ok := catalogToWire(target.Rung.Codec)
			if !ok {
				continue
			}
			for _, b := range bitrates {
				cells = append(cells, certCellPlan{
					ProfileID:       target.LaunchProfileID,
					StreamProfileID: target.Rung.ID,
					Codec:           wire,
					BitrateKbps:     b,
				})
			}
		}
	}
	totalPts := len(cells)

	run, ok := h.coord.certRuns.Start(hostID, totalPts)
	if !ok {
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "a certification run is already in progress for this host")
		return
	}

	httpx.WriteJSON(w, http.StatusAccepted, certRunResp{
		RunID:     run.ID,
		HostID:    hostID,
		Status:    run.Status,
		StartedAt: run.StartedAt,
		TotalPts:  totalPts,
		Encoder:   req.Encoder,
		GPUIndex:  req.GPUIndex,
		Cells:     cells,
	})
}

// expandCertTargets turns one plan id into the cells it stands for: every rung
// of a launch profile, or the single named rung. An id resolving to neither
// yields nothing (skipped).
func (h *Handler) expandCertTargets(ctx context.Context, id string) []CertTarget {
	if lp, err := h.store.GetLaunchProfile(ctx, id); err == nil {
		out := make([]CertTarget, 0, len(lp.Rungs))
		for _, r := range lp.Rungs {
			out = append(out, CertTarget{Rung: r, LaunchProfileID: lp.ID})
		}
		return out
	}
	target, err := h.store.ResolveCertTarget(ctx, "", id)
	if err != nil {
		return nil
	}
	return []CertTarget{target}
}

type certCellReq struct {
	GPUIndex int    `json:"gpu_index"`
	Encoder  string `json:"encoder"`
	// StreamProfileID names the rung to certify (0041), what the run plan
	// returns. ProfileID still accepted: resolves to that chain's first h264
	// rung, so an un-updated harness keeps measuring what it always measured.
	ProfileID       string `json:"profile_id"`
	StreamProfileID string `json:"stream_profile_id"`
	BitrateKbps     int    `json:"bitrate_kbps"`
}

type certCellResp struct {
	SessionID       string  `json:"session_id"`
	HostID          string  `json:"host_id"`
	ProfileID       string  `json:"profile_id"`
	StreamProfileID string  `json:"stream_profile_id"`
	Codec           string  `json:"codec"`
	BitrateKbps     int     `json:"bitrate_kbps"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	FPS             int     `json:"fps"`
	BudgetMs        float64 `json:"budget_ms"`
	SignalingURL    string  `json:"signaling_url"`
	SignToken       string  `json:"signaling_token"`
}

// handleCertCellLaunch launches one pinned bench cell session and returns its
// signaling token for a real CFT peer. The session reaches `running` before
// this returns; measurement + verdict happen later via /finalize.
func (h *Handler) handleCertCellLaunch(w http.ResponseWriter, r *http.Request) {
	hostID := r.PathValue("id")
	if hostID == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "missing host id")
		return
	}
	host, err := h.store.GetHost(r.Context(), hostID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "host lookup failed")
		return
	}
	if host.Status != "online" {
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "host is not online")
		return
	}

	var req certCellReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ProfileID == "" && req.StreamProfileID == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"stream_profile_id (or profile_id) is required")
		return
	}
	if req.BitrateKbps <= 0 {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "bitrate_kbps must be > 0")
		return
	}

	target, targetErr := h.store.ResolveCertTarget(r.Context(), req.ProfileID, req.StreamProfileID)
	if targetErr != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"unresolvable certification target: "+targetErr.Error())
		return
	}

	out, launchErr := h.coord.launchCertCell(r.Context(), hostID, req.GPUIndex, target, req.BitrateKbps)
	if launchErr != nil {
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "cert cell launch failed: "+launchErr.Error())
		return
	}

	prof := target.Rung
	wire, _ := catalogToWire(prof.Codec)
	budgetMs := 1000.0 / float64(prof.FPS)
	httpx.WriteJSON(w, http.StatusCreated, certCellResp{
		SessionID:       out.sessionID,
		HostID:          hostID,
		ProfileID:       target.LaunchProfileID,
		StreamProfileID: prof.ID,
		Codec:           wire,
		BitrateKbps:     req.BitrateKbps,
		Width:           int(prof.Width),
		Height:          int(prof.Height),
		FPS:             int(prof.FPS),
		BudgetMs:        budgetMs,
		SignalingURL:    h.signalingURL(r),
		SignToken:       out.signToken,
	})
}

type certFinalizeReq struct {
	GPUIndex        int    `json:"gpu_index"`
	Encoder         string `json:"encoder"`
	ProfileID       string `json:"profile_id"`
	StreamProfileID string `json:"stream_profile_id"`
	BitrateKbps     int    `json:"bitrate_kbps"`
	RunID           string `json:"run_id,omitempty"`
}

type certFinalizeResp struct {
	SessionID       string  `json:"session_id"`
	ProfileID       string  `json:"profile_id"`
	StreamProfileID string  `json:"stream_profile_id"`
	Codec           string  `json:"codec"`
	BitrateKbps     int     `json:"bitrate_kbps"`
	Verdict         string  `json:"verdict"`
	EncodeP50       float64 `json:"encode_ms_p50"`
	EncodeP95       float64 `json:"encode_ms_p95"`
	EncodeMax       float64 `json:"encode_ms_max"`
	OutputFPS       float64 `json:"output_fps"`
	DropRate        float64 `json:"drop_rate"`
	LiveWriteStable bool    `json:"live_write_stable"`
	SampleCount     int     `json:"sample_count"`
}

// handleCertCellFinalize reads the bench session's accumulated agent metrics,
// derives the verdict, upserts the cert row, and tears the session down. The
// caller must have driven a real CFT peer first. No agent samples ⇒ refuses to
// write a verdict, 422 — never a bogus `unsafe` row that would mis-cap a
// capable host.
func (h *Handler) handleCertCellFinalize(w http.ResponseWriter, r *http.Request) {
	hostID := r.PathValue("id")
	sessionID := r.PathValue("session_id")
	if hostID == "" || sessionID == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "missing host id or session id")
		return
	}

	var req certFinalizeReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Encoder == "" || (req.ProfileID == "" && req.StreamProfileID == "") || req.BitrateKbps <= 0 {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"encoder, stream_profile_id (or profile_id) and bitrate_kbps are required")
		return
	}

	target, targetErr := h.store.ResolveCertTarget(r.Context(), req.ProfileID, req.StreamProfileID)
	if targetErr != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"unresolvable certification target: "+targetErr.Error())
		return
	}
	prof := target.Rung

	verdict, m, ferr := h.coord.finalizeCertCell(r.Context(), hostID, sessionID, req.GPUIndex,
		req.Encoder, target, req.BitrateKbps)
	if ferr != nil {
		httpx.WriteError(w, http.StatusUnprocessableEntity, httpx.CodeValidationFailed, ferr.Error())
		return
	}

	if req.RunID != "" {
		h.coord.certRuns.Increment(req.RunID, verdict)
	}

	wire, _ := catalogToWire(prof.Codec)
	httpx.WriteJSON(w, http.StatusOK, certFinalizeResp{
		SessionID:       sessionID,
		ProfileID:       target.LaunchProfileID,
		StreamProfileID: prof.ID,
		Codec:           wire,
		BitrateKbps:     req.BitrateKbps,
		Verdict:         verdict,
		EncodeP50:       m.EncodeP50,
		EncodeP95:       m.EncodeP95,
		EncodeMax:       m.EncodeMax,
		OutputFPS:       m.OutputFPS,
		DropRate:        m.DropRate,
		LiveWriteStable: m.LiveWriteStable,
		SampleCount:     m.SampleCount,
	})
}

func (h *Handler) handleCompleteCertRun(w http.ResponseWriter, r *http.Request) {
	hostID := r.PathValue("id")
	runID := r.PathValue("run_id")
	if hostID == "" || runID == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "missing host id or run id")
		return
	}
	run, ok := h.coord.certRuns.Get(runID, hostID)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "cert run not found")
		return
	}
	h.coord.certRuns.Complete(runID, run.SummaryOK, run.SummaryCapped, run.SummaryUnsafe)
	updated, _ := h.coord.certRuns.Get(runID, hostID)
	httpx.WriteJSON(w, http.StatusOK, certRunStatusResp{
		RunID:         updated.ID,
		HostID:        updated.HostID,
		Status:        updated.Status,
		StartedAt:     updated.StartedAt,
		EndedAt:       updated.EndedAt,
		ErrorMessage:  updated.ErrorMessage,
		CompletedPts:  updated.CompletedPts,
		TotalPts:      updated.TotalPts,
		SummaryOK:     updated.SummaryOK,
		SummaryCapped: updated.SummaryCapped,
		SummaryUnsafe: updated.SummaryUnsafe,
	})
}

type certRunStatusResp struct {
	RunID         string     `json:"run_id"`
	HostID        string     `json:"host_id"`
	Status        string     `json:"status"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	ErrorMessage  *string    `json:"error_message,omitempty"`
	CompletedPts  int        `json:"completed_pts"`
	TotalPts      int        `json:"total_pts"`
	SummaryOK     int        `json:"summary_ok"`
	SummaryCapped int        `json:"summary_capped"`
	SummaryUnsafe int        `json:"summary_unsafe"`
}

func (h *Handler) handleGetCertRun(w http.ResponseWriter, r *http.Request) {
	hostID := r.PathValue("id")
	runID := r.PathValue("run_id")
	if hostID == "" || runID == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "missing host id or run id")
		return
	}

	if _, err := h.store.GetHost(r.Context(), hostID); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "host lookup failed")
		return
	}

	run, ok := h.coord.certRuns.Get(runID, hostID)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "cert run not found")
		return
	}

	httpx.WriteJSON(w, http.StatusOK, certRunStatusResp{
		RunID:         run.ID,
		HostID:        run.HostID,
		Status:        run.Status,
		StartedAt:     run.StartedAt,
		EndedAt:       run.EndedAt,
		ErrorMessage:  run.ErrorMessage,
		CompletedPts:  run.CompletedPts,
		TotalPts:      run.TotalPts,
		SummaryOK:     run.SummaryOK,
		SummaryCapped: run.SummaryCapped,
		SummaryUnsafe: run.SummaryUnsafe,
	})
}

// benchRunToRunningTimeout: early-exit wait for a bench session to reach
// `running`, so a stuck point doesn't block the whole cert run (the normal
// 90s stuck-start watchdog still fires independently).
const benchRunToRunningTimeout = 120 * time.Second

type certCellLaunchOut struct {
	sessionID string
	signToken string // plaintext signaling token for the CFT peer
}

// launchCertCell launches one pinned bench cell session, dispatches
// assign→start, waits for `running`, and returns the session id + a fresh
// signaling token for a real CFT peer. Does not measure or tear down (the
// script drives frames, then finalizeCertCell); a launch or running-wait
// failure tears the session down here and returns an error.
func (c *Coordinator) launchCertCell(
	ctx context.Context,
	hostID string, gpuIndex int,
	target CertTarget, bitrateKbps int,
) (certCellLaunchOut, error) {
	prof := target.Rung
	wire, ok := catalogToWire(prof.Codec)
	if !ok {
		return certCellLaunchOut{}, fmt.Errorf("rung %s has an unknown codec %q", prof.ID, prof.Codec)
	}
	c.log.Info("SPT-06: cert cell launch",
		"host_id", hostID, "gpu_index", gpuIndex,
		"profile_id", target.LaunchProfileID, "stream_profile_id", prof.ID,
		"codec", wire, "bitrate_kbps", bitrateKbps)

	// The bench must stream the codec it labels (0041): refuse up front when
	// the host can't encode it, rather than an opaque "never reached running".
	hostCodecs, hcErr := c.store.HostCodecs(ctx, hostID)
	if hcErr != nil {
		c.log.Warn("SPT-06: host codec set load failed, assuming h264-only",
			"host_id", hostID, "err", hcErr)
		hostCodecs = nil
	}
	if !codecSet(hostCodecs)[wire] {
		return certCellLaunchOut{}, fmt.Errorf("host cannot encode %s (rung %s): %w", wire, prof.ID, ErrCodecUnsupportedByHost)
	}

	userID, err := c.store.EnsureBenchUser(ctx)
	if err != nil {
		return certCellLaunchOut{}, fmt.Errorf("bench user setup failed: %w", err)
	}
	diagAppID, err := c.store.GetDiagnosticsAppID(ctx)
	if err != nil {
		return certCellLaunchOut{}, fmt.Errorf("diagnostics app lookup failed: %w", err)
	}
	if diagAppID == "" {
		return certCellLaunchOut{}, fmt.Errorf("'Quasar Stream Diagnostics' app not found or not enabled; " +
			"create the app with that exact name before running certification")
	}

	tok, err := newSignalingToken(time.Now())
	if err != nil {
		return certCellLaunchOut{}, fmt.Errorf("signaling token failed: %w", err)
	}

	p := CreateParams{
		UserID:          userID,
		AppID:           diagAppID,
		Width:           prof.Width,
		Height:          prof.Height,
		FPS:             prof.FPS,
		BitrateKbps:     int32(bitrateKbps),
		H264Profile:     "constrained-baseline",
		Codec:           wire, // set, not left to the h264 default: cert must match its filed codec (0041)
		ProfileID:       target.LaunchProfileID,
		NeedEncodeSlots: 1,
		// SkipVramVeto stays false (#383 §4.4): a VRAM-pressured pinned host is a
		// real finding. AppImage stays empty: the bench app isn't catalog-managed.
		TokenHash:    tok.Hash,
		TokenExpires: tok.ExpiresAt,
		PinHostID:    hostID, // SPT-06: force to the host being certified
	}

	sess, schedErr := c.store.ScheduleAndCreate(ctx, p)
	if schedErr != nil {
		c.logVramVetoRejection(userID, diagAppID, schedErr)
		return certCellLaunchOut{}, fmt.Errorf("schedule failed: %w", schedErr)
	}
	sessionID := sess.ID

	go c.dispatchAssignStart(sess, nil)

	if !c.waitForRunning(ctx, sessionID, benchRunToRunningTimeout) {
		c.teardownCertSession(sessionID)
		return certCellLaunchOut{}, fmt.Errorf("session %s did not reach running", sessionID)
	}

	return certCellLaunchOut{sessionID: sessionID, signToken: tok.Plaintext}, nil
}

// finalizeCertCell reads the bench session's agent metrics, derives the
// verdict, upserts the cert row, and tears the session down on every path.
// Zero agent samples means the peer never drove frames — returns an error and
// writes no cert row rather than a fabricated `unsafe` that would mis-cap a
// capable host; the session is still torn down.
func (c *Coordinator) finalizeCertCell(
	ctx context.Context,
	hostID, sessionID string, gpuIndex int, encoder string,
	target CertTarget, bitrateKbps int,
) (string, *CertMetrics, error) {
	defer c.teardownCertSession(sessionID)
	prof := target.Rung

	m, aggErr := c.store.AggregateMetricsForSessionWindow(ctx, sessionID, certWarmupMs)
	if aggErr != nil {
		return "", nil, fmt.Errorf("metrics aggregation failed: %w", aggErr)
	}
	if m == nil || m.SampleCount == 0 {
		c.log.Warn("SPT-06: cert finalize — no agent metrics (no peer drove frames?), refusing to write verdict",
			"host_id", hostID, "session_id", sessionID,
			"profile_id", prof.ID, "bitrate_kbps", bitrateKbps)
		return "", nil, fmt.Errorf("no agent metrics for session %s — the bench peer never drove frames; "+
			"no cert row written (would falsely read unsafe)", sessionID)
	}

	budgetMs := 1000.0 / float64(prof.FPS)
	verdict := DeriveVerdict(m.EncodeP95, budgetMs, m.OutputFPS, float64(prof.FPS), m.DropRate)

	var agentVersion *string
	if host, herr := c.store.GetHost(ctx, hostID); herr == nil {
		agentVersion = host.AgentVersion
	}

	row := EncoderCertRow{
		HostID:          hostID,
		GPUIndex:        gpuIndex,
		Encoder:         encoder,
		ProfileID:       target.LaunchProfileID,
		StreamProfileID: prof.ID,
		Width:           int(prof.Width),
		Height:          int(prof.Height),
		FPS:             int(prof.FPS),
		BitrateKbps:     bitrateKbps,
		Verdict:         verdict,
		EncodeP50:       m.EncodeP50,
		EncodeP95:       m.EncodeP95,
		EncodeMax:       m.EncodeMax,
		OutputFPS:       m.OutputFPS,
		DropRate:        m.DropRate,
		LiveWriteStable: m.LiveWriteStable,
		SampleWindowMs:  int(m.WindowMs),
		SampleCount:     m.SampleCount,
		AgentVersion:    agentVersion,
		MeasuredAt:      time.Now(),
	}
	if upsertErr := c.store.UpsertEncoderCert(ctx, row); upsertErr != nil {
		return "", nil, fmt.Errorf("upsert cert row failed: %w", upsertErr)
	}

	c.log.Info("SPT-06: cert cell measured",
		"host_id", hostID, "session_id", sessionID,
		"profile_id", target.LaunchProfileID, "stream_profile_id", prof.ID,
		"codec", prof.Codec, "bitrate_kbps", bitrateKbps,
		"encode_ms_p95", m.EncodeP95, "budget_ms", budgetMs,
		"output_fps", m.OutputFPS, "drop_rate", m.DropRate,
		"live_write_stable", m.LiveWriteStable, "sample_count", m.SampleCount,
		"verdict", verdict)

	return verdict, m, nil
}

// teardownCertSession: bounded-timeout stop, best-effort (failure is logged only).
func (c *Coordinator) teardownCertSession(sessionID string) {
	tctx, cancel := context.WithTimeout(context.Background(), stopAckTimeout+5*time.Second)
	defer cancel()
	if _, stopErr := c.Stop(tctx, sessionID, "cert bench complete"); stopErr != nil {
		c.log.Warn("SPT-06: cert cell — stop failed", "session_id", sessionID, "err", stopErr)
	}
}

// waitForRunning polls until `running`, a terminal state, or timeout.
func (c *Coordinator) waitForRunning(ctx context.Context, sessionID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			sess, err := c.store.Get(ctx, sessionID)
			if err != nil {
				return false
			}
			if sess.State == StateRunning {
				return true
			}
			if sess.State.IsTerminal() {
				return false
			}
			if time.Now().After(deadline) {
				return false
			}
		}
	}
}

// lowerProfileRung returns the next-lower launch-profile id (fps-downshift
// within a resolution, then resolution-downshift), or "" if none. The list
// must match `launch_profiles` ids, not stream profiles — encoder
// certification caps a session's launch profile, not the rung it resolves to
// (chosen after placement, migration 0036).
func lowerProfileRung(profileID string) string {
	// Ordered from highest to lowest; each entry maps to the next rung down.
	rungs := []string{
		"1440p120",
		"1440p60",
		"1080p120",
		"1080p60",
		"720p60",
		"720p30",
	}
	for i, id := range rungs {
		if id == profileID && i+1 < len(rungs) {
			return rungs[i+1]
		}
	}
	return ""
}

func toCertRowResp(r EncoderCertRow) certRowResp {
	return certRowResp{
		ID:              r.ID,
		GPUIndex:        r.GPUIndex,
		Encoder:         r.Encoder,
		ProfileID:       r.ProfileID,
		StreamProfileID: r.StreamProfileID,
		Width:           r.Width,
		Height:          r.Height,
		FPS:             r.FPS,
		BitrateKbps:     r.BitrateKbps,
		Verdict:         r.Verdict,
		EncodeP50:       r.EncodeP50,
		EncodeP95:       r.EncodeP95,
		EncodeMax:       r.EncodeMax,
		OutputFPS:       r.OutputFPS,
		DropRate:        r.DropRate,
		LiveWriteStable: r.LiveWriteStable,
		SampleWindowMs:  r.SampleWindowMs,
		SampleCount:     r.SampleCount,
		AgentVersion:    r.AgentVersion,
		MeasuredAt:      r.MeasuredAt,
	}
}
