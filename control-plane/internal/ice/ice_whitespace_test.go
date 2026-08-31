package ice

import "testing"

// A URL with surrounding whitespace used to pass validation (which trimmed a
// loop copy) and then reach the browser untrimmed, where the
// RTCPeerConnection constructor throws SyntaxError. Parse must normalize what
// it stores, not just what it checks.
func TestParseTrimsURLsItStores(t *testing.T) {
	servers, err := Parse(`[{"urls":[" turn:relay.example.net:3478 ","\tstuns:stun.example.net:5349"],"username":"u","credential":"c"}]`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(servers))
	}
	want := []string{"turn:relay.example.net:3478", "stuns:stun.example.net:5349"}
	for i, w := range want {
		if got := servers[i%1].URLs[i]; got != w {
			t.Errorf("URL %d = %q, want %q (stored value must be trimmed)", i, got, w)
		}
	}
}

// Whitespace must not let an otherwise-invalid scheme through either.
func TestParseStillRejectsBadSchemeWithWhitespace(t *testing.T) {
	if _, err := Parse(`[{"urls":["  https://relay.example.net  "]}]`); err == nil {
		t.Fatal("Parse accepted an https:// URL padded with whitespace, want an error")
	}
}
