package hostcfg

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// uncertainRecoveryHold leaves one started hardware attempt uncertain on a host
// whose journal (on offerConnection) is complete: it holds a recovery restriction.
func uncertainRecoveryHold(t *testing.T, pool *pgxpool.Pool, store *Store, hostID string) (string, *PolicyOffer) {
	t.Helper()
	ctx := context.Background()
	boot, offer := offerRestartAttempt(t, pool, store, hostID)
	if _, err := store.ObserveIdleState(ctx, hostID, offerConnection, liveReport(boot, offer, "accepted", "1")); err != nil {
		t.Fatal(err)
	}
	if fresh, err := store.ObserveIdleState(ctx, hostID, offerConnection, liveReport(boot, offer, "uncertain", "2")); err != nil || !fresh {
		t.Fatalf("uncertain: %v %v", fresh, err)
	}
	if got := hostStatus(t, pool, hostID); got != "draining" {
		t.Fatalf("uncertain attempt left the host %s, want draining", got)
	}
	return boot, offer
}

// releasedReport is what the agent sends after it writes `released`: the next
// sequence, the record's grant identity and its original error (policy.rs release).
func releasedReport(boot string, offer *PolicyOffer) IdleJournalState {
	state := liveReport(boot, offer, "released", "3")
	state.ErrorCode = "candidate_verification_failed"
	return state
}

