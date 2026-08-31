package ice

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseUnsetIsEmpty pins the default that makes #509 a capability rather
// than a behaviour change: no configuration means no ICE servers, which is what
// the client hardcoded before.
func TestParseUnsetIsEmpty(t *testing.T) {
	for _, raw := range []string{"", "   ", "\n", "[]", " [ ] "} {
		got, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): unexpected error: %v", raw, err)
		}
		if len(got) != 0 {
			t.Fatalf("Parse(%q) = %v, want empty", raw, got)
		}
	}
}

func TestParseAcceptsStunAndTurn(t *testing.T) {
	raw := `[
	  {"urls":["stun:stun.example.net:3478"]},
	  {"urls":["turn:turn.example.net:3478?transport=udp","turns:turn.example.net:5349"],
	   "username":"u","credential":"c"}
	]`
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d servers, want 2", len(got))
	}
	if got[0].Username != "" || got[0].Credential != "" {
		t.Errorf("stun entry carried credentials: %+v", got[0])
	}
	if got[1].Username != "u" || got[1].Credential != "c" {
		t.Errorf("turn entry lost credentials: %+v", got[1])
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"not an array":        `{"urls":["stun:a:3478"]}`,
		"no urls":             `[{"urls":[]}]`,
		"missing urls key":    `[{"username":"u","credential":"c"}]`,
		"non-ICE scheme":      `[{"urls":["https://stun.example.net"]}]`,
		"bare host":           `[{"urls":["stun.example.net:3478"]}]`,
		"turn without creds":  `[{"urls":["turn:t.example.net:3478"]}]`,
		"turn without cred":   `[{"urls":["turn:t.example.net:3478"],"username":"u"}]`,
		"stun with creds":     `[{"urls":["stun:s.example.net:3478"],"username":"u","credential":"c"}]`,
		"misspelled url key":  `[{"url":["stun:s.example.net:3478"]}]`,
		"misspelled cred key": `[{"urls":["turn:t:3478"],"username":"u","password":"c"}]`,
		"trailing garbage":    `[{"urls":["stun:s:3478"]}] oops`,
		"not json at all":     `stun:stun.example.net:3478`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(raw); err == nil {
				t.Fatalf("Parse(%q) accepted an invalid value", raw)
			} else if !strings.Contains(err.Error(), "QUASAR_ICE_SERVERS") {
				t.Fatalf("error does not name the knob the operator must fix: %v", err)
			}
		})
	}
}

func TestParseRejectsRunawayList(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < maxServers+1; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"urls":["stun:stun.example.net:3478"]}`)
	}
	b.WriteString("]")
	if _, err := Parse(b.String()); err == nil {
		t.Fatal("accepted a list above maxServers")
	}
}

// TestServerMarshalsAsRTCIceServer is the contract check: what the control plane
// writes must be a dictionary the browser's RTCPeerConnection constructor takes
// as-is, with no empty credential keys on a STUN entry.
func TestServerMarshalsAsRTCIceServer(t *testing.T) {
	b, err := json.Marshal(Server{URLs: []string{"stun:stun.example.net:3478"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(b), `{"urls":["stun:stun.example.net:3478"]}`; got != want {
		t.Fatalf("stun entry = %s, want %s", got, want)
	}
	b, err = json.Marshal(Server{URLs: []string{"turn:t.example.net:3478"}, Username: "u", Credential: "c"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(b), `{"urls":["turn:t.example.net:3478"],"username":"u","credential":"c"}`; got != want {
		t.Fatalf("turn entry = %s, want %s", got, want)
	}
}

func TestRedactHidesCredentialsOnly(t *testing.T) {
	in := []Server{
		{URLs: []string{"stun:s.example.net:3478"}},
		{URLs: []string{"turn:t.example.net:3478"}, Username: "u", Credential: "secret"},
	}
	out := Redact(in)
	if out[1].Credential == "secret" {
		t.Fatal("Redact leaked the credential")
	}
	if out[1].Username != "u" || out[1].URLs[0] != "turn:t.example.net:3478" {
		t.Fatalf("Redact dropped identifying detail: %+v", out[1])
	}
	if in[1].Credential != "secret" {
		t.Fatal("Redact mutated its input")
	}
	if Redact(nil) != nil {
		t.Fatal("Redact(nil) should stay nil")
	}
}
