// agent_handler_db_test.go — the pull channel's contract, against a real store.
//
// These are DB tests rather than handler unit tests because every claim the
// channel makes is a STORAGE claim: single-flight is a partial unique index,
// claiming is FOR UPDATE ... SKIP LOCKED, and report idempotency is "did the
// UPDATE match a `running` row". A fake store would only test that this file
// agrees with itself.
package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeAgents stands in for storage.Manager's node_secret verification. The real
// verification (hex(sha256) under ConstantTimeCompare) is internal/storage's
// test's business; what this file must prove is that the ROUTES refuse a caller
// the verifier rejected and scope everything else to the host it returned.
type fakeAgents struct {
	// hosts maps node_secret -> (node_name, host_id). BOTH must match, as the
	// real verifier requires: it looks the host up by node_name and only then
	// compares the secret hash.
	hosts map[string]fakeHost
}

type fakeHost struct {
	node string
	id   string
}

var errFakeAuth = errors.New("agent auth failed")

func (f fakeAgents) AuthAgentHost(_ context.Context, nodeName, nodeSecret string) (string, error) {
	if h, ok := f.hosts[nodeSecret]; ok && h.node == nodeName {
		return h.id, nil
	}
	return "", errFakeAuth
}

type agentFixture struct {
	srv   *httptest.Server
	store *Store
	disp  *Dispatcher
	host  string
	other string
}

func newAgentFixture(t *testing.T, defs ...Definition) agentFixture {
	t.Helper()
	pool := testDB(t)
	store := seed(t, pool, defs...)
	hostA := newHost(t, pool, "host-a")
	hostB := newHost(t, pool, "host-b")

	reg := NewRegistry()
	for _, d := range defs {
		if err := reg.Register(d); err != nil {
			t.Fatalf("register %s: %v", d.ID, err)
		}
	}
	disp := New(store, reg, DefaultConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := NewAgentHandler(store, disp, fakeAgents{hosts: map[string]fakeHost{
		"secret-a": {node: "host-a", id: hostA},
		"secret-b": {node: "host-b", id: hostB},
	}}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return agentFixture{srv: srv, store: store, disp: disp, host: hostA, other: hostB}
}

func agentReq(t *testing.T, method, url, node, secret string, body any) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	req.Header.Set("X-Quasar-Node", node)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodePending(t *testing.T, resp *http.Response) pendingResponse {
	t.Helper()
	var out pendingResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode pending: %v", err)
	}
	return out
}

// materializeFor queues a run for a host-scoped job, ready to be claimed now.
func (f agentFixture) materializeFor(t *testing.T, jobID, hostID string, params any) Run {
	t.Helper()
	run, _, err := f.store.Materialize(context.Background(), MaterializeParams{
		JobID:        jobID,
		HostID:       hostID,
		Trigger:      TriggerSchedule,
		ScheduledFor: time.Now().Add(-time.Second),
		Params:       params,
	})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return run
}

// --- auth ------------------------------------------------------------------

// TestPendingRejectsBadNodeSecret is the security floor of the whole channel:
// without it, any caller that can reach the control plane can claim and close
// another host's jobs.
func TestPendingRejectsBadNodeSecret(t *testing.T) {
	f := newAgentFixture(t, hostDef("template.warmup"))

	for _, tc := range []struct{ name, node, secret string }{
		{"wrong secret", "host-a", "not-the-secret"},
		{"unknown node", "nobody", "secret-a"},
		{"no bearer at all", "host-a", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := agentReq(t, "GET", f.srv.URL+"/v1/agent/jobs/pending", tc.node, tc.secret, nil)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("want 401, got %d", resp.StatusCode)
			}
		})
	}
}

