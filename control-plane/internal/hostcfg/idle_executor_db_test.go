package hostcfg

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/jobs"
)

// The operator sees a waiting approval until both durable inventories are
// current. Dispatch writes the frozen offer before transport can see it;
// neither that write nor a duplicate dispatch claims application.
func TestIdleExecutorOffersOnlyAfterFreshIdleAndKeepsStatusUnapplied(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "hardware")
	ctx := context.Background()
	if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{
		"encoder": {Source: "explicit", Value: "openh264"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	boot, err := store.StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completeEmptyHostJournal(t, store, hostID)
	preview, err := store.PreviewIdleApply(ctx, hostID, "hardware")
	if err != nil || preview == nil || !preview.Available {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	approved, err := store.ApproveIdleApply(ctx, hostID, "hardware", reviewedIdleApply(preview))
	if err != nil {
		t.Fatal(err)
	}
	connection := "00000000-0000-4000-8000-000000000338"
	if offer, err := store.NextIdleOffer(ctx, hostID, boot, connection); err != nil || offer != nil {
		t.Fatalf("unknown inventory offered: %+v %v", offer, err)
	}
	if err := store.ObserveIdleHeartbeat(ctx, hostID, connection, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET source_preparation='{"steam":{"images":[]}}'::jsonb,
		source_preparation_reported_at=now(),last_registered_at=now()-interval '1 minute' WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	offer, err := store.NextIdleOffer(ctx, hostID, boot, connection)
	if err != nil || offer == nil {
		t.Fatalf("fresh idle did not offer: %+v %v", offer, err)
	}
	if offer.AttemptID != approved.AttemptID || offer.Scope != "restart" ||
		offer.ContentSHA256 != preview.ContentSHA256 || offer.PrerequisitesSHA256 != preview.PrerequisitesSHA256 {
		t.Fatalf("offer changed reviewed content: %+v", offer)
	}
	if repeated, err := store.NextIdleOffer(ctx, hostID, boot, connection); err != nil || repeated != nil {
		t.Fatalf("offer repeated without new approval: %+v %v", repeated, err)
	}
	for retry := 0; retry < 3; retry++ {
		if _, err := pool.Exec(ctx, `UPDATE host_reconcile_obligations SET next_attempt_at=now()-interval '1 second'
			WHERE host_id=$1::uuid AND kind='idle_apply' AND resource_key=$2`, hostID, approved.AttemptID); err != nil {
			t.Fatal(err)
		}
		repeated, err := store.NextIdleOffer(ctx, hostID, boot, connection)
		if err != nil || repeated == nil || repeated.AttemptID != offer.AttemptID || repeated.ContentSHA256 != offer.ContentSHA256 ||
			repeated.PrerequisitesSHA256 != offer.PrerequisitesSHA256 {
			t.Fatalf("retry %d changed grant: %+v %v", retry, repeated, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE host_reconcile_obligations SET next_attempt_at=now()-interval '1 second'
		WHERE host_id=$1::uuid AND kind='idle_apply' AND resource_key=$2`, hostID, approved.AttemptID); err != nil {
		t.Fatal(err)
	}
	if fourth, err := store.NextIdleOffer(ctx, hostID, boot, connection); err != nil || fourth != nil {
		t.Fatalf("unbounded delivery: %+v %v", fourth, err)
	}
	status, err := store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || status.Phase != "offered" || status.Started || !status.AdmissionRestricted || status.Remedy == nil {
		t.Fatalf("send was mistaken for durable application: %+v %v", status, err)
	}
}

func TestIdleExecutorDistinguishesScheduledFromActiveHostJobs(t *testing.T) {
	for _, tc := range []struct {
		name, state, schedule string
		wantOffer             bool
		disabled              bool
	}{
		{"future cleanup", "pending", "1 hour", true, false},
		{"due cleanup", "pending", "-1 minute", false, false},
		{"running cleanup", "running", "1 hour", false, false},
		{"disabled due cleanup", "pending", "-1 minute", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			store := NewStore(pool)
			hostID := seedHost(t, pool)
			confirmPolicyGroups(t, pool, hostID, "hardware")
			ctx := context.Background()
			if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{
				"encoder": {Source: "explicit", Value: "openh264"},
			}, nil); err != nil {
				t.Fatal(err)
			}
			boot, err := store.StartRH05Boot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			completeEmptyHostJournal(t, store, hostID)
			preview, err := store.PreviewIdleApply(ctx, hostID, "hardware")
			if err != nil || preview == nil || !preview.Available {
				t.Fatalf("preview: %+v %v", preview, err)
			}
			approved, err := store.ApproveIdleApply(ctx, hostID, "hardware", reviewedIdleApply(preview))
			if err != nil {
				t.Fatal(err)
			}
			connection := "00000000-0000-4000-8000-000000000338"
			if err := store.ObserveIdleHeartbeat(ctx, hostID, connection, []string{}); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE hosts SET source_preparation='{"steam":{"images":[]}}'::jsonb,
				source_preparation_reported_at=now(),last_registered_at=now()-interval '1 minute' WHERE id=$1::uuid`, hostID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO jobs(id,name,plane,scope,schedule_kind)
				VALUES('rh05.test.cleanup','Managed home cleanup','agent','host','manual') ON CONFLICT(id) DO NOTHING`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE jobs SET enabled=$1 WHERE id='rh05.test.cleanup'`, !tc.disabled); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO job_runs(job_id,host_id,state,trigger,scheduled_for)
				VALUES('rh05.test.cleanup',$1::uuid,$2,'schedule',now()+$3::interval)`, hostID, tc.state, tc.schedule); err != nil {
				t.Fatal(err)
			}
			status, err := store.GetIdleApply(ctx, hostID, approved.AttemptID)
			if err != nil || status.Remedy == nil {
				t.Fatalf("status: %+v %v", status, err)
			}
			blocked := strings.Contains(*status.Remedy, "conflicting preparation or cleanup")
			if blocked == tc.wantOffer || strings.Contains(*status.Remedy, "Execution support is unavailable") {
				t.Fatalf("job wait reason: %q, want offer %v", *status.Remedy, tc.wantOffer)
			}
			offer, err := store.NextIdleOffer(ctx, hostID, boot, connection)
			if err != nil || (offer != nil) != tc.wantOffer {
				t.Fatalf("offer: %+v %v, want offer %v", offer, err, tc.wantOffer)
			}
			claimed, err := jobs.NewStore(pool).ClaimDue(ctx, jobs.ClaimOptions{
				Plane: jobs.PlaneAgent, HostID: hostID, Now: time.Now().Add(2 * time.Hour), Limit: 5,
			})
			if err != nil {
				t.Fatal(err)
			}
			ownClaims := 0
			for _, run := range claimed {
				if run.HostID == hostID {
					ownClaims++
				}
			}
			wantClaims := 0
			if tc.state == "pending" && !tc.wantOffer && !tc.disabled {
				wantClaims = 1
			}
			if ownClaims != wantClaims {
				t.Fatalf("scheduler claimed %d host jobs after offer=%v, disabled=%v; want %d", ownClaims, tc.wantOffer, tc.disabled, wantClaims)
			}
		})
	}
}

func TestIdleOfferFenceKeepsOtherHostJobsIndependent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	first := seedHost(t, pool)
	var second string
	if err := pool.QueryRow(ctx, `INSERT INTO hosts(node_name,status) VALUES('h2','online') RETURNING id::text`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs(id,name,plane,scope,schedule_kind)
		VALUES('rh05.test.admission.fence','Host cleanup','agent','host','manual') ON CONFLICT(id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	for _, hostID := range []string{first, second} {
		if _, err := pool.Exec(ctx, `INSERT INTO job_runs(job_id,host_id,state,trigger,scheduled_for)
			VALUES('rh05.test.admission.fence',$1::uuid,'pending','event',now()-interval '1 minute')`, hostID); err != nil {
			t.Fatal(err)
		}
	}
	owner, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Rollback(ctx) //nolint:errcheck
	if _, err := owner.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::uuid::text,339))`, first); err != nil {
		t.Fatal(err)
	}
	claim := func(hostID string, want int) {
		t.Helper()
		runs, err := jobs.NewStore(pool).ClaimDue(ctx, jobs.ClaimOptions{Plane: jobs.PlaneAgent, HostID: hostID})
		if err != nil || len(runs) != want {
			t.Fatalf("host %s claimed %d jobs, want %d: %v", hostID, len(runs), want, err)
		}
	}
	claim(first, 0)
	claim(second, 1)
	if err := owner.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	claim(first, 1)
}

func TestIdleExecutorExpiresNeverOfferedApprovalWithoutTouchingOtherHolds(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "hardware")
	ctx := context.Background()
	if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{
		"encoder": {Source: "explicit", Value: "openh264"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	boot, err := store.StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completeEmptyHostJournal(t, store, hostID)
	preview, err := store.PreviewIdleApply(ctx, hostID, "hardware")
	if err != nil || preview == nil || !preview.Available {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	approved, err := store.ApproveIdleApply(ctx, hostID, "hardware", reviewedIdleApply(preview))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO host_admission_restrictions(host_id,owner_kind,owner_id,reason)
		VALUES($1::uuid,'manual','00000000-0000-0000-0000-000000000000'::uuid,'manual_drain')`, hostID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE host_config_approvals SET expires_at=now()-interval '1 second'
		WHERE id=$1::uuid`, approved.AttemptID); err != nil {
		t.Fatal(err)
	}
	connection := "00000000-0000-4000-8000-000000000338"
	if offer, err := store.NextIdleOffer(ctx, hostID, boot, connection); err != nil || offer != nil {
		t.Fatalf("expired approval offered: %+v %v", offer, err)
	}
	status, err := store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || status.Phase != "revoked_unstarted" || status.Started || status.AdmissionRestricted {
		t.Fatalf("expired approval kept its restriction: %+v %v", status, err)
	}
	var hostStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM hosts WHERE id=$1::uuid`, hostID).Scan(&hostStatus); err != nil || hostStatus != "draining" {
		t.Fatalf("other owner's drain changed: %s %v", hostStatus, err)
	}
	policy, err := store.GetPolicy(ctx, hostID)
	if err != nil || policy.Groups["hardware"].Status != "pending" {
		t.Fatalf("saved setting lost on approval expiry: %+v %v", policy.Groups["hardware"], err)
	}
}

func TestIdleExecutorPerDeliveryFailureStaysProtectedUntilJournal(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "hardware")
	ctx := context.Background()
	if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{
		"encoder": {Source: "explicit", Value: "openh264"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	boot, err := store.StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completeEmptyHostJournal(t, store, hostID)
	preview, err := store.PreviewIdleApply(ctx, hostID, "hardware")
	if err != nil || preview == nil || !preview.Available {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	approved, err := store.ApproveIdleApply(ctx, hostID, "hardware", reviewedIdleApply(preview))
	if err != nil {
		t.Fatal(err)
	}
	connection := "00000000-0000-4000-8000-000000000338"
	if err := store.ObserveIdleHeartbeat(ctx, hostID, connection, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET source_preparation='{"steam":{"images":[]}}'::jsonb,
		source_preparation_reported_at=now(),last_registered_at=now()-interval '1 minute' WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	offer, err := store.NextIdleOffer(ctx, hostID, boot, connection)
	if err != nil || offer == nil {
		t.Fatalf("offer: %+v %v", offer, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE host_config_approvals SET expires_at=now()-interval '1 second'
		WHERE id=$1::uuid`, approved.AttemptID); err != nil {
		t.Fatal(err)
	}
	expiredOffer, err := store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || expiredOffer.Phase != "offered" || !expiredOffer.AdmissionRestricted ||
		expiredOffer.Remedy == nil || !strings.Contains(*expiredOffer.Remedy, "authenticated agent journal") || expiredOffer.NextRetryAt != nil {
		t.Fatalf("expired offer did not explain protected wait: %+v %v", expiredOffer, err)
	}
	rejected := IdleJournalState{AttemptID: offer.AttemptID, Group: "hardware", Revision: offer.Revision,
		Digest: offer.ContentSHA256, GrantBoot: boot, GrantConnection: connection,
		Phase: "failed", Sequence: "0", ErrorCode: "prerequisite_mismatch"}
	if fresh, err := store.ObserveIdleState(ctx, hostID, connection, rejected); err != nil || fresh {
		t.Fatalf("authenticated rejection: %v %v", fresh, err)
	}
	status, err := store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || status.Phase != "offered" || status.Started || !status.AdmissionRestricted ||
		status.Remedy == nil || !strings.Contains(*status.Remedy, "authenticated journal") {
		t.Fatalf("per-delivery rejection released admission: %+v %v", status, err)
	}
	var approvalState string
	if err := pool.QueryRow(ctx, `SELECT state FROM host_config_approvals WHERE id=$1::uuid`, approved.AttemptID).Scan(&approvalState); err != nil || approvalState != "offered" {
		t.Fatalf("per-delivery rejection changed approval: %s %v", approvalState, err)
	}
	opened, err := store.BeginCurrentJournalRefresh(ctx, hostID, connection)
	if err != nil || !opened {
		t.Fatalf("current-connection refresh: opened=%v err=%v", opened, err)
	}
	if err := store.CompleteJournalReconciliation(ctx, hostID, connection, nil,
		map[string]PolicySnapshot{"hardware": {Kind: "seeded", Digest: strings.Repeat("a", 64)}}); err != nil {
		t.Fatal(err)
	}
	status, err = store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || status.Phase != "revoked_unstarted" || status.Started || status.AdmissionRestricted {
		t.Fatalf("complete absent journal did not close offer: %+v %v", status, err)
	}
	next, err := store.PreviewIdleApply(ctx, hostID, "hardware")
	if err != nil || next == nil || !next.Available || next.ApprovalReviewID == preview.ApprovalReviewID {
		t.Fatalf("new review unavailable after rejection: %+v %v", next, err)
	}
	third := "00000000-0000-4000-8000-000000000340"
	if err := store.BeginJournalReconciliation(ctx, hostID, third); err != nil {
		t.Fatal(err)
	}
	contradictory := JournalInventoryEntry{AttemptID: offer.AttemptID, HostID: hostID, Group: "hardware",
		Digest: offer.ContentSHA256, Scope: "restart", Phase: "accepted", Sequence: "1",
		Revision: offer.Revision, GrantBoot: boot, GrantConnection: connection}
	if err := store.CompleteJournalReconciliation(ctx, hostID, third, []JournalInventoryEntry{contradictory},
		map[string]PolicySnapshot{"hardware": {Kind: "seeded", Digest: strings.Repeat("a", 64)}}); err != nil {
		t.Fatal(err)
	}
	gate, err := store.JournalGate(ctx, hostID)
	if err != nil || gate != "quarantined" {
		t.Fatalf("contradictory started record did not quarantine host: gate=%s err=%v", gate, err)
	}
	if reopened, err := store.BeginCurrentJournalRefresh(ctx, hostID, third); err != nil || reopened {
		t.Fatalf("quarantined journal was reopened: opened=%v err=%v", reopened, err)
	}
	var held bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM host_admission_restrictions WHERE host_id=$1::uuid)`, hostID).Scan(&held); err != nil || !held {
		t.Fatalf("contradictory journal left admission open: held=%v err=%v", held, err)
	}
}

func TestIdleExecutorReconnectClosesUnstartedFailureAndApprovalTogether(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "hardware")
	ctx := context.Background()
	if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{
		"encoder": {Source: "explicit", Value: "openh264"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	boot, err := store.StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completeEmptyHostJournal(t, store, hostID)
	preview, err := store.PreviewIdleApply(ctx, hostID, "hardware")
	if err != nil || preview == nil || !preview.Available {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	approved, err := store.ApproveIdleApply(ctx, hostID, "hardware", reviewedIdleApply(preview))
	if err != nil {
		t.Fatal(err)
	}
	first := "00000000-0000-4000-8000-000000000338"
	if err := store.ObserveIdleHeartbeat(ctx, hostID, first, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET source_preparation='{"steam":{"images":[]}}'::jsonb,
		source_preparation_reported_at=now(),last_registered_at=now()-interval '1 minute' WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	offer, err := store.NextIdleOffer(ctx, hostID, boot, first)
	if err != nil || offer == nil {
		t.Fatalf("offer: %+v %v", offer, err)
	}
	second := "00000000-0000-4000-8000-000000000339"
	if err := store.BeginJournalReconciliation(ctx, hostID, second); err != nil {
		t.Fatal(err)
	}
	entry := JournalInventoryEntry{AttemptID: offer.AttemptID, HostID: hostID, Group: "hardware",
		Digest: offer.ContentSHA256, Scope: "restart", Phase: "failed", Sequence: "0",
		Revision: offer.Revision, GrantBoot: boot, GrantConnection: first,
		ErrorCode: "prerequisite_mismatch"}
	if err := store.CompleteJournalReconciliation(ctx, hostID, second, []JournalInventoryEntry{entry},
		map[string]PolicySnapshot{"hardware": {Kind: "seeded", Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}); err != nil {
		t.Fatal(err)
	}
	status, err := store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || status.Phase != "failed" || status.Started || status.AdmissionRestricted {
		t.Fatalf("reconnected rejection status: %+v %v", status, err)
	}
	var approvalState string
	if err := pool.QueryRow(ctx, `SELECT state FROM host_config_approvals WHERE id=$1::uuid`, approved.AttemptID).Scan(&approvalState); err != nil || approvalState != "revoked_unstarted" {
		t.Fatalf("reconnected rejection kept approval live: %s %v", approvalState, err)
	}
	next, err := store.PreviewIdleApply(ctx, hostID, "hardware")
	if err != nil || next == nil || !next.Available || next.ApprovalReviewID == preview.ApprovalReviewID {
		t.Fatalf("new review unavailable after reconnect rejection: %+v %v", next, err)
	}
}

// A socket send is only an offer. The authenticated journal can then report
// acceptance, repeat it, arrive out of order, and finally prove startup.
func TestIdleExecutorJournalAdvancesOnlyOneOwnedAttempt(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "hardware")
	ctx := context.Background()
	if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{
		"encoder": {Source: "explicit", Value: "openh264"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	boot, err := store.StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completeEmptyHostJournal(t, store, hostID)
	preview, err := store.PreviewIdleApply(ctx, hostID, "hardware")
	if err != nil || preview == nil || !preview.Available {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	approved, err := store.ApproveIdleApply(ctx, hostID, "hardware", reviewedIdleApply(preview))
	if err != nil {
		t.Fatal(err)
	}
	connection := "00000000-0000-4000-8000-000000000338"
	if err := store.ObserveIdleHeartbeat(ctx, hostID, connection, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET source_preparation='{"steam":{"images":[]}}'::jsonb,
		source_preparation_reported_at=now(),last_registered_at=now()-interval '1 minute' WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	offer, err := store.NextIdleOffer(ctx, hostID, boot, connection)
	if err != nil || offer == nil {
		t.Fatalf("offer: %+v %v", offer, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO host_admission_restrictions(host_id,owner_kind,owner_id,reason)
		VALUES($1::uuid,'manual','00000000-0000-0000-0000-000000000000'::uuid,'manual_drain')`, hostID); err != nil {
		t.Fatal(err)
	}
	state := IdleJournalState{AttemptID: offer.AttemptID, Group: "hardware", Revision: offer.Revision,
		Digest: offer.ContentSHA256, GrantBoot: boot, GrantConnection: connection, Phase: "accepted", Sequence: "1"}
	busy := state
	busy.Phase = "failed"
	busy.Sequence = "0"
	busy.ErrorCode = "host_busy"
	if _, err := pool.Exec(ctx, `UPDATE host_reconcile_obligations
		SET retry_count=3,next_attempt_at=now()-interval '1 second'
		WHERE host_id=$1::uuid AND kind='idle_apply' AND resource_key=$2`, hostID, approved.AttemptID); err != nil {
		t.Fatal(err)
	}
	if fresh, err := store.ObserveIdleState(ctx, hostID, connection, busy); err != nil || fresh {
		t.Fatalf("local busy must keep waiting: %v %v", fresh, err)
	}
	var retryCount int
	var retryAt time.Time
	if err := pool.QueryRow(ctx, `SELECT retry_count,next_attempt_at FROM host_reconcile_obligations
		WHERE host_id=$1::uuid AND kind='idle_apply' AND resource_key=$2`, hostID, approved.AttemptID).
		Scan(&retryCount, &retryAt); err != nil || retryCount != 0 || !retryAt.After(time.Now()) {
		t.Fatalf("authenticated busy consumed delivery budget: count=%d next=%s err=%v", retryCount, retryAt, err)
	}
	busyStatus, err := store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || busyStatus.Phase != "offered" || !busyStatus.AdmissionRestricted {
		t.Fatalf("local busy lost approval: %+v %v", busyStatus, err)
	}
	rejected := busy
	rejected.ErrorCode = "prerequisite_mismatch"
	if fresh, err := store.ObserveIdleState(ctx, hostID, connection, rejected); err != nil || fresh {
		t.Fatalf("per-delivery rejection must keep waiting: %v %v", fresh, err)
	}
	if fresh, err := store.ObserveIdleState(ctx, hostID, connection, state); err != nil || !fresh {
		t.Fatalf("accepted after earlier delivery rejection: %v %v", fresh, err)
	}
	status, err := store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || !status.Started || status.Phase != "accepted" || !status.AdmissionRestricted {
		t.Fatalf("acceptance status: %+v %v", status, err)
	}
	if fresh, err := store.ObserveIdleState(ctx, hostID, connection, state); err != nil || fresh {
		t.Fatalf("repeat: %v %v", fresh, err)
	}
	state.Phase = "awaiting_startup"
	state.Sequence = "2"
	if fresh, err := store.ObserveIdleState(ctx, hostID, connection, state); err != nil || !fresh {
		t.Fatalf("awaiting: %v %v", fresh, err)
	}
	state.Phase = "accepted"
	state.Sequence = "1"
	if fresh, err := store.ObserveIdleState(ctx, hostID, connection, state); err != nil || fresh {
		t.Fatalf("stale: %v %v", fresh, err)
	}
	state.Phase = "applied"
	state.Sequence = "3"
	now := time.Now().UTC()
	state.VerifiedAt = &now
	if fresh, err := store.ObserveIdleState(ctx, hostID, connection, state); err != nil || !fresh {
		t.Fatalf("applied: %v %v", fresh, err)
	}
	status, err = store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || status.Phase != "applied" || status.AdmissionRestricted {
		t.Fatalf("applied status: %+v %v", status, err)
	}
	var hostStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM hosts WHERE id=$1::uuid`, hostID).Scan(&hostStatus); err != nil || hostStatus != "draining" {
		t.Fatalf("another owner's drain was released: %s %v", hostStatus, err)
	}
	view, err := store.GetPolicy(ctx, hostID)
	if err != nil || view.Groups["hardware"].Status != "applied" {
		t.Fatalf("policy: %+v %v", view.Groups["hardware"], err)
	}
}

func TestIdleExecutorReconstructsLostAcceptedReportFromAuthenticatedInventory(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "hardware")
	ctx := context.Background()
	if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{"encoder": {Source: "explicit", Value: "openh264"}}, nil); err != nil {
		t.Fatal(err)
	}
	boot, err := store.StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completeEmptyHostJournal(t, store, hostID)
	preview, err := store.PreviewIdleApply(ctx, hostID, "hardware")
	if err != nil || preview == nil || !preview.Available {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	approved, err := store.ApproveIdleApply(ctx, hostID, "hardware", reviewedIdleApply(preview))
	if err != nil {
		t.Fatal(err)
	}
	first := "00000000-0000-4000-8000-000000000338"
	if err := store.ObserveIdleHeartbeat(ctx, hostID, first, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET source_preparation='{"steam":{"images":[]}}'::jsonb,
		source_preparation_reported_at=now(),last_registered_at=now()-interval '1 minute' WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	offer, err := store.NextIdleOffer(ctx, hostID, boot, first)
	if err != nil || offer == nil {
		t.Fatalf("offer: %+v %v", offer, err)
	}
	second := "00000000-0000-4000-8000-000000000339"
	if err := store.BeginJournalReconciliation(ctx, hostID, second); err != nil {
		t.Fatal(err)
	}
	entry := JournalInventoryEntry{AttemptID: offer.AttemptID, HostID: hostID, Group: "hardware", Digest: offer.ContentSHA256,
		Scope: "restart", Phase: "awaiting_startup", Sequence: "2", Revision: offer.Revision, GrantBoot: boot, GrantConnection: first}
	if err := store.CompleteJournalReconciliation(ctx, hostID, second, []JournalInventoryEntry{entry}); err != nil {
		t.Fatal(err)
	}
	gate, err := store.JournalGate(ctx, hostID)
	if err != nil || gate != "complete" {
		t.Fatalf("reconciliation gate: %s %v", gate, err)
	}
	status, err := store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || status.Phase != "awaiting_startup" || !status.Started || !status.AdmissionRestricted {
		t.Fatalf("reconstructed status: %+v %v", status, err)
	}
	if next, err := store.NextIdleOffer(ctx, hostID, boot, second); err != nil || next != nil {
		t.Fatalf("second offer escaped open attempt: %+v %v", next, err)
	}
	uncertain := IdleJournalState{AttemptID: offer.AttemptID, Group: "hardware", Revision: offer.Revision,
		Digest: offer.ContentSHA256, GrantBoot: boot, GrantConnection: first, Phase: "uncertain", Sequence: "3", ErrorCode: "recovery_verification_failed"}
	if fresh, err := store.ObserveIdleState(ctx, hostID, second, uncertain); err != nil || !fresh {
		t.Fatalf("uncertain: %v %v", fresh, err)
	}
	status, err = store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || status.Phase != "uncertain" || !status.AdmissionRestricted || status.Remedy == nil ||
		!strings.Contains(*status.Remedy, "recovery_verification_failed") {
		t.Fatalf("uncertain protection: %+v %v", status, err)
	}
	var recoveryHolds int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM host_admission_restrictions WHERE host_id=$1::uuid
		AND owner_kind='recovery' AND owner_id=$2::uuid`, hostID, approved.AttemptID).Scan(&recoveryHolds); err != nil || recoveryHolds != 1 {
		t.Fatalf("recovery hold missing: %d %v", recoveryHolds, err)
	}
}

func TestHistoricalAppliedInventoryCannotClaimAChangedActiveSnapshot(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "hardware")
	ctx := context.Background()
	if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{"encoder": {Source: "explicit", Value: "openh264"}}, nil); err != nil {
		t.Fatal(err)
	}
	boot, err := store.StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completeEmptyHostJournal(t, store, hostID)
	preview, err := store.PreviewIdleApply(ctx, hostID, "hardware")
	if err != nil || preview == nil || !preview.Available {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	approved, err := store.ApproveIdleApply(ctx, hostID, "hardware", reviewedIdleApply(preview))
	if err != nil {
		t.Fatal(err)
	}
	first := "00000000-0000-4000-8000-000000000338"
	if err := store.ObserveIdleHeartbeat(ctx, hostID, first, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET source_preparation='{"steam":{"images":[]}}'::jsonb,
		source_preparation_reported_at=now(),last_registered_at=now()-interval '1 minute' WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	offer, err := store.NextIdleOffer(ctx, hostID, boot, first)
	if err != nil || offer == nil {
		t.Fatalf("offer: %+v %v", offer, err)
	}
	second := "00000000-0000-4000-8000-000000000339"
	if err := store.BeginJournalReconciliation(ctx, hostID, second); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	entry := JournalInventoryEntry{AttemptID: offer.AttemptID, HostID: hostID, Group: "hardware", Digest: offer.ContentSHA256,
		Scope: "restart", Phase: "applied", Sequence: "4", Revision: offer.Revision, GrantBoot: boot, GrantConnection: first, VerifiedAt: &now}
	active := map[string]PolicySnapshot{"hardware": {Kind: "verified", Digest: strings.Repeat("a", 64)}}
	if active["hardware"].Digest == offer.ContentSHA256 {
		t.Fatal("fixture must differ from candidate")
	}
	if err := store.CompleteJournalReconciliation(ctx, hostID, second, []JournalInventoryEntry{entry}, active); err != nil {
		t.Fatal(err)
	}
	status, err := store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || status.Phase != "applied" || status.AdmissionRestricted {
		t.Fatalf("historical attempt: %+v %v", status, err)
	}
	view, err := store.GetPolicy(ctx, hostID)
	if err != nil || view.Groups["hardware"].Status == "applied" {
		t.Fatalf("historical record falsely claimed current application: %+v %v", view.Groups["hardware"], err)
	}
}
