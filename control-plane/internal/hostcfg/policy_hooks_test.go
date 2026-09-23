package hostcfg

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// offerDispatcher is a current typed connection with complete inventory: it
// reports the connection identity and its per-group active snapshots.
type offerDispatcher struct {
	fakeDispatcher
	connection string
	snapshots  map[string]PolicySnapshot
}

func (d *offerDispatcher) PolicyIdentity(string) (string, string, bool) {
	return "22222222-2222-4222-8222-222222222222", d.connection, true
}

func (d *offerDispatcher) PolicyActiveSnapshots(_, connection string) map[string]PolicySnapshot {
	if connection != d.connection {
		return nil
	}
	return d.snapshots
}

func (d *offerDispatcher) takeOffers() map[string]*PolicyOffer {
	offers := map[string]*PolicyOffer{}
	for _, sent := range d.sent {
		if offer, ok := sent.(*PolicyOffer); ok {
			offers[offer.Group] = offer
		}
	}
	d.sent = nil
	return offers
}

func policyMux(h *Handler) *http.ServeMux {
	mux := http.NewServeMux()
	pass := func(next http.Handler) http.Handler { return next }
	h.Register(mux, pass, pass)
	return mux
}

func patchPolicy(t *testing.T, mux *http.ServeMux, hostID, revision string, changes map[string]PolicyChoice) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"expected_revision": revision, "changes": changes})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPatch, "/v1/admin/hosts/"+hostID+"/policy", strings.NewReader(string(body))))
	return rr
}

func errorCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body %q: %v", rr.Body.String(), err)
	}
	return body.Error.Code
}