func TestReportRejectsBadNodeSecret(t *testing.T) {
	f := newAgentFixture(t, hostDef("template.warmup"))
	run := f.materializeFor(t, "template.warmup", f.host, nil)

	resp := agentReq(t, "POST", f.srv.URL+"/v1/agent/jobs/report", "host-a", "not-the-secret",
		map[string]any{"run_id": run.ID, "state": "succeeded"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

// TestReportForAnotherHostsRunIs401 is the ownership rule: a host may only close
// its own claims, and "not yours" is indistinguishable from "no such run" so the
// endpoint is not a run-id oracle.
func TestReportForAnotherHostsRunIs401(t *testing.T) {
	f := newAgentFixture(t, hostDef("template.warmup"))
	run := f.materializeFor(t, "template.warmup", f.other, nil)

	resp := agentReq(t, "POST", f.srv.URL+"/v1/agent/jobs/report", "host-a", "secret-a",
		map[string]any{"run_id": run.ID, "state": "succeeded"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 for another host's run, got %d", resp.StatusCode)
	}
	// And the run is untouched: still pending, still claimable by its owner.
	got, err := f.store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != StatePending {
		t.Fatalf("a refused report must not touch the run: state=%s", got.State)
	}
}

func TestReportForAnUnknownRunIs401(t *testing.T) {
	f := newAgentFixture(t, hostDef("template.warmup"))
	for _, id := range []string{"11111111-1111-1111-1111-111111111111", "not-a-uuid"} {
		resp := agentReq(t, "POST", f.srv.URL+"/v1/agent/jobs/report", "host-a", "secret-a",
			map[string]any{"run_id": id, "state": "succeeded"})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("run_id %q: want 401, got %d", id, resp.StatusCode)
		}
	}
}

// --- claim -----------------------------------------------------------------

// TestPendingClaimsOnlyThisHostsRuns: the channel is host-scoped, and a poll
// must never hand a host work materialized for a different one.
func TestPendingClaimsOnlyThisHostsRuns(t *testing.T) {
	f := newAgentFixture(t, hostDef("template.warmup"))
	mine := f.materializeFor(t, "template.warmup", f.host, map[string]any{"image_id": "steam"})
	theirs := f.materializeFor(t, "template.warmup", f.other, nil)

	got := decodePending(t, agentReq(t, "GET", f.srv.URL+"/v1/agent/jobs/pending", "host-a", "secret-a", nil))
	if len(got.Runs) != 1 {
		t.Fatalf("want exactly this host's 1 run, got %d", len(got.Runs))
	}
	if got.Runs[0].RunID != mine.ID {
		t.Fatalf("claimed the wrong run: %s", got.Runs[0].RunID)
	}
	if got.Runs[0].JobID != "template.warmup" {
		t.Fatalf("job_id = %q", got.Runs[0].JobID)
	}
	if string(got.Runs[0].Params) != `{"image_id": "steam"}` &&
		string(got.Runs[0].Params) != `{"image_id":"steam"}` {
		t.Fatalf("params not carried verbatim: %s", got.Runs[0].Params)
	}
	if got.Runs[0].DeadlineSecs != int(DefaultClaimTimeout.Seconds()) {
		t.Fatalf("deadline_secs = %d", got.Runs[0].DeadlineSecs)
	}

	other, err := f.store.GetRun(context.Background(), theirs.ID)
	if err != nil {
		t.Fatalf("get other run: %v", err)
	}
	if other.State != StatePending {
		t.Fatalf("another host's run was claimed: %s", other.State)
	}
}

// TestPendingIsSingleFlightAcrossConcurrentPolls is the claim's whole reason for
// being one statement: two polls arriving together (a slow response plus a
// retry, or two agent processes during a restart overlap) must take DISJOINT
// sets, or the same warm-up runs twice on one host.
func TestPendingIsSingleFlightAcrossConcurrentPolls(t *testing.T) {
	f := newAgentFixture(t, hostDef("template.warmup"))
	run := f.materializeFor(t, "template.warmup", f.host, nil)

	const pollers = 6
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total int
	)
	start := make(chan struct{})
	for i := 0; i < pollers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp := agentReq(t, "GET", f.srv.URL+"/v1/agent/jobs/pending", "host-a", "secret-a", nil)
			var out pendingResponse
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				return
			}
			mu.Lock()
			total += len(out.Runs)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if total != 1 {
		t.Fatalf("a pending run must be handed out exactly once across concurrent polls, got %d", total)
	}
	got, err := f.store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != StateRunning {
		t.Fatalf("claimed run state = %s, want running", got.State)
	}
}

// TestPendingIsEmptyWhenTheFrameworkIsDisabled: QUASAR_JOBS=0 must stop work
// being handed out, not merely stop new work being created.
func TestPendingIsEmptyWhenTheFrameworkIsDisabled(t *testing.T) {
	f := newAgentFixture(t, hostDef("template.warmup"))
	f.materializeFor(t, "template.warmup", f.host, nil)

	cfg := DefaultConfig()
	cfg.Enabled = false
	disp := New(f.store, NewRegistry(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := NewAgentHandler(f.store, disp,
		fakeAgents{hosts: map[string]fakeHost{"secret-a": {node: "host-a", id: f.host}}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got := decodePending(t, agentReq(t, "GET", srv.URL+"/v1/agent/jobs/pending", "host-a", "secret-a", nil))
	if len(got.Runs) != 0 {
		t.Fatalf("a disabled framework must hand out nothing, got %d run(s)", len(got.Runs))
	}
}

// TestPendingCapsOneClaim: a host returning after an outage must not start every
// due job at once.
func TestPendingCapsOneClaim(t *testing.T) {
	defs := make([]Definition, 0, agentClaimLimit+2)
	for i := 0; i < agentClaimLimit+2; i++ {
		defs = append(defs, hostDef(string(rune('a'+i))+".job"))
	}
	f := newAgentFixture(t, defs...)
	for _, d := range defs {
		f.materializeFor(t, d.ID, f.host, nil)
	}

	got := decodePending(t, agentReq(t, "GET", f.srv.URL+"/v1/agent/jobs/pending", "host-a", "secret-a", nil))
	if len(got.Runs) != agentClaimLimit {
		t.Fatalf("want the claim capped at %d, got %d", agentClaimLimit, len(got.Runs))
	}
}

// --- report ----------------------------------------------------------------

func TestReportClosesTheRunAndIsIdempotent(t *testing.T) {
	f := newAgentFixture(t, hostDef("template.warmup"))
	f.materializeFor(t, "template.warmup", f.host, nil)
	claimed := decodePending(t, agentReq(t, "GET", f.srv.URL+"/v1/agent/jobs/pending", "host-a", "secret-a", nil))
	if len(claimed.Runs) != 1 {
		t.Fatalf("setup: want 1 claimed run, got %d", len(claimed.Runs))
	}
	runID := claimed.Runs[0].RunID

	body := map[string]any{
		"run_id":  runID,
		"state":   "succeeded",
		"summary": map[string]any{"files": 23701, "mib": 2512},
	}
	resp := agentReq(t, "POST", f.srv.URL+"/v1/agent/jobs/report", "host-a", "secret-a", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	got, err := f.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != StateSucceeded {
		t.Fatalf("state = %s", got.State)
	}
	if got.FinishedAt == nil {
		t.Fatal("finished_at must be stamped")
	}
	var summary map[string]any
	if err := json.Unmarshal(got.Summary, &summary); err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary["files"] != float64(23701) {
		t.Fatalf("summary not stored verbatim: %v", summary)
	}

	// THE RETRY-AFTER-A-BLIP CASE. A second identical report is a 200 no-op, not
	// a 409 the agent could never act on — and it must not overwrite the record.
	resp2 := agentReq(t, "POST", f.srv.URL+"/v1/agent/jobs/report", "host-a", "secret-a",
		map[string]any{"run_id": runID, "state": "failed", "error": "should not land"})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("retry: want 200, got %d", resp2.StatusCode)
	}
	after, err := f.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if after.State != StateSucceeded || after.Error != "" {
		t.Fatalf("a retried report rewrote a terminal run: state=%s err=%q", after.State, after.Error)
	}
}

// TestReportDeferredSchedulesTheBackoffRetry is the gates-are-the-job's property
// end to end over the wire: a runner's own refusal is an ordinary report, and
// the control plane turns it into a PERSISTED backoff — the thing the in-memory
// ladder it replaces could not survive a reconnect to provide.
func TestReportDeferredSchedulesTheBackoffRetry(t *testing.T) {
	f := newAgentFixture(t, hostDef("template.warmup"))
	f.materializeFor(t, "template.warmup", f.host, nil)
	claimed := decodePending(t, agentReq(t, "GET", f.srv.URL+"/v1/agent/jobs/pending", "host-a", "secret-a", nil))
	runID := claimed.Runs[0].RunID

	resp := agentReq(t, "POST", f.srv.URL+"/v1/agent/jobs/report", "host-a", "secret-a",
		map[string]any{
			"run_id":  runID,
			"state":   "deferred",
			"summary": map[string]any{"reason": "host has 1 live session(s)"},
		})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	closed, err := f.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if closed.State != StateDeferred {
		t.Fatalf("state = %s", closed.State)
	}
	next, open, err := f.store.OpenRun(context.Background(), "template.warmup", f.host)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	if !open {
		t.Fatal("a deferral must leave a fresh pending run behind")
	}
	if next.Attempt != closed.Attempt+1 {
		t.Fatalf("attempt did not advance: %d -> %d", closed.Attempt, next.Attempt)
	}
	if !next.ScheduledFor.After(time.Now()) {
		t.Fatalf("the retry must be in the future, got %s", next.ScheduledFor)
	}
}

func TestReportRejectsANonAgentState(t *testing.T) {
	f := newAgentFixture(t, hostDef("template.warmup"))
	f.materializeFor(t, "template.warmup", f.host, nil)
	claimed := decodePending(t, agentReq(t, "GET", f.srv.URL+"/v1/agent/jobs/pending", "host-a", "secret-a", nil))
	runID := claimed.Runs[0].RunID

	// `aborted` is the reaper's verdict on a silent host; a host claiming it would
	// be describing a decision it does not get to make. `running`/`pending` are
	// not outcomes at all.
	for _, state := range []string{"aborted", "running", "pending", ""} {
		resp := agentReq(t, "POST", f.srv.URL+"/v1/agent/jobs/report", "host-a", "secret-a",
			map[string]any{"run_id": runID, "state": state})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("state %q: want 400, got %d", state, resp.StatusCode)
		}
	}
	got, err := f.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != StateRunning {
		t.Fatalf("a rejected report must leave the run running, got %s", got.State)
	}
}
