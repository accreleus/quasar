package session

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// ST-09 — the two Verdict routes, over a real database.
//
// Skips without TEST_DATABASE_URL, like every other *_db_test.go here; run them
// with `make test-db`, which provisions a fresh ephemeral Postgres.

type verdictBody struct {
	Verdict  string   `json:"verdict"`
	Evidence []string `json:"evidence"`
	Reason   string   `json:"reason"`
	Window   struct {
		FromMs  int64 `json:"from_ms"`
		ToMs    int64 `json:"to_ms"`
		NHost   int   `json:"n_host"`
		NClient int   `json:"n_client"`
	} `json:"window"`
	Clock struct {
		Quality       string   `json:"quality"`
		OffsetMs      *float64 `json:"offset_ms"`
		UncertaintyMs *float64 `json:"uncertainty_ms"`
	} `json:"clock"`
	EvidenceTier string `json:"evidence_tier"`
	Falsifiers   []struct {
		Name      string   `json:"name"`
		Estimator string   `json:"estimator"`
		Value     *float64 `json:"value"`
		Op        string   `json:"op"`
		Threshold float64  `json:"threshold"`
		Unit      string   `json:"unit"`
		N         int      `json:"n"`
		Holds     bool     `json:"holds"`
		Note      string   `json:"note"`
	} `json:"falsifiers"`
	ThresholdsVersion string `json:"thresholds_version"`
}

func getVerdict(t *testing.T, url, token string) (int, verdictBody) {
	t.Helper()
	resp := doJSON(t, http.MethodGet, url, token, nil)
	defer resp.Body.Close()
	var out verdictBody
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode verdict: %v", err)
		}
	}
	return resp.StatusCode, out
}

// seedCongestion writes a congestion trace into the last 30 s so the default
// 5-minute window catches it.
func seedCongestion(t *testing.T, store *Store, sid string) {
	t.Helper()
	ctx := context.Background()
	base := time.Now().Add(-30 * time.Second).UnixMilli()
	loss := []float64{0, 6, 16, 34, 60}
	rtt := []float64{45, 75, 95, 110, 130}
	for i := range loss {
		ts := base + int64(i)*1000
		bm := json.RawMessage(`{"packets_lost":` + ftoa(loss[i]) + `,"rtt_ms":` + ftoa(rtt[i]) + `,"is_hidden":0}`)
		if err := store.Telemetry().Append(ctx, sid, telemetry.SourceBrowser, telemetry.SampleInput{TsUnixMs: ts, Metrics: bm}); err != nil {
			t.Fatalf("insert browser metric: %v", err)
		}
		if err := store.Telemetry().Append(ctx, sid, telemetry.SourceAgent, telemetry.SampleInput{TsUnixMs: ts, Metrics: json.RawMessage(`{"encode_ms":4,"fps":60}`)}); err != nil {
			t.Fatalf("insert agent metric: %v", err)
		}
	}
}

