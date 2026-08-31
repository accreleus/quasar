package library

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/storage"
)

// AgentAuthenticator resolves the calling node-agent's host_id from its per-node credentials.
// Interface at the consumer (internal/storage.Manager satisfies it, app.go wires the same
// homeProvider) so the timing-safe node_secret comparison has exactly one implementation.
type AgentAuthenticator interface {
	AuthAgentHost(ctx context.Context, nodeName, nodeSecret string) (string, error)
}

// DriverResolver resolves the storage driver a host's home resolves to right now (#472):
// storage_provider alone can't answer this once it's "auto". internal/storage.Manager
// implements it and satisfies AgentAuthenticator too, so NewHandler needs no new parameter.
type DriverResolver interface {
	ResolvedDriverName(ctx context.Context, hostID string) (string, error)
}

// SettingsReader: read per call, never cached at construction (§11.1 step 1).
type SettingsReader interface {
	LibraryDiscoveryEnabled(ctx context.Context) (bool, error)
	StorageProvider(ctx context.Context) (string, error)
}

// Handler serves the Phase 4 surfaces: the agent pull channel (§7.2/§7.3) and
// the admin denylist/status surfaces (§8.2).
type Handler struct {
	store    *Store
	agents   AgentAuthenticator
	settings SettingsReader
	details  *AppDetails
	log      *slog.Logger
	auditor  interface {
		Record(context.Context, string, string, string, string, map[string]any) error
	}
	// resolver is the shared env-override-else-database resolution (resolve.go) for scan
	// interval and appdetails, shared with the janitor (app.go wires one instance into
	// both) so the status read and scheduler can't disagree about which value won.
	resolver *Resolver
	// drivers resolves #472's per-host driver when agents also implements DriverResolver
	// (prod and every test fixture pass the same *storage.Manager). nil is handled
	// explicitly in inertReason, not assumed impossible.
	drivers DriverResolver
}

// NewHandler builds the library HTTP handler.
func NewHandler(store *Store, agents AgentAuthenticator, settings SettingsReader,
	details *AppDetails, resolver *Resolver, log *slog.Logger,
	auditors ...interface {
		Record(context.Context, string, string, string, string, map[string]any) error
	}) *Handler {
	h := &Handler{store: store, agents: agents, settings: settings,
		details: details, resolver: resolver, log: log}
	if dr, ok := agents.(DriverResolver); ok {
		h.drivers = dr
	}
	if len(auditors) > 0 {
		h.auditor = auditors[0]
	}
	return h
}

func (h *Handler) recordActivity(r *http.Request, action, targetType, targetID string, details map[string]any) {
	if h.auditor == nil {
		return
	}
	actor := ""
	if u, ok := auth.UserFromContext(r.Context()); ok {
		actor = u.ID
	}
	if err := h.auditor.Record(r.Context(), actor, action, targetType, targetID, details); err != nil {
		h.log.Warn("record admin activity failed", "action", action, "err", err)
	}
}

// Register wires the library routes. Two chains: /v1/agent/library/* authenticate the
// node-agent by node_secret (authAgent), no user/admin middleware involved; every
// /v1/admin/* route goes through the shared RequireAuth -> RequireAdmin chain, wired here.
// No inline `role` check in this file, and there must not be one (CLAUDE.md invariant #6).
func (h *Handler) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/agent/library/scan-pending", http.HandlerFunc(h.handleScanPending))
	mux.Handle("POST /v1/agent/library/scan-report", http.HandlerFunc(h.handleScanReport))

	mux.Handle("GET /v1/admin/library/status", admin(http.HandlerFunc(h.handleStatus)))
	mux.Handle("POST /v1/admin/library/scan", admin(http.HandlerFunc(h.handleForceScan)))
	mux.Handle("GET /v1/admin/apps/{id}/library/unpublished", admin(http.HandlerFunc(h.handleUnpublished)))
	mux.Handle("GET /v1/admin/apps/{id}/library/rules", admin(http.HandlerFunc(h.handleListRules)))
	mux.Handle("PUT /v1/admin/apps/{id}/library/rules/{external_id}", admin(http.HandlerFunc(h.handleSetRule)))
	mux.Handle("DELETE /v1/admin/apps/{id}/library/rules/{external_id}", admin(http.HandlerFunc(h.handleDeleteRule)))
}

