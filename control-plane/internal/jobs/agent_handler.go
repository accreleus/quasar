package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// The agent pull channel (design §3.6, §4.2): two node-secret HTTP routes that
// carry the schedule downstream and the outcome upstream for PlaneAgent jobs.
//
// HTTP, not a new AgentMsg — the repo's third pull channel (#175 storage GC,
// library discovery): a claim is a database row, so an agent reconnect has
// nothing to correlate and protocol/agent-api.md stays byte-identical. Needing
// a new AgentMsg variant here is an escalation, not a patch. The agent asks;
// it is never told — a runner's gate refusing is an ordinary `deferred`
// report, not an error.

// AgentAuthenticator resolves the calling node-agent's host_id from its
// per-node credentials. An interface at the consumer (as internal/library
// declares it; internal/storage.Manager satisfies both) so the
// constant-time node_secret check has one implementation — and not typed
// against internal/storage's error value, which would be an import cycle once
// the janitors this package will host are adopted.
type AgentAuthenticator interface {
	AuthAgentHost(ctx context.Context, nodeName, nodeSecret string) (string, error)
}

// AgentHandler serves GET /v1/agent/jobs/pending and POST /v1/agent/jobs/report.
type AgentHandler struct {
	store  *Store
	disp   *Dispatcher
	agents AgentAuthenticator
	log    *slog.Logger
}

// NewAgentHandler builds the pull-channel handler. disp is the SAME dispatcher
// the control plane ticks: a report from a host and a report from an in-process
// RunFunc must take one path, or the deferral ladder and the log vocabulary
// would exist twice and drift.
func NewAgentHandler(store *Store, disp *Dispatcher, agents AgentAuthenticator, log *slog.Logger) *AgentHandler {
	return &AgentHandler{store: store, disp: disp, agents: agents, log: log}
}

// Register wires the two agent routes. No admin middleware by design: these
// authenticate the node-agent by node_secret (authAgent), as the other
// /v1/agent/* routes do — no user is involved. /v1/agent/ is already exempt
// from the HTTPS redirect (httpx/redirect.go).
func (h *AgentHandler) Register(mux httpx.Router) {
	mux.Handle("GET /v1/agent/jobs/pending", http.HandlerFunc(h.handlePending))
	mux.Handle("POST /v1/agent/jobs/report", http.HandlerFunc(h.handleReport))
}

// maxReportBytes bounds a report body. A summary is CHECK-constrained to 4096
// bytes at the storage layer, so 64 KiB is roughly sixteen times the largest
// legal payload — generous for a malformed-but-honest agent, tight enough that a
// runaway one cannot stream into this handler.
const maxReportBytes = 64 << 10

// agentClaimLimit caps one poll's claim (design §3.6). A host coming back after
// an outage with a dozen jobs due must not start all of them at once; the next
// poll is 60 s away and the rest are still pending, which is the whole point of
// the work being a durable row.
const agentClaimLimit = 5

// authAgent verifies the agent bearer (node_secret) + X-Quasar-Node header and
// resolves the calling host_id — the storage/library authAgent scheme. Every
// failure is a flat 401 with the same message: these routes must not become an
// oracle for which node names exist. The real reason is logged, never returned.
func (h *AgentHandler) authAgent(w http.ResponseWriter, r *http.Request) (hostID string, ok bool) {
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
		h.log.Debug("job: agent authentication failed", "node", nodeName, "err", err)
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "agent authentication failed")
		return "", false
	}
	return id, true
}

// pendingRun is one claimed run as the agent sees it (design §3.6, verbatim).
type pendingRun struct {
	RunID string `json:"run_id"`
	JobID string `json:"job_id"`
	// Params is the opaque blob the control plane stored when it materialized the
	// run. The framework never interprets it; the agent hands it to the runner.
	Params json.RawMessage `json:"params"`
	// DeadlineSecs is QUASAR_JOBS_CLAIM_TIMEOUT_SECS: after this long with no
	// report the dispatcher aborts the run and re-materializes it. Sent so the
	// agent can bound its own execution rather than discover the abort by racing.
	DeadlineSecs int `json:"deadline_secs"`
}

type pendingResponse struct {
	Runs []pendingRun `json:"runs"`
}

