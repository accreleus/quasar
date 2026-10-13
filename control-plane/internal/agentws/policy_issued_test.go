package agentws

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/hostcfg"
)

// #498: a next_session state is accepted only for a grant this connection
// issued or an attempt the journal inventory bound, never on the connection
// identity alone.
func TestPolicyGrantMatchesOnlyIssuedOrInventoriedAttempts(t *testing.T) {
	c := &conn{hostID: "h", bootIncarnation: "boot", connectionIncarnation: "conn"}
	offer := &hostcfg.PolicyOffer{AttemptID: "issued", HostID: "h", BootIncarnation: "boot", ConnectionIncarnation: "conn",
		Group: "gop", Revision: "3", ContentSHA256: "d", Scope: "next_session"}
	c.policyIssued = map[string]*hostcfg.PolicyOffer{"issued": offer}
	state := ConfigPolicyStateMsg{AttemptID: "issued", HostID: "h", Group: "gop", Revision: "3", ContentSHA256: "d",
		Scope: "next_session", GrantBootIncarnation: "boot", GrantConnectionIncarnation: "conn"}
	if !policyGrantMatches(c, state) {
		t.Fatal("issued grant refused")
	}
	forged := state
	forged.AttemptID = "never-issued"
	if policyGrantMatches(c, forged) {
		t.Fatal("unissued attempt id accepted on connection identity alone")
	}
	other := state
	other.Group = "slices"
	if policyGrantMatches(c, other) {
		t.Fatal("issued id accepted for a different group")
	}
	c.policyOutstanding = map[string]ConfigPolicyStateMsg{"historical": {AttemptID: "historical", HostID: "h", Group: "gop",
		Revision: "2", ContentSHA256: "old", Scope: "next_session", GrantBootIncarnation: "old-boot", GrantConnectionIncarnation: "old-conn"}}
	historical := c.policyOutstanding["historical"]
	if !policyGrantMatches(c, historical) {
		t.Fatal("inventoried historical attempt refused")
	}
}

// #498: a fabricated attempt id in an accepted-type phase must not enter
// policyOutstanding; the issued grant still applies.
func TestUnissuedNextSessionAttemptIsRefused(t *testing.T) {
	pool := testPool(t)
	store := hostcfg.NewStore(pool)
	ctx := context.Background()
	h := NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil, store, nil)
	t.Cleanup(h.Close)
	agent, _ := connectTypedAgent(t, pool, h, hostcfg.NextSessionPolicyGroups(), nil)
	if _, err := store.SavePolicy(ctx, agent.hostID, "0", map[string]hostcfg.PolicyChoice{
		"gop": {Source: "explicit", Value: policyExplicit["gop"]},
	}, nil); err != nil {
		t.Fatal(err)
	}
	offer := agent.collectOffers(t, pool, 1)["gop"]
	state := func(attemptID, phase string) map[string]any {
		msg := map[string]any{
			"type": "config_policy_state", "attempt_id": attemptID, "host_id": agent.hostID,
			"group": "gop", "revision": offer["revision"], "content_sha256": offer["content_sha256"], "scope": "next_session",
			"grant_boot_incarnation": offer["boot_incarnation"], "grant_connection_incarnation": offer["connection_incarnation"],
			"journal_sequence": "1", "phase": phase, "active_scope": nil, "evidence": nil, "error": nil,
		}
		if phase == "applied" {
			msg["active_scope"] = "next_session"
			msg["evidence"] = map[string]any{
				"revision": offer["revision"], "content_sha256": offer["content_sha256"], "resolved_settings": offer["resolved_settings"],
				"agent_process_id": "4242", "observed_at": time.Now().UTC().Format(time.RFC3339), "evidence_ids": []string{},
			}
		}
		return msg
	}
	for i := 0; i < 3; i++ {
		agent.send(t, state(newPolicyUUID(), "accepted"))
	}
	agent.send(t, state(offer["attempt_id"].(string), "applied"))
	waitUntil(t, "issued grant applied", func() bool {
		view, err := store.GetPolicy(ctx, agent.hostID)
		return err == nil && view.Groups["gop"].Status == "applied"
	})
	c, ok := h.registry.get(agent.hostID)
	if !ok {
		t.Fatal("agent connection gone")
	}
	// Set before the applied report is read, so it is settled by now.
	if c.policyAttemptOutstanding.Load() {
		t.Fatal("fabricated attempt ids are outstanding on the connection")
	}
}

// #498: an admin policy save issues its offer through the connection, so the
// agent's answer to that offer is accepted.
func TestHTTPPolicySaveOfferIsAccepted(t *testing.T) {
	pool := testPool(t)
	store := hostcfg.NewStore(pool)
	ctx := context.Background()
	h := NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil, store, nil)
	t.Cleanup(h.Close)
	agent, _ := connectTypedAgent(t, pool, h, hostcfg.NextSessionPolicyGroups(), nil)
	// The handshake's last capacity runs an offer pass on the read loop; a bare
	// capacity behind it runs none, so once it lands only the PATCH can offer.
	agent.send(t, map[string]any{"type": "capacity", "host": map[string]any{"cpu_cores": 8, "mem_mb": 32001},
		"gpus": []map[string]any{{"index": 0, "vendor": "nvidia", "model": "test", "vram_mb_total": 16384, "encode_slots_total": 2, "codecs": []string{"h264"}}}})
	waitUntil(t, "the read loop to go idle", func() bool {
		var mem int
		return pool.QueryRow(ctx, `SELECT mem_mb FROM hosts WHERE id=$1::uuid`, agent.hostID).Scan(&mem) == nil && mem == 32001
	})
	mux := http.NewServeMux()
	pass := func(next http.Handler) http.Handler { return next }
	hostcfg.NewHandler(store, h.registry, nil).Register(mux, pass, pass)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPatch, "/v1/admin/hosts/"+agent.hostID+"/policy",
		strings.NewReader(`{"expected_revision":"0","changes":{"gop":{"source":"explicit","value":1}}}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("policy PATCH = %d %s", rr.Code, rr.Body.String())
	}
	offer := agent.readUntil(t, "config_policy_offer")
	agent.send(t, map[string]any{
		"type": "config_policy_state", "attempt_id": offer["attempt_id"], "host_id": agent.hostID,
		"group": offer["group"], "revision": offer["revision"], "content_sha256": offer["content_sha256"], "scope": "next_session",
		"grant_boot_incarnation": offer["boot_incarnation"], "grant_connection_incarnation": offer["connection_incarnation"],
		"journal_sequence": "1", "phase": "applied", "active_scope": "next_session", "error": nil,
		"evidence": map[string]any{
			"revision": offer["revision"], "content_sha256": offer["content_sha256"], "resolved_settings": offer["resolved_settings"],
			"agent_process_id": "4242", "observed_at": time.Now().UTC().Format(time.RFC3339), "evidence_ids": []string{},
		},
	})
	waitUntil(t, "the PATCH's offer applied", func() bool {
		view, err := store.GetPolicy(ctx, agent.hostID)
		return err == nil && view.Groups["gop"].Status == "applied"
	})
}
