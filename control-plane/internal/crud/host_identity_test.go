package crud

import (
	"encoding/json"
	"testing"
	"time"
)

// openapi.yaml's Host lists all four identity fields as REQUIRED, so they are
// serialized even when null — a client must be able to tell "unknown" from
// "this control plane predates the amendment".
func TestHostRespAlwaysSerializesTheIdentityFields(t *testing.T) {
	raw, err := json.Marshal(hostToResp(Host{ID: "h1", NodeName: "n1", Status: "online"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"source_commit", "built_at", "install_mode", "updater_present"} {
		v, ok := got[key]
		if !ok {
			t.Errorf("%s is absent from the host body; it is required and null-until-reported", key)
			continue
		}
		if string(v) != "null" {
			t.Errorf("%s = %s on an unreported host, want null", key, v)
		}
	}
}

func TestHostRespServesBuiltAtAsRFC3339UTC(t *testing.T) {
	// A non-UTC zone in, UTC out: a client rendering a build age should not have
	// to reason about the control plane's local zone.
	built := time.Date(2026, 9, 4, 22, 0, 0, 0, time.FixedZone("CEST", 2*60*60))
	mode := "source"
	no := false
	commit := "1f0c1e0"

	resp := hostToResp(Host{
		ID: "h1", NodeName: "n1", Status: "online",
		SourceCommit: &commit, BuiltAt: &built, InstallMode: &mode, UpdaterPresent: &no,
	})

	if resp.BuiltAt == nil || *resp.BuiltAt != "2026-09-04T20:00:00Z" {
		t.Errorf("built_at = %v, want 2026-09-04T20:00:00Z", resp.BuiltAt)
	}
	if resp.SourceCommit == nil || *resp.SourceCommit != commit {
		t.Errorf("source_commit = %v, want it passed through exactly as stored", resp.SourceCommit)
	}
	if resp.InstallMode == nil || *resp.InstallMode != "source" {
		t.Errorf("install_mode = %v", resp.InstallMode)
	}
	// false must not collapse to null on the way out — the difference is the
	// whole reason the column is nullable.
	if resp.UpdaterPresent == nil || *resp.UpdaterPresent {
		t.Errorf("updater_present = %v, want a non-nil false", resp.UpdaterPresent)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["updater_present"] != false {
		t.Errorf("updater_present on the wire = %v, want false", body["updater_present"])
	}
}
