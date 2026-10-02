package crud

// Amendment 17 (RH-07 #396): the host's container engine, its version and its engine
// mode, end to end through the agent WebSocket `register` (agent-api.md §register
// "Engine facts") and the admin host body (control-api.md §Hosts; openapi.yaml Host).
// Real Postgres: TEST_DATABASE_URL-gated.

import (
	"strings"
	"testing"
)

func engineFields(engine, version, mode any) map[string]any {
	f := map[string]any{}
	for k, v := range map[string]any{"engine": engine, "engine_version": version, "engine_mode": mode} {
		if v != nil {
			f[k] = v
		}
	}
	return f
}

// Any install mode reports them; they are stored as sent and served on the host body.
func TestRegisterStoresAndServesTheEngineFacts(t *testing.T) {
	pool := testDB(t)
	srv, adminTok, _, _ := overrideServer(t, pool)
	url := agentEndpoint(t, pool)

	reply := agentRegister(t, url, "podman-host", enroll(), engineFields("podman", "5.8.4", "rootless"))
	hostID, _ := reply["host_id"].(string)
	wantBody(t, hostBody(t, srv, adminTok, hostID), map[string]string{
		"engine":         `"podman"`,
		"engine_version": `"5.8.4"`,
		"engine_mode":    `"rootless"`,
	})
}

// A host whose agent predates the amendment serializes all three as null, never omits them.
func TestAHostThatNeverReportedAnEngineServesNulls(t *testing.T) {
	pool := testDB(t)
	srv, adminTok, _, _ := overrideServer(t, pool)
	url := agentEndpoint(t, pool)

	reply := agentRegister(t, url, "old-agent", enroll(), nil)
	hostID, _ := reply["host_id"].(string)
	wantBody(t, hostBody(t, srv, adminTok, hostID), map[string]string{
		"engine": `null`, "engine_version": `null`, "engine_mode": `null`,
	})
}

// The contract's shapes: engine is an open lowercase token (a later engine needs no
// amendment), engine_version 1-64 printable ASCII, engine_mode rootful|rootless. Anything
// else is stored NULL and never refuses the registration.
func TestEngineFactsOutsideTheContractAreStoredNull(t *testing.T) {
	pool := testDB(t)
	srv, adminTok, _, _ := overrideServer(t, pool)
	url := agentEndpoint(t, pool)

	cases := []struct {
		name            string
		engine, version any
		mode            any
		want            map[string]string
	}{
		{"open-token", "lxd", "6.0", "rootful", map[string]string{"engine": `"lxd"`, "engine_version": `"6.0"`, "engine_mode": `"rootful"`}},
		{"uppercase", "Docker", "29.7.2", "rootful", map[string]string{"engine": `null`, "engine_version": `"29.7.2"`}},
		{"too-long", strings.Repeat("a", 33), "1", "rootful", map[string]string{"engine": `null`}},
		{"leading-digit", "9engine", "1", "rootful", map[string]string{"engine": `null`}},
		{"spaced-version", "docker", "29 beta", "rootful", map[string]string{"engine": `"docker"`, "engine_version": `null`}},
		{"long-version", "docker", strings.Repeat("9", 65), "rootful", map[string]string{"engine_version": `null`}},
		{"third-mode", "podman", "5.8.4", "rootlessish", map[string]string{"engine": `"podman"`, "engine_mode": `null`}},
		{"not-a-string", 7, "5.8.4", "rootless", map[string]string{"engine": `null`, "engine_mode": `"rootless"`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reply := agentRegister(t, url, "engine-"+c.name, enroll(), engineFields(c.engine, c.version, c.mode))
			hostID, _ := reply["host_id"].(string)
			if hostID == "" {
				t.Fatalf("registration refused over an engine fact: %v", reply)
			}
			wantBody(t, hostBody(t, srv, adminTok, hostID), c.want)
		})
	}
}

// Wholesale on every register: an agent moved to another engine reads as that engine, and
// one that no longer says reads unknown rather than keeping the engine it had.
func TestReRegisterReplacesTheEngineFactsWholesale(t *testing.T) {
	pool := testDB(t)
	srv, adminTok, _, _ := overrideServer(t, pool)
	url := agentEndpoint(t, pool)

	first := agentRegister(t, url, "engine-rereg", enroll(), engineFields("docker", "29.7.2", "rootful"))
	hostID, _ := first["host_id"].(string)
	secret, _ := first["node_secret"].(string)
	reconnect := map[string]string{"node_secret": secret}

	agentRegister(t, url, "engine-rereg", reconnect, engineFields("podman", "5.8.4", "rootless"))
	wantBody(t, hostBody(t, srv, adminTok, hostID), map[string]string{
		"engine": `"podman"`, "engine_version": `"5.8.4"`, "engine_mode": `"rootless"`,
	})

	agentRegister(t, url, "engine-rereg", reconnect, engineFields("podman", nil, nil))
	wantBody(t, hostBody(t, srv, adminTok, hostID), map[string]string{
		"engine": `"podman"`, "engine_version": `null`, "engine_mode": `null`,
	})

	agentRegister(t, url, "engine-rereg", reconnect, nil)
	wantBody(t, hostBody(t, srv, adminTok, hostID), map[string]string{
		"engine": `null`, "engine_version": `null`, "engine_mode": `null`,
	})
}
