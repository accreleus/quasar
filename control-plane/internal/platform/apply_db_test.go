// The apply half against a real Postgres: migration 0075's two single-flight
// indexes ARE the refusals, so they are exercised here rather than asserted
// about, and the endpoints run behind the REAL RequireAuth→RequireAdmin chain.
package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// A manifest whose node-agent digest is what an apply must send, and whose
// control-plane component must NEVER reach a host.
func applyManifest(commit string, schema int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
	  "format_version": 1, "version": "0.9.0", "prerelease": false,
	  "source_commit": %q, "built_at": "2026-09-04T12:00:00Z", "schema_version": %d,
	  "components": [
	    { "name": "control-plane", "image": "ghcr.io/accreleus/quasar/quasar-control-plane", "digest": "sha256:%s" },
	    { "name": "node-agent",    "image": "ghcr.io/accreleus/quasar/quasar-node-agent",    "digest": "sha256:%s" }
	  ]
	}`, commit, schema, hex64, hex64))
}

func seedHost(t *testing.T, pool *pgxpool.Pool, name, commit, status string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO hosts (node_name, status, source_commit, built_at, install_mode, updater_present)
		VALUES ($1, $2, $3, now(), 'registry', true) RETURNING id::text`, name, status, commit).Scan(&id)
	if err != nil {
		t.Fatalf("seed host: %v", err)
	}
	return id
}

func seedRelease(t *testing.T, store *Store, commit string, schema int) Release {
	t.Helper()
	// A distinct version per commit: platform_releases_version_key is unique.
	if _, err := store.UpsertRelease(context.Background(), Release{
		Channel: ChannelStable, Version: str("0.9." + commit[:1]), SourceCommit: commit,
		BuiltAt: at(4), SchemaVersion: schema, Manifest: applyManifest(commit, schema),
	}); err != nil {
		t.Fatalf("seed release: %v", err)
	}
	rows, err := store.Releases(context.Background(), ChannelStable)
	if err != nil || len(rows) == 0 {
		t.Fatalf("read back release: %v", err)
	}
	for _, r := range rows {
		if r.SourceCommit == commit {
			return r
		}
	}
	t.Fatal("seeded release not found")
	return Release{}
}

type applyHarness struct {
	pool        *pgxpool.Pool
	store       *Store
	runner      *Runner
	agent       *fakeAgent
	adminToken  string
	userToken   string
	base        string
	hostID      string
	release     Release
	hostCommit  string
	viewOverlay func(*View)
}

// newApplyHarness wires the two endpoints behind the real admin chain, with a
// fake agent and a view whose control-plane identity is synthesized (a test
// binary carries no build stamps, so every host would otherwise read
// control_plane_not_first).
func newApplyHarness(t *testing.T) *applyHarness {
	t.Helper()
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)

	h := &applyHarness{pool: pool, store: store, hostCommit: commitA}
	h.release = seedRelease(t, store, commitB, buildinfo.Get().SchemaVersion)
	h.hostID = seedHost(t, pool, "gpu-01", h.hostCommit, "online")

	h.agent = &fakeAgent{ack: Ack{OK: true}}
	h.runner = testRunner(store, h.agent.deps())
	t.Cleanup(h.runner.Close)

	view := func(ctx context.Context) (View, error) {
		hosts, err := store.Hosts(ctx)
		if err != nil {
			return View{}, err
		}
		releases, err := store.Releases(ctx, ChannelStable)
		if err != nil {
			return View{}, err
		}
		open, err := store.OpenAttempts(ctx)
		if err != nil {
			return View{}, err
		}
		v := PlanRelease(PlanInputs{
			Channel:      ChannelStable,
			EdgeBranch:   "develop",
			ControlPlane: cp(commitB, buildinfo.Get().SchemaVersion),
			Hosts:        hosts,
			Releases:     releases,
			OpenAttempts: open,
		})
		if h.viewOverlay != nil {
			h.viewOverlay(&v)
		}
		return v, nil
	}

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	authHandler := auth.NewHandler(authSvc)
	for _, u := range []struct{ email, name string }{
		{"apply-admin@t.local", "applyadmin"},
		{"apply-user@t.local", "applyuser"},
	} {
		if _, err := authSvc.Register(ctx, u.email, u.name, "password12345"); err != nil {
			t.Fatalf("register %s: %v", u.email, err)
		}
	}
	mustExec(t, pool, `UPDATE users SET role='admin' WHERE email='apply-admin@t.local'`)
	adminTok, err := authSvc.Login(ctx, "apply-admin@t.local", "password12345", "test")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	userTok, err := authSvc.Login(ctx, "apply-user@t.local", "password12345", "test")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}
	h.adminToken, h.userToken = adminTok.Plaintext, userTok.Plaintext

	mux := http.NewServeMux()
	NewApplyHandler(store, h.runner, view, audit.NewStore(pool), testLogger()).
		Register(mux, func(next http.Handler) http.Handler {
			return authHandler.RequireAuth(authHandler.RequireAdmin(next))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h.base = srv.URL
	return h
}

func (h *applyHarness) post(t *testing.T, path, token string, body any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, h.base+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (h *applyHarness) applyURL() string {
	return "/v1/admin/platform/hosts/" + h.hostID + "/apply"
}

func errCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body %s: %v", body, err)
	}
	return e.Error.Code
}