func currentRevision(t *testing.T, store *Store, hostID string) string {
	t.Helper()
	view, err := store.GetPolicy(context.Background(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	return view.Revision
}

// TestTypedPatchEveryNextSessionKeySavesValidatesAndOffers drives every
// next-session key through the operator PATCH: an explicit value is saved
// pending and offered as exactly that group, an invalid value writes nothing,
// and a deployment choice is offered with the current-connection baseline.
func TestTypedPatchEveryNextSessionKeySavesValidatesAndOffers(t *testing.T) {
	store := NewStore(testPool(t))
	baseline := deploymentBaseline()
	for _, group := range NextSessionPolicyGroups() {
		t.Run(group, func(t *testing.T) {
			pool := testPool(t)
			host := newTypedHost(t, pool, group)
			dispatcher := &offerDispatcher{connection: host.connection, snapshots: host.snapshots}
			mux := policyMux(NewHandler(store, dispatcher, nil))
			value := explicitSamples[group].valid

			rr := patchPolicy(t, mux, host.id, "0", map[string]PolicyChoice{group: {Source: "explicit", Value: value}})
			if rr.Code != http.StatusOK {
				t.Fatalf("explicit save: %d %s", rr.Code, rr.Body.String())
			}
			var view PolicyView
			if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if g := view.Groups[group]; g.Status != "pending" || g.Scope != "next_session" || g.DesiredDigest == nil || g.DesiredRevision != "1" {
				t.Fatalf("saved group = %+v", g)
			}
			offers := dispatcher.takeOffers()
			offer := offers[group]
			if len(offers) != 1 || offer == nil || offer.ResolvedSettings[group] != value || offer.Revision != "1" || offer.ContentSHA256 != *view.Groups[group].DesiredDigest {
				t.Fatalf("explicit offers = %+v", offers)
			}

			rr = patchPolicy(t, mux, host.id, "1", map[string]PolicyChoice{group: {Source: "explicit", Value: explicitSamples[group].invalid}})
			if rr.Code != http.StatusBadRequest || errorCode(t, rr) != "validation_failed" {
				t.Fatalf("invalid value: %d %s", rr.Code, rr.Body.String())
			}
			if rev := currentRevision(t, store, host.id); rev != "1" {
				t.Fatalf("invalid value wrote revision %s", rev)
			}
			if len(dispatcher.takeOffers()) != 0 {
				t.Fatal("an invalid edit sent an offer")
			}

			rr = patchPolicy(t, mux, host.id, "1", map[string]PolicyChoice{group: {Source: "deployment"}})
			if rr.Code != http.StatusOK {
				t.Fatalf("deployment save: %d %s", rr.Code, rr.Body.String())
			}
			offer = dispatcher.takeOffers()[group]
			if offer == nil || offer.Settings[group].Source != "deployment" || offer.ResolvedSettings[group] != baseline[group] || offer.Revision != "2" {
				t.Fatalf("deployment offer = %+v", offer)
			}
		})
	}
}

func TestGetPolicyProjectsEveryNextSessionGroupBeforeFirstEdit(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	confirmed := seedNamedHost(t, pool, "rh05-336-confirmed")
	confirmPolicyGroups(t, pool, confirmed, NextSessionPolicyGroups()...)
	legacy := seedNamedHost(t, pool, "rh05-336-legacy")
	for hostID, want := range map[string]string{confirmed: "pending", legacy: "upgrade_required"} {
		view, err := store.GetPolicy(ctx, hostID)
		if err != nil {
			t.Fatal(err)
		}
		for _, group := range NextSessionPolicyGroups() {
			g, ok := view.Groups[group]
			if !ok || g.Status != want || g.Scope != "next_session" || g.DesiredRevision != "0" || g.Remedy == nil {
				t.Fatalf("%s projection = %+v (present %v), want %s", group, g, ok, want)
			}
		}
		if _, ok := view.Groups["hardware"]; ok {
			t.Fatal("restart-scope hardware is not part of #336 and must not be projected")
		}
	}
}

// TestPolicyCrossKeyUsesReportedBaselineNotCatalogDefault: a deployment side
// resolves only from the agent's baseline.
func TestPolicyCrossKeyUsesReportedBaselineNotCatalogDefault(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	host := newTypedHost(t, pool, "abr_ladder_res_engage_frac", "abr_ladder_res_recover_frac")
	setBaseline := func(recover float64) {
		baseline := deploymentBaseline()
		baseline["abr_ladder_res_recover_frac"] = recover
		if _, err := pool.Exec(ctx, `UPDATE hosts SET deployment_settings=$2::jsonb WHERE id=$1::uuid`, host.id, mustJSON(t, baseline)); err != nil {
			t.Fatal(err)
		}
	}
	engage := func(v float64) map[string]PolicyChoice {
		return map[string]PolicyChoice{"abr_ladder_res_engage_frac": {Source: "explicit", Value: v}}
	}
	// The catalog default recover (0.8) would accept 0.6; the reported 0.5 does not.
	setBaseline(0.5)
	_, err := store.SavePolicy(ctx, host.id, "0", engage(0.6), nil)
	assertPolicyCode(t, err, "validation_failed")
	// The catalog default would reject 0.9; the reported 1.0 accepts it.
	setBaseline(1.0)
	if _, err := store.SavePolicy(ctx, host.id, "0", engage(0.9), nil); err != nil {
		t.Fatalf("baseline-valid pair rejected: %v", err)
	}
}

func seedLocalHome(t *testing.T, pool *pgxpool.Pool, hostID, ref string) {
	t.Helper()
	ctx := context.Background()
	var userID, appID string
	suffix := strings.ReplaceAll(ref, "/", "-")
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash) VALUES ($1,$2,'x') RETURNING id::text`, "rh05-336"+suffix+"@example.test", "rh05336"+suffix).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO apps (name) VALUES ($1) RETURNING id::text`, "rh05-336 app"+suffix).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_homes WHERE user_id=$1::uuid`, userID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM apps WHERE id=$1::uuid`, appID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1::uuid`, userID)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO user_homes (user_id, app_id, host_id, provider, ref) VALUES ($1::uuid,$2::uuid,$3::uuid,'local',$4)`, userID, appID, hostID, ref); err != nil {
		t.Fatal(err)
	}
}

