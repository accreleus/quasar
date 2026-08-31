package agentws

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func rawReadiness(t *testing.T, pool *pgxpool.Pool, hostID string) ([]byte, *string) {
	t.Helper()
	var raw []byte
	var at *string
	if err := pool.QueryRow(context.Background(),
		`SELECT readiness, readiness_reported_at::text FROM hosts WHERE id::text = $1`,
		hostID).Scan(&raw, &at); err != nil {
		t.Fatalf("query readiness: %v", err)
	}
	return raw, at
}

// First-run-experience S1: a readiness report persists whole — including the
// remediation text, which IS the product of the feature. A red row with no fix
// is the state this whole mechanism exists to replace.
func TestUpsertHostReadinessStoresTheWholeCheckSet(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)

	if raw, at := rawReadiness(t, pool, hostID); raw != nil || at != nil {
		t.Fatalf("fresh host: got readiness=%s reported_at=%v, want both NULL", raw, at)
	}

	// The payload carries a field this control plane has never heard of
	// ("severity"), exactly as a newer agent would send. Review finding #7: it
	// must reach storage — and therefore the admin UI — untouched.
	payload := json.RawMessage(`[
		{"id":"nvidia_egl_vendor_json","status":"fail",
		 "summary":"no NVIDIA EGL vendor config",
		 "remediation":"sudo dnf install -y nvidia-driver-libs egl-wayland",
		 "severity":"blocking","doc_url":"https://example.invalid/egl"},
		{"id":"uinput","status":"pass","summary":"/dev/uinput present and writable"}
	]`)
	if err := s.upsertHostReadiness(context.Background(), hostID, payload); err != nil {
		t.Fatalf("upsert readiness: %v", err)
	}

	raw, at := rawReadiness(t, pool, hostID)
	var got []map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal readiness: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("stored readiness: got %d checks, want 2 (%s)", len(got), raw)
	}
	if got[0]["id"] != "nvidia_egl_vendor_json" || got[0]["status"] != "fail" {
		t.Errorf("check 0 round-trip: got %+v", got[0])
	}
	if got[0]["remediation"] == "" || got[0]["remediation"] == nil {
		t.Error("remediation text was dropped — the fix instruction is the point of the check")
	}
	// The load-bearing assertion for finding #7. A typed decode/re-encode round
	// trip silently drops these, and the failure only shows up in production
	// when an upgraded agent meets a not-yet-upgraded control plane.
	if got[0]["severity"] != "blocking" {
		t.Errorf("unknown per-check field `severity` was dropped: %+v", got[0])
	}
	if got[0]["doc_url"] != "https://example.invalid/egl" {
		t.Errorf("unknown per-check field `doc_url` was dropped: %+v", got[0])
	}
	if at == nil {
		t.Error("readiness_reported_at must be stamped so the UI can show freshness")
	}
}

// A payload that could not possibly render is kept OUT of the hosts row — but
// validation must reject only that, never an unfamiliar vocabulary.
func TestUpsertHostReadinessRejectsOnlyMalformedPayloads(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	ctx := context.Background()

	for _, bad := range []string{`{"id":"not-an-array"}`, `[{"status":"fail"}]`, `"nope"`, `null`} {
		if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(bad)); err == nil {
			t.Errorf("malformed payload %s was accepted", bad)
		}
	}
	if raw, _ := rawReadiness(t, pool, hostID); raw != nil {
		t.Fatalf("a malformed payload reached the row: %s", raw)
	}

	// Explicit JSON `null` (review round 2, finding #2). It unmarshals into a
	// slice without error, leaving it nil — so without an explicit guard it
	// reads as a VALID report, overwrites a good stored check set with SQL NULL,
	// and advances readiness_reported_at to say the destruction is fresh. That
	// is strictly worse than the keep-if-absent case it looks like.
	if _, ok := ValidReadiness(json.RawMessage(`null`)); ok {
		t.Error("JSON null was accepted as a valid readiness report")
	}
	if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(`null`)); err == nil {
		t.Error("JSON null was written to the row")
	}

	// …while an explicit empty array remains valid: "I ran the checks, nothing
	// to say" is a real report and must still be storable. The two differ only
	// in the decoded slice being nil vs empty, which is exactly what the guard
	// keys on.
	if _, ok := ValidReadiness(json.RawMessage(`[]`)); !ok {
		t.Error("an explicit empty array must remain a valid report")
	}

	// An unfamiliar STATUS is not malformed — it is a newer agent.
	ok := json.RawMessage(`[{"id":"future_check","status":"warn","summary":"s"}]`)
	if err := s.upsertHostReadiness(ctx, hostID, ok); err != nil {
		t.Fatalf("an unrecognized status must be stored, not rejected: %v", err)
	}
	raw, _ := rawReadiness(t, pool, hostID)
	if !strings.Contains(string(raw), `"warn"`) {
		t.Fatalf("unrecognized status not stored verbatim: %s", raw)
	}
}