// authAgent verifies the agent bearer (node_secret) + X-Quasar-Node header and resolves the
// calling host_id. Same scheme as storage.Handler.authAgent (see AgentAuthenticator).
func (h *Handler) authAgent(w http.ResponseWriter, r *http.Request) (hostID string, ok bool) {
	nodeName := r.Header.Get("X-Quasar-Node")
	authz := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(authz, prefix) {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "agent authentication required")
		return "", false
	}
	secret := strings.TrimSpace(strings.TrimPrefix(authz, prefix))
	id, err := h.agents.AuthAgentHost(r.Context(), nodeName, secret)
	if err != nil {
		if errors.Is(err, storage.ErrAgentAuth) {
			httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "agent authentication failed")
			return "", false
		}
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "agent auth failed")
		return "", false
	}
	return id, true
}

// discoveryLive fails closed: any error, or "off", yields false.
func (h *Handler) discoveryLive(ctx context.Context) bool {
	on, err := h.settings.LibraryDiscoveryEnabled(ctx)
	return err == nil && on
}

// GET /v1/agent/library/scan-pending — claim scan jobs for the caller's host.
//
// Nothing in the response identifies a user (§7.3): scan_id, an opaque home path, two
// relative roots, two bounds.
func (h *Handler) handleScanPending(w http.ResponseWriter, r *http.Request) {
	hostID, ok := h.authAgent(w, r)
	if !ok {
		return
	}
	// Checked here too, not just in the janitor: this stops an already-queued row being
	// handed out after discovery was turned off ("no new work" vs "no work").
	if !h.discoveryLive(r.Context()) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"scans": []PendingScan{}})
		return
	}
	scans, err := h.store.ClaimPending(r.Context(), hostID)
	if err != nil {
		h.log.Warn("library: claim pending scans failed", "host_id", hostID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list pending scans")
		return
	}
	if scans == nil {
		scans = []PendingScan{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"scans": scans})
}

// POST /v1/agent/library/scan-report — accept a report and reconcile it.
func (h *Handler) handleScanReport(w http.ResponseWriter, r *http.Request) {
	hostID, ok := h.authAgent(w, r)
	if !ok {
		return
	}
	var req ScanReport
	// 4 MiB is ~10x the 512-manifest cap: a large legitimate library fits, a runaway agent does not.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid request body")
		return
	}
	if req.ScanID == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "scan_id is required")
		return
	}

	if !req.OK {
		// A failed scan changes nothing but its own row (§7.7 step 1): reconciling whatever
		// entries did arrive would make a partial walk indistinguishable from an uninstall.
		if err := h.store.MarkFailed(r.Context(), req.ScanID, hostID, req.Error); err != nil {
			h.writeScanErr(w, err, "could not record scan failure")
			return
		}
		h.log.Info("library: scan reported failure", "scan_id", req.ScanID, "host_id", hostID)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"accepted": true})
		return
	}

	// Opt-in appdetails rung, resolved before the transaction opens (§8.3). Skipped entirely
	// when disabled (no lookup, no third-party disclosure). Resolved per report via the shared
	// resolver, never cached, so a database-switch flip takes effect on the next report.
	appDetailsEnabled, _, err := h.resolver.AppDetailsEnabled(r.Context())
	if err != nil {
		h.log.Warn("library: could not resolve appdetails setting", "err", err)
		appDetailsEnabled = false
	}
	var details map[string]AppDetail
	if appDetailsEnabled {
		parent, err := h.store.ScanParent(r.Context(), req.ScanID, hostID)
		if err != nil {
			h.writeScanErr(w, err, "could not resolve scan")
			return
		}
		publishIDs, err := h.store.PublishableAppIDs(r.Context(), parent, req.Entries)
		if err != nil {
			h.log.Warn("library: could not compute publishable set", "scan_id", req.ScanID, "err", err)
			publishIDs = nil
		}
		// Backfill candidates (existing tiles with a blank description) fold into the same
		// appdetails pass as the suppression rung, one bounded Fetch instead of two, so a
		// library over the per-scan cap doesn't double the traffic to Valve.
		backfillIDs, err := h.store.BackfillCandidates(r.Context(), parent, req.Entries)
		if err != nil {
			h.log.Warn("library: could not compute backfill candidates", "scan_id", req.ScanID, "err", err)
			backfillIDs = nil
		}
		if merged := mergeIDs(publishIDs, backfillIDs); len(merged) > 0 {
			details = h.details.Fetch(r.Context(), merged)
		}
	}

	res, err := h.store.Reconcile(r.Context(), req.ScanID, hostID, req.Entries, details)
	if err != nil {
		h.writeScanErr(w, err, "could not reconcile scan")
		return
	}
	h.log.Info("library: scan reconciled",
		"scan_id", req.ScanID, "host_id", hostID,
		"observed", res.Observed, "suppressed", res.Suppressed,
		"created", res.Created, "disabled", res.Disabled,
		"granted", res.Granted, "revoked", res.Revoked, "rejected", res.Rejected,
		"backfilled", res.Backfilled)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"accepted": true})
}

