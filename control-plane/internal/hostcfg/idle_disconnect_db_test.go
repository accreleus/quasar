package hostcfg

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const offerConnection = "00000000-0000-4000-8000-000000000338"

var seededHardware = map[string]PolicySnapshot{"hardware": {Kind: "seeded", Digest: strings.Repeat("a", 64)}}

// offerRestartAttempt leaves one hardware attempt offered on offerConnection,
// holding its idle_apply restriction.
func offerRestartAttempt(t *testing.T, pool *pgxpool.Pool, store *Store, hostID string) (string, *PolicyOffer) {
	t.Helper()
	ctx := context.Background()
	confirmPolicyGroups(t, pool, hostID, "hardware")
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
	if _, err := store.ApproveIdleApply(ctx, hostID, "hardware", reviewedIdleApply(preview)); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveIdleHeartbeat(ctx, hostID, offerConnection, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET source_preparation='{"steam":{"images":[]}}'::jsonb,
		source_preparation_reported_at=now(),last_registered_at=now()-interval '1 minute' WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	offer, err := store.NextIdleOffer(ctx, hostID, boot, offerConnection)
	if err != nil || offer == nil {
		t.Fatalf("offer: %+v %v", offer, err)
	}
	return boot, offer
}

func liveReport(boot string, offer *PolicyOffer, phase, sequence string) IdleJournalState {
	return IdleJournalState{AttemptID: offer.AttemptID, Group: "hardware", Revision: offer.Revision,
		Digest: offer.ContentSHA256, GrantBoot: boot, GrantConnection: offerConnection, Phase: phase, Sequence: sequence}
}

func hostStatus(t *testing.T, pool *pgxpool.Pool, hostID string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM hosts WHERE id=$1::uuid`, hostID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// Each writer that lifts a draining host's last restriction projects it from
// the recorded disconnect, never online for an agent that is gone (#656).
func TestReleasingTheLastHoldKeepsADisconnectedHostOffline(t *testing.T) {
	// What markOffline writes for a held host.
	stamp := func(t *testing.T, pool *pgxpool.Pool, hostID string) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), `UPDATE hosts SET status='draining',agent_disconnected_at=now() WHERE id=$1::uuid`, hostID); err != nil {
			t.Fatal(err)
		}
	}
	for name, release := range map[string]func(t *testing.T, pool *pgxpool.Pool, store *Store, hostID string) error{
		"policy save": func(t *testing.T, pool *pgxpool.Pool, store *Store, hostID string) error {
			confirmPolicyGroups(t, pool, hostID, "hardware")
			stamp(t, pool, hostID)
			_, err := store.SavePolicy(context.Background(), hostID, "0", map[string]PolicyChoice{"encoder": {Source: "explicit", Value: "openh264"}}, nil)
			return err
		},
		"hardware report": func(t *testing.T, pool *pgxpool.Pool, store *Store, hostID string) error {
			if _, err := store.StartRH05Boot(context.Background()); err != nil {
				t.Fatal(err)
			}
			completeEmptyHostJournal(t, store, hostID)
			stamp(t, pool, hostID)
			return store.ObserveHardwareReport(context.Background(), hostID, offerConnection,
				json.RawMessage(`[{"index":0,"vendor":"AMD","render_node":"/dev/dri/by-path/pci-0000:04:00.0-render","driver_identity":"vk:radv:test","encode_slots_total":1}]`),
				json.RawMessage(`[{"id":"media_probe_gpu0","status":"pass","source":"host_probe","observed_at":"2026-09-23T16:00:00Z"}]`))
		},
		"journal reconciliation": func(t *testing.T, pool *pgxpool.Pool, store *Store, hostID string) error {
			ctx := context.Background()
			if _, err := store.StartRH05Boot(ctx); err != nil {
				t.Fatal(err)
			}
			if err := store.BeginJournalReconciliation(ctx, hostID, offerConnection); err != nil {
				t.Fatal(err)
			}
			stamp(t, pool, hostID)
			return store.CompleteJournalReconciliation(ctx, hostID, offerConnection, nil, seededHardware)
		},
		"approval expiry": func(t *testing.T, pool *pgxpool.Pool, store *Store, hostID string) error {
			ctx := context.Background()
			confirmPolicyGroups(t, pool, hostID, "hardware")
			if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{"encoder": {Source: "explicit", Value: "openh264"}}, nil); err != nil {
				t.Fatal(err)
			}
			boot, err := store.StartRH05Boot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			completeEmptyHostJournal(t, store, hostID)
			preview, err := store.PreviewIdleApply(ctx, hostID, "hardware")
			if err != nil || preview == nil {
				t.Fatalf("preview: %+v %v", preview, err)
			}
			approved, err := store.ApproveIdleApply(ctx, hostID, "hardware", reviewedIdleApply(preview))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE host_config_approvals SET expires_at=now()-interval '1 second' WHERE id=$1::uuid`, approved.AttemptID); err != nil {
				t.Fatal(err)
			}
			stamp(t, pool, hostID)
			_, err = store.NextIdleOffer(ctx, hostID, boot, offerConnection)
			return err
		},
		"live terminal report": func(t *testing.T, pool *pgxpool.Pool, store *Store, hostID string) error {
			ctx := context.Background()
			boot, offer := offerRestartAttempt(t, pool, store, hostID)
			if _, err := store.ObserveIdleState(ctx, hostID, offerConnection, liveReport(boot, offer, "accepted", "1")); err != nil {
				t.Fatal(err)
			}
			stamp(t, pool, hostID)
			applied := liveReport(boot, offer, "applied", "2")
			now := time.Now().UTC()
			applied.VerifiedAt = &now
			_, err := store.ObserveIdleState(ctx, hostID, offerConnection, applied)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			pool := testPool(t)
			store := NewStore(pool)
			hostID := seedHost(t, pool)
			if err := release(t, pool, store, hostID); err != nil {
				t.Fatal(err)
			}
			var held bool
			if err := pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM host_admission_restrictions WHERE host_id=$1::uuid)`, hostID).Scan(&held); err != nil || held {
				t.Fatalf("a hold remains (%v), so nothing was released: %v", held, err)
			}
			if status := hostStatus(t, pool, hostID); status != "offline" {
				t.Fatalf("the last hold left a disconnected host %s, want offline", status)
			}
		})
	}
}

// A complete journal that closed a delivery as seq-0 failed released its hold;
// a later live report cannot restart that attempt (#656).
func TestALiveReportCannotReopenASeqZeroFailureTheJournalClosed(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	boot, offer := offerRestartAttempt(t, pool, store, hostID)
	second := "00000000-0000-4000-8000-000000000339"
	if err := store.BeginJournalReconciliation(ctx, hostID, second); err != nil {
		t.Fatal(err)
	}
	failed := JournalInventoryEntry{AttemptID: offer.AttemptID, HostID: hostID, Group: "hardware",
		Digest: offer.ContentSHA256, Scope: "restart", Phase: "failed", Sequence: "0",
		Revision: offer.Revision, GrantBoot: boot, GrantConnection: offerConnection, ErrorCode: "prerequisite_mismatch"}
	if err := store.CompleteJournalReconciliation(ctx, hostID, second, []JournalInventoryEntry{failed}, seededHardware); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ObserveIdleState(ctx, hostID, second, liveReport(boot, offer, "accepted", "1")); !errors.Is(err, ErrApprovalSuperseded) {
		t.Fatalf("live report on a closed attempt: %v, want ErrApprovalSuperseded", err)
	}
	status, err := store.GetIdleApply(ctx, hostID, offer.AttemptID)
	if err != nil || status.Phase != "failed" || status.Started {
		t.Fatalf("closed attempt moved: %+v %v", status, err)
	}
}

// An uncertain live report keeps the host restricted even when the ledger lost
// the attempt's idle_apply hold (#656).
func TestALiveUncertainReportRestoresAMissingRecoveryHold(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	boot, offer := offerRestartAttempt(t, pool, store, hostID)
	if _, err := store.ObserveIdleState(ctx, hostID, offerConnection, liveReport(boot, offer, "accepted", "1")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM host_admission_restrictions WHERE host_id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET status='online' WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	if fresh, err := store.ObserveIdleState(ctx, hostID, offerConnection, liveReport(boot, offer, "uncertain", "2")); err != nil || !fresh {
		t.Fatalf("uncertain: %v %v", fresh, err)
	}
	var recovery int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM host_admission_restrictions WHERE host_id=$1::uuid
		AND owner_kind='recovery' AND owner_id=$2::uuid`, hostID, offer.AttemptID).Scan(&recovery); err != nil || recovery != 1 {
		t.Fatalf("recovery holds = %d (%v), want 1", recovery, err)
	}
	if status := hostStatus(t, pool, hostID); status != "draining" {
		t.Fatalf("uncertain attempt left the host %s, want draining", status)
	}
}