func ftoa(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// verdictSession is adminBundleSession plus the auth service, so a test can mint
// a THIRD identity (neither the owner nor an admin) to prove the ownership gate.
func verdictSession(t *testing.T) (sid, ownerTok, adminTok string, store *Store, srvURL string, authSvc *auth.Service) {
	t.Helper()
	pool := testDB(t)
	srv, svc, st := newMetricsServer(t, pool)
	ctx := context.Background()
	_ = seed(t, pool, 4)
	owner, err := svc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	if _, err := svc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE email='admin@test.local'`); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	s := currentSeed(t, pool)
	sid = sessionForUser(t, st, s, owner.ID)
	return sid, loginTok(t, svc, "owner@test.local", "quasar-fixture-pw-03"),
		loginTok(t, svc, "admin@test.local", "quasar-fixture-pw-01"), st, srv.URL, svc
}

// The admin route is gated by RequireAdmin, so a valid NON-admin bearer is 403
// before any lookup — an admin endpoint must never leak existence.
func TestAdminVerdictAdminGate(t *testing.T) {
	sid, ownerTok, adminTok, _, srvURL := adminBundleSession(t)

	if code, _ := getVerdict(t, srvURL+"/v1/admin/sessions/"+sid+"/verdict", ownerTok); code != http.StatusForbidden {
		t.Fatalf("non-admin on the admin verdict route: got %d want 403", code)
	}
	if code, _ := getVerdict(t, srvURL+"/v1/admin/sessions/"+sid+"/verdict", adminTok); code != http.StatusOK {
		t.Fatalf("admin verdict: got %d want 200", code)
	}
	code, _ := getVerdict(t,
		srvURL+"/v1/admin/sessions/00000000-0000-0000-0000-000000000000/verdict", adminTok)
	if code != http.StatusNotFound {
		t.Fatalf("admin verdict, unknown session: got %d want 404", code)
	}
}

// The owner route follows the ownership convention the other owner-scoped
// session routes use: the owner reads their own session, an admin reads anyone's,
// an unknown id is 404.
func TestSessionVerdictOwnerAccess(t *testing.T) {
	sid, ownerTok, adminTok, _, srvURL := adminBundleSession(t)

	if code, _ := getVerdict(t, srvURL+"/v1/sessions/"+sid+"/verdict", ownerTok); code != http.StatusOK {
		t.Fatalf("owner on their own session: got %d want 200", code)
	}
	if code, _ := getVerdict(t, srvURL+"/v1/sessions/"+sid+"/verdict", adminTok); code != http.StatusOK {
		t.Fatalf("admin on someone else's session: got %d want 200", code)
	}
	code, _ := getVerdict(t, srvURL+"/v1/sessions/00000000-0000-0000-0000-000000000000/verdict", ownerTok)
	if code != http.StatusNotFound {
		t.Fatalf("owner route, unknown session: got %d want 404", code)
	}
}

// A different (non-admin) user must not read someone else's verdict. Ownership
// is server-enforced here, never UI-gated.
func TestSessionVerdictOtherUserForbidden(t *testing.T) {
	sid, _, _, _, srvURL, authSvc := verdictSession(t)
	if _, err := authSvc.Register(context.Background(), "stranger@test.local", "stranger", "unrelated-pw-17"); err != nil {
		t.Fatalf("register stranger: %v", err)
	}
	otherTok := loginTok(t, authSvc, "stranger@test.local", "unrelated-pw-17")

	if code, _ := getVerdict(t, srvURL+"/v1/sessions/"+sid+"/verdict", otherTok); code != http.StatusForbidden {
		t.Fatalf("a stranger on someone else's session: got %d want 403", code)
	}
	// And the admin form stays 403 for the same bearer — a non-admin never
	// reaches the admin surface, regardless of ownership.
	if code, _ := getVerdict(t, srvURL+"/v1/admin/sessions/"+sid+"/verdict", otherTok); code != http.StatusForbidden {
		t.Fatalf("a stranger on the admin verdict route: got %d want 403", code)
	}
}

// Both routes must return byte-identical values, and the same value the bundle
// carries as `classifier`. Three copies of "is this healthy" is the thing this
// whole change exists to prevent.
func TestVerdictRoutesAgreeWithBundleClassifier(t *testing.T) {
	sid, ownerTok, adminTok, store, srvURL := adminBundleSession(t)
	seedCongestion(t, store, sid)

	_, adminV := getVerdict(t, srvURL+"/v1/admin/sessions/"+sid+"/verdict", adminTok)
	_, ownerV := getVerdict(t, srvURL+"/v1/sessions/"+sid+"/verdict", ownerTok)

	if adminV.Verdict != verdictNetworkCongestion {
		t.Fatalf("admin verdict = %q want %q (reason: %s)", adminV.Verdict, verdictNetworkCongestion, adminV.Reason)
	}
	if ownerV.Verdict != adminV.Verdict || ownerV.EvidenceTier != adminV.EvidenceTier {
		t.Fatalf("owner verdict %+v disagrees with admin %+v", ownerV, adminV)
	}
	if len(ownerV.Falsifiers) != len(adminV.Falsifiers) {
		t.Fatalf("owner has %d falsifiers, admin has %d", len(ownerV.Falsifiers), len(adminV.Falsifiers))
	}

	resp := doJSON(t, http.MethodGet, srvURL+"/v1/admin/sessions/"+sid+"/diagnostic-bundle", adminTok, nil)
	var bundle struct {
		Classifier verdictBody `json:"classifier"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&bundle)
	resp.Body.Close()
	if bundle.Classifier.Verdict != adminV.Verdict {
		t.Fatalf("bundle.classifier.verdict = %q but the verdict route said %q",
			bundle.Classifier.Verdict, adminV.Verdict)
	}
	if len(bundle.Classifier.Falsifiers) != len(adminV.Falsifiers) {
		t.Fatalf("bundle.classifier carries %d falsifiers, the verdict route %d",
			len(bundle.Classifier.Falsifiers), len(adminV.Falsifiers))
	}
	if bundle.Classifier.ThresholdsVersion != thresholdsVersion {
		t.Fatalf("bundle thresholds_version = %q want %q",
			bundle.Classifier.ThresholdsVersion, thresholdsVersion)
	}
}

