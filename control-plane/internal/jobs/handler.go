// handler.go — the admin HTTP surface (design §3.6): the viewer's five routes.
// The only consumer of Registry.Get outside boot wiring (the registry supplies
// identity the Store does not carry); Dispatcher.RunNow gives the admin
// trigger and the schedule one implementation. No inline role check in this
// file — RequireAuth -> RequireAdmin composes at registration (CLAUDE.md
// invariant #6).
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// Recorder is the audit.Store surface this package depends on, taken as an
// interface (as internal/library and internal/session already do) so this
// package never imports internal/audit and a nil auditor in a route-recording
// test (cmd/quasar-control's OpenAPI drift test) is simply "don't audit".
type Recorder interface {
	Record(ctx context.Context, actorUserID, action, targetType, targetID string, details map[string]any) error
}

// Handler serves GET/PATCH /v1/admin/jobs* (design §3.6). It never talks to
// the agent pull channel (WP4) and adopts no job (WP2/WP5/WP6) — it is a pure
// read/PATCH/trigger surface over WP1's Store, Registry and Dispatcher.
type Handler struct {
	store      *Store
	reg        *Registry
	dispatcher *Dispatcher
	log        *slog.Logger
	auditor    Recorder
	// getenv is a test seam for the env-lock check (EffectiveInterval), the
	// same seam the Dispatcher itself uses — production is os.Getenv.
	getenv func(string) string
}

// NewHandler builds the admin jobs handler. auditor may be nil (routes still
// work; nothing is recorded — see Recorder's doc comment).
func NewHandler(store *Store, reg *Registry, dispatcher *Dispatcher, log *slog.Logger, auditor Recorder) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: store, reg: reg, dispatcher: dispatcher, log: log, auditor: auditor, getenv: os.Getenv}
}

// Register wires the five admin routes behind the caller-supplied
// RequireAuth -> RequireAdmin gate (app.go's `admin` closure).
func (h *Handler) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/admin/jobs", admin(http.HandlerFunc(h.handleList)))
	mux.Handle("GET /v1/admin/jobs/{job_id}", admin(http.HandlerFunc(h.handleGet)))
	mux.Handle("PATCH /v1/admin/jobs/{job_id}", admin(http.HandlerFunc(h.handlePatch)))
	mux.Handle("POST /v1/admin/jobs/{job_id}/run", admin(http.HandlerFunc(h.handleRunNow)))
	mux.Handle("GET /v1/admin/jobs/{job_id}/runs", admin(http.HandlerFunc(h.handleListRuns)))
}

func (h *Handler) recordActivity(r *http.Request, action, targetType, targetID string, details map[string]any) {
	if h.auditor == nil {
		return
	}
	if err := h.auditor.Record(r.Context(), actorID(r), action, targetType, targetID, details); err != nil {
		h.log.Warn("job: record admin activity failed", "action", action, "err", err)
	}
}

// actorID returns the acting admin's user id, or "" off the HTTP path (mirrors
// internal/library/handler.go's actorID, string form for audit.Record/RunNow).
func actorID(r *http.Request) string {
	if u, ok := auth.UserFromContext(r.Context()); ok {
		return u.ID
	}
	return ""
}

// --- wire shapes (design §3.6, verbatim) ------------------------------------

type scheduleJSON struct {
	Kind         string  `json:"kind"`
	IntervalSecs *int32  `json:"interval_secs"`
	WindowStart  *string `json:"window_start"`
	WindowEnd    *string `json:"window_end"`
	WindowDays   []int   `json:"window_days"`
	Timezone     string  `json:"timezone"`
	Locked       bool    `json:"locked"`
	LockedBy     *string `json:"locked_by"`
}

type runJSON struct {
	ID         string          `json:"id"`
	HostID     *string         `json:"host_id"`
	State      string          `json:"state"`
	Trigger    string          `json:"trigger"`
	StartedAt  *time.Time      `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at"`
	DurationMS *int64          `json:"duration_ms"`
	Summary    json.RawMessage `json:"summary"`
	Error      *string         `json:"error"`
}

type jobTargetJSON struct {
	HostID    string     `json:"host_id"`
	NodeName  string     `json:"node_name"`
	Running   bool       `json:"running"`
	NextRunAt *time.Time `json:"next_run_at"`
	LastRun   *runJSON   `json:"last_run"`
}