// A restored ledger can omit both an unfinished attempt and its restriction.
// The journal rebuilds the attempt and its hold together (#656).
func TestAJournalRebuiltUnfinishedAttemptHoldsAdmission(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	boot, offer := offerRestartAttempt(t, pool, store, hostID)
	if _, err := pool.Exec(ctx, `DELETE FROM host_config_attempts WHERE id=$1::uuid`, offer.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM host_admission_restrictions WHERE host_id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	second := "00000000-0000-4000-8000-000000000339"
	if err := store.BeginJournalReconciliation(ctx, hostID, second); err != nil {
		t.Fatal(err)
	}
	entry := JournalInventoryEntry{AttemptID: offer.AttemptID, HostID: hostID, Group: "hardware", Digest: offer.ContentSHA256,
		Scope: "restart", Phase: "awaiting_startup", Sequence: "2", Revision: offer.Revision, GrantBoot: boot, GrantConnection: offerConnection}
	if err := store.CompleteJournalReconciliation(ctx, hostID, second, []JournalInventoryEntry{entry}, seededHardware); err != nil {
		t.Fatal(err)
	}
	status, err := store.GetIdleApply(ctx, hostID, offer.AttemptID)
	if err != nil || status.Phase != "awaiting_startup" || !status.AdmissionRestricted {
		t.Fatalf("rebuilt attempt: %+v %v", status, err)
	}
	if got := hostStatus(t, pool, hostID); got != "draining" {
		t.Fatalf("rebuilt unfinished attempt left the host %s, want draining", got)
	}
}

// While this connection's journal is pending, the journal is the record: a live
// report writes nothing, so it can neither close the attempt nor plant a recovery
// hold the journal then contradicts and quarantines on (#656).
func TestALiveReportBeforeTheJournalCompletesWritesNothing(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	boot, offer := offerRestartAttempt(t, pool, store, hostID)
	if _, err := store.ObserveIdleState(ctx, hostID, offerConnection, liveReport(boot, offer, "accepted", "1")); err != nil {
		t.Fatal(err)
	}
	second := "00000000-0000-4000-8000-000000000339"
	if err := store.BeginJournalReconciliation(ctx, hostID, second); err != nil {
		t.Fatal(err)
	}
	if fresh, err := store.ObserveIdleState(ctx, hostID, second, liveReport(boot, offer, "uncertain", "6")); err != nil || fresh {
		t.Fatalf("live report on a pending journal: fresh=%v err=%v, want no write", fresh, err)
	}
	entry := JournalInventoryEntry{AttemptID: offer.AttemptID, HostID: hostID, Group: "hardware", Digest: offer.ContentSHA256,
		Scope: "restart", Phase: "awaiting_startup", Sequence: "5", Revision: offer.Revision, GrantBoot: boot, GrantConnection: offerConnection}
	if err := store.CompleteJournalReconciliation(ctx, hostID, second, []JournalInventoryEntry{entry}, seededHardware); err != nil {
		t.Fatal(err)
	}
	if gate, err := store.JournalGate(ctx, hostID); err != nil || gate != "complete" {
		t.Fatalf("journal gate = %s (%v), want complete", gate, err)
	}
	var phase string
	var seq int64
	if err := pool.QueryRow(ctx, `SELECT phase,journal_sequence FROM host_config_attempts WHERE id=$1::uuid`, offer.AttemptID).Scan(&phase, &seq); err != nil || phase != "awaiting_startup" || seq != 5 {
		t.Fatalf("attempt = %s/%d (%v), want the journal's awaiting_startup/5", phase, seq, err)
	}
	var recovery, idle int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE owner_kind='recovery'),count(*) FILTER (WHERE owner_kind='idle_apply')
		FROM host_admission_restrictions WHERE host_id=$1::uuid AND owner_id=$2::uuid`, hostID, offer.AttemptID).Scan(&recovery, &idle); err != nil || recovery != 0 || idle != 1 {
		t.Fatalf("holds recovery=%d idle_apply=%d (%v), want 0 and 1", recovery, idle, err)
	}
}
