package library

// #738: agent-supplied text is untrusted bytes, and Postgres refuses U+0000 and
// invalid UTF-8 in `text`. A manifest name (or a scan error message) carrying one
// used to fail the scan's transaction on this report — and again on every later
// scan of the same home, since the agent reports the same bytes back each time.
// The repair happens at the parse boundary of the scan report (the same repair
// #736 applies to Steam store texts), and these tests drive the real agent HTTP
// endpoints with dirty bytes to prove the scan succeeds and the stored text is
// clean. DB-backed only.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestScanReportCleansAgentReportedTitleNames posts a successful report whose
// names carry a NUL byte and an invalid UTF-8 byte: the scan must be accepted,
// the observation must store the CLEANED name, a name that cleans to "" must
// keep its existing empty-name behaviour (tile falls back to the appid), and
// nothing in the transaction may fail.
func TestScanReportCleansAgentReportedTitleNames(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)

	set := &fakeSettings{enabled: true, appDetailsEnabled: false}
	srv := newTestServer(t, f, set, NewAppDetails(false, quietLogger()))

	must(t, execT(context.Background(), pool, `INSERT INTO library_scans (user_id, app_id, host_id, state)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'pending')`, f.user, f.parent, f.host))

	resp := agentReq(t, http.MethodGet, srv.URL+"/v1/agent/library/scan-pending", f.nodeName, f.secret, nil)
	var pending struct {
		Scans []PendingScan `json:"scans"`
	}
	must(t, json.NewDecoder(resp.Body).Decode(&pending))
	resp.Body.Close()
	if len(pending.Scans) != 1 {
		t.Fatalf("claimed %d scans, want 1", len(pending.Scans))
	}

	resp = agentReq(t, http.MethodPost, srv.URL+"/v1/agent/library/scan-report", f.nodeName, f.secret,
		ScanReport{ScanID: pending.Scans[0].ScanID, OK: true, Entries: []ReportEntry{
			{ExternalID: "517710", Name: "Redout\x00 Enhanced Edition\xff"},
			{ExternalID: "3179810", Name: "\x00"},
		}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scan-report = %d, want 200: a NUL/invalid-UTF-8 name must not fail the scan", resp.StatusCode)
	}

	// The observation stores the clean name: the NUL is gone (the case #738 is
	// about), and the raw \xff the body carried was already normalised to U+FFFD
	// by the JSON decoder before the handler saw it — valid UTF-8 Postgres
	// accepts, so it survives. Reconcile's own TrimSpace is untouched.
	var obsName string
	must(t, pool.QueryRow(context.Background(),
		`SELECT name FROM library_observations
		  WHERE user_id = $1::uuid AND external_id = '517710'`, f.user).Scan(&obsName))
	if obsName != "Redout Enhanced Edition\uFFFD" {
		t.Fatalf("observation name = %q, want %q", obsName, "Redout Enhanced Edition\uFFFD")
	}

	// A name that is only NULs cleans to "": it keeps the existing empty-name
	// behaviour (an empty observation name, a tile that falls back to the appid).
	var emptyName string
	must(t, pool.QueryRow(context.Background(),
		`SELECT name FROM library_observations
		  WHERE user_id = $1::uuid AND external_id = '3179810'`, f.user).Scan(&emptyName))
	if emptyName != "" {
		t.Fatalf("observation name = %q, want empty after cleaning", emptyName)
	}

	// Both tiles still auto-publish; the tile name is the cleaned manifest name,
	// falling back to the appid where cleaning left nothing.
	if _, enabled, ok := f.tile(t, "517710"); !ok || !enabled {
		t.Fatalf("517710 tile ok=%v enabled=%v; a cleaned name publishes as before", ok, enabled)
	}
	var tileName, emptyTileName string
	id, _, _ := f.tile(t, "517710")
	must(t, pool.QueryRow(context.Background(),
		`SELECT name FROM apps WHERE id::text = $1`, id).Scan(&tileName))
	if tileName != "Redout Enhanced Edition\uFFFD" {
		t.Fatalf("tile name = %q, want %q", tileName, "Redout Enhanced Edition\uFFFD")
	}
	if emptyID, _, ok := f.tile(t, "3179810"); !ok {
		t.Fatal("3179810 produced no tile: a name emptied by cleaning must never fail the scan")
	} else {
		must(t, pool.QueryRow(context.Background(),
			`SELECT name FROM apps WHERE id::text = $1`, emptyID).Scan(&emptyTileName))
		if emptyTileName != "3179810" {
			t.Fatalf("tile name = %q, want the appid fallback after the name cleaned to empty", emptyTileName)
		}
	}
}

// TestScanFailureMessageIsCleanedToo covers the failed-report arm: MarkFailed
// stores the agent's error text in library_scans.error, so a NUL/invalid byte in
// it used to fail the one operation that is supposed to record a failure.
func TestScanFailureMessageIsCleanedToo(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)

	set := &fakeSettings{enabled: true, appDetailsEnabled: false}
	srv := newTestServer(t, f, set, NewAppDetails(false, quietLogger()))

	must(t, execT(context.Background(), pool, `INSERT INTO library_scans (user_id, app_id, host_id, state)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'pending')`, f.user, f.parent, f.host))

	resp := agentReq(t, http.MethodGet, srv.URL+"/v1/agent/library/scan-pending", f.nodeName, f.secret, nil)
	var pending struct {
		Scans []PendingScan `json:"scans"`
	}
	must(t, json.NewDecoder(resp.Body).Decode(&pending))
	resp.Body.Close()
	if len(pending.Scans) != 1 {
		t.Fatalf("claimed %d scans, want 1", len(pending.Scans))
	}

	resp = agentReq(t, http.MethodPost, srv.URL+"/v1/agent/library/scan-report", f.nodeName, f.secret,
		ScanReport{ScanID: pending.Scans[0].ScanID, OK: false, Error: "walk failed \x00bad\xff UTF8"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scan-report = %d, want 200: a dirty failure message must not fail MarkFailed", resp.StatusCode)
	}

	var state, stored string
	must(t, pool.QueryRow(context.Background(),
		`SELECT state, error FROM library_scans WHERE id::text = $1`,
		pending.Scans[0].ScanID).Scan(&state, &stored))
	if state != "failed" {
		t.Fatalf("scan state = %q, want failed", state)
	}
	// As above: the \xff arrived as U+FFFD (JSON decode), the NUL is stripped.
	if stored != "walk failed bad\uFFFD UTF8" {
		t.Fatalf("stored error = %q, want %q", stored, "walk failed bad\uFFFD UTF8")
	}
}
