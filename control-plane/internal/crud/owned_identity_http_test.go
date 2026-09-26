package crud

// Amendment 14 (#353, RH06-05 #357): an OWNED host's recovery-actor and seed
// identity, end to end through the two interfaces that carry it — the agent
// WebSocket `register` (agent-api.md §register "Owned installs") and the admin
// host body (control-api.md §"Owned hosts on the host body and the release
// view"; openapi.yaml Host). Real Postgres: TEST_DATABASE_URL-gated.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

const (
	ownedAgentCommit = "1f0c1e0e0c5a9d1b7a2f3e4d5c6b7a8901234567"
	ownedActorCommit = "abcdef0123456789abcdef0123456789abcdef01"
)

// agentEndpoint serves the real agent WebSocket handler over the same pool.
func agentEndpoint(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	// Every enrollment redeems a minted token (the static ENROLLMENT_TOKEN is
	// retired); enroll() presents this one.
	sum := sha256.Sum256([]byte("test-token"))
	if _, err := pool.Exec(context.Background(), `INSERT INTO host_enrollments (token_hash, created_by, node_name, max_uses, expires_at, note)
		VALUES ($1, NULL, NULL, 1000000, NULL, 'test fixture')
		ON CONFLICT (token_hash) DO UPDATE SET used_count = 0, revoked_at = NULL, expires_at = NULL,
		    node_name = NULL, max_uses = 1000000`, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("seed the test enrollment token: %v", err)
	}
	h := agentws.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil, nil, nil, nil, nil)
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// agentRegister dials, sends one `register` carrying `fields` beside the
// required keys, and returns the `registered` reply. auth is either an
// enrollment token or a node_secret. The connection is closed on return: the
// identity is written before `registered` is sent, so the host body already
// reflects it.
func agentRegister(t *testing.T, url, nodeName string, auth map[string]string, fields map[string]any) map[string]any {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	msg := map[string]any{}
	for k, v := range fields {
		msg[k] = v
	}
	msg["type"] = "register"
	msg["node_name"] = nodeName
	msg["agent_version"] = "0.4.0-dev"
	msg["auth"] = auth
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("write register: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var reply map[string]any
	if err := conn.ReadJSON(&reply); err != nil {
		t.Fatalf("read registered: %v", err)
	}
	if reply["type"] != "registered" {
		t.Fatalf("register reply = %v, want registered", reply)
	}
	return reply
}

func enroll() map[string]string { return map[string]string{"enrollment_token": "test-token"} }

// ownedFields is the amendment-14 example register, verbatim in shape.
func ownedFields() map[string]any {
	return map[string]any{
		"source_commit":                ownedAgentCommit,
		"built_at":                     "2026-09-24T12:00:00Z",
		"install_mode":                 "owned",
		"updater_present":              true,
		"recovery_actor_version":       "0.4.0",
		"recovery_actor_source_commit": ownedActorCommit,
		"seed_version":                 "0.4.0",
	}
}

// hostBody fetches GET /v1/hosts/{id} and the same host's entry in GET
// /v1/hosts, asserting the two agree — they are one serialization.
func hostBody(t *testing.T, srv *httptest.Server, adminTok, hostID string) map[string]json.RawMessage {
	t.Helper()
	resp, body := do(t, http.MethodGet, srv.URL+"/v1/hosts/"+hostID, adminTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/hosts/%s: %d %s", hostID, resp.StatusCode, body)
	}
	// The detail route wraps the host body as {"host": Host}.
	var wrapped struct {
		Host map[string]json.RawMessage `json:"host"`
	}
	if err := json.Unmarshal(body, &wrapped); err != nil || wrapped.Host == nil {
		t.Fatalf("decode host %s: %v", body, err)
	}
	one := wrapped.Host

	resp, body = do(t, http.MethodGet, srv.URL+"/v1/hosts", adminTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/hosts: %d %s", resp.StatusCode, body)
	}
	var list struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode host list: %v", err)
	}
	var fromList map[string]json.RawMessage
	for _, item := range list.Items {
		if string(item["id"]) == `"`+hostID+`"` {
			fromList = item
		}
	}
	if fromList == nil {
		t.Fatalf("host %s missing from GET /v1/hosts: %s", hostID, body)
	}
	for _, k := range identityKeys {
		if string(fromList[k]) != string(one[k]) {
			t.Errorf("%s: list says %s, detail says %s", k, fromList[k], one[k])
		}
	}
	return one
}

var identityKeys = []string{
	"source_commit", "built_at", "install_mode", "updater_present",
	"recovery_actor_version", "recovery_actor_source_commit", "seed_version",
}