// The full value over real rows: sample counts from the DB, an unmeasured clock
// stated as such, and falsifiers computed off the stored series.
func TestVerdictOverRealRows(t *testing.T) {
	sid, _, adminTok, store, srvURL := adminBundleSession(t)
	seedCongestion(t, store, sid)

	code, v := getVerdict(t, srvURL+"/v1/admin/sessions/"+sid+"/verdict", adminTok)
	if code != http.StatusOK {
		t.Fatalf("verdict: got %d want 200", code)
	}
	if v.Window.NHost != 5 || v.Window.NClient != 5 {
		t.Errorf("window counts = %d host / %d client, want 5 / 5", v.Window.NHost, v.Window.NClient)
	}
	if v.Clock.Quality != clockUnmeasured {
		t.Errorf("clock.quality = %q want unmeasured (no clock row was written)", v.Clock.Quality)
	}
	if v.EvidenceTier == tierFull {
		t.Errorf("evidence_tier must not be full with an unmeasured clock")
	}
	if v.Reason == "" {
		t.Error("verdict carries no reason")
	}
	if v.ThresholdsVersion != thresholdsVersion {
		t.Errorf("thresholds_version = %q want %q", v.ThresholdsVersion, thresholdsVersion)
	}

	byName := map[string]bool{}
	for _, f := range v.Falsifiers {
		byName[f.Name] = true
		if f.Estimator == "" || f.Op == "" || f.Unit == "" {
			t.Errorf("falsifier %q is under-specified: %+v", f.Name, f)
		}
		if f.Value == nil && f.Note == "" {
			t.Errorf("falsifier %q has a null value and no note", f.Name)
		}
	}
	for _, want := range []string{"transport.packets_lost", "transport.rtt_ms", "encoder.encode_ms"} {
		if !byName[want] {
			t.Errorf("congestion verdict has no %q falsifier (got %v)", want, byName)
		}
	}
}

// A measured clock lifts the tier to full and is reported with its uncertainty.
func TestVerdictMeasuredClockOverDB(t *testing.T) {
	sid, _, adminTok, store, srvURL := adminBundleSession(t)
	seedCongestion(t, store, sid)
	if err := store.Telemetry().UpsertClock(context.Background(), sid, -3.2, 1.8); err != nil {
		t.Fatalf("upsert clock: %v", err)
	}

	_, v := getVerdict(t, srvURL+"/v1/admin/sessions/"+sid+"/verdict", adminTok)
	if v.Clock.Quality != clockMeasured {
		t.Fatalf("clock.quality = %q want measured", v.Clock.Quality)
	}
	if v.Clock.OffsetMs == nil || *v.Clock.OffsetMs != -3.2 {
		t.Errorf("offset_ms = %v want -3.2", v.Clock.OffsetMs)
	}
	if v.EvidenceTier != tierFull {
		t.Errorf("evidence_tier = %q want full (both sides reported, clock measured)", v.EvidenceTier)
	}
}

// A session with no telemetry at all must say so — insufficient, with null
// falsifiers — rather than report a confident nominal over nothing.
func TestVerdictEmptyWindowIsInsufficient(t *testing.T) {
	sid, _, adminTok, _, srvURL := adminBundleSession(t)

	code, v := getVerdict(t, srvURL+"/v1/admin/sessions/"+sid+"/verdict", adminTok)
	if code != http.StatusOK {
		t.Fatalf("verdict: got %d want 200", code)
	}
	if v.EvidenceTier != tierInsufficient {
		t.Fatalf("evidence_tier = %q want %q on an empty window", v.EvidenceTier, tierInsufficient)
	}
	if v.Window.NHost != 0 || v.Window.NClient != 0 {
		t.Errorf("window counts = %+v want zeros", v.Window)
	}
	for _, f := range v.Falsifiers {
		if f.Value != nil {
			t.Errorf("falsifier %q has a value over an empty window: %v", f.Name, *f.Value)
		}
		if f.Holds {
			t.Errorf("falsifier %q holds over an empty window", f.Name)
		}
		if f.Note == "" {
			t.Errorf("falsifier %q has no note over an empty window", f.Name)
		}
	}
}