// mergeIDs unions a and b, deduplicated, a's order then any of b's not already in a.
func mergeIDs(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, id := range a {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, id := range b {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func (h *Handler) writeScanErr(w http.ResponseWriter, err error, msg string) {
	switch {
	case errors.Is(err, ErrNotFound):
		// A scan the caller's host doesn't own reads as nonexistent: a 403 would confirm
		// another host's scan id to anyone holding one valid node secret.
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "scan not found")
	case errors.Is(err, ErrScanNotOpen):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "scan is not claimed")
	default:
		h.log.Warn("library: "+msg, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, msg)
	}
}

// --- admin surfaces ----------------------------------------------------------

// GET /v1/admin/library/status — is discovery actually doing anything, and if not, why (§7.5).
// Under auto-publish the only visible signal of a working scan is tiles appearing, so
// "nothing appeared" and "nothing ran" look identical without `inert_reason`.
func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	enabled, err := h.settings.LibraryDiscoveryEnabled(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read settings")
		return
	}
	provider, err := h.settings.StorageProvider(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read settings")
		return
	}
	counts, err := h.store.Counts(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read scan counts")
		return
	}

	// Resolved via the same shared resolver the janitor reads, so this panel can't disagree
	// with what actually ran.
	interval, intervalOverridden, err := h.resolver.ScanInterval(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read settings")
		return
	}
	appDetailsEnabled, appDetailsOverridden, err := h.resolver.AppDetailsEnabled(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read settings")
		return
	}
	lastScanCompletedAt, err := h.store.LastScanCompletedAt(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read scan history")
		return
	}
	// Last 20 terminal scans, newest first (see RecentScans for the completed_at/reported_at
	// choice).
	recentScans, err := h.store.RecentScans(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read scan history")
		return
	}

	reason, err := h.inertReason(r.Context(), enabled, provider, interval)
	if err != nil {
		h.log.Warn("library: could not compute the inert reason", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read the app catalog")
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"enabled":                      enabled,
		"storage_provider":             provider,
		"scan_interval_secs":           interval.Seconds(),
		"appdetails_lookup":            appDetailsEnabled,
		"interval_overridden_by_env":   intervalOverridden,
		"appdetails_overridden_by_env": appDetailsOverridden,
		"last_scan_completed_at":       lastScanCompletedAt,
		"recent_scans":                 recentScans,
		"inert_reason":                 reason,
		"scans":                        counts,
	})
}

// Instance-level inert reasons: named so the janitor's log and both admin surfaces quote one
// string each, never a copy that can drift.
//
// storage_provider can no longer hold "volume" (#473: settings validation rejects it, migration
// 0068 coerced existing rows), so there is no reasonVolumeProvider case any more.
const (
	reasonDiscoveryOff  = "library discovery is switched off"
	reasonIntervalZero  = "QUASAR_LIBRARY_SCAN_INTERVAL is 0, which disables discovery regardless of the instance setting"
	reasonNoProviderApp = "no app is marked as a library provider, so there is nothing to scan — " +
		"set Library provider to Steam on your Steam app (Identity section of the app editor)"
	// A fresh install defaults storage_provider to 'auto', which is not itself an error, but
	// with no host storage root anywhere the instance is just as library-dead as the removed
	// explicit 'volume' setting used to make it (#472). Must not present as "you own no games".
	reasonNoHostStorageRoot = "no registered host has a managed-home storage root, " +
		"so no home can be created and there is no host path for the scanner to walk — " +
		"set a storage root for a host (Admin → Hosts, or the setup wizard's host-check step)"
)