// Keep-if-absent, matching storage/effective_settings/codecs: a pre-amendment
// agent (nil slice) must not blank a set an amended agent already reported, and
// must not move the freshness stamp either — a stale set presented as fresh is
// worse than a stale set that says so.
func TestUpsertHostReadinessKeepsPriorValueWhenAbsent(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	ctx := context.Background()

	first := json.RawMessage(`[{"id":"uinput","status":"fail","summary":"missing","remediation":"modprobe uinput"}]`)
	if err := s.upsertHostReadiness(ctx, hostID, first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	_, firstAt := rawReadiness(t, pool, hostID)

	if err := s.upsertHostReadiness(ctx, hostID, nil); err != nil {
		t.Fatalf("absent re-report: %v", err)
	}
	raw, at := rawReadiness(t, pool, hostID)
	var got []ReadinessCheck
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal readiness: %v", err)
	}
	if len(got) != 1 || got[0].ID != "uinput" {
		t.Fatalf("after an absent re-report: got %s, want the first report retained", raw)
	}
	if at == nil || firstAt == nil || *at != *firstAt {
		t.Errorf("reported_at moved on an absent report: %v -> %v", firstAt, at)
	}
}

// An EXPLICIT empty array is a real report ("I ran the checks, nothing to say")
// and must overwrite — distinct from the nil/absent case above.
func TestUpsertHostReadinessEmptyArrayOverwrites(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	ctx := context.Background()

	if err := s.upsertHostReadiness(ctx, hostID,
		json.RawMessage(`[{"id":"uinput","status":"fail"}]`)); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(`[]`)); err != nil {
		t.Fatalf("empty upsert: %v", err)
	}
	raw, _ := rawReadiness(t, pool, hostID)
	if string(raw) != "[]" {
		t.Fatalf("explicit empty report: got %s, want []", raw)
	}
}

// The wire decode must tolerate a check set from a NEWER agent: an unknown
// status value, and extra keys, must survive rather than being dropped. The
// check set is agent-owned by design.
func TestCapacityReadinessDecodeIsForwardCompatible(t *testing.T) {
	raw := []byte(`{"type":"capacity","host":{"cpu_cores":8,"mem_mb":16000},
		"readiness":[{"id":"future_check","status":"warn","summary":"s","remediation":"r","extra":1}]}`)
	var cap CapacityMsg
	if err := json.Unmarshal(raw, &cap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	checks, ok := ValidReadiness(cap.Readiness)
	if !ok || len(checks) != 1 {
		t.Fatalf("readiness: got %+v ok=%v, want one valid check", checks, ok)
	}
	if checks[0].Status != "warn" {
		t.Errorf("an unrecognized status must pass through verbatim, got %q", checks[0].Status)
	}
	// And the raw payload — the thing that actually gets stored — still has the
	// key the typed view does not model.
	if !strings.Contains(string(cap.Readiness), `"extra"`) {
		t.Errorf("unknown key lost before storage: %s", cap.Readiness)
	}
}

// A capacity report with NO readiness key must decode to a nil slice — that is
// what drives keep-if-absent, and an empty-but-non-nil slice here would silently
// blank every pre-amendment agent's stored set.
func TestCapacityWithoutReadinessDecodesToNil(t *testing.T) {
	var cap CapacityMsg
	if err := json.Unmarshal([]byte(`{"type":"capacity","host":{"cpu_cores":8,"mem_mb":16000}}`), &cap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cap.Readiness != nil {
		t.Fatalf("absent readiness: got %+v, want nil", cap.Readiness)
	}
}

// Review round 2, finding #2, stated as the damage it prevents: an explicit
// JSON `null` must not be able to destroy a good stored check set — nor stamp
// readiness_reported_at, which would present the destruction as fresh evidence.
func TestReadinessNullDoesNotDestroyAStoredReport(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	ctx := context.Background()

	good := json.RawMessage(`[{"id":"nvidia_lib32_gl","status":"fail","summary":"missing","remediation":"dnf install …"}]`)
	if err := s.upsertHostReadiness(ctx, hostID, good); err != nil {
		t.Fatalf("seed good report: %v", err)
	}
	before, beforeAt := rawReadiness(t, pool, hostID)

	if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(`null`)); err == nil {
		t.Error("JSON null was accepted")
	}

	after, afterAt := rawReadiness(t, pool, hostID)
	if string(after) != string(before) {
		t.Fatalf("a null report altered the stored set: %s -> %s", before, after)
	}
	if afterAt == nil || beforeAt == nil || *afterAt != *beforeAt {
		t.Errorf("a null report moved the freshness stamp: %v -> %v", beforeAt, afterAt)
	}
}