func holdKinds(t *testing.T, pool *pgxpool.Pool, hostID string) string {
	t.Helper()
	var kinds string
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(string_agg(owner_kind,',' ORDER BY owner_kind),'')
		FROM host_admission_restrictions WHERE host_id=$1::uuid`, hostID).Scan(&kinds); err != nil {
		t.Fatal(err)
	}
	return kinds
}

func hardwareGroupStatus(t *testing.T, pool *pgxpool.Pool, hostID string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM host_setting_groups WHERE host_id=$1::uuid AND group_key='hardware'`, hostID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func advertiseRecoveryRelease(t *testing.T, pool *pgxpool.Pool, hostID string, on bool) {
	t.Helper()
	versions := `{"typed_settings":2,"execution_journal":1,"deployment_baseline":1,"idle_apply":1}`
	if on {
		versions = `{"typed_settings":2,"execution_journal":1,"deployment_baseline":1,"idle_apply":1,"recovery_release":1}`
	}
	if _, err := pool.Exec(context.Background(), `UPDATE hosts SET config_policy_versions=$2::jsonb WHERE id=$1::uuid`, hostID, versions); err != nil {
		t.Fatal(err)
	}
}

// A request lifts nothing. The hold, the group and the host move only when the
// agent reports `released` (#656, seat B M1/M2).
func TestARecoveryReleaseLiftsNothingUntilTheAgentReportsIt(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	boot, offer := uncertainRecoveryHold(t, pool, store, hostID)
	advertiseRecoveryRelease(t, pool, hostID, true)

	if first, err := store.RequestRecoveryRelease(ctx, hostID, offer.AttemptID); err != nil || !first {
		t.Fatalf("request: first=%v err=%v", first, err)
	}
	if first, err := store.RequestRecoveryRelease(ctx, hostID, offer.AttemptID); err != nil || first {
		t.Fatalf("repeat: first=%v err=%v, want an idempotent repeat", first, err)
	}
	if got := holdKinds(t, pool, hostID); got != "recovery" {
		t.Fatalf("holds %q while pending, want recovery", got)
	}
	if got := hostStatus(t, pool, hostID); got != "draining" {
		t.Fatalf("host %s while pending, want draining", got)
	}
	if got := hardwareGroupStatus(t, pool, hostID); got != "uncertain" {
		t.Fatalf("group %s while pending, want uncertain (the restart guards key on it)", got)
	}
	view, err := store.GetIdleApply(ctx, hostID, offer.AttemptID)
	if err != nil || view.Phase != "uncertain" || !view.AdmissionRestricted || view.RecoveryRelease == nil ||
		view.RecoveryRelease.State != "pending" || view.RecoveryRelease.RequestedAt == nil ||
		view.RecoveryRelease.WaitingFor == nil || *view.RecoveryRelease.WaitingFor != "agent_report" {
		t.Fatalf("pending view: %+v %+v %v", view, view.RecoveryRelease, err)
	}
	if ids, err := store.PendingRecoveryReleases(ctx, hostID, offerConnection); err != nil || len(ids) != 1 || ids[0] != offer.AttemptID {
		t.Fatalf("pending on the reconciled connection = %v (%v)", ids, err)
	}
	if ids, err := store.PendingRecoveryReleases(ctx, hostID, "00000000-0000-4000-8000-000000000999"); err != nil || len(ids) != 0 {
		t.Fatalf("pending on another connection = %v (%v), want none", ids, err)
	}

	reviewID := func() string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `SELECT review_id::text FROM host_approval_review_tokens WHERE host_id=$1::uuid AND group_key='hardware'`, hostID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	before := reviewID()
	if fresh, err := store.ObserveIdleState(ctx, hostID, offerConnection, releasedReport(boot, offer)); err != nil || !fresh {
		t.Fatalf("released report: fresh=%v err=%v", fresh, err)
	}
	if reviewID() == before {
		t.Fatal("the resolved hold left the approval review ID in place")
	}
	if got := holdKinds(t, pool, hostID); got != "" {
		t.Fatalf("holds %q after released, want none", got)
	}
	if got := hostStatus(t, pool, hostID); got != "online" {
		t.Fatalf("host %s after released, want online", got)
	}
	if got := hardwareGroupStatus(t, pool, hostID); got != "failed" {
		t.Fatalf("group %s after released, want failed", got)
	}
	view, err = store.GetIdleApply(ctx, hostID, offer.AttemptID)
	if err != nil || view.Phase != "released" || view.AdmissionRestricted || view.RecoveryRelease == nil ||
		view.RecoveryRelease.State != "released" || view.RecoveryRelease.WaitingFor != nil {
		t.Fatalf("released view: %+v %+v %v", view, view.RecoveryRelease, err)
	}
	if ids, _ := store.PendingRecoveryReleases(ctx, hostID, offerConnection); len(ids) != 0 {
		t.Fatalf("still pending after released: %v", ids)
	}
	if first, err := store.RequestRecoveryRelease(ctx, hostID, offer.AttemptID); err != nil || first {
		t.Fatalf("request after released: first=%v err=%v", first, err)
	}
	if preview, err := store.PreviewIdleApply(ctx, hostID, "hardware"); err != nil || preview == nil {
		t.Fatalf("a released host offers no fresh review: %+v %v", preview, err)
	}
}

// The released report recomputes the host as admission.Release does (#656 A1/N1).
func TestAReleasedReportRecomputesTheHostLikeAdmissionRelease(t *testing.T) {
	for name, tc := range map[string]struct {
		setup      string
		wantStatus string
		wantKinds  string
	}{
		"another owner remains": {setup: `INSERT INTO host_admission_restrictions(host_id,owner_kind,owner_id,reason)
			VALUES($1::uuid,'manual','00000000-0000-0000-0000-000000000000','manual_drain')`, wantStatus: "draining", wantKinds: "manual"},
		"an online host with a lingering hold": {setup: `WITH m AS (INSERT INTO host_admission_restrictions(host_id,owner_kind,owner_id,reason)
			VALUES($1::uuid,'manual','00000000-0000-0000-0000-000000000000','manual_drain'))
			UPDATE hosts SET status='online' WHERE id=$1::uuid`, wantStatus: "draining", wantKinds: "manual"},
		"a recorded disconnect": {setup: `UPDATE hosts SET agent_disconnected_at=now() WHERE id=$1::uuid`, wantStatus: "offline"},
	} {
		t.Run(name, func(t *testing.T) {
			pool := testPool(t)
			store := NewStore(pool)
			hostID := seedHost(t, pool)
			ctx := context.Background()
			boot, offer := uncertainRecoveryHold(t, pool, store, hostID)
			if _, err := store.RequestRecoveryRelease(ctx, hostID, offer.AttemptID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, tc.setup, hostID); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ObserveIdleState(ctx, hostID, offerConnection, releasedReport(boot, offer)); err != nil {
				t.Fatal(err)
			}
			if got := hostStatus(t, pool, hostID); got != tc.wantStatus {
				t.Fatalf("host %s, want %s", got, tc.wantStatus)
			}
			if got := holdKinds(t, pool, hostID); got != tc.wantKinds {
				t.Fatalf("holds %q, want %q", got, tc.wantKinds)
			}
		})
	}
}

// The agent never prunes its journal: a reconnect replays `uncertain` (the hold
// stays and the release is sent again) until the agent's journal says `released`.
func TestAReconnectKeepsThePendingReleaseAndTakesTheReleasedEntry(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	boot, offer := uncertainRecoveryHold(t, pool, store, hostID)
	if _, err := store.RequestRecoveryRelease(ctx, hostID, offer.AttemptID); err != nil {
		t.Fatal(err)
	}
	entry := func(phase, seq string) JournalInventoryEntry {
		return JournalInventoryEntry{AttemptID: offer.AttemptID, HostID: hostID, Group: "hardware", Digest: offer.ContentSHA256,
			Scope: "restart", Phase: phase, Sequence: seq, Revision: offer.Revision, GrantBoot: boot, GrantConnection: offerConnection,
			ErrorCode: "candidate_verification_failed"}
	}
	second := "00000000-0000-4000-8000-000000000339"
	if err := store.BeginJournalReconciliation(ctx, hostID, second); err != nil {
		t.Fatal(err)
	}
	if ids, _ := store.PendingRecoveryReleases(ctx, hostID, second); len(ids) != 0 {
		t.Fatalf("sent before reconciliation: %v", ids)
	}
	if err := store.CompleteJournalReconciliation(ctx, hostID, second, []JournalInventoryEntry{entry("uncertain", "2")}, seededHardware); err != nil {
		t.Fatal(err)
	}
	if got := holdKinds(t, pool, hostID); got != "recovery" {
		t.Fatalf("holds %q after an uncertain replay, want recovery", got)
	}
	if ids, err := store.PendingRecoveryReleases(ctx, hostID, second); err != nil || len(ids) != 1 {
		t.Fatalf("pending after reconnect = %v (%v), want the release to send again", ids, err)
	}

	third := "00000000-0000-4000-8000-00000000033a"
	if err := store.BeginJournalReconciliation(ctx, hostID, third); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteJournalReconciliation(ctx, hostID, third, []JournalInventoryEntry{entry("released", "3")}, seededHardware); err != nil {
		t.Fatal(err)
	}
	if gate, err := store.JournalGate(ctx, hostID); err != nil || gate != "complete" {
		t.Fatalf("gate %s (%v), want complete", gate, err)
	}
	if got := holdKinds(t, pool, hostID); got != "" {
		t.Fatalf("holds %q after the released entry, want none", got)
	}
	if got := hostStatus(t, pool, hostID); got != "online" {
		t.Fatalf("host %s, want online", got)
	}
	if got := hardwareGroupStatus(t, pool, hostID); got != "failed" {
		t.Fatalf("group %s, want failed", got)
	}
	// An exact replay of the released entry changes nothing and does not quarantine.
	fourth := "00000000-0000-4000-8000-00000000033b"
	if err := store.BeginJournalReconciliation(ctx, hostID, fourth); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteJournalReconciliation(ctx, hostID, fourth, []JournalInventoryEntry{entry("released", "3")}, seededHardware); err != nil {
		t.Fatal(err)
	}
	if gate, _ := store.JournalGate(ctx, hostID); gate != "complete" {
		t.Fatalf("a released replay left the gate %s", gate)
	}
	if got := holdKinds(t, pool, hostID); got != "" {
		t.Fatalf("a released replay planted %q", got)
	}
}

func TestOnlyAnUncertainAttemptTakesARelease(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	boot, offer := offerRestartAttempt(t, pool, store, hostID)
	if _, err := store.ObserveIdleState(ctx, hostID, offerConnection, liveReport(boot, offer, "accepted", "1")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestRecoveryRelease(ctx, hostID, offer.AttemptID); !errors.Is(err, ErrAttemptNotUncertain) {
		t.Fatalf("accepted attempt: %v, want ErrAttemptNotUncertain", err)
	}
	if _, err := store.RequestRecoveryRelease(ctx, hostID, "00000000-0000-4000-8000-0000000003ff"); !errors.Is(err, ErrIdleAttemptNotFound) {
		t.Fatalf("unknown attempt: %v", err)
	}
	if _, err := store.RequestRecoveryRelease(ctx, "00000000-0000-4000-8000-0000000003fe", offer.AttemptID); !errors.Is(err, ErrIdleAttemptNotFound) {
		t.Fatalf("another host: %v", err)
	}
	// An uncertain attempt nobody asked to release has nothing to send.
	if _, err := store.ObserveIdleState(ctx, hostID, offerConnection, liveReport(boot, offer, "uncertain", "2")); err != nil {
		t.Fatal(err)
	}
	if ids, _ := store.PendingRecoveryReleases(ctx, hostID, offerConnection); len(ids) != 0 {
		t.Fatalf("pending without a request: %v", ids)
	}
}

// A database restored from before the request holds the attempt uncertain with no
// request while the agent's journal says released. The agent writes released only on
// the release command, so the inventory entry is taken and the host returns
// (control-api.md "Releasing a recovery hold").
func TestARestoredDatabaseTakesAReleasedEntryWithoutARequest(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	boot, offer := uncertainRecoveryHold(t, pool, store, hostID)
	second := "00000000-0000-4000-8000-000000000339"
	if err := store.BeginJournalReconciliation(ctx, hostID, second); err != nil {
		t.Fatal(err)
	}
	released := JournalInventoryEntry{AttemptID: offer.AttemptID, HostID: hostID, Group: "hardware", Digest: offer.ContentSHA256,
		Scope: "restart", Phase: "released", Sequence: "3", Revision: offer.Revision, GrantBoot: boot, GrantConnection: offerConnection,
		ErrorCode: "candidate_verification_failed"}
	if err := store.CompleteJournalReconciliation(ctx, hostID, second, []JournalInventoryEntry{released}, seededHardware); err != nil {
		t.Fatal(err)
	}
	if gate, _ := store.JournalGate(ctx, hostID); gate != "complete" {
		t.Fatalf("gate %s, want complete", gate)
	}
	if got := holdKinds(t, pool, hostID); got != "" {
		t.Fatalf("holds %q, want none", got)
	}
	if got := hostStatus(t, pool, hostID); got != "online" {
		t.Fatalf("host %s, want online", got)
	}
	view, err := store.GetIdleApply(ctx, hostID, offer.AttemptID)
	if err != nil || view.Phase != "released" || view.RecoveryRelease == nil || view.RecoveryRelease.State != "released" || view.RecoveryRelease.RequestedAt != nil {
		t.Fatalf("view: %+v %+v %v", view, view.RecoveryRelease, err)
	}
}

// What the pending release waits for, as the console shows it (#656, item 4).
func TestAPendingReleaseSaysWhatItWaitsFor(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	_, offer := uncertainRecoveryHold(t, pool, store, hostID)
	if _, err := store.RequestRecoveryRelease(ctx, hostID, offer.AttemptID); err != nil {
		t.Fatal(err)
	}
	waiting := func() string {
		t.Helper()
		view, err := store.GetIdleApply(ctx, hostID, offer.AttemptID)
		if err != nil || view.RecoveryRelease == nil || view.RecoveryRelease.WaitingFor == nil {
			t.Fatalf("view: %+v %v", view, err)
		}
		return *view.RecoveryRelease.WaitingFor
	}
	advertiseRecoveryRelease(t, pool, hostID, false)
	if got := waiting(); got != "agent_upgrade" {
		t.Fatalf("an agent without recovery_release: %s", got)
	}
	advertiseRecoveryRelease(t, pool, hostID, true)
	if got := waiting(); got != "agent_report" {
		t.Fatalf("a capable reconciled agent: %s", got)
	}
	confirmPolicyGroups(t, pool, hostID, "idle_timeout_secs")
	if got := waiting(); got != "agent_connection" {
		t.Fatalf("a capable agent without the hardware grant is sent nothing: %s", got)
	}
	confirmPolicyGroups(t, pool, hostID, "hardware")
	if _, err := pool.Exec(ctx, `UPDATE hosts SET agent_disconnected_at=now() WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	if got := waiting(); got != "agent_connection" {
		t.Fatalf("a disconnected host: %s", got)
	}
}

type releaseDispatcher struct {
	fakeDispatcher
	capable bool
}

func (d *releaseDispatcher) PolicyIdentity(string) (string, string, bool) {
	return "00000000-0000-4000-8000-0000000003bb", offerConnection, true
}
func (d *releaseDispatcher) SupportsRecoveryRelease(string) bool { return d.capable }

type recordedActivity struct{ action, targetType, targetID string }

type fakeAuditor struct{ rows []recordedActivity }

func (f *fakeAuditor) Record(_ context.Context, _ string, action, targetType, targetID string, _ map[string]any) error {
	f.rows = append(f.rows, recordedActivity{action, targetType, targetID})
	return nil
}

func TestReleaseRecoveryHoldEndpoint(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	boot, offer := uncertainRecoveryHold(t, pool, store, hostID)
	auditor := &fakeAuditor{}
	dispatcher := &releaseDispatcher{}
	mux := http.NewServeMux()
	pass := func(next http.Handler) http.Handler { return next }
	NewHandler(store, dispatcher, nil, auditor).Register(mux, pass, pass)
	post := func(host, attempt, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/hosts/"+host+"/idle-apply/"+attempt+"/release-recovery", strings.NewReader(body)))
		return w
	}
	const ack = `{"acknowledge":"unverified"}`
	for _, body := range []string{``, `{}`, `{"acknowledge":"verified"}`, `{"acknowledge":"unverified","x":1}`, ack + `{}`} {
		if w := post(hostID, offer.AttemptID, body); w.Code != http.StatusBadRequest {
			t.Fatalf("body %q: %d %s, want 400", body, w.Code, w.Body.String())
		}
	}
	if w := post("not-a-uuid", offer.AttemptID, ack); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed host id: %d, want 400", w.Code)
	}
	if w := post(hostID, "00000000-0000-4000-8000-0000000003ff", ack); w.Code != http.StatusNotFound {
		t.Fatalf("unknown attempt: %d, want 404", w.Code)
	}
	var marked bool
	if err := pool.QueryRow(context.Background(), `SELECT release_requested_at IS NOT NULL FROM host_config_attempts WHERE id=$1::uuid`, offer.AttemptID).Scan(&marked); err != nil || marked || len(auditor.rows) != 0 {
		t.Fatalf("a refused request wrote something: marked=%v audit=%v err=%v", marked, auditor.rows, err)
	}

	// An agent without the capability: recorded and pending, nothing sent.
	w := post(hostID, offer.AttemptID, ack)
	var view IdleApplyAttempt
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.RecoveryRelease == nil || view.RecoveryRelease.State != "pending" {
		t.Fatalf("request: %d %s, want 202 pending", w.Code, w.Body.String())
	}
	if len(dispatcher.sent) != 0 {
		t.Fatalf("sent to an agent without recovery_release: %v", dispatcher.sent)
	}
	if len(auditor.rows) != 1 || auditor.rows[0] != (recordedActivity{"host.recovery.release", "host", hostID}) {
		t.Fatalf("audit %v", auditor.rows)
	}
	// A capable agent on the reconciled connection: a repeat sends it, audited once.
	dispatcher.capable = true
	if w := post(hostID, offer.AttemptID, ack); w.Code != http.StatusAccepted {
		t.Fatalf("repeat: %d %s", w.Code, w.Body.String())
	}
	if len(auditor.rows) != 1 {
		t.Fatalf("a repeat audited again: %v", auditor.rows)
	}
	if len(dispatcher.sent) != 1 {
		t.Fatalf("sent %v, want one release", dispatcher.sent)
	}
	if cmd, ok := dispatcher.sent[0].(RecoveryReleaseCmd); !ok || cmd != NewRecoveryReleaseCmd(offer.AttemptID, "00000000-0000-4000-8000-0000000003bb", offerConnection) {
		t.Fatalf("sent %+v", dispatcher.sent[0])
	}
	if _, err := store.ObserveIdleState(context.Background(), hostID, offerConnection, releasedReport(boot, offer)); err != nil {
		t.Fatal(err)
	}
	if w := post(hostID, offer.AttemptID, ack); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"phase":"released"`) {
		t.Fatalf("after released: %d %s, want 200", w.Code, w.Body.String())
	}
	if len(dispatcher.sent) != 1 {
		t.Fatalf("a released attempt was sent again: %v", dispatcher.sent)
	}
	if fresh, _ := store.ObserveIdleState(context.Background(), hostID, offerConnection, liveReport(boot, offer, "uncertain", "2")); fresh {
		t.Fatal("a released attempt took an older report")
	}
	if got := holdKinds(t, pool, hostID); got != "" {
		t.Fatalf("an older uncertain report re-planted %q", got)
	}
}

func TestReleaseRecoveryHoldEndpointRefusesANonUncertainAttempt(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	boot, offer := offerRestartAttempt(t, pool, store, hostID)
	if _, err := store.ObserveIdleState(context.Background(), hostID, offerConnection, liveReport(boot, offer, "accepted", "1")); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	pass := func(next http.Handler) http.Handler { return next }
	NewHandler(store, &releaseDispatcher{capable: true}, nil).Register(mux, pass, pass)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/hosts/"+hostID+"/idle-apply/"+offer.AttemptID+"/release-recovery",
		strings.NewReader(`{"acknowledge":"unverified"}`)))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "attempt_not_uncertain") {
		t.Fatalf("accepted attempt: %d %s, want 409", w.Code, w.Body.String())
	}
}
