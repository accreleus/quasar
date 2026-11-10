package session

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The display cache is keyed on the row's id, whatever case the PATCH path used (#1100).
func TestDisplayUpdateKeysCacheOnCanonicalID(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store, _, coord := newStreamDisplayServer(t, pool, true)
	ctx := context.Background()

	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "strmowner@test.local", "strmowner", "unrelated-pw-03")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	tok := loginTok(t, authSvc, "strmowner@test.local", "unrelated-pw-03")
	sid := session1080ForUser(t, store, currentSeed(t, pool), owner.ID)
	upper := strings.ToUpper(sid)
	if upper == sid {
		t.Fatalf("session id %s has no hex letters to recase", sid)
	}

	resp := doJSON(t, http.MethodPatch, srv.URL+"/v1/sessions/"+upper+"/display", tok,
		map[string]any{"stream_width": 1280, "stream_height": 720})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("PATCH stream: got %d want 202", resp.StatusCode)
	}
	resp.Body.Close()

	if _, ok := coord.display.get(sid); !ok {
		t.Errorf("no display state under the canonical id %s", sid)
	}
}
