package platform

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// `POST /v1/admin/platform/hosts/{id}/remove`: the console's "remove host" for an
// owned GPU host, carried out by that host's recovery actor over agent-api.md
// `host_remove`. Not a platform-release attempt: it writes no attempt row.
// semantics: control-api.md amendment 14 §"Removing an owned GPU host"

// CodeHostNotRemovable (409): not an owned GPU host, or its actor refused.
const CodeHostNotRemovable = "host_not_removable"

// RemoveHostRequest is the optional body.
type RemoveHostRequest struct {
	Force bool `json:"force"`
}

// RemovableHost is what the pre-send checks read about one host.
type RemovableHost struct {
	ID             string
	NodeName       string
	InstallMode    *string
	UpdaterPresent *bool
}

// removeStore is the persistence the checks read; *Store implements it.
type removeStore interface {
	RemovableHost(ctx context.Context, hostID string) (RemovableHost, error)
	ActiveRunExists(ctx context.Context) (bool, error)
	OpenHostAttempt(ctx context.Context, hostID string) (Attempt, string, error)
	NonTerminalSessions(ctx context.Context, hostID string) (int, error)
}

// RemoveDeps are the removal's effects, wired in the composition root so this
// package imports neither internal/session nor internal/agentws.
type RemoveDeps struct {
	Store removeStore
	// Connected is whether the host's agent is on the wire right now.
	Connected func(hostID string) bool
	// OwnNodeName is the node name of the agent that shares this control plane's
	// machine, when it is a combined host; ok false when there is none or it is unknown.
	OwnNodeName func(ctx context.Context) (string, bool)
	// Cordon takes the host out of scheduling and returns what puts its cordon
	// back as it was found.
	Cordon func(ctx context.Context, hostID string) (restore func(context.Context), err error)
	// StopSessions sends session_stop (host_draining) to each non-terminal session.
	StopSessions func(ctx context.Context, hostID string) error
	// Send dispatches host_remove and waits for the ack; ctx carries the ack timeout.
	Send func(ctx context.Context, hostID, requestID string) (Ack, error)
	// Host is the host body (`GET /v1/hosts/{id}`) as it stands, for the 202.
	Host func(ctx context.Context, hostID string) (any, error)
}

// RemoveHandler serves the route.
type RemoveHandler struct {
	deps       RemoveDeps
	auditor    audit.Recorder
	log        logger
	AckTimeout time.Duration
	// NewRequestID mints the request id; a field so a test can pin it.
	NewRequestID func() string
}

func NewRemoveHandler(deps RemoveDeps, auditor audit.Recorder, log logger) *RemoveHandler {
	return &RemoveHandler{deps: deps, auditor: auditor, log: log, AckTimeout: DefaultAckTimeout, NewRequestID: newRequestID}
}

// Register wires the route. admin must compose RequireAuth→RequireAdmin.
func (h *RemoveHandler) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/admin/platform/hosts/{id}/remove", admin(http.HandlerFunc(h.handleRemove)))
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (h *RemoveHandler) handleRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hostID := r.PathValue("id")
	if !looksLikeUUID(hostID) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such host")
		return
	}
	var req RemoveHostRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "the request body is not valid JSON")
		return
	}

	// 1. Validate, changing nothing, in the contract's order.
	host, err := h.deps.Store.RemovableHost(ctx, hostID)
	if errors.Is(err, ErrHostNotFound) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such host")
		return
	}
	if err != nil {
		h.internal(w, "read host", err)
		return
	}
	active, err := h.deps.Store.ActiveRunExists(ctx)
	if err != nil {
		h.internal(w, "read active run", err)
		return
	}
	if active {
		httpx.WriteError(w, http.StatusConflict, CodeRunActive,
			"a fleet update is running; a host may not be removed while it owns the fleet")
		return
	}
	if _, _, err := h.deps.Store.OpenHostAttempt(ctx, hostID); err == nil {
		httpx.WriteError(w, http.StatusConflict, CodeAttemptInFlight,
			"an update is in flight on this host; remove it once the update has finished")
		return
	} else if !errors.Is(err, ErrAttemptNotFound) {
		h.internal(w, "read open attempt", err)
		return
	}
	if h.deps.Connected != nil && !h.deps.Connected(hostID) {
		writeNotEligible(w, ReasonHostOffline)
		return
	}
	if host.UpdaterPresent != nil && !*host.UpdaterPresent {
		writeNotEligible(w, ReasonUpdaterAbsent)
		return
	}
	if host.InstallMode == nil || *host.InstallMode != InstallOwned {
		httpx.WriteError(w, http.StatusConflict, CodeHostNotRemovable,
			"only a host whose recovery actor installed it can be removed from here; stop its agent on the machine, then forget the host once it is offline")
		return
	}
	if h.deps.OwnNodeName != nil {
		if own, ok := h.deps.OwnNodeName(ctx); ok && own == host.NodeName {
			httpx.WriteError(w, http.StatusConflict, CodeHostNotRemovable,
				"this host shares the control plane's machine; take that machine apart with the uninstall command on it")
			return
		}
	}

	// 2. Cordon, then 3. check sessions: none can be placed between the two.
	restore, err := h.deps.Cordon(ctx, hostID)
	if err != nil {
		h.internal(w, "cordon host", err)
		return
	}
	n, err := h.deps.Store.NonTerminalSessions(ctx, hostID)
	if err != nil {
		restore(ctx)
		h.internal(w, "count sessions", err)
		return
	}
	if n > 0 && !req.Force {
		restore(ctx)
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict,
			fmt.Sprintf("%d session(s) are still live on this host; drain it and ask again once it is empty, or remove it with force", n))
		return
	}
	if n > 0 && h.deps.StopSessions != nil {
		if err := h.deps.StopSessions(ctx, hostID); err != nil {
			h.log.Warn("host removal: stopping the host's sessions failed", "host_id", hostID, "err", err)
		}
	}

	// 4. Send, and wait for the ack.
	requestID := h.NewRequestID()
	sctx, cancel := context.WithTimeout(ctx, h.AckTimeout)
	ack, err := h.deps.Send(sctx, hostID, requestID)
	cancel()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		restore(ctx)
		httpx.WriteError(w, http.StatusNotImplemented, CodeApplyUnsupported,
			"this host's agent did not answer the removal, so it predates it and nothing was removed; update it first")
		return
	case err != nil:
		// Undeliverable: the agent's connection is gone (the send found none, or it closed
		// before an ack). The contract's word for "nobody to tell" is host_offline; a
		// removal the actor did accept shows as the host staying offline.
		restore(ctx)
		writeNotEligible(w, ReasonHostOffline)
		return
	case !ack.OK:
		restore(ctx)
		httpx.WriteError(w, http.StatusConflict, CodeHostNotRemovable,
			"the host's recovery actor refused the removal ("+ack.Error+"); nothing was removed")
		return
	}

	// control-api.md amendment 14: the host id, node_name and force, nothing more.
	audit.TryRecord(ctx, h.auditor, actorID(r), "platform.remove.host", "host", hostID, map[string]any{
		"node_name": host.NodeName,
		"force":     req.Force,
	})
	body, err := h.deps.Host(ctx, hostID)
	if err != nil {
		h.log.Warn("host removal accepted; the host could not be read back", "host_id", hostID, "err", err)
		body = map[string]any{"id": hostID, "node_name": host.NodeName}
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"host": body})
}

func (h *RemoveHandler) internal(w http.ResponseWriter, what string, err error) {
	h.log.Error("host removal: "+what+" failed", "err", err)
	httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not remove the host")
}
