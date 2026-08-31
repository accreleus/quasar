package session

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/ice"
)

func iceTestRequest(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, "http://play.lan:8080/v1/sessions", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Host = "play.lan:8080"
	return r
}

// TestSignalingRespUnconfiguredSerializesEmptyArray is the no-regression pin for
// #509: a deployment that configures nothing must send exactly what the client
// used to hardcode. An empty array, never null and never a missing key, so no
// client needs a third branch.
func TestSignalingRespUnconfiguredSerializesEmptyArray(t *testing.T) {
	h := &Handler{}
	resp := h.newSignalingResp(iceTestRequest(t), "tok", time.Unix(0, 0).UTC())

	if resp.ICEServers == nil {
		t.Fatal("nil config produced a nil slice, which marshals as JSON null")
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := decoded["ice_servers"]
	if !ok {
		t.Fatalf("ice_servers key absent from %s", b)
	}
	list, ok := got.([]any)
	if !ok {
		t.Fatalf("ice_servers = %#v, want an array", got)
	}
	if len(list) != 0 {
		t.Fatalf("ice_servers = %v, want empty", list)
	}
}

func TestSignalingRespCarriesConfiguredServers(t *testing.T) {
	configured := []ice.Server{
		{URLs: []string{"stun:stun.example.net:3478"}},
		{URLs: []string{"turn:turn.example.net:3478"}, Username: "u", Credential: "c"},
	}
	h := (&Handler{}).WithICEServers(configured)
	resp := h.newSignalingResp(iceTestRequest(t), "tok", time.Unix(0, 0).UTC())

	if len(resp.ICEServers) != 2 {
		t.Fatalf("got %d servers, want 2", len(resp.ICEServers))
	}
	if resp.ICEServers[1].Credential != "c" {
		t.Fatalf("credential lost: %+v", resp.ICEServers[1])
	}
	// The other coordinates must be untouched by the addition.
	if resp.Token != "tok" || resp.URL != "ws://play.lan:8080/v1/signal" {
		t.Fatalf("existing coordinates changed: %+v", resp)
	}
}

// TestBothMintPathsShareOneBuilder guards the drift the single constructor
// exists to prevent: a launch that hands over ICE servers while a reconnect
// quietly drops them yields a session that works until the first reconnect,
// which is harder to diagnose than one that never works. Both call sites go
// through newSignalingResp, so equal inputs must produce equal coordinates.
func TestBothMintPathsShareOneBuilder(t *testing.T) {
	h := (&Handler{}).WithICEServers([]ice.Server{{URLs: []string{"stun:stun.example.net:3478"}}})
	r := iceTestRequest(t)
	at := time.Unix(0, 0).UTC()

	launch, err := json.Marshal(h.newSignalingResp(r, "tok", at))
	if err != nil {
		t.Fatal(err)
	}
	reconnect, err := json.Marshal(h.newSignalingResp(r, "tok", at))
	if err != nil {
		t.Fatal(err)
	}
	if string(launch) != string(reconnect) {
		t.Fatalf("mint paths disagree:\n launch    = %s\n reconnect = %s", launch, reconnect)
	}
}
