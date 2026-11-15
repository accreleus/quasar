package agentws

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// capacity.compat_tools replaces the host's set wholesale, drops a tool outside the grammar,
// keeps the set when a report omits the field, and says when the set changed.
func TestCapacityCompatToolsReplacesTheHostsSet(t *testing.T) {
	pool := testPool(t)
	s := storeWithMintedTokens(pool, nil)
	host := seedHost(t, pool)
	ctx := context.Background()
	sha := strings.Repeat("a", 128)
	report := func(raw string) bool {
		t.Helper()
		changed, err := s.upsertHostCompatTools(ctx, host, json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	names := func() string {
		t.Helper()
		var got string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(string_agg(name, ',' ORDER BY name), '') FROM host_compat_tools
			WHERE host_id = $1::uuid`, host).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	one := `{"free_bytes": 1000, "tools": [
		{"name": "GE-Proton11-12-x86_64", "tag": "GE-Proton11-12", "sha512": "` + sha + `", "bytes": 7},
		{"name": "../evil", "tag": "GE-Proton11-12", "sha512": "` + sha + `", "bytes": 7},
		{"name": "GE-Proton9-1", "tag": "not-a-tag", "sha512": "` + sha + `", "bytes": 7}]}`
	if !report(one) || names() != "GE-Proton11-12-x86_64" {
		t.Errorf("first report: %q", names())
	}
	var free int64
	if err := pool.QueryRow(ctx, `SELECT compat_tools_free_bytes FROM hosts WHERE id = $1::uuid`, host).Scan(&free); err != nil || free != 1000 {
		t.Errorf("free_bytes = %d (%v)", free, err)
	}
	if report(one) {
		t.Error("the same set reported as a change")
	}
	if report(``) || report(`null`) || names() != "GE-Proton11-12-x86_64" {
		t.Error("a report without the field changed the set")
	}
	if !report(`{"free_bytes": 5, "tools": []}`) || names() != "" {
		t.Errorf("an empty set kept %q", names())
	}
}

// A host that reconnects without compat_tools_v1 (a downgraded agent, or the tools bind mount
// removed) keeps no installed set: a v1 connection's rows would otherwise keep placing launches.
func TestRegisterWithoutCompatToolsV1ForgetsTheHostsTools(t *testing.T) {
	pool := testPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(pool, log, NewRegistry(log), nil, nil, nil, nil)
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	register := func(v1 bool, auth map[string]string) RegisteredMsg {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if err := conn.WriteJSON(map[string]any{
			"type": "register", "node_name": "downgraded-host", "agent_version": "dev",
			"compat_tools_v1": v1, "auth": auth,
		}); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var reg RegisteredMsg
		if err := conn.ReadJSON(&reg); err != nil || reg.HostID == "" {
			t.Fatalf("register: %v %+v", err, reg)
		}
		return reg
	}
	rows := func(host string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM host_compat_tools WHERE host_id = $1::uuid`, host).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	seedTool := func(host string) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), `INSERT INTO host_compat_tools (host_id, name, tag, sha512, bytes)
			VALUES ($1::uuid, 'GE-Proton11-12-x86_64', 'GE-Proton11-12', $2, 7) ON CONFLICT DO NOTHING`,
			host, strings.Repeat("a", 128)); err != nil {
			t.Fatal(err)
		}
	}

	reg := register(true, map[string]string{"enrollment_token": testEnrollmentToken})
	reconnect := map[string]string{"node_secret": reg.NodeSecret}
	seedTool(reg.HostID)
	register(true, reconnect)
	time.Sleep(500 * time.Millisecond)
	if n := rows(reg.HostID); n != 1 {
		t.Fatalf("a v1 reconnect left %d rows, want 1", n)
	}

	register(false, reconnect)
	deadline := time.Now().Add(5 * time.Second)
	for rows(reg.HostID) != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := rows(reg.HostID); n != 0 {
		t.Fatalf("a reconnect without compat_tools_v1 left %d rows, want 0", n)
	}
}
