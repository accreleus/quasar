package settings

// Amendment 61 (#1217): default_transport on GET/PATCH /v1/admin/settings. DB-backed: run via
// `make test-db`.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDefaultTransportReadPatchAndAudit(t *testing.T) {
	pool := testDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE instance_settings SET default_transport = 'webrtc'`)
	})

	// No singleton row yet: the default is webrtc.
	st, err := NewStore(pool).Get(context.Background())
	must(t, err)
	if st.DefaultTransport != "webrtc" {
		t.Fatalf("unseeded default_transport = %q, want webrtc", st.DefaultTransport)
	}

	patch, _ := auditedPatch(t, pool)
	if code := patch(`{"default_transport":"webtransport"}`); code != http.StatusOK {
		t.Fatalf("PATCH webtransport: %d", code)
	}
	st, err = NewStore(pool).Get(context.Background())
	must(t, err)
	if st.DefaultTransport != "webtransport" {
		t.Errorf("after PATCH = %q, want webtransport", st.DefaultTransport)
	}

	// Absent leaves it; any other value is a 400 that writes nothing and audits nothing.
	if code := patch(`{"registration_mode":"open"}`); code != http.StatusOK {
		t.Fatalf("unrelated PATCH: %d", code)
	}
	for _, bad := range []string{`{"default_transport":"default"}`, `{"default_transport":""}`, `{"default_transport":"udp"}`, `{"default_transport":1}`} {
		if code := patch(bad); code != http.StatusBadRequest {
			t.Errorf("PATCH %s: %d, want 400", bad, code)
		}
	}
	st, err = NewStore(pool).Get(context.Background())
	must(t, err)
	if st.DefaultTransport != "webtransport" {
		t.Errorf("after absent and invalid PATCHes = %q, want webtransport", st.DefaultTransport)
	}

	rows := settingsAuditRows(t, pool)
	if len(rows) != 2 {
		t.Fatalf("recorded %d audit rows, want 2 (the rejected PATCHes are not audited)", len(rows))
	}
	var details struct {
		Keys []string `json:"keys"`
	}
	must(t, json.Unmarshal([]byte(rows[0].Details), &details))
	if len(details.Keys) != 1 || details.Keys[0] != "default_transport" {
		t.Errorf("audit keys = %v, want [default_transport]", details.Keys)
	}
	if strings.Contains(rows[0].Details, "webtransport") {
		t.Errorf("audit details leaked the value: %s", rows[0].Details)
	}
}