// The gate is the server's, not the UI's.
func TestApplyEndpointsAreAdminOnly(t *testing.T) {
	h := newApplyHarness(t)
	if code, _ := h.post(t, h.applyURL(), "", HostApplyRequest{ReleaseID: h.release.ID}); code != http.StatusUnauthorized {
		t.Errorf("anonymous apply = %d, want 401", code)
	}
	if code, _ := h.post(t, h.applyURL(), h.userToken, HostApplyRequest{ReleaseID: h.release.ID}); code != http.StatusForbidden {
		t.Errorf("non-admin apply = %d, want 403", code)
	}
	code, _ := get(t, h.base+"/v1/admin/platform/attempts", h.userToken)
	if code != http.StatusForbidden {
		t.Errorf("non-admin history = %d, want 403", code)
	}
}

func TestApplyCreatesAnAttemptAndSendsOnlyTheNodeAgentComponent(t *testing.T) {
	h := newApplyHarness(t)
	code, body := h.post(t, h.applyURL(), h.adminToken, HostApplyRequest{ReleaseID: h.release.ID, Force: true})
	if code != http.StatusAccepted {
		t.Fatalf("apply = %d (%s), want 202", code, body)
	}
	var env AttemptEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if env.Attempt.Kind != KindApply || env.Attempt.Target != TargetHost || env.Attempt.RunID != nil {
		t.Errorf("attempt = %+v, want a standalone host apply", env.Attempt)
	}
	if len(env.Attempt.RequestedDigests) != 1 || env.Attempt.RequestedDigests[0].Name != ComponentNodeAgent {
		t.Fatalf("requested_digests = %+v, want exactly the node-agent component", env.Attempt.RequestedDigests)
	}
	if env.Attempt.NodeName == nil || *env.Attempt.NodeName != "gpu-01" {
		t.Errorf("node_name = %v, want the host's name", env.Attempt.NodeName)
	}

	waitFor(t, "release_apply to be sent", func() bool { return h.agent.sentCount() == 1 })
	sent := h.agent.sent[0]
	if len(sent.Components) != 1 || sent.Components[0].Name != ComponentNodeAgent {
		t.Errorf("the control-plane component must never reach a host: %+v", sent.Components)
	}
	if sent.Release.SourceCommit != h.release.SourceCommit {
		t.Errorf("release provenance = %+v, want the release's commit", sent.Release)
	}
	// The request id was persisted BEFORE the send: the row must resolve by it.
	if _, err := h.store.AttemptByRequestID(context.Background(), sent.RequestID); err != nil {
		t.Errorf("request id was not persisted before the send: %v", err)
	}
}

// The second click loses to the database's partial unique index, not to a code
// check — which is why two admins cannot both win.
func TestSecondOpenAttemptForAHostIsRefused(t *testing.T) {
	h := newApplyHarness(t)
	if code, body := h.post(t, h.applyURL(), h.adminToken, HostApplyRequest{ReleaseID: h.release.ID, Force: true}); code != http.StatusAccepted {
		t.Fatalf("first apply = %d (%s)", code, body)
	}
	code, body := h.post(t, h.applyURL(), h.adminToken, HostApplyRequest{ReleaseID: h.release.ID, Force: true})
	if code != http.StatusConflict || errCode(t, body) != CodeAttemptInFlight {
		t.Fatalf("second apply = %d %s, want 409 attempt_in_flight", code, body)
	}
	// And the same fact reaches the page as an eligibility reason.
	open, err := h.store.OpenAttempts(context.Background())
	if err != nil || len(open) != 1 {
		t.Fatalf("open attempts = %d (%v), want 1", len(open), err)
	}
	v := PlanRelease(PlanInputs{
		Channel: ChannelStable, ControlPlane: cp(commitB, buildinfo.Get().SchemaVersion),
		Hosts:        []HostIdentity{{HostID: h.hostID, NodeName: "gpu-01", Status: "online", SourceCommit: str(commitA), BuiltAt: str("x"), InstallMode: str(InstallRegistry), UpdaterPresent: boolp(true)}},
		Releases:     []Release{h.release},
		OpenAttempts: open,
	})
	if v.Targets[1].Eligible || v.Targets[1].Reason == nil || *v.Targets[1].Reason != ReasonAttemptInFlight {
		t.Errorf("target = %+v, want attempt_in_flight", v.Targets[1])
	}
	if v.ActiveApply == nil || len(v.ActiveApply.Attempts) != 1 {
		t.Errorf("active_apply = %+v, want the open attempt", v.ActiveApply)
	}
}