// wantBody asserts each key is PRESENT with exactly this JSON value ("null"
// included: a server implementing the amendment always serializes all three).
func wantBody(t *testing.T, body map[string]json.RawMessage, want map[string]string) {
	t.Helper()
	for k, v := range want {
		got, ok := body[k]
		if !ok {
			t.Errorf("%s is absent from the host body; it is always serialized", k)
			continue
		}
		if string(got) != v {
			t.Errorf("%s = %s, want %s", k, got, v)
		}
	}
}

func TestOwnedRegisterStoresAndServesTheRecoveryActorIdentity(t *testing.T) {
	pool := testDB(t)
	srv, adminTok, _, _ := overrideServer(t, pool)
	url := agentEndpoint(t, pool)

	reply := agentRegister(t, url, "owned-host", enroll(), ownedFields())
	hostID, _ := reply["host_id"].(string)

	wantBody(t, hostBody(t, srv, adminTok, hostID), map[string]string{
		"source_commit":                `"` + ownedAgentCommit + `"`,
		"built_at":                     `"2026-09-24T12:00:00Z"`,
		"install_mode":                 `"owned"`,
		"updater_present":              `true`,
		"recovery_actor_version":       `"0.4.0"`,
		"recovery_actor_source_commit": `"` + ownedActorCommit + `"`,
		"seed_version":                 `"0.4.0"`,
	})
}

// "anything that is not MAJOR.MINOR.PATCH[-prerelease] is treated as absent"
// — and, deliberately unlike agent_version, stored NULL rather than as sent.
// The registration itself is never refused over it.
func TestOwnedRegisterStoresAnUnorderableActorVersionAsNull(t *testing.T) {
	pool := testDB(t)
	srv, adminTok, _, _ := overrideServer(t, pool)
	url := agentEndpoint(t, pool)

	for _, bad := range []string{"v0.4.0", "0.4", "dev", "0.4.0+build.7", "01.4.0", " 0.4.0", ""} {
		t.Run(bad, func(t *testing.T) {
			f := ownedFields()
			f["recovery_actor_version"] = bad
			reply := agentRegister(t, url, "owned-bad-"+strings.TrimSpace(bad), enroll(), f)
			hostID, _ := reply["host_id"].(string)
			wantBody(t, hostBody(t, srv, adminTok, hostID), map[string]string{
				"recovery_actor_version": `null`,
				// The rest of the message is unaffected by one unorderable field.
				"install_mode":                 `"owned"`,
				"recovery_actor_source_commit": `"` + ownedActorCommit + `"`,
				"seed_version":                 `"0.4.0"`,
			})
		})
	}

	// A prerelease is a version: it orders, so it is kept exactly as sent.
	f := ownedFields()
	f["recovery_actor_version"] = "0.5.0-rc.1"
	reply := agentRegister(t, url, "owned-prerelease", enroll(), f)
	hostID, _ := reply["host_id"].(string)
	wantBody(t, hostBody(t, srv, adminTok, hostID), map[string]string{
		"recovery_actor_version": `"0.5.0-rc.1"`,
	})
}

// recovery_actor_source_commit follows exactly the source_commit rule: 7-40
// lowercase hex stored as sent, anything else absent.
func TestOwnedRegisterAppliesTheCommitRuleToTheActorCommit(t *testing.T) {
	pool := testDB(t)
	srv, adminTok, _, _ := overrideServer(t, pool)
	url := agentEndpoint(t, pool)

	cases := map[string]string{
		"abcdef0":         `"abcdef0"`, // short is a real, less specific identity
		"ABCDEF0123":      `null`,      // not lowercase
		"abc":             `null`,      // too short
		"not-a-commit-at": `null`,
	}
	i := 0
	for sent, want := range cases {
		i++
		f := ownedFields()
		f["recovery_actor_source_commit"] = sent
		reply := agentRegister(t, url, "owned-commit-"+string(rune('a'+i)), enroll(), f)
		hostID, _ := reply["host_id"].(string)
		wantBody(t, hostBody(t, srv, adminTok, hostID), map[string]string{
			"recovery_actor_source_commit": want,
		})
	}
}