type jobJSON struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Plane       string       `json:"plane"`
	Scope       string       `json:"scope"`
	Managed     bool         `json:"managed"`
	Enabled     bool         `json:"enabled"`
	Schedule    scheduleJSON `json:"schedule"`

	// Top-level only for scope=instance (design §3.6). Present-but-null for an
	// unmanaged job of ANY scope — see toJobJSON.
	Running             *bool      `json:"running,omitempty"`
	NextRunAt           *time.Time `json:"next_run_at,omitempty"`
	LastRun             *runJSON   `json:"last_run,omitempty"`
	ConsecutiveFailures *int       `json:"consecutive_failures,omitempty"`

	// Present only for a MANAGED scope=host job.
	Targets []jobTargetJSON `json:"targets,omitempty"`

	HistoryLimit int `json:"history_limit"`

	// UnmanagedNote names the file that hard-codes an unmanaged job (design
	// §3.7). Derived from the Definition's Description — see toJobJSON.
	UnmanagedNote string `json:"unmanaged_note,omitempty"`
}

func toRunJSON(r Run) runJSON {
	j := runJSON{
		ID:         r.ID,
		State:      string(r.State),
		Trigger:    string(r.Trigger),
		StartedAt:  r.StartedAt,
		FinishedAt: r.FinishedAt,
		DurationMS: r.DurationMS(),
		Summary:    r.Summary,
	}
	if r.HostID != "" {
		j.HostID = &r.HostID
	}
	if r.Error != "" {
		j.Error = &r.Error
	}
	if len(j.Summary) == 0 {
		j.Summary = json.RawMessage(`{}`)
	}
	return j
}

func timeOfDayStr(t *TimeOfDay) *string {
	if t == nil {
		return nil
	}
	s := t.String()
	return &s
}

// toScheduleJSON resolves the effective schedule, incl. the env-lock verdict
// (design §3.3), exactly as the dispatcher's own materializeDue resolves it —
// one implementation of "which source won" so the viewer and the scheduler
// cannot disagree.
func (h *Handler) toScheduleJSON(j Job, envOverride string) scheduleJSON {
	s := scheduleJSON{
		Kind:        string(j.Schedule.Kind),
		WindowStart: timeOfDayStr(j.Schedule.WindowStart),
		WindowEnd:   timeOfDayStr(j.Schedule.WindowEnd),
		WindowDays:  j.Schedule.WindowDays,
		Timezone:    j.Schedule.Timezone,
	}
	if s.WindowDays == nil {
		s.WindowDays = []int{}
	}
	if j.Schedule.Kind == KindInterval {
		interval, locked, lockedBy := EffectiveInterval(j, envOverride, h.getenv)
		secs := int32(interval.Seconds())
		s.IntervalSecs = &secs
		s.Locked = locked
		if lockedBy != "" {
			s.LockedBy = &lockedBy
		}
	}
	return s
}

// toJobJSON assembles one list/get item. hostNames is the fleet's id->node_name
// map (nil is fine — an unresolved name renders as "", never an error: a stale
// host row must not break the whole list).
func (h *Handler) toJobJSON(ctx context.Context, j Job, hostNames map[string]string) jobJSON {
	def, _ := h.reg.Get(j.ID)
	out := jobJSON{
		ID:           j.ID,
		Name:         j.Name,
		Description:  j.Description,
		Plane:        string(j.Plane),
		Scope:        string(j.Scope),
		Managed:      j.Managed,
		Enabled:      j.Enabled,
		Schedule:     h.toScheduleJSON(j, def.EnvOverride),
		HistoryLimit: j.HistoryLimit,
	}

	if !j.Managed {
		// design §3.7: null run-derived fields plus the note naming where the
		// unadopted goroutine lives; the Description is that note's one author.
		out.UnmanagedNote = j.Description
		return out
	}

	if j.Scope == ScopeHost {
		hostIDs, err := h.store.HostIDs(ctx)
		if err != nil {
			h.log.Warn("job: could not list hosts for target rows", "job_id", j.ID, "err", err)
			hostIDs = nil
		}
		targets := make([]jobTargetJSON, 0, len(hostIDs))
		for _, hostID := range hostIDs {
			targets = append(targets, h.toTargetJSON(ctx, j.ID, hostID, hostNames[hostID]))
		}
		out.Targets = targets
		return out
	}

	running, nextRunAt, lastRun := h.runStateFor(ctx, j.ID, "")
	out.Running = &running
	out.NextRunAt = nextRunAt
	out.LastRun = lastRun
	cf := h.consecutiveFailures(ctx, j.ID, "")
	out.ConsecutiveFailures = &cf
	return out
}