// inertReason names why discovery can do no work, or "" when it can. Shared by the status
// read and force-scan so they can't disagree about what "inert" means.
//
// Order is deliberate: operator-fixable switch, then env kill switch, then the provider-app
// check last (checking it first would send someone with discovery off to configure an app
// that would be ignored anyway). Returns an error because the provider-app reason is a DB
// read; a failed read is never silently treated as "no provider app". interval is
// already-resolved (resolve.go), passed in rather than re-queried.
//
// #472/#473: asks the resolver rather than comparing the raw storage_provider column, over
// every provider (not just 'auto') since the storage root, not the provider string, became
// the control (2026-08-10). Instance-wide by construction though resolution is per-host:
// discovery only needs one host with a walkable local home, so the reason fires only when no
// registered host resolves to 'local' (a per-host breakdown belongs on the host settings
// panel). A host whose resolution errors counts as "not local" and is logged, not aborted;
// storage.ErrNoHomeRoot is the expected case (debug, not warn), not a fault.
//
// provider is unused here (#473 removed the storage_provider=="volume" case) but kept so both
// callers' shapes stay untouched.
func (h *Handler) inertReason(ctx context.Context, enabled bool, provider string, interval time.Duration) (string, error) {
	switch {
	case !enabled:
		return reasonDiscoveryOff, nil
	case interval <= 0:
		return reasonIntervalZero, nil
	case h.drivers != nil:
		blocked, err := h.noHostHasStorageRoot(ctx)
		if err != nil {
			return "", err
		}
		if blocked {
			return reasonNoHostStorageRoot, nil
		}
	}
	hasProvider, err := h.store.HasProviderApp(ctx)
	if err != nil {
		return "", err
	}
	if !hasProvider {
		return reasonNoProviderApp, nil
	}
	return "", nil
}

// noHostHasStorageRoot: does no registered host resolve to the local driver / have an
// effective storage root? See inertReason for why "no host" is the right reduction.
func (h *Handler) noHostHasStorageRoot(ctx context.Context) (bool, error) {
	hostIDs, err := h.store.HostIDs(ctx)
	if err != nil {
		return false, err
	}
	if len(hostIDs) == 0 {
		return true, nil
	}
	for _, id := range hostIDs {
		name, err := h.drivers.ResolvedDriverName(ctx, id)
		if errors.Is(err, storage.ErrNoHomeRoot) {
			// Expected on an unconfigured host, not a fault.
			h.log.Debug("library: host has no storage root while computing inert reason", "host_id", id)
			continue
		}
		if err != nil {
			h.log.Warn("library: could not resolve storage driver for host while computing inert reason",
				"host_id", id, "err", err)
			continue
		}
		if name == "local" {
			return false, nil
		}
	}
	return true, nil
}

