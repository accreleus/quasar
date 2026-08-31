package session

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// A client stamp that is not plausibly Unix-epoch milliseconds is dropped at
// ingest rather than stored where no read window reaches it. The POST still 202s
// — telemetry never fails a client — and the drop is visible in the bundle's
// ingest counters, which is the whole point: before this, the row existed, the
// data did not, and nothing anywhere said so.
func TestIngestDropsImplausibleTimestampsAndCountsThem(t *testing.T) {
	sid, ownerTok, adminTok, store, srvURL := adminBundleSession(t)

	nowMs := time.Now().UnixMilli()
	body := map[string]any{"samples": []map[string]any{
		{"ts_unix_ms": nowMs / 1000, "metrics": map[string]any{"fps": 60}}, // seconds
		{"ts_unix_ms": nowMs, "metrics": map[string]any{"fps": 60}},        // good
	}}
	resp := doJSON(t, http.MethodPost, srvURL+"/v1/sessions/"+sid+"/stats", ownerTok, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stats POST: got %d want 202 — a rejected sample must never fail the batch", resp.StatusCode)
	}
	resp.Body.Close()

	samples, _, err := store.Telemetry().Recent(context.Background(), sid, 100, nil, "")
	if err != nil {
		t.Fatalf("read back samples: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("stored %d samples, want exactly 1 — the seconds-stamped sample must not be stored", len(samples))
	}
	if samples[0].TsUnixMs != nowMs {
		t.Errorf("stored ts = %d, want the plausible one (%d)", samples[0].TsUnixMs, nowMs)
	}

	resp = doJSON(t, http.MethodGet, srvURL+"/v1/admin/sessions/"+sid+"/diagnostic-bundle", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin bundle: got %d want 200", resp.StatusCode)
	}
	var out struct {
		Ingest *struct {
			RejectedTs           int64  `json:"rejected_ts"`
			LastRejectedTsUnixMs int64  `json:"last_rejected_ts_unix_ms"`
			LastRejectedReason   string `json:"last_rejected_reason"`
		} `json:"ingest"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	resp.Body.Close()

	if out.Ingest == nil {
		t.Fatal("bundle carries no ingest object after a rejection")
	}
	if out.Ingest.RejectedTs != 1 {
		t.Errorf("rejected_ts = %d, want 1", out.Ingest.RejectedTs)
	}
	if out.Ingest.LastRejectedTsUnixMs != nowMs/1000 {
		t.Errorf("last_rejected_ts_unix_ms = %d, want the offending value %d verbatim",
			out.Ingest.LastRejectedTsUnixMs, nowMs/1000)
	}
	if out.Ingest.LastRejectedReason == "" {
		t.Error("last_rejected_reason is empty; the operator must be told which domain the value looked like")
	}
}

// Nothing rejected ⇒ no ingest object at all. A counter of zero is noise.
func TestBundleOmitsIngestWhenNothingWasRejected(t *testing.T) {
	sid, _, adminTok, _, srvURL := adminBundleSession(t)
	resp := doJSON(t, http.MethodGet, srvURL+"/v1/admin/sessions/"+sid+"/diagnostic-bundle", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin bundle: got %d want 200", resp.StatusCode)
	}
	var out map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	resp.Body.Close()
	if _, present := out["ingest"]; present {
		t.Error("bundle carries an ingest object on a clean session")
	}
}

func TestPlausibleTsNamesTheLikelyDomain(t *testing.T) {
	now := time.Now()
	nowMs := now.UnixMilli()
	cases := []struct {
		ts     int64
		ok     bool
		reason string
	}{
		{nowMs, true, ""},
		{nowMs - 60_000, true, ""},
		{nowMs / 1000, false, "looks like seconds, not milliseconds"},
		{nowMs * 1_000_000, false, "looks like nanoseconds, not milliseconds"},
		{nowMs * 1000, false, "looks like microseconds, not milliseconds"},
		{123_456, false, "looks like performance.now (ms since page load), not a Unix epoch stamp"},
		{0, false, "not a timestamp (zero or negative)"},
	}
	for _, c := range cases {
		ok, reason := telemetry.PlausibleTsUnixMs(c.ts, now)
		if ok != c.ok || reason != c.reason {
			t.Errorf("PlausibleTsUnixMs(%d) = (%v,%q), want (%v,%q)", c.ts, ok, reason, c.ok, c.reason)
		}
	}
}

// The WARN is bounded to one per session per minute: a client stuck posting
// seconds posts them forever, and the log must name the problem once rather than
// become the problem.
func TestIngestCountersLogAtMostOncePerMinute(t *testing.T) {
	c := newIngestCounters()
	base := time.Unix(1_700_000, 0)
	if !c.reject("s1", 1, "seconds", base) {
		t.Fatal("first rejection must log")
	}
	if c.reject("s1", 1, "seconds", base.Add(30*time.Second)) {
		t.Error("second rejection within the minute must not log")
	}
	if !c.reject("s1", 1, "seconds", base.Add(61*time.Second)) {
		t.Error("rejection after the minute must log again")
	}
	rep := c.report("s1")
	if rep == nil || rep.RejectedTs != 3 {
		t.Errorf("report = %+v, want 3 rejections counted regardless of logging", rep)
	}
	if c.report("never-seen") != nil {
		t.Error("a session with no rejections must report nothing")
	}
}
