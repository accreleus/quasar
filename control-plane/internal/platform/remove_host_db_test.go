// POST /v1/admin/platform/hosts/{id}/remove against a real Postgres, behind the
// real RequireAuth→RequireAdmin chain, with the operator-drain cordon the
// composition root wires (#366; control-api.md amendment 14 §"Removing an owned
// GPU host").
package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/admission"
	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
)

const removeRequestID = "3c0a6f2e-8d1b-4f7e-9a55-2b8e1c0d9f41"

// fakeRemoveAgent records what was sent and answers a fixed ack.
type fakeRemoveAgent struct {
	mu        sync.Mutex
	connected bool
	ack       Ack
	err       error
	sent      []string
	stopped   int
}

func (f *fakeRemoveAgent) send(_ context.Context, _ string, requestID string) (Ack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, requestID)
	return f.ack, f.err
}

func (f *fakeRemoveAgent) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type removeHarness struct {
	pool       *pgxpool.Pool
	store      *Store
	hostID     string
	agent      *fakeRemoveAgent
	ownNode    string
	base       string
	adminToken string
	userToken  string
}

func newRemoveHarness(t *testing.T) *removeHarness {
	t.Helper()
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)
	h := &removeHarness{pool: pool, store: store, agent: &fakeRemoveAgent{connected: true, ack: Ack{OK: true}}}
	h.hostID = seedHost(t, pool, "gpu-host-4", commitA, "online")
	mustExec(t, pool, `UPDATE hosts SET install_mode = 'owned' WHERE id = $1::uuid`, h.hostID)

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	authHandler := auth.NewHandler(authSvc)
	for _, u := range []struct{ email, name string }{{"rm-admin@t.local", "rmadmin"}, {"rm-user@t.local", "rmuser"}} {
		if _, err := authSvc.Register(ctx, u.email, u.name, "password12345"); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, pool, `UPDATE users SET role='admin' WHERE email='rm-admin@t.local'`)
	adminTok, err := authSvc.Login(ctx, "rm-admin@t.local", "password12345", "test")
	if err != nil {
		t.Fatal(err)
	}
	userTok, err := authSvc.Login(ctx, "rm-user@t.local", "password12345", "test")
	if err != nil {
		t.Fatal(err)
	}
	h.adminToken, h.userToken = adminTok.Plaintext, userTok.Plaintext

	adm := admission.NewStore(pool)
	handler := NewRemoveHandler(RemoveDeps{
		Store:     store,
		Connected: func(string) bool { return h.agent.connected },
		OwnNodeName: func(context.Context) (string, bool) {
			return h.ownNode, h.ownNode != ""
		},
		Cordon: func(ctx context.Context, hostID string) (func(context.Context), error) {
			held, err := adm.List(ctx, hostID)
			if err != nil {
				return nil, err
			}
			found := false
			for _, r := range held {
				found = found || r.OwnerKind == admission.Manual
			}
			if _, err := adm.Acquire(ctx, hostID, admission.ManualOwner, admission.ReasonManualDrain); err != nil {
				return nil, err
			}
			return func(ctx context.Context) {
				if !found {
					_, _ = adm.Release(ctx, hostID, admission.ManualOwner, true)
				}
			}, nil
		},
		StopSessions: func(ctx context.Context, hostID string) error {
			h.agent.mu.Lock()
			h.agent.stopped++
			h.agent.mu.Unlock()
			_, err := pool.Exec(ctx, `UPDATE sessions SET state = 'stopped' WHERE host_id = $1::uuid`, hostID)
			return err
		},
		Send: h.agent.send,
		Host: func(ctx context.Context, hostID string) (any, error) {
			var status string
			err := pool.QueryRow(ctx, `SELECT status FROM hosts WHERE id = $1::uuid`, hostID).Scan(&status)
			return map[string]any{"id": hostID, "status": status}, err
		},
	}, audit.NewStore(pool), testLogger())
	handler.NewRequestID = func() string { return removeRequestID }
	handler.AckTimeout = 200 * time.Millisecond
	mux := http.NewServeMux()
	handler.Register(mux, func(next http.Handler) http.Handler {
		return authHandler.RequireAuth(authHandler.RequireAdmin(next))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h.base = srv.URL
	return h
}

func (h *removeHarness) remove(t *testing.T, token string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(http.MethodPost, h.base+"/v1/admin/platform/hosts/"+h.hostID+"/remove", reader)
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

func (h *removeHarness) status(t *testing.T) string {
	t.Helper()
	var s string
	if err := h.pool.QueryRow(context.Background(), `SELECT status FROM hosts WHERE id = $1::uuid`, h.hostID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func reasonOf(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(body, &e)
	return e.Reason
}

func TestRemoveHostIsAdminOnly(t *testing.T) {
	h := newRemoveHarness(t)
	if code, _ := h.remove(t, "", nil); code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", code)
	}
	if code, _ := h.remove(t, h.userToken, nil); code != http.StatusForbidden {
		t.Errorf("non-admin = %d, want 403", code)
	}
	if h.agent.sentCount() != 0 || h.status(t) != "online" {
		t.Fatal("a refused request reached the host or cordoned it")
	}
}

func TestRemoveHostCordonsSendsHostRemoveAndAudits(t *testing.T) {
	h := newRemoveHarness(t)
	code, body := h.remove(t, h.adminToken, nil)
	if code != http.StatusAccepted {
		t.Fatalf("remove = %d (%s), want 202", code, body)
	}
	var env struct {
		Host map[string]any `json:"host"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Host["status"] != "draining" {
		t.Fatalf("body = %s, want the host as it stands (draining)", body)
	}
	if h.agent.sentCount() != 1 || h.agent.sent[0] != removeRequestID {
		t.Fatalf("sent = %v, want one host_remove carrying the minted request id", h.agent.sent)
	}
	var action string
	var details []byte
	if err := h.pool.QueryRow(context.Background(), `
		SELECT action, details::text FROM admin_activity WHERE target_id = $1 ORDER BY created_at DESC LIMIT 1`,
		h.hostID).Scan(&action, &details); err != nil {
		t.Fatalf("audit: %v", err)
	}
	var d map[string]any
	_ = json.Unmarshal(details, &d)
	if _, extra := d["request_id"]; action != "platform.remove.host" || d["node_name"] != "gpu-host-4" || d["force"] != false || extra {
		t.Errorf("audit = %s %s", action, details)
	}
	var attempts int
	_ = h.pool.QueryRow(context.Background(), `SELECT count(*) FROM platform_apply_attempts`).Scan(&attempts)
	if attempts != 0 {
		t.Errorf("a removal wrote %d platform_apply_attempts rows, want none", attempts)
	}
}

func TestRemoveHostRefusesInTheContractsOrderChangingNothing(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(*removeHarness, *testing.T)
		code   int
		errc   string
		reason string
	}{
		{"an attempt in flight", func(h *removeHarness, t *testing.T) {
			if _, err := h.store.CreateHostAttempt(context.Background(), NewHostAttempt{
				Kind: KindApply, HostID: h.hostID,
				Requested: []ComponentDigest{{Name: ComponentNodeAgent, Image: devAgentImage, Digest: devAgentDigest}},
			}); err != nil {
				t.Fatal(err)
			}
		}, http.StatusConflict, CodeAttemptInFlight, ""},
		{"an offline host", func(h *removeHarness, _ *testing.T) { h.agent.connected = false },
			http.StatusConflict, CodeHostNotEligible, ReasonHostOffline},
		{"no recovery actor answered", func(h *removeHarness, t *testing.T) {
			mustExec(t, h.pool, `UPDATE hosts SET updater_present = false WHERE id = $1::uuid`, h.hostID)
		}, http.StatusConflict, CodeHostNotEligible, ReasonUpdaterAbsent},
		{"a host that is not owned", func(h *removeHarness, t *testing.T) {
			mustExec(t, h.pool, `UPDATE hosts SET install_mode = 'registry' WHERE id = $1::uuid`, h.hostID)
		}, http.StatusConflict, CodeHostNotRemovable, ""},
		{"the control plane's own machine", func(h *removeHarness, _ *testing.T) { h.ownNode = "gpu-host-4" },
			http.StatusConflict, CodeHostNotRemovable, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newRemoveHarness(t)
			c.setup(h, t)
			code, body := h.remove(t, h.adminToken, nil)
			if code != c.code || errCode(t, body) != c.errc || reasonOf(t, body) != c.reason {
				t.Fatalf("= %d %s, want %d %s reason=%q", code, body, c.code, c.errc, c.reason)
			}
			if h.agent.sentCount() != 0 {
				t.Error("host_remove was sent")
			}
			if s := h.status(t); s != "online" {
				t.Errorf("status = %s: a pre-send refusal changed the cordon", s)
			}
		})
	}
}

func TestRemoveHostWithSessionsRefusesWithoutForceAndRestoresTheCordon(t *testing.T) {
	h := newRemoveHarness(t)
	seedSession(t, h.pool, h.hostID)
	code, body := h.remove(t, h.adminToken, map[string]any{"force": false})
	if code != http.StatusConflict || errCode(t, body) != "conflict" {
		t.Fatalf("= %d %s, want 409 conflict", code, body)
	}
	if h.status(t) != "online" || h.agent.sentCount() != 0 || h.agent.stopped != 0 {
		t.Fatal("the refusal left a cordon, sent host_remove or stopped a session")
	}

	// Already drained by the operator: a refusal keeps that drain.
	mustExec(t, h.pool, `INSERT INTO host_admission_restrictions (host_id, owner_kind, owner_id, reason)
		VALUES ($1::uuid, 'manual', '00000000-0000-0000-0000-000000000000', 'manual_drain')`, h.hostID)
	mustExec(t, h.pool, `UPDATE hosts SET status = 'draining' WHERE id = $1::uuid`, h.hostID)
	if code, _ := h.remove(t, h.adminToken, nil); code != http.StatusConflict {
		t.Fatalf("= %d, want 409", code)
	}
	if h.status(t) != "draining" {
		t.Fatal("a refusal lifted the operator's own drain")
	}

	code, body = h.remove(t, h.adminToken, map[string]any{"force": true})
	if code != http.StatusAccepted {
		t.Fatalf("forced = %d %s, want 202", code, body)
	}
	if h.agent.stopped != 1 || h.agent.sentCount() != 1 {
		t.Fatalf("stopped=%d sent=%d, want the sessions stopped, then host_remove", h.agent.stopped, h.agent.sentCount())
	}
}

func TestRemoveHostAckOutcomesRestoreTheCordon(t *testing.T) {
	for _, c := range []struct {
		name string
		ack  Ack
		err  error
		code int
		errc string
	}{
		{"the actor refused", Ack{OK: false, Error: "busy"}, nil, http.StatusConflict, CodeHostNotRemovable},
		{"no ack", Ack{}, context.DeadlineExceeded, http.StatusNotImplemented, CodeApplyUnsupported},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newRemoveHarness(t)
			h.agent.ack, h.agent.err = c.ack, c.err
			code, body := h.remove(t, h.adminToken, nil)
			if code != c.code || errCode(t, body) != c.errc {
				t.Fatalf("= %d %s, want %d %s", code, body, c.code, c.errc)
			}
			if c.ack.Error != "" && !bytes.Contains(body, []byte(c.ack.Error)) {
				t.Errorf("the message does not name the ack's identifier: %s", body)
			}
			if h.status(t) != "online" {
				t.Error("the cordon was not restored")
			}
		})
	}
}

func TestRemoveHostOfAnUnknownHostIs404(t *testing.T) {
	h := newRemoveHarness(t)
	h.hostID = "00000000-0000-4000-8000-000000000999"
	if code, _ := h.remove(t, h.adminToken, nil); code != http.StatusNotFound {
		t.Fatalf("= %d, want 404", code)
	}
}