// POST /v1/admin/library/scan — the operator's "scan now". Bypasses the janitor's pacing and
// Enqueue's "no successful scan inside the interval" predicate only; the instance switch, the
// QUASAR_LIBRARY_SCAN_INTERVAL=0 kill switch and inertness are still enforced before any row
// is written, and every path that returns zero carries an inert_reason (see handleStatus).
func (h *Handler) handleForceScan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AppID  string `json:"app_id"`
		UserID string `json:"user_id"`
	}
	// An empty body ("scan everything, now") is the common case, so EOF is not an error.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid request body")
		return
	}
	appID := strings.TrimSpace(req.AppID)
	userID := strings.TrimSpace(req.UserID)
	if appID != "" && !isUUID(appID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "app_id must be a uuid")
		return
	}
	if userID != "" && !isUUID(userID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "user_id must be a uuid")
		return
	}

	enabled, err := h.settings.LibraryDiscoveryEnabled(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read settings")
		return
	}
	provider, err := h.settings.StorageProvider(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read settings")
		return
	}
	interval, _, err := h.resolver.ScanInterval(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read settings")
		return
	}
	reason, err := h.inertReason(r.Context(), enabled, provider, interval)
	if err != nil {
		h.log.Warn("library: could not compute the inert reason", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read the app catalog")
		return
	}
	if reason != "" {
		writeForceScan(w, ForceScanResult{}, reason)
		return
	}

	// A non-provider app would match no home and report a bare zero; answer with the same
	// errNotAProviderMsg 400 the other /library/* routes use.
	if appID != "" {
		if err := h.store.requireProviderApp(r.Context(), appID); err != nil {
			if writeAppErr(w, err) {
				return
			}
			h.log.Warn("library: force scan app lookup failed", "app_id", appID, "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not resolve app")
			return
		}
	}

	// Reap before enqueueing: a scan whose report failed stays 'claimed', and
	// library_scans_open_uk (partial on 'pending','claimed') blocks every subsequent enqueue
	// for that (user, app, host) triple until reaped. Without this, an operator hitting a
	// transient failure was stuck at "skipped: 1" for up to six hours (the janitor's cadence) —
	// happened live: a 12:09 reconcile failure held queued=2 skipped=1 across four presses.
	// Safe here for the same reason as in the janitor: only claims older than ClaimTTL return.
	if n, err := h.store.ReapClaimed(r.Context(), ClaimTTL); err != nil {
		h.log.Warn("library: force scan could not reap abandoned claims", "err", err)
	} else if n > 0 {
		h.log.Info("library: force scan returned abandoned scans to pending", "count", n)
	}

	res, err := h.store.ForceEnqueue(r.Context(), optional(appID), optional(userID))
	if err != nil {
		h.log.Warn("library: force scan failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not enqueue scans")
		return
	}

	if res.Eligible == 0 {
		// Distinct from "everything already queued" (Eligible > 0, Queued 0): this scope
		// matched no managed home at all.
		reason = "no eligible managed home matched: discovery enqueues one scan per " +
			"(user, library-provider app, host) triple whose managed home is on the 'local' " +
			"driver and not tombstoned, and none matched this request"
	}
	h.log.Info("library: force scan requested",
		"app_id", appID, "user_id", userID,
		"queued", res.Queued, "skipped", res.Skipped)
	// Audited: makes the fleet walk every user's home directory now. Identifiers and counts
	// only, no free text.
	h.recordActivity(r, "library.scan.force", "library", appID, map[string]any{
		"app_id":   appID,
		"user_id":  userID,
		"queued":   res.Queued,
		"skipped":  res.Skipped,
		"eligible": res.Eligible,
	})
	writeForceScan(w, res, reason)
}

// writeForceScan is the one response shape, so the inert path and the working
// path cannot disagree about field names.
func writeForceScan(w http.ResponseWriter, res ForceScanResult, reason string) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"queued":       res.Queued,
		"skipped":      res.Skipped,
		"eligible":     res.Eligible,
		"inert_reason": reason,
	})
}