func (h *Handler) toTargetJSON(ctx context.Context, jobID, hostID, nodeName string) jobTargetJSON {
	running, nextRunAt, lastRun := h.runStateFor(ctx, jobID, hostID)
	return jobTargetJSON{
		HostID:    hostID,
		NodeName:  nodeName,
		Running:   running,
		NextRunAt: nextRunAt,
		LastRun:   lastRun,
	}
}

// runStateFor is the one place "running / next_run_at / last_run" is computed,
// shared by the instance-scoped top-level fields and each host-scoped target
// row, so the two shapes cannot drift apart.
func (h *Handler) runStateFor(ctx context.Context, jobID, hostID string) (running bool, nextRunAt *time.Time, lastRun *runJSON) {
	open, found, err := h.store.OpenRun(ctx, jobID, hostID)
	if err != nil {
		h.log.Warn("job: could not read open run", "job_id", jobID, "host_id", hostField(hostID), "err", err)
	} else if found {
		if open.State == StateRunning {
			running = true
		} else if open.State == StatePending {
			t := open.ScheduledFor
			nextRunAt = &t
		}
	}
	last, hadLast, err := h.store.LastTerminalRun(ctx, jobID, hostID)
	if err != nil {
		h.log.Warn("job: could not read last run", "job_id", jobID, "host_id", hostField(hostID), "err", err)
	} else if hadLast {
		rj := toRunJSON(last)
		lastRun = &rj
	}
	return running, nextRunAt, lastRun
}

// consecutiveFailures counts terminal `failed` runs since the last non-failed
// terminal outcome (design §7.3) — derived from history rather than stored, so
// it cannot drift from the rows it describes.
func (h *Handler) consecutiveFailures(ctx context.Context, jobID, hostID string) int {
	runs, err := h.store.ListRuns(ctx, jobID, hostID, 500)
	if err != nil {
		h.log.Warn("job: could not read run history for failure count", "job_id", jobID, "err", err)
		return 0
	}
	n := 0
	for _, r := range runs {
		if !r.State.Terminal() {
			continue // an open run in the same page: skip, it is not a terminal outcome
		}
		if r.State != StateFailed {
			break
		}
		n++
	}
	return n
}

// --- GET /v1/admin/jobs -------------------------------------------------------

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	all, err := h.store.List(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list jobs")
		return
	}
	limit := parseLimit(r, 50)
	cursor := r.URL.Query().Get("cursor")
	start := 0
	if cursor != "" {
		start = len(all)
		for i, j := range all {
			if j.ID > cursor {
				start = i
				break
			}
		}
	}
	page := all[start:]
	var next string
	if len(page) > limit {
		next = page[limit-1].ID
		page = page[:limit]
	}

	hostNames, err := h.store.HostNames(r.Context())
	if err != nil {
		h.log.Warn("job: could not list host names", "err", err)
		hostNames = nil
	}
	items := make([]jobJSON, 0, len(page))
	for _, j := range page {
		items = append(items, h.toJobJSON(r.Context(), j, hostNames))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nullableStr(next)})
}

// --- GET /v1/admin/jobs/{job_id} ----------------------------------------------

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("job_id")
	job, err := h.store.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "job not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read job")
		return
	}
	hostNames, err := h.store.HostNames(r.Context())
	if err != nil {
		h.log.Warn("job: could not list host names", "job_id", id, "err", err)
		hostNames = nil
	}
	httpx.WriteJSON(w, http.StatusOK, h.toJobJSON(r.Context(), job, hostNames))
}

// --- PATCH /v1/admin/jobs/{job_id} --------------------------------------------