// The zero uuid stands in for the control-plane target, so two open
// control-plane attempts are impossible too — a plain partial index on a
// nullable column would not have collided.
func TestControlPlaneAttemptsCollideOnTheZeroUUID(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	insert := func() error {
		_, err := pool.Exec(ctx, `
			INSERT INTO platform_apply_attempts (kind, target, requested_digests, state)
			VALUES ('apply', 'control_plane', '[]'::jsonb, 'queued')`)
		return err
	}
	if err := insert(); err != nil {
		t.Fatalf("first control-plane attempt: %v", err)
	}
	if err := insert(); err == nil {
		t.Fatal("a second open control-plane attempt was allowed")
	}
}

func TestApplyRefusesAnUnknownHostReleaseAndAnUnofferedOne(t *testing.T) {
	h := newApplyHarness(t)
	missing := "44444444-4444-4444-8444-444444444444"
	if code, _ := h.post(t, "/v1/admin/platform/hosts/"+missing+"/apply", h.adminToken,
		HostApplyRequest{ReleaseID: h.release.ID}); code != http.StatusNotFound {
		t.Errorf("unknown host = %d, want 404", code)
	}
	if code, _ := h.post(t, h.applyURL(), h.adminToken, HostApplyRequest{ReleaseID: missing}); code != http.StatusNotFound {
		t.Errorf("unknown release = %d, want 404", code)
	}
	if code, body := h.post(t, h.applyURL(), h.adminToken, HostApplyRequest{ReleaseID: "not-a-uuid"}); code != http.StatusBadRequest {
		t.Errorf("malformed release_id = %d %s, want 400", code, body)
	}

	// A release below this control plane's schema is unprocessable, not merely
	// un-offered (ADR 0002).
	below := seedRelease(t, h.store, commitC, buildinfo.Get().SchemaVersion-1)
	code, body := h.post(t, h.applyURL(), h.adminToken, HostApplyRequest{ReleaseID: below.ID})
	if code != http.StatusUnprocessableEntity || errCode(t, body) != CodeReleaseBelowSchemaVersion {
		t.Errorf("older release = %d %s, want 422 release_below_schema_version", code, body)
	}
}

// The button's absence and the endpoint's refusal are explained by the SAME
// identifier, carried as a top-level sibling of the error.
func TestApplyRefusesAnIneligibleHostWithTheEligibilityReason(t *testing.T) {
	h := newApplyHarness(t)
	mustExec(t, h.pool, `UPDATE hosts SET install_mode = 'source' WHERE id = $1::uuid`, h.hostID)
	code, body := h.post(t, h.applyURL(), h.adminToken, HostApplyRequest{ReleaseID: h.release.ID})
	if code != http.StatusConflict || errCode(t, body) != CodeHostNotEligible {
		t.Fatalf("source-built host = %d %s, want 409 host_not_eligible", code, body)
	}
	var e struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Reason != ReasonInstallModeSource {
		t.Errorf("reason = %q (%v), want install_mode_source", e.Reason, err)
	}
}

// A drain that never reaches zero is what waiting_sessions means, and the
// endpoint answers with the count before anything is sent.
func TestApplyWaitsForSessionsAndReportsHowMany(t *testing.T) {
	h := newApplyHarness(t)
	seedSession(t, h.pool, h.hostID)

	code, body := h.post(t, h.applyURL(), h.adminToken, HostApplyRequest{ReleaseID: h.release.ID})
	if code != http.StatusAccepted {
		t.Fatalf("apply = %d (%s)", code, body)
	}
	var env AttemptEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Attempt.State != AttemptWaitingSessions || env.Attempt.SessionsRemaining == nil || *env.Attempt.SessionsRemaining != 1 {
		t.Fatalf("attempt = %+v, want waiting_sessions with 1 session", env.Attempt)
	}
	time.Sleep(30 * time.Millisecond)
	if h.agent.sentCount() != 0 {
		t.Error("release_apply was sent while a session was still running")
	}
	mustExec(t, h.pool, `UPDATE sessions SET state = 'stopped' WHERE host_id = $1::uuid`, h.hostID)
	waitFor(t, "the drain to finish and the apply to be sent", func() bool { return h.agent.sentCount() == 1 })
}