// GET /v1/agent/jobs/pending — claim this host's due agent-plane runs. The
// claim is the response: returning a run and marking it `running` is one
// statement (Store.ClaimDue), so concurrent polls take disjoint sets. An
// unreported claim is not lost — the reaper aborts it after deadline_secs and
// materializes a fresh pending row.
func (h *AgentHandler) handlePending(w http.ResponseWriter, r *http.Request) {
	hostID, ok := h.authAgent(w, r)
	if !ok {
		return
	}
	// The master switch is honoured here as well as in the dispatcher: this is
	// what stops an already-materialized row being handed out after
	// QUASAR_JOBS=0 — the difference between "no new work" and "no work".
	if !h.disp.Config().Enabled {
		httpx.WriteJSON(w, http.StatusOK, pendingResponse{Runs: []pendingRun{}})
		return
	}

	runs, err := h.store.ClaimDue(r.Context(), ClaimOptions{
		Plane:  PlaneAgent,
		HostID: hostID,
		Limit:  agentClaimLimit,
	})
	if err != nil {
		h.log.Warn("job: could not claim agent runs", "host_id", hostID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not claim pending jobs")
		return
	}

	deadline := int(h.disp.Config().ClaimTimeout.Seconds())
	out := make([]pendingRun, 0, len(runs))
	for _, run := range runs {
		params := run.Params
		if len(params) == 0 {
			params = json.RawMessage(`{}`)
		}
		out = append(out, pendingRun{
			RunID:        run.ID,
			JobID:        run.JobID,
			Params:       params,
			DeadlineSecs: deadline,
		})
		// The same "run started" line the in-process executor logs, with the same
		// fields, so one `grep 'job: '` across both containers reconstructs a run
		// end to end (design §3.8).
		h.log.Info("job: run started", "job_id", run.JobID, "run_id", run.ID,
			"host_id", hostField(run.HostID), "trigger", string(run.Trigger),
			"attempt", run.Attempt, "plane", "agent")
	}
	httpx.WriteJSON(w, http.StatusOK, pendingResponse{Runs: out})
}

// reportRequest is the POST /v1/agent/jobs/report body (design §3.6, verbatim).
type reportRequest struct {
	RunID   string         `json:"run_id"`
	State   string         `json:"state"`
	Summary map[string]any `json:"summary"`
	Error   *string        `json:"error"`
}

// agentReportable is the closed set of states a HOST may report. `aborted` is
// absent on purpose: it is the reaper's verdict on a host that said nothing, and
// a host claiming it would be describing a decision it does not get to make.
func agentReportable(s State) bool {
	switch s {
	case StateSucceeded, StateFailed, StateDeferred, StateSkipped:
		return true
	}
	return false
}

// POST /v1/agent/jobs/report — close a claimed run. Idempotent by contract: a
// report for an already-terminal run is a 200 no-op, so a retry after a
// network blip never turns a successful run into a permanent error.
func (h *AgentHandler) handleReport(w http.ResponseWriter, r *http.Request) {
	hostID, ok := h.authAgent(w, r)
	if !ok {
		return
	}
	var req reportRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReportBytes)).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid request body")
		return
	}
	if strings.TrimSpace(req.RunID) == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "run_id is required")
		return
	}
	state := State(req.State)
	if !agentReportable(state) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"state must be one of succeeded, failed, deferred, skipped")
		return
	}

	// Ownership is checked before anything is written; a failure is a 401, not a
	// 404 — "no such run" and "not your run" must be the same answer.
	run, err := h.store.GetRun(r.Context(), req.RunID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "agent authentication failed")
			return
		}
		h.log.Warn("job: could not read reported run", "run_id", req.RunID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not record job report")
		return
	}
	if run.HostID == "" || run.HostID != hostID {
		h.log.Warn("job: report for a run this host does not own",
			"run_id", req.RunID, "host_id", hostID, "run_host_id", hostField(run.HostID))
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "agent authentication failed")
		return
	}
	if run.State.Terminal() {
		// The retry-after-a-blip case. Nothing to do and nothing to say.
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	errText := ""
	if req.Error != nil {
		errText = *req.Error
	}
	summary := req.Summary
	if summary == nil {
		summary = map[string]any{}
	}
	// Dispatcher.Report, NOT Store.Report: the deferral ladder, the run-finished /
	// run-deferred log lines and the follow-up pending row all live there, and an
	// agent-plane run must get exactly what a control-plane run gets.
	if _, err := h.disp.Report(r.Context(), req.RunID, state, summary, errText); err != nil {
		// A run that moved under us (reaped between the read above and here) or a
		// summary that blew the 4096-byte ceiling. Neither is the agent's fault to
		// fix by retrying differently, but both are conflicts with the stored row.
		h.log.Warn("job: could not record agent report",
			"run_id", req.RunID, "job_id", run.JobID, "state", req.State, "err", err)
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "could not record job report")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}