// TestHomeRootStaysInsideMountAndNeverStrandsExistingHomes covers both
// operator writers: the typed PATCH and the legacy settings PATCH.
func TestHomeRootStaysInsideMountAndNeverStrandsExistingHomes(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	host := newTypedHost(t, pool, "home_root")
	seedLocalHome(t, pool, host.id, "/srv/homes/u1/app")
	// The legacy writer's own subpath check reads the effective report.
	if _, err := pool.Exec(ctx, `UPDATE hosts SET effective_settings='{"home_root":"/srv/homes"}'::jsonb WHERE id=$1::uuid`, host.id); err != nil {
		t.Fatal(err)
	}
	dispatcher := &offerDispatcher{connection: host.connection, snapshots: host.snapshots}
	mux := policyMux(NewHandler(store, dispatcher, stubCounter{}))
	homeRoot := func(v string) map[string]PolicyChoice {
		return map[string]PolicyChoice{"home_root": {Source: "explicit", Value: v}}
	}
	for _, tc := range []struct {
		value, code string
	}{
		{"/other/homes", "validation_failed"},
		{"/srv/homes-evil", "validation_failed"},
		{"/srv/homes/u2", "validation_failed"},
		{"", "validation_failed"},
	} {
		rr := patchPolicy(t, mux, host.id, "0", homeRoot(tc.value))
		if rr.Code != http.StatusBadRequest || errorCode(t, rr) != tc.code {
			t.Fatalf("typed home_root %q: %d %s, want 400 %s", tc.value, rr.Code, rr.Body.String(), tc.code)
		}
		legacy := httptest.NewRecorder()
		mux.ServeHTTP(legacy, httptest.NewRequest(http.MethodPatch, "/v1/admin/hosts/"+host.id+"/settings", strings.NewReader(fmt.Sprintf(`{"overrides":{"home_root":%q}}`, tc.value))))
		if legacy.Code != http.StatusBadRequest || errorCode(t, legacy) != tc.code {
			t.Fatalf("legacy home_root %q: %d %s", tc.value, legacy.Code, legacy.Body.String())
		}
	}
	if rev := currentRevision(t, store, host.id); rev != "0" {
		t.Fatalf("a refused storage change wrote revision %s", rev)
	}
	var overrides []byte
	if err := pool.QueryRow(ctx, `SELECT COALESCE((SELECT overrides FROM host_settings WHERE host_id=$1::uuid),'{}'::jsonb)`, host.id).Scan(&overrides); err != nil || string(overrides) != "{}" {
		t.Fatalf("a refused storage change wrote overrides %s err=%v", overrides, err)
	}
	if rr := patchPolicy(t, mux, host.id, "0", homeRoot("/srv/homes/u1")); rr.Code != http.StatusOK {
		t.Fatalf("root containing every existing home: %d %s", rr.Code, rr.Body.String())
	}
	var ref string
	if err := pool.QueryRow(ctx, `SELECT ref FROM user_homes WHERE host_id=$1::uuid`, host.id).Scan(&ref); err != nil || ref != "/srv/homes/u1/app" {
		t.Fatalf("existing home moved to %q err=%v", ref, err)
	}
}

func seedNamedHost(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `INSERT INTO hosts (node_name, status) VALUES ($1, 'online') RETURNING id::text`, name).Scan(&id); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	return id
}

type stubCounter struct{}

func (stubCounter) LiveSessions(string) int { return 0 }