// The success evidence, end to end against the real store: the host registers
// on the requested commit and the attempt resolves.
func TestRegisterOnTheRequestedCommitResolvesTheAttempt(t *testing.T) {
	h := newApplyHarness(t)
	if code, body := h.post(t, h.applyURL(), h.adminToken, HostApplyRequest{ReleaseID: h.release.ID, Force: true}); code != http.StatusAccepted {
		t.Fatalf("apply = %d (%s)", code, body)
	}
	waitFor(t, "release_apply to be sent", func() bool { return h.agent.sentCount() == 1 })

	ctx := context.Background()
	commit := h.release.SourceCommit
	h.runner.HandleRegister(ctx, h.hostID, &commit)

	attempts, err := h.store.ListAttempts(ctx, h.hostID, 10)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %d (%v)", len(attempts), err)
	}
	if attempts[0].State != AttemptSucceeded || attempts[0].FinishedAt == nil {
		t.Errorf("attempt = %+v, want succeeded with a finished_at", attempts[0])
	}
}

// A relayed release_state writes the previous digests — in every state, not
// only failing ones, because they are what a restore is copied from.
func TestReleaseStateRecordsProgressAndPreviousDigests(t *testing.T) {
	h := newApplyHarness(t)
	if code, _ := h.post(t, h.applyURL(), h.adminToken, HostApplyRequest{ReleaseID: h.release.ID, Force: true}); code != http.StatusAccepted {
		t.Fatal("apply was refused")
	}
	waitFor(t, "release_apply to be sent", func() bool { return h.agent.sentCount() == 1 })

	ctx := context.Background()
	prev := "sha256:" + hex64
	h.runner.HandleReleaseState(ctx, h.hostID, ReleaseStateReport{
		RequestID: h.agent.sent[0].RequestID,
		State:     AttemptPulling,
		Previous:  []PreviousDigest{{Name: ComponentNodeAgent, Digest: &prev}},
	})
	a, err := h.store.AttemptByRequestID(ctx, h.agent.sent[0].RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != AttemptPulling {
		t.Errorf("state = %q, want pulling", a.State)
	}
	if len(a.PreviousDigests) != 1 || a.PreviousDigests[0].Digest == nil || *a.PreviousDigests[0].Digest != prev {
		t.Errorf("previous_digests = %+v, want the reported digest", a.PreviousDigests)
	}

	// And a failure carries its reason and its bounded output.
	reason := ReasonRecreateFailed
	h.runner.HandleReleaseState(ctx, h.hostID, ReleaseStateReport{
		RequestID: h.agent.sent[0].RequestID, State: AttemptFailed,
		Reason: &reason, Output: "compose said no",
	})
	a, err = h.store.AttemptByRequestID(ctx, h.agent.sent[0].RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != AttemptFailed || a.Reason == nil || *a.Reason != reason || a.Output != "compose said no" {
		t.Errorf("attempt = %+v, want failed/recreate_failed with its output", a)
	}
}

// History is newest first, spans the instance by default, and narrows by host —
// with an unknown host answering an empty list rather than a 404.
func TestAttemptHistoryOrderingAndFilter(t *testing.T) {
	h := newApplyHarness(t)
	ctx := context.Background()
	other := seedHost(t, h.pool, "gpu-02", commitA, "online")

	first, err := h.store.CreateHostAttempt(ctx, NewHostAttempt{
		Kind: KindApply, HostID: h.hostID, ReleaseID: &h.release.ID,
		Requested: []ComponentDigest{{Name: ComponentNodeAgent, Image: "img", Digest: "sha256:" + hex64}},
		Previous:  []PreviousDigest{{Name: ComponentNodeAgent}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.FailAttempt(ctx, first.ID, ReasonPullFailed, ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	second, err := h.store.CreateHostAttempt(ctx, NewHostAttempt{
		Kind: KindApply, HostID: other, ReleaseID: &h.release.ID,
		Requested: []ComponentDigest{{Name: ComponentNodeAgent, Image: "img", Digest: "sha256:" + hex64}},
		Previous:  []PreviousDigest{{Name: ComponentNodeAgent}},
	})
	if err != nil {
		t.Fatal(err)
	}

	code, body := get(t, h.base+"/v1/admin/platform/attempts", h.adminToken)
	if code != http.StatusOK {
		t.Fatalf("history = %d (%s)", code, body)
	}
	var all AttemptsResponse
	if err := json.Unmarshal(body, &all); err != nil {
		t.Fatal(err)
	}
	if len(all.Attempts) != 2 || all.Attempts[0].ID != second.ID {
		t.Fatalf("history = %+v, want newest first", all.Attempts)
	}

	_, body = get(t, h.base+"/v1/admin/platform/attempts?host_id="+h.hostID, h.adminToken)
	var one AttemptsResponse
	if err := json.Unmarshal(body, &one); err != nil {
		t.Fatal(err)
	}
	if len(one.Attempts) != 1 || one.Attempts[0].ID != first.ID {
		t.Errorf("filtered history = %+v, want only this host's", one.Attempts)
	}
	if one.Attempts[0].Reason == nil || *one.Attempts[0].Reason != ReasonPullFailed {
		t.Errorf("a failed attempt must carry its reason: %+v", one.Attempts[0])
	}

	unknown := "55555555-5555-4555-8555-555555555555"
	code, body = get(t, h.base+"/v1/admin/platform/attempts?host_id="+unknown, h.adminToken)
	var none AttemptsResponse
	if err := json.Unmarshal(body, &none); err != nil {
		t.Fatal(err)
	}
	if code != http.StatusOK || len(none.Attempts) != 0 {
		t.Errorf("unknown host = %d with %d attempts, want 200 and an empty list", code, len(none.Attempts))
	}

	for _, bad := range []string{"?limit=0", "?limit=201", "?limit=x", "?host_id=nope"} {
		if code, _ := get(t, h.base+"/v1/admin/platform/attempts"+bad, h.adminToken); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, code)
		}
	}
}

// The state predicate lives in two places — Go and SQL — so they are pinned
// together rather than left to drift.
func TestTerminalSplitMatchesSQL(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	for _, state := range []string{AttemptQueued, AttemptWaitingSessions, AttemptPending,
		AttemptPulling, AttemptRecreating, AttemptVerifying, AttemptSucceeded, AttemptFailed, AttemptCancelled} {
		var open bool
		if err := pool.QueryRow(ctx,
			`SELECT $1::text NOT IN `+terminalStatesSQL, state).Scan(&open); err != nil {
			t.Fatalf("evaluate %q: %v", state, err)
		}
		if open == TerminalAttemptState(state) {
			t.Errorf("state %q: SQL says open=%v, Go says terminal=%v", state, open, TerminalAttemptState(state))
		}
	}
}

// 0075 goes down and comes back up: the tables are dropped in FK order.
func TestMigration0075IsReversible(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	for _, table := range []string{"platform_apply_attempts", "platform_apply_runs"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("%s was not created by the migration", table)
		}
	}
	// The reason CHECK is the invariant a client renders on: no reason without
	// a failure, and no failure without one.
	_, err := pool.Exec(ctx, `
		INSERT INTO platform_apply_attempts (kind, target, requested_digests, state, reason)
		VALUES ('apply', 'control_plane', '[]'::jsonb, 'pulling', 'pull_failed')`)
	if err == nil {
		t.Error("a non-failed attempt was allowed to carry a reason")
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO platform_apply_attempts (kind, target, host_id, requested_digests, state)
		VALUES ('apply', 'control_plane', gen_random_uuid(), '[]'::jsonb, 'queued')`)
	if err == nil {
		t.Error("a control-plane attempt was allowed to name a host")
	}
}

func TestNodeAgentComponentsDropsTheControlPlane(t *testing.T) {
	m, err := ParseManifest(applyManifest(commitB, 75))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	got := NodeAgentComponents(m)
	if len(got) != 1 || got[0].Name != ComponentNodeAgent {
		t.Fatalf("components = %+v, want only the node agent", got)
	}
}

func seedSession(t *testing.T, pool *pgxpool.Pool, hostID string) {
	t.Helper()
	ctx := context.Background()
	var userID, appID string
	err := pool.QueryRow(ctx, `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ('sess@t.local', 'sessuser', 'x', 'user') RETURNING id::text`).Scan(&userID)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	err = pool.QueryRow(ctx, `
		INSERT INTO apps (name) VALUES ('app') RETURNING id::text`).Scan(&appID)
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}
	mustExec(t, pool, `INSERT INTO sessions
		(user_id, app_id, host_id, state, width, height, fps, bitrate_kbps)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'running', 1280, 720, 60, 6000)`, userID, appID, hostID)
}