// Wholesale on every register: a re-register replaces the three, and one that
// omits a field stores it NULL ("absent is stored NULL") rather than keeping
// the old value.
func TestOwnedReRegisterReplacesTheActorIdentityWholesale(t *testing.T) {
	pool := testDB(t)
	srv, adminTok, _, _ := overrideServer(t, pool)
	url := agentEndpoint(t, pool)

	first := agentRegister(t, url, "owned-rereg", enroll(), ownedFields())
	hostID, _ := first["host_id"].(string)
	secret, _ := first["node_secret"].(string)
	if secret == "" {
		t.Fatalf("enrollment returned no node_secret: %v", first)
	}
	reconnect := map[string]string{"node_secret": secret}

	// The actor was replaced by a newer release.
	next := ownedFields()
	next["recovery_actor_version"] = "0.5.0"
	next["recovery_actor_source_commit"] = "0123456789abcdef0123456789abcdef01234567"
	next["seed_version"] = "0.4.1"
	agentRegister(t, url, "owned-rereg", reconnect, next)
	wantBody(t, hostBody(t, srv, adminTok, hostID), map[string]string{
		"recovery_actor_version":       `"0.5.0"`,
		"recovery_actor_source_commit": `"0123456789abcdef0123456789abcdef01234567"`,
		"seed_version":                 `"0.4.1"`,
	})

	// The actor saw no seed this time, and could not report its commit.
	partial := ownedFields()
	delete(partial, "seed_version")
	delete(partial, "recovery_actor_source_commit")
	agentRegister(t, url, "owned-rereg", reconnect, partial)
	wantBody(t, hostBody(t, srv, adminTok, hostID), map[string]string{
		"install_mode":                 `"owned"`,
		"recovery_actor_version":       `"0.4.0"`,
		"recovery_actor_source_commit": `null`,
		"seed_version":                 `null`,
	})

	// Downgraded to an agent that predates amendment 14 AND amendment 1: every
	// identity field reads unknown, the actor's included.
	agentRegister(t, url, "owned-rereg", reconnect, nil)
	body := hostBody(t, srv, adminTok, hostID)
	want := map[string]string{}
	for _, k := range identityKeys {
		want[k] = `null`
	}
	wantBody(t, body, want)
}

// "The three are sent only with install_mode: owned; a control plane ignores
// them beside any other mode." A registry host that (wrongly) sends them reads
// exactly as a registry host that does not.
func TestActorIdentityBesideANonOwnedModeIsIgnored(t *testing.T) {
	pool := testDB(t)
	srv, adminTok, _, _ := overrideServer(t, pool)
	url := agentEndpoint(t, pool)

	for _, mode := range []any{"registry", "source", "kubernetes", nil} {
		f := ownedFields()
		if mode == nil {
			delete(f, "install_mode")
		} else {
			f["install_mode"] = mode
		}
		name := "ignored-mode-none"
		if mode != nil {
			name = "ignored-mode-" + mode.(string)
		}
		reply := agentRegister(t, url, name, enroll(), f)
		hostID, _ := reply["host_id"].(string)
		wantMode := `null`
		if mode == "registry" || mode == "source" {
			wantMode = `"` + mode.(string) + `"`
		}
		wantBody(t, hostBody(t, srv, adminTok, hostID), map[string]string{
			"install_mode":                 wantMode,
			"recovery_actor_version":       `null`,
			"recovery_actor_source_commit": `null`,
			"seed_version":                 `null`,
		})
	}
}

// Regression: an amendment-1 registry agent and a pre-amendment agent register
// exactly as before, and the host body carries the three new keys as null.
func TestNonOwnedAgentsRegisterExactlyAsBefore(t *testing.T) {
	pool := testDB(t)
	srv, adminTok, _, _ := overrideServer(t, pool)
	url := agentEndpoint(t, pool)

	reg := agentRegister(t, url, "registry-host", enroll(), map[string]any{
		"source_commit":   ownedAgentCommit,
		"built_at":        "2026-09-04T12:00:00Z",
		"install_mode":    "registry",
		"updater_present": false,
	})
	regID, _ := reg["host_id"].(string)
	wantBody(t, hostBody(t, srv, adminTok, regID), map[string]string{
		"source_commit":                `"` + ownedAgentCommit + `"`,
		"built_at":                     `"2026-09-04T12:00:00Z"`,
		"install_mode":                 `"registry"`,
		"updater_present":              `false`,
		"recovery_actor_version":       `null`,
		"recovery_actor_source_commit": `null`,
		"seed_version":                 `null`,
	})

	old := agentRegister(t, url, "pre-amendment-host", enroll(), nil)
	oldID, _ := old["host_id"].(string)
	body := hostBody(t, srv, adminTok, oldID)
	want := map[string]string{"agent_version": `"0.4.0-dev"`}
	for _, k := range identityKeys {
		want[k] = `null`
	}
	wantBody(t, body, want)
}