// knownPatchFields is the PATCH body's whole vocabulary (design §3.6). A key
// outside this set is rejected rather than silently ignored — a typo'd field
// name in a request that is accepted with a 200 is the exact "did my edit take
// effect" confusion this framework exists to end.
var knownPatchFields = map[string]bool{
	"enabled": true, "interval_secs": true, "window_start": true,
	"window_end": true, "window_days": true, "timezone": true, "history_limit": true,
}

func (h *Handler) handlePatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("job_id")
	job, err := h.store.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "job not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read job")
		return
	}
	if !job.Managed {
		httpx.WriteError(w, http.StatusConflict, httpx.CodeJobUnmanaged,
			"this job is not adopted by the job framework and has no schedule to edit")
		return
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&raw); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "malformed JSON body")
		return
	}
	for k := range raw {
		if !knownPatchFields[k] {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "unknown field: "+k)
			return
		}
	}

	upd := ScheduleUpdate{}

	if v, ok := raw["enabled"]; ok {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "enabled must be a boolean")
			return
		}
		upd.Enabled = &b
	}

	if v, ok := raw["interval_secs"]; ok {
		if job.Schedule.Kind != KindInterval {
			httpx.WriteError(w, http.StatusUnprocessableEntity, httpx.CodeValidationFailed,
				"this job's schedule is "+string(job.Schedule.Kind)+", not interval — interval_secs does not apply")
			return
		}
		var n int32
		if err := json.Unmarshal(v, &n); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "interval_secs must be an integer")
			return
		}
		if n < MinIntervalSecs {
			httpx.WriteError(w, http.StatusUnprocessableEntity, httpx.CodeValidationFailed,
				fmt.Sprintf("interval_secs must be at least %d", MinIntervalSecs))
			return
		}
		def, _ := h.reg.Get(id)
		if def.EnvOverride != "" {
			if _, locked, lockedBy := EffectiveInterval(job, def.EnvOverride, h.getenv); locked {
				httpx.WriteError(w, http.StatusConflict, httpx.CodeScheduleLocked,
					lockedBy+" is set in the environment and is authoritative over this job's interval; "+
						"unset it to make the admin-owned interval take effect")
				return
			}
		}
		upd.IntervalSecs = &n
	}

	_, hasStart := raw["window_start"]
	_, hasEnd := raw["window_end"]
	if hasStart != hasEnd {
		httpx.WriteError(w, http.StatusUnprocessableEntity, httpx.CodeValidationFailed,
			"window_start and window_end must both be set or both cleared together")
		return
	}
	if hasStart {
		upd.SetWindow = true
		var startStr, endStr *string
		if err := json.Unmarshal(raw["window_start"], &startStr); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "window_start must be a string or null")
			return
		}
		if err := json.Unmarshal(raw["window_end"], &endStr); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "window_end must be a string or null")
			return
		}
		switch {
		case startStr == nil && endStr == nil:
			// clear the window — upd.WindowStart/WindowEnd stay nil.
		case startStr != nil && endStr != nil:
			ws, err := ParseTimeOfDay(*startStr)
			if err != nil {
				httpx.WriteError(w, http.StatusUnprocessableEntity, httpx.CodeValidationFailed, err.Error())
				return
			}
			we, err := ParseTimeOfDay(*endStr)
			if err != nil {
				httpx.WriteError(w, http.StatusUnprocessableEntity, httpx.CodeValidationFailed, err.Error())
				return
			}
			if ws == we {
				httpx.WriteError(w, http.StatusUnprocessableEntity, httpx.CodeValidationFailed,
					"window_start and window_end must not be equal (an empty window would never open)")
				return
			}
			upd.WindowStart, upd.WindowEnd = &ws, &we
		default:
			httpx.WriteError(w, http.StatusUnprocessableEntity, httpx.CodeValidationFailed,
				"window_start and window_end must both be null (clear) or both set")
			return
		}
	}

	if v, ok := raw["window_days"]; ok {
		var days []int
		if err := json.Unmarshal(v, &days); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "window_days must be an array of integers")
			return
		}
		for _, d := range days {
			if d < 0 || d > 6 {
				httpx.WriteError(w, http.StatusUnprocessableEntity, httpx.CodeValidationFailed,
					"window_days values must be 0 (Sunday) through 6 (Saturday)")
				return
			}
		}
		upd.WindowDays = &days
	}

	if v, ok := raw["timezone"]; ok {
		var tz string
		if err := json.Unmarshal(v, &tz); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "timezone must be a string")
			return
		}
		if _, err := LoadLocation(tz); err != nil {
			httpx.WriteError(w, http.StatusUnprocessableEntity, httpx.CodeValidationFailed, err.Error())
			return
		}
		upd.Timezone = &tz
	}

	if v, ok := raw["history_limit"]; ok {
		var n int32
		if err := json.Unmarshal(v, &n); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "history_limit must be an integer")
			return
		}
		if n < 1 || n > 500 {
			httpx.WriteError(w, http.StatusUnprocessableEntity, httpx.CodeValidationFailed, "history_limit must be between 1 and 500")
			return
		}
		upd.HistoryLimit = &n
	}

	updated, err := h.store.UpdateSchedule(r.Context(), id, upd)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "job not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not update job schedule")
		return
	}

	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h.recordActivity(r, "job.update", "job", id, map[string]any{"keys": keys})

	hostNames, _ := h.store.HostNames(r.Context())
	httpx.WriteJSON(w, http.StatusOK, h.toJobJSON(r.Context(), updated, hostNames))
}