func TestNewBaselineRefreshesEveryDeploymentSourceGroup(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	host := newTypedHost(t, pool, "gop", "slices")
	save(t, store, host.id, map[string]PolicyChoice{"gop": {Source: "deployment"}, "slices": {Source: "explicit", Value: float64(4)}})
	offers := host.offers(t, store)
	for _, group := range []string{"gop", "slices"} {
		if ok, err := store.ObservePolicyApplied(ctx, host.id, group, offers[group].Revision, offers[group].ContentSHA256, "next_session", host.connection); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	baseline := deploymentBaseline()
	baseline["gop"] = float64(240)
	if err := store.ObserveDeploymentSettings(ctx, host.id, host.connection, mustJSON(t, baseline)); err != nil {
		t.Fatal(err)
	}
	view, err := store.GetPolicy(ctx, host.id)
	if err != nil {
		t.Fatal(err)
	}
	if view.Groups["gop"].Status != "pending" || *view.Groups["gop"].DesiredDigest == offers["gop"].ContentSHA256 || view.Groups["slices"].Status != "applied" {
		t.Fatalf("after new baseline gop=%+v slices=%s", view.Groups["gop"], view.Groups["slices"].Status)
	}
	if offer := host.offers(t, store)["gop"]; offer == nil || offer.ResolvedSettings["gop"] != float64(240) {
		t.Fatalf("changed baseline not offered: %+v", offer)
	}
}

func TestInitialDeliveryAckLiftsGateForAnyNextSessionGroup(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	for groups, lifted := range map[string]bool{"gop": true, "": false} {
		hostID := seedNamedHost(t, pool, "rh05-336-ack-"+groups)
		connection := "00000000-0000-4000-8000-000000000301"
		accepted := []string{}
		if groups != "" {
			accepted = strings.Split(groups, ",")
		}
		if _, err := store.BeginPolicyConnection(ctx, hostID, connection, map[string]int{"typed_settings": 2}, accepted, true); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ConfirmPolicyGroups(ctx, hostID, connection, accepted); err != nil {
			t.Fatal(err)
		}
		id := "00000000-0000-4000-8000-000000000302"
		if _, ok, err := store.PrepareLegacyDelivery(ctx, hostID, connection, id, accepted); err != nil || !ok {
			t.Fatalf("delivery: %v %v", ok, err)
		}
		if ok, err := store.AcknowledgeInitialDelivery(ctx, hostID, connection, id); err != nil || !ok {
			t.Fatalf("ack: %v %v", ok, err)
		}
		var gated bool
		if err := pool.QueryRow(ctx, `SELECT config_policy_gate_connection IS NOT NULL FROM hosts WHERE id=$1::uuid`, hostID).Scan(&gated); err != nil {
			t.Fatal(err)
		}
		if gated == lifted {
			t.Fatalf("groups %q: gated=%v", groups, gated)
		}
	}
}

func TestReconnectInvalidatesEveryNextSessionGroup(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	host := newTypedHost(t, pool, "gop", "zerocopy")
	save(t, store, host.id, map[string]PolicyChoice{"gop": {Source: "explicit", Value: float64(90)}, "zerocopy": {Source: "explicit", Value: true}})
	offers := host.offers(t, store)
	for _, group := range []string{"gop", "zerocopy"} {
		if ok, err := store.ObservePolicyApplied(ctx, host.id, group, offers[group].Revision, offers[group].ContentSHA256, "next_session", host.connection); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	if err := store.InvalidatePolicyEvidenceOnReconnect(ctx, host.id); err != nil {
		t.Fatal(err)
	}
	view, err := store.GetPolicy(ctx, host.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"gop", "zerocopy"} {
		if view.Groups[group].Status != "pending" {
			t.Fatalf("%s kept old-connection evidence: %s", group, view.Groups[group].Status)
		}
	}
}

func TestExpectedPolicyResolvedEveryNextSessionGroup(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	groups := NextSessionPolicyGroups()
	host := newTypedHost(t, pool, groups...)
	baseline := deploymentBaseline()
	for _, group := range groups {
		got, ok, err := store.ExpectedPolicyResolved(ctx, host.id, host.connection, group)
		if err != nil || !ok || len(got) != 1 || got[group] != baseline[group] {
			t.Fatalf("%s deployment expectation = %v ok=%v err=%v", group, got, ok, err)
		}
		if _, ok, _ := store.ExpectedPolicyResolved(ctx, host.id, "33333333-3333-4333-8333-333333333333", group); ok {
			t.Fatalf("%s resolved from another connection's baseline", group)
		}
	}
	if _, ok, _ := store.ExpectedPolicyResolved(ctx, host.id, host.connection, "hardware"); ok {
		t.Fatal("restart-scope hardware has no next-session expectation")
	}
}

func TestGetPolicyReportsNextRetryDuringBackoff(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	host := newTypedHost(t, pool, "gop")
	save(t, store, host.id, map[string]PolicyChoice{"gop": {Source: "explicit", Value: float64(90)}})
	offer := host.offers(t, store)["gop"]
	if _, err := store.ObservePolicyRejected(ctx, host.id, "gop", offer.Revision, offer.ContentSHA256, "journal_write_failed"); err != nil {
		t.Fatal(err)
	}
	view, err := store.GetPolicy(ctx, host.id)
	if err != nil {
		t.Fatal(err)
	}
	if g := view.Groups["gop"]; g.Status != "pending" || g.NextRetryAt == nil || g.Remedy == nil || !strings.Contains(*g.Remedy, "attempt 1 of 5") {
		t.Fatalf("backoff view = %+v", g)
	}
}

// policyRows is every durable row a Retry may write, for no-write checks.
func policyRows(t *testing.T, pool *pgxpool.Pool, hostID string) string {
	t.Helper()
	var rows string
	err := pool.QueryRow(context.Background(), `SELECT
		COALESCE((SELECT string_agg(group_key||':'||status||':'||desired_revision,',' ORDER BY group_key) FROM host_setting_groups WHERE host_id=$1::uuid),'')||'|'||
		COALESCE((SELECT string_agg(resource_key||':'||revision||':'||retry_count||':'||next_attempt_at,',' ORDER BY resource_key) FROM host_reconcile_obligations WHERE host_id=$1::uuid),'')||'|'||
		(SELECT revision FROM host_policy_revisions WHERE host_id=$1::uuid)`, hostID).Scan(&rows)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func retryRequest(h *Handler, hostID, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/admin/hosts/"+hostID+"/policy/retry", strings.NewReader(body))
	rr := httptest.NewRecorder()
	policyMux(h).ServeHTTP(rr, r)
	return rr
}

// exhaust spends group's whole transient retry budget.
func exhaust(t *testing.T, pool *pgxpool.Pool, store *Store, host typedHost, group string) {
	t.Helper()
	for attempt := 0; attempt < policyRetryBudget; attempt++ {
		offer := host.offers(t, store)[group]
		if offer == nil {
			t.Fatalf("attempt %d: no %s offer", attempt, group)
		}
		if _, err := store.ObservePolicyRejected(context.Background(), host.id, group, offer.Revision, offer.ContentSHA256, "journal_write_failed"); err != nil {
			t.Fatal(err)
		}
		expireObligations(t, pool, host.id)
	}
	if host.offers(t, store)[group] != nil {
		t.Fatal("offer after exhaustion")
	}
}

func TestRetryPolicyHandlerResponses(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	groups := []string{"gop", "slices", "zerocopy", "abr_mode", "target_usage", "idle_timeout_secs"}
	host := newTypedHost(t, pool, append([]string{"hardware"}, groups...)...)
	// gop: exhausted (retryable). slices: applied. zerocopy: rejected as
	// invalid. abr_mode: upgrade_required. target_usage: uncertain.
	// idle_timeout_secs: pending. hardware: restart scope.
	save(t, store, host.id, map[string]PolicyChoice{"gop": {Source: "explicit", Value: float64(90)}})
	exhaust(t, pool, store, host, "gop")
	changes := map[string]PolicyChoice{"encoder": {Source: "automatic"}}
	for _, group := range groups[1:] {
		changes[group] = PolicyChoice{Source: "explicit", Value: explicitSamples[group].valid}
	}
	save(t, store, host.id, changes)
	offers := host.offers(t, store)
	if ok, err := store.ObservePolicyApplied(ctx, host.id, "slices", offers["slices"].Revision, offers["slices"].ContentSHA256, "next_session", host.connection); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := store.ObservePolicyRejected(ctx, host.id, "zerocopy", offers["zerocopy"].Revision, offers["zerocopy"].ContentSHA256, "invalid_value"); err != nil {
		t.Fatal(err)
	}
	if err := store.ParkPolicyGroupUpgradeRequired(ctx, host.id, "abr_mode"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE host_setting_groups SET status='uncertain' WHERE host_id=$1::uuid AND group_key='target_usage'`, host.id); err != nil {
		t.Fatal(err)
	}
	dispatcher := &offerDispatcher{connection: host.connection, snapshots: host.snapshots}
	h := NewHandler(store, dispatcher, nil)
	view, err := store.GetPolicy(ctx, host.id)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"gop": "failed", "slices": "applied", "zerocopy": "failed", "abr_mode": "upgrade_required", "target_usage": "uncertain", "idle_timeout_secs": "pending"}
	for group, status := range want {
		if view.Groups[group].Status != status {
			t.Fatalf("setup %s = %s, want %s", group, view.Groups[group].Status, status)
		}
	}
	if !strings.HasPrefix(*view.Groups["gop"].Remedy, "retry_exhausted:") || !strings.HasPrefix(*view.Groups["zerocopy"].Remedy, "validation_failed:") {
		t.Fatalf("remedies gop=%q zerocopy=%q", *view.Groups["gop"].Remedy, *view.Groups["zerocopy"].Remedy)
	}

	before := policyRows(t, pool, host.id)
	for _, tc := range []struct {
		name, hostID, body string
		status             int
		code               string
	}{
		{"not json", host.id, `{`, http.StatusBadRequest, "validation_failed"},
		{"unknown field", host.id, `{"group":"gop","force":true}`, http.StatusBadRequest, "validation_failed"},
		{"missing group", host.id, `{}`, http.StatusBadRequest, "validation_failed"},
		{"trailing data", host.id, `{"group":"gop"}{}`, http.StatusBadRequest, "validation_failed"},
		{"group not a string", host.id, `{"group":7}`, http.StatusBadRequest, "validation_failed"},
		{"unknown group", host.id, `{"group":"no_such_group"}`, http.StatusBadRequest, "validation_failed"},
		{"key inside a group", host.id, `{"group":"encoder"}`, http.StatusBadRequest, "validation_failed"},
		{"unknown host", "00000000-0000-4000-8000-0000000003ff", `{"group":"gop"}`, http.StatusNotFound, "not_found"},
		{"pending", host.id, `{"group":"idle_timeout_secs"}`, http.StatusConflict, "conflict"},
		{"applied", host.id, `{"group":"slices"}`, http.StatusConflict, "conflict"},
		{"failed invalid intent", host.id, `{"group":"zerocopy"}`, http.StatusConflict, "conflict"},
		{"upgrade_required", host.id, `{"group":"abr_mode"}`, http.StatusConflict, "conflict"},
		{"uncertain", host.id, `{"group":"target_usage"}`, http.StatusConflict, "conflict"},
		{"restart scope", host.id, `{"group":"hardware"}`, http.StatusConflict, "conflict"},
	} {
		rr := retryRequest(h, tc.hostID, tc.body)
		if rr.Code != tc.status || errorCode(t, rr) != tc.code {
			t.Fatalf("%s: %d %s, want %d %s", tc.name, rr.Code, rr.Body.String(), tc.status, tc.code)
		}
		if after := policyRows(t, pool, host.id); after != before {
			t.Fatalf("%s wrote:\n%s\n%s", tc.name, before, after)
		}
		if len(dispatcher.sent) != 0 {
			t.Fatalf("%s dispatched %v", tc.name, dispatcher.sent)
		}
	}

	rr := retryRequest(h, host.id, `{"group":"gop"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("retry exhausted: %d %s", rr.Code, rr.Body.String())
	}
	var retried PolicyView
	if err := json.Unmarshal(rr.Body.Bytes(), &retried); err != nil {
		t.Fatal(err)
	}
	if g := retried.Groups["gop"]; g.Status != "pending" || g.DesiredRevision != view.Groups["gop"].DesiredRevision {
		t.Fatalf("retried gop = %+v", g)
	}
	offer := dispatcher.takeOffers()["gop"]
	if offer == nil {
		t.Fatal("retry did not re-offer the group")
	}
	var retries int
	if err := pool.QueryRow(ctx, `SELECT retry_count FROM host_reconcile_obligations WHERE host_id=$1::uuid AND resource_key='gop'`, host.id).Scan(&retries); err != nil || retries != 1 {
		t.Fatalf("fresh budget: retry_count=%d (one offer since Retry) err=%v", retries, err)
	}
	if again := retryRequest(h, host.id, `{"group":"gop"}`); again.Code != http.StatusConflict {
		t.Fatalf("second retry while pending: %d", again.Code)
	}
}
