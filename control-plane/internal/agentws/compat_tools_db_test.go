package agentws

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
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