// --- POST /v1/admin/jobs/{job_id}/run -----------------------------------------

func (h *Handler) handleRunNow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("job_id")

	var req struct {
		HostID *string `json:"host_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "malformed JSON body")
		return
	}
	hostID := ""
	if req.HostID != nil {
		hostID = strings.TrimSpace(*req.HostID)
	}

	run, err := h.dispatcher.RunNow(r.Context(), id, hostID, actorID(r))
	if err != nil {
		writeRunNowErr(w, err)
		return
	}

	h.recordActivity(r, "job.run", "job", id, map[string]any{"run_id": run.ID, "host_id": hostID})

	def, known := h.reg.Get(id)
	etaNote := "queued; the control plane's dispatcher checks for due runs about every 10 s"
	if known && def.Plane == PlaneAgent {
		etaNote = "queued; the host claims due jobs about every 60 s"
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{
		"run_id":        run.ID,
		"state":         string(run.State),
		"scheduled_for": run.ScheduledFor,
		"eta_note":      etaNote,
	})
}

// writeRunNowErr maps internal/jobs' sentinel errors to the WP3 wire codes
// (design §3.6). One switch, so the run-now path and any future caller of
// Dispatcher.RunNow cannot map the same error to two different responses.
func writeRunNowErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "job not found")
	case errors.Is(err, ErrUnmanaged):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeJobUnmanaged,
			"this job is not adopted by the job framework and cannot be triggered")
	case errors.Is(err, ErrDisabled):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeJobDisabled,
			"this job is disabled; a disabled job never runs, not even manually")
	case errors.Is(err, ErrAlreadyRunning):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeJobAlreadyRunning, "a run for this job is already in progress")
	case errors.Is(err, ErrHostRequired):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "host_id is required for a host-scoped job")
	case errors.Is(err, ErrParamsUnavailable):
		// The wrapped cause is the operator-readable half ("host … has no adopted
		// image …"); rendering it is the whole point of refusing here rather than
		// letting the run fail on the host with `params incomplete`.
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
	case errors.Is(err, ErrHostNotAllowed):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "host_id is not allowed for an instance-scoped job")
	default:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not trigger job")
	}
}

// --- GET /v1/admin/jobs/{job_id}/runs -----------------------------------------

func (h *Handler) handleListRuns(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("job_id")
	if _, err := h.store.Get(r.Context(), id); errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "job not found")
		return
	} else if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read job")
		return
	}

	hostID := r.URL.Query().Get("host_id")
	limit := parseLimit(r, 50)
	cursor := r.URL.Query().Get("cursor")

	runs, next, err := h.store.ListRunsPage(r.Context(), id, hostID, cursor, limit)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list job runs")
		return
	}
	items := make([]runJSON, 0, len(runs))
	for _, run := range runs {
		items = append(items, toRunJSON(run))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nullableStr(next)})
}

// --- helpers -------------------------------------------------------------------

func parseLimit(r *http.Request, def int) int {
	l := r.URL.Query().Get("limit")
	if l == "" {
		return def
	}
	n, err := strconv.Atoi(l)
	if err != nil || n < 1 || n > 500 {
		return def
	}
	return n
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