// optional turns "" into a NULL parameter, which is how ForceEnqueue's scope
// predicates spell "unscoped".
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// isUUID reports whether s is a canonical 8-4-4-4-12 hex UUID. A cheap format
// guard so a malformed scope is a 400 rather than a Postgres 22P02 surfacing as
// a 500. Mirrors internal/devices/handler.go's isUUID.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// GET /v1/admin/apps/{id}/library/unpublished — the "Seen, not published" read.
func (h *Handler) handleUnpublished(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	items, err := h.store.Unpublished(r.Context(), id)
	if writeAppErr(w, err) {
		return
	}
	if err != nil {
		h.log.Warn("library: unpublished read failed", "app_id", id, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list unpublished appids")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// GET /v1/admin/apps/{id}/library/rules
func (h *Handler) handleListRules(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	items, err := h.store.ListRules(r.Context(), id)
	if writeAppErr(w, err) {
		return
	}
	if err != nil {
		h.log.Warn("library: rule list failed", "app_id", id, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list appid rules")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// PUT /v1/admin/apps/{id}/library/rules/{external_id} — Ignore (`rule: "ignore"`, §8.2's three
// steps, one transaction) and un-ignore (`rule: "allow"`, beats the built-in denylist). The
// primary key is the idempotency key: a repeat replaces, never accumulates.
func (h *Handler) handleSetRule(w http.ResponseWriter, r *http.Request) {
	appID := r.PathValue("id")
	externalID := r.PathValue("external_id")

	// Validated here too, on top of the DB CHECK (§10 point 3): the appid comes straight from
	// an HTTP body and ends up in STEAM_STARTUP_FLAGS via a tile; this turns a bad value into
	// a 400 instead of a 500.
	if !validAppID(externalID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"external_id must be a Steam appid (a bare positive integer, no leading zero)")
		return
	}
	var req struct {
		Rule           string `json:"rule"`
		Note           string `json:"note"`
		ExternalSource string `json:"external_source"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid request body")
		return
	}
	if !ValidRule(req.Rule) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "rule must be ignore or allow")
		return
	}
	source := req.ExternalSource
	if source == "" {
		source = SourceSteam
	}
	if source != SourceSteam {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, `external_source must be "steam"`)
		return
	}

	res, err := h.store.SetRule(r.Context(), appID, source, externalID, req.Rule, req.Note, actorID(r))
	if writeAppErr(w, err) {
		return
	}
	if err != nil {
		h.log.Warn("library: set rule failed", "app_id", appID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not write appid rule")
		return
	}
	// Audited: an Ignore revokes entitlements fleet-wide. Identifiers and counts only, to
	// stay inside admin_activity's 4096-byte CHECK.
	h.recordActivity(r, "app.library.rule.set", "app", appID, map[string]any{
		"external_id": externalID,
		"rule":        req.Rule,
		"disabled":    res.Disabled,
		"revoked":     res.Revoked,
	})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"rule":     res.Rule,
		"disabled": res.Disabled,
		"revoked":  res.Revoked,
	})
}

// DELETE /v1/admin/apps/{id}/library/rules/{external_id}
func (h *Handler) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	appID := r.PathValue("id")
	externalID := r.PathValue("external_id")
	if !validAppID(externalID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"external_id must be a Steam appid (a bare positive integer, no leading zero)")
		return
	}
	source := r.URL.Query().Get("external_source")
	if source == "" {
		source = SourceSteam
	}
	err := h.store.DeleteRule(r.Context(), appID, source, externalID)
	if errors.Is(err, ErrNotAProvider) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, errNotAProviderMsg)
		return
	}
	if errors.Is(err, ErrNotFound) {
		// Covers both "no such app" and "no such rule": distinguishing them would confirm an
		// app id's existence to a request that only named a rule.
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "rule not found")
		return
	}
	if err != nil {
		h.log.Warn("library: delete rule failed", "app_id", appID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not delete appid rule")
		return
	}
	h.recordActivity(r, "app.library.rule.delete", "app", appID, map[string]any{"external_id": externalID})
	w.WriteHeader(http.StatusNoContent)
}

// actorID returns the acting admin's user id, or nil off the HTTP path.
func actorID(r *http.Request) *string {
	user, ok := auth.UserFromContext(r.Context())
	if !ok || user.ID == "" {
		return nil
	}
	id := user.ID
	return &id
}

// errNotAProviderMsg names the remedy rather than saying "bad request": one dropdown in the
// app editor.
const errNotAProviderMsg = "this app is not a library provider, so a library rule on it " +
	"could never take effect — set Library provider to Steam on the app first " +
	"(Identity section of the app editor)"

// writeAppErr maps the shared {id}-resolution errors of the four /library/* admin routes.
// Returns true when it wrote a response.
//
// ErrNotAProvider is a 400, not a 404 (the app is real) and not a 200: a rule written against
// a non-provider app is otherwise stored, acknowledged, and permanently inert, since the
// reconciler only reads rules whose parent is a provider.
func writeAppErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotAProvider):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, errNotAProviderMsg)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "app not found")
	default:
		return false
	}
	return true
}
