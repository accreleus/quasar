package hostcfg

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// typedHost is a host whose current connection typed-owns groups, has a
// complete deployment baseline and a seeded active snapshot per group.
type typedHost struct {
	id, connection string
	snapshots      map[string]PolicySnapshot
}

func newTypedHost(t *testing.T, pool *pgxpool.Pool, groups ...string) typedHost {
	t.Helper()
	ctx := context.Background()
	host := typedHost{id: seedHost(t, pool), connection: "11111111-1111-4111-8111-111111111111", snapshots: map[string]PolicySnapshot{}}
	confirmPolicyGroups(t, pool, host.id, groups...)
	if _, err := pool.Exec(ctx, `UPDATE hosts SET deployment_settings=$2::jsonb,deployment_settings_connection=$3::uuid,deployment_settings_reported_at=now() WHERE id=$1::uuid`, host.id, mustJSON(t, deploymentBaseline()), host.connection); err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		host.snapshots[group] = PolicySnapshot{Kind: "seeded", Digest: strings.Repeat("a", 64)}
	}
	return host
}

func (h typedHost) offers(t *testing.T, store *Store) map[string]*PolicyOffer {
	t.Helper()
	offers, err := store.NextSessionOffers(context.Background(), h.id, "22222222-2222-4222-8222-222222222222", h.connection, h.snapshots, newPolicyAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	byGroup := map[string]*PolicyOffer{}
	for _, offer := range offers {
		byGroup[offer.Group] = offer
	}
	return byGroup
}

func save(t *testing.T, store *Store, hostID string, changes map[string]PolicyChoice) PolicyView {
	t.Helper()
	current, err := store.GetPolicy(context.Background(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := store.SavePolicy(context.Background(), hostID, current.Revision, changes, nil)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func expireObligations(t *testing.T, pool *pgxpool.Pool, hostID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE host_reconcile_obligations SET next_attempt_at=now()-interval '1 second' WHERE host_id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
}

func TestNextSessionOffersEveryGroupIndependently(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	groups := NextSessionPolicyGroups()
	host := newTypedHost(t, pool, groups...)
	changes := map[string]PolicyChoice{}
	for i, group := range groups {
		if i%2 == 0 {
			changes[group] = PolicyChoice{Source: "explicit", Value: explicitSamples[group].valid}
		} else {
			changes[group] = PolicyChoice{Source: "deployment"}
		}
	}
	// home_root needs a mount that contains it; the baseline reports /srv/homes.
	changes["home_root"] = PolicyChoice{Source: "explicit", Value: "/srv/homes/users"}
	save(t, store, host.id, changes)

	offers := host.offers(t, store)
	if len(offers) != len(groups) {
		t.Fatalf("offers for %d groups, want %d", len(offers), len(groups))
	}
	baseline := deploymentBaseline()
	for group, offer := range offers {
		want := changes[group]
		if offer.Scope != "next_session" || offer.Settings[group].Source != want.Source || len(offer.Settings) != 1 {
			t.Fatalf("%s offer = %+v", group, offer)
		}
		wantValue := want.Value
		if want.Source == "deployment" {
			wantValue = baseline[group]
		}
		if offer.ResolvedSettings[group] != wantValue {
			t.Fatalf("%s resolved %v, want %v", group, offer.ResolvedSettings[group], wantValue)
		}
		kinds := []string{}
		for _, fact := range offer.Prerequisites {
			kinds = append(kinds, fact.(map[string]any)["kind"].(string))
		}
		wantKinds := "seeded_group_digest"
		if want.Source == "deployment" {
			wantKinds = "deployment_baseline,seeded_group_digest"
		}
		if strings.Join(kinds, ",") != wantKinds {
			t.Fatalf("%s prerequisites %v", group, kinds)
		}
		digest, _ := digestPolicyFacts(offer.Prerequisites)
		if digest != offer.PrerequisitesSHA256 {
			t.Fatalf("%s prerequisite digest mismatch", group)
		}
	}
	// One group proving applied does not advance another.
	gop := offers["gop"]
	if ok, err := store.ObservePolicyApplied(context.Background(), host.id, "gop", gop.Revision, gop.ContentSHA256, "next_session", host.connection); err != nil || !ok {
		t.Fatalf("observe gop = %v %v", ok, err)
	}
	view, err := store.GetPolicy(context.Background(), host.id)
	if err != nil {
		t.Fatal(err)
	}
	if view.Groups["gop"].Status != "applied" || view.Groups["slices"].Status != "pending" {
		t.Fatalf("independent status: gop=%s slices=%s", view.Groups["gop"].Status, view.Groups["slices"].Status)
	}
	// An offer is not repeated while its backoff window is open.
	if again := host.offers(t, store); again["slices"] != nil || again["gop"] != nil {
		t.Fatalf("offers repeated inside backoff: %v", again)
	}
}

func TestNextSessionOffersWaitForCurrentConnectionEvidence(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	host := newTypedHost(t, pool, "gop", "slices")
	save(t, store, host.id, map[string]PolicyChoice{"gop": {Source: "explicit", Value: float64(90)}, "slices": {Source: "deployment"}})
	stale := host
	stale.connection = "33333333-3333-4333-8333-333333333333"
	offers := stale.offers(t, store)
	if offers["gop"] == nil || offers["slices"] != nil {
		t.Fatalf("a baseline from another connection resolved a deployment choice: %v", offers)
	}
	noSnapshot := host
	noSnapshot.snapshots = map[string]PolicySnapshot{}
	expireObligations(t, pool, host.id)
	if offers := noSnapshot.offers(t, store); len(offers) != 0 {
		t.Fatalf("offer sent without an active snapshot: %v", offers)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE hosts SET config_policy_gate_connection=$2::uuid WHERE id=$1::uuid`, host.id, host.connection); err != nil {
		t.Fatal(err)
	}
	if offers := host.offers(t, store); len(offers) != 0 {
		t.Fatalf("offer sent while the admission gate is closed: %v", offers)
	}
}

func TestTransientFailureExhaustsBudgetThenRetryResumes(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	host := newTypedHost(t, pool, "gop", "slices")
	save(t, store, host.id, map[string]PolicyChoice{"gop": {Source: "explicit", Value: float64(90)}, "slices": {Source: "explicit", Value: float64(4)}})
	for attempt := 1; attempt <= policyRetryBudget; attempt++ {
		offers := host.offers(t, store)
		if offers["gop"] == nil {
			t.Fatalf("attempt %d: no offer", attempt)
		}
		if slices := offers["slices"]; slices != nil {
			if ok, err := store.ObservePolicyApplied(ctx, host.id, "slices", slices.Revision, slices.ContentSHA256, "next_session", host.connection); err != nil || !ok {
				t.Fatal(ok, err)
			}
		}
		if _, err := store.ObservePolicyRejected(ctx, host.id, "gop", offers["gop"].Revision, offers["gop"].ContentSHA256, "journal_write_failed"); err != nil {
			t.Fatal(err)
		}
		var nextDelay float64
		if err := pool.QueryRow(ctx, `SELECT extract(epoch FROM next_attempt_at-now()) FROM host_reconcile_obligations WHERE host_id=$1::uuid AND resource_key='gop'`, host.id).Scan(&nextDelay); err != nil {
			t.Fatal(err)
		}
		if want := float64(policyRetryBase.Seconds()) * float64(int(1)<<(attempt-1)); nextDelay < want-2 || nextDelay > want+1 {
			t.Fatalf("attempt %d backoff %.1fs, want ~%.0fs", attempt, nextDelay, want)
		}
		expireObligations(t, pool, host.id)
	}
	if offers := host.offers(t, store); offers["gop"] != nil {
		t.Fatal("offer sent after the retry budget was exhausted")
	}
	view, err := store.GetPolicy(ctx, host.id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DecoratePolicyGroups(ctx, host.id, &view); err != nil {
		t.Fatal(err)
	}
	gop := view.Groups["gop"]
	if gop.Status != "failed" || gop.Remedy == nil || !strings.HasPrefix(*gop.Remedy, "retry_exhausted:") || !strings.Contains(*gop.Remedy, "journal_write_failed") {
		t.Fatalf("exhausted gop = %+v remedy=%v", gop, gop.Remedy)
	}
	if view.Groups["slices"].Status != "applied" {
		t.Fatalf("slices must be unaffected: %s", view.Groups["slices"].Status)
	}
	if err := store.RetryPolicyGroup(ctx, host.id, "gop"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if offers := host.offers(t, store); offers["gop"] == nil {
		t.Fatal("retry did not resume the offer")
	}
	if err := store.RetryPolicyGroup(ctx, host.id, "gop"); !errors.Is(err, ErrPolicyNotRetryable) {
		t.Fatalf("retry of a pending group = %v", err)
	}
}

func TestInvalidRejectionNeverLoopsButChangedConditionsResume(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	host := newTypedHost(t, pool, "abr_ladder_res_engage_frac", "gop")
	save(t, store, host.id, map[string]PolicyChoice{"abr_ladder_res_engage_frac": {Source: "deployment"}, "gop": {Source: "explicit", Value: float64(90)}})
	offer := host.offers(t, store)["abr_ladder_res_engage_frac"]
	if offer == nil {
		t.Fatal("no offer")
	}
	if _, err := store.ObservePolicyRejected(ctx, host.id, offer.Group, offer.Revision, offer.ContentSHA256, "cross_key_invalid"); err != nil {
		t.Fatal(err)
	}
	expireObligations(t, pool, host.id)
	if again := host.offers(t, store); again["abr_ladder_res_engage_frac"] != nil {
		t.Fatal("an invalid request was offered again")
	}
	if err := store.RetryPolicyGroup(ctx, host.id, "abr_ladder_res_engage_frac"); !errors.Is(err, ErrPolicyNotRetryable) {
		t.Fatalf("retry of an invalid request = %v", err)
	}
	view, _ := store.GetPolicy(ctx, host.id)
	if _, err := store.DecoratePolicyGroups(ctx, host.id, &view); err != nil {
		t.Fatal(err)
	}
	if g := view.Groups["abr_ladder_res_engage_frac"]; g.Status != "failed" || g.Remedy == nil || !strings.HasPrefix(*g.Remedy, "validation_failed:") {
		t.Fatalf("invalid group view = %+v", g)
	}
	// A changed deployment baseline is a relevant condition: the group resumes.
	baseline := deploymentBaseline()
	baseline["abr_ladder_res_engage_frac"] = 0.5
	if _, err := pool.Exec(ctx, `UPDATE hosts SET deployment_settings=$2::jsonb WHERE id=$1::uuid`, host.id, mustJSON(t, baseline)); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := refreshDeploymentDigests(ctx, tx, host.id, host.connection); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if resumed := host.offers(t, store)["abr_ladder_res_engage_frac"]; resumed == nil || resumed.ResolvedSettings["abr_ladder_res_engage_frac"] != 0.5 {
		t.Fatalf("changed baseline did not resume: %+v", resumed)
	}
}

func TestStaleRejectionCannotFailANewerChoice(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	host := newTypedHost(t, pool, "gop")
	save(t, store, host.id, map[string]PolicyChoice{"gop": {Source: "explicit", Value: float64(90)}})
	old := host.offers(t, store)["gop"]
	save(t, store, host.id, map[string]PolicyChoice{"gop": {Source: "explicit", Value: float64(120)}})
	if changed, err := store.ObservePolicyRejected(ctx, host.id, "gop", old.Revision, old.ContentSHA256, "invalid_value"); err != nil || changed {
		t.Fatalf("stale rejection changed state: %v %v", changed, err)
	}
	if offer := host.offers(t, store)["gop"]; offer == nil || offer.ResolvedSettings["gop"] != float64(120) {
		t.Fatalf("newer choice blocked: %+v", offer)
	}
}

func TestReconcileSnapshotsPerGroup(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	host := newTypedHost(t, pool, "gop", "slices")
	save(t, store, host.id, map[string]PolicyChoice{"gop": {Source: "explicit", Value: float64(90)}, "slices": {Source: "explicit", Value: float64(4)}})
	offers := host.offers(t, store)
	for _, group := range []string{"gop", "slices"} {
		if ok, err := store.ObservePolicyApplied(ctx, host.id, group, offers[group].Revision, offers[group].ContentSHA256, "next_session", host.connection); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	next := "44444444-4444-4444-8444-444444444444"
	if err := store.InvalidateNextSessionEvidenceOnReconnect(ctx, host.id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET config_policy_gate_connection=$2::uuid WHERE id=$1::uuid`, host.id, next); err != nil {
		t.Fatal(err)
	}
	snapshots := map[string]PolicySnapshot{
		"gop":    {Kind: "verified", Digest: offers["gop"].ContentSHA256},
		"slices": {Kind: "seeded", Digest: offers["slices"].ContentSHA256},
	}
	if err := store.ReconcilePolicySnapshots(ctx, host.id, next, snapshots); err != nil {
		t.Fatal(err)
	}
	view, _ := store.GetPolicy(ctx, host.id)
	got := []string{view.Groups["gop"].Status, view.Groups["slices"].Status}
	if strings.Join(got, ",") != "applied,pending" {
		t.Fatalf("reconciled statuses = %v (a seed is never applied proof)", got)
	}
}

func TestPolicyEditContextReadsMountAndExistingHomes(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	host := newTypedHost(t, pool, "home_root")
	var userID, appID string
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash) VALUES ('rh05-336@example.test','rh05336','x') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO apps (name) VALUES ('rh05-336 app') RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_homes WHERE user_id=$1::uuid`, userID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM apps WHERE id=$1::uuid`, appID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1::uuid`, userID)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO user_homes (user_id, app_id, host_id, provider, ref) VALUES ($1::uuid,$2::uuid,$3::uuid,'local','/srv/homes/u1/app')`, userID, appID, host.id); err != nil {
		t.Fatal(err)
	}
	policyCtx, err := store.PolicyEditContext(ctx, host.id)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(policyCtx.ExistingHomeRefs)
	if policyCtx.MountedHomeRoot != "/srv/homes" || strings.Join(policyCtx.ExistingHomeRefs, ",") != "/srv/homes/u1/app" || policyCtx.Baseline == nil {
		t.Fatalf("context = %+v", policyCtx)
	}
	assertPolicyCode(t, ValidatePolicyEdit(map[string]PolicyChoice{"home_root": {Source: "explicit", Value: "/srv/homes/u2"}}, policyCtx), "home_conflict")
}

// agent-api.md §RH05: on deployment_baseline_changed the agent's fresh
// capacity precedes the rejection. When the stored baseline still resolves
// the group to the rejected offer's fact, it is invalidated; the rejection
// never spends the retry budget; an independent group is untouched.
func TestBaselineChangedRejectionInvalidatesAMatchingStoredBaseline(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	host := newTypedHost(t, pool, "gop", "slices")
	save(t, store, host.id, map[string]PolicyChoice{"gop": {Source: "deployment"}, "slices": {Source: "explicit", Value: float64(4)}})
	if _, err := pool.Exec(ctx, `UPDATE host_reconcile_obligations SET retry_count=$2 WHERE host_id=$1::uuid AND resource_key='gop'`, host.id, policyRetryBudget-1); err != nil {
		t.Fatal(err)
	}
	offers := host.offers(t, store)
	gop, slices := offers["gop"], offers["slices"]
	if gop == nil || slices == nil {
		t.Fatalf("offers = %v", offers)
	}
	if ok, err := store.ObservePolicyApplied(ctx, host.id, "slices", slices.Revision, slices.ContentSHA256, "next_session", host.connection); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// The fresh report changes only an unrelated key: gop's projection still
	// hashes to the rejected fact, so the report cannot be what the agent had.
	fresh := deploymentBaseline()
	fresh["slices"] = float64(3)
	if err := store.ObserveDeploymentSettings(ctx, host.id, host.connection, mustJSON(t, fresh)); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.ObservePolicyRejected(ctx, host.id, "gop", gop.Revision, gop.ContentSHA256, "deployment_baseline_changed"); err != nil || !changed {
		t.Fatalf("rejection = %v %v", changed, err)
	}
	if baseline, err := store.DeploymentSettingsForConnection(ctx, host.id, host.connection); err != nil || baseline != nil {
		t.Fatalf("matching stored baseline survived: %v %v", baseline, err)
	}
	expireObligations(t, pool, host.id)
	if again := host.offers(t, store); len(again) != 0 {
		t.Fatalf("offered from invalidated evidence: %v", again)
	}
	view, err := store.GetPolicy(ctx, host.id)
	if err != nil {
		t.Fatal(err)
	}
	if view.Groups["gop"].Status != "pending" || view.Groups["slices"].Status != "applied" {
		t.Fatalf("status gop=%s slices=%s", view.Groups["gop"].Status, view.Groups["slices"].Status)
	}
	// The agent's baseline flips back: the same projection re-resolves the
	// same candidate, and the rejected attempt did not exhaust the budget.
	if err := store.ObserveDeploymentSettings(ctx, host.id, host.connection, mustJSON(t, deploymentBaseline())); err != nil {
		t.Fatal(err)
	}
	resumed := host.offers(t, store)["gop"]
	if resumed == nil || resumed.ContentSHA256 != gop.ContentSHA256 {
		t.Fatalf("re-offer after fresh evidence = %+v", resumed)
	}
}

// A newer capacity already ingested before the rejection is authoritative:
// it is kept, and the group re-offers from it at once without spending budget.
func TestBaselineChangedRejectionKeepsANewerIngestedBaseline(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	host := newTypedHost(t, pool, "gop")
	save(t, store, host.id, map[string]PolicyChoice{"gop": {Source: "deployment"}})
	if _, err := pool.Exec(ctx, `UPDATE host_reconcile_obligations SET retry_count=$2 WHERE host_id=$1::uuid AND resource_key='gop'`, host.id, policyRetryBudget-1); err != nil {
		t.Fatal(err)
	}
	stale := host.offers(t, store)["gop"]
	if stale == nil {
		t.Fatal("no offer")
	}
	newer := deploymentBaseline()
	newer["gop"] = float64(90)
	if err := store.ObserveDeploymentSettings(ctx, host.id, host.connection, mustJSON(t, newer)); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.ObservePolicyRejected(ctx, host.id, "gop", stale.Revision, stale.ContentSHA256, "deployment_baseline_changed"); err != nil || changed {
		t.Fatalf("stale rejection changed state: %v %v", changed, err)
	}
	if baseline, err := store.DeploymentSettingsForConnection(ctx, host.id, host.connection); err != nil || baseline["gop"] != float64(90) {
		t.Fatalf("newer baseline lost: %v %v", baseline, err)
	}
	if offer := host.offers(t, store)["gop"]; offer == nil || offer.ResolvedSettings["gop"] != float64(90) {
		t.Fatalf("re-offer from newer baseline = %+v", offer)
	}
}
