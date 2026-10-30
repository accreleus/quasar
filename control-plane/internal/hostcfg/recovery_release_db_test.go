package hostcfg

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// uncertainRecoveryHold leaves one started hardware attempt uncertain on a host
// whose journal gate is complete: the attempt holds a recovery restriction.
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

// The explicit repair deletes only that attempt's recovery row, marks its group
// failed, and projects the host from the holds left and the connection (#656).
func TestReleasingARecoveryHoldRecomputesTheHostStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		connected  bool
		manual     bool
		wantStatus string
		wantKinds  string
	}{
		"connected, last hold":    {connected: true, wantStatus: "online"},
		"disconnected, last hold": {connected: false, wantStatus: "offline"},
		"another owner remains":   {connected: true, manual: true, wantStatus: "draining", wantKinds: "manual"},
	} {
		t.Run(name, func(t *testing.T) {
			pool := testPool(t)
			store := NewStore(pool)
			hostID := seedHost(t, pool)
			ctx := context.Background()
			_, offer := uncertainRecoveryHold(t, pool, store, hostID)
			if tc.manual {
				if _, err := pool.Exec(ctx, `INSERT INTO host_admission_restrictions(host_id,owner_kind,owner_id,reason)
					VALUES($1::uuid,'manual','00000000-0000-0000-0000-000000000000','manual_drain')`, hostID); err != nil {
					t.Fatal(err)
				}
			}
			status, err := store.ReleaseRecoveryHold(ctx, hostID, offer.AttemptID, tc.connected)
			if err != nil || status != tc.wantStatus {
				t.Fatalf("release = %q, %v; want %q", status, err, tc.wantStatus)
			}
			if got := hostStatus(t, pool, hostID); got != tc.wantStatus {
				t.Fatalf("host row status %s, want %s", got, tc.wantStatus)
			}
			if got := holdKinds(t, pool, hostID); got != tc.wantKinds {
				t.Fatalf("holds left %q, want %q", got, tc.wantKinds)
			}
			if got := hardwareGroupStatus(t, pool, hostID); got != "failed" {
				t.Fatalf("group status %s, want failed", got)
			}
			attempt, err := store.GetIdleApply(ctx, hostID, offer.AttemptID)
			if err != nil || attempt.Phase != "uncertain" || attempt.AdmissionRestricted {
				t.Fatalf("released attempt: %+v %v", attempt, err)
			}
		})
	}
}

// The agent never prunes its journal, so every reconnect replays the uncertain
// entry. A released hold must stay released; an unreleased one is still rebuilt.
func TestAJournalReplayDoesNotReplantAReleasedRecoveryHold(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	boot, offer := uncertainRecoveryHold(t, pool, store, hostID)
	if _, err := store.ReleaseRecoveryHold(ctx, hostID, offer.AttemptID, true); err != nil {
		t.Fatal(err)
	}
	second := "00000000-0000-4000-8000-000000000339"
	if err := store.BeginJournalReconciliation(ctx, hostID, second); err != nil {
		t.Fatal(err)
	}
	entry := JournalInventoryEntry{AttemptID: offer.AttemptID, HostID: hostID, Group: "hardware", Digest: offer.ContentSHA256,
		Scope: "restart", Phase: "uncertain", Sequence: "2", Revision: offer.Revision, GrantBoot: boot, GrantConnection: offerConnection}
	if err := store.CompleteJournalReconciliation(ctx, hostID, second, []JournalInventoryEntry{entry}, seededHardware); err != nil {
		t.Fatal(err)
	}
	if gate, err := store.JournalGate(ctx, hostID); err != nil || gate != "complete" {
		t.Fatalf("journal gate = %s (%v), want complete", gate, err)
	}
	if got := holdKinds(t, pool, hostID); got != "" {
		t.Fatalf("replay re-planted %q", got)
	}
	if got := hostStatus(t, pool, hostID); got != "online" {
		t.Fatalf("host %s after replay, want online", got)
	}
}

func TestReleasingARecoveryHoldRefusesUntilTheJournalIsReconciled(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	_, offer := uncertainRecoveryHold(t, pool, store, hostID)
	if err := store.BeginJournalReconciliation(ctx, hostID, "00000000-0000-4000-8000-000000000339"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReleaseRecoveryHold(ctx, hostID, offer.AttemptID, true); !errors.Is(err, ErrJournalNotReconciled) {
		t.Fatalf("release during reconciliation: %v, want ErrJournalNotReconciled", err)
	}
	if got := holdKinds(t, pool, hostID); got != "reconciliation,recovery" {
		t.Fatalf("holds %q, want reconciliation,recovery untouched", got)
	}
	if got := hardwareGroupStatus(t, pool, hostID); got != "uncertain" {
		t.Fatalf("group status %s, want uncertain", got)
	}
	var released bool
	if err := pool.QueryRow(ctx, `SELECT recovery_released_at IS NOT NULL FROM host_config_attempts WHERE id=$1::uuid`, offer.AttemptID).Scan(&released); err != nil || released {
		t.Fatalf("refused release still marked the attempt: %v %v", released, err)
	}
}

func TestReleasingARecoveryHoldNeedsOne(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	_, offer := uncertainRecoveryHold(t, pool, store, hostID)
	if _, err := store.ReleaseRecoveryHold(ctx, hostID, "00000000-0000-4000-8000-0000000003ff", true); !errors.Is(err, ErrRecoveryHoldNotFound) {
		t.Fatalf("unknown attempt: %v", err)
	}
	if _, err := store.ReleaseRecoveryHold(ctx, hostID, offer.AttemptID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReleaseRecoveryHold(ctx, hostID, offer.AttemptID, true); !errors.Is(err, ErrRecoveryHoldNotFound) {
		t.Fatalf("second release: %v", err)
	}
	if _, err := store.ReleaseRecoveryHold(ctx, "00000000-0000-4000-8000-0000000003fe", offer.AttemptID, true); !errors.Is(err, ErrHostNotFound) {
		t.Fatalf("unknown host: %v", err)
	}
}

type connectedDispatcher struct{ fakeDispatcher }

func (connectedDispatcher) IsConnected(string) bool { return true }

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
	_, offer := uncertainRecoveryHold(t, pool, store, hostID)
	auditor := &fakeAuditor{}
	mux := http.NewServeMux()
	pass := func(next http.Handler) http.Handler { return next }
	NewHandler(store, &connectedDispatcher{}, nil, auditor).Register(mux, pass, pass)
	url := "/v1/admin/hosts/" + hostID + "/idle-apply/" + offer.AttemptID + "/release-recovery"
	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, url, strings.NewReader(body)))
		return w
	}
	for _, body := range []string{``, `{}`, `{"acknowledge":"verified"}`, `{"acknowledge":"unverified","x":1}`} {
		if w := post(body); w.Code != http.StatusBadRequest {
			t.Fatalf("body %q: %d %s, want 400", body, w.Code, w.Body.String())
		}
	}
	if got := holdKinds(t, pool, hostID); got != "recovery" || len(auditor.rows) != 0 {
		t.Fatalf("a refused request changed something: holds %q, audit %v", got, auditor.rows)
	}
	w := post(`{"acknowledge":"unverified"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"host_status":"online"`) || !strings.Contains(w.Body.String(), `"admission_restricted":false`) {
		t.Fatalf("release: %d %s", w.Code, w.Body.String())
	}
	if len(auditor.rows) != 1 || auditor.rows[0] != (recordedActivity{"host.recovery.release", "host", hostID}) {
		t.Fatalf("audit %v", auditor.rows)
	}
	if w := post(`{"acknowledge":"unverified"}`); w.Code != http.StatusNotFound {
		t.Fatalf("repeat: %d %s, want 404", w.Code, w.Body.String())
	}
}

func TestReleaseRecoveryHoldEndpointRefusesDuringReconciliation(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	_, offer := uncertainRecoveryHold(t, pool, store, hostID)
	if err := store.BeginJournalReconciliation(context.Background(), hostID, "00000000-0000-4000-8000-000000000339"); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	pass := func(next http.Handler) http.Handler { return next }
	NewHandler(store, &connectedDispatcher{}, nil).Register(mux, pass, pass)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/hosts/"+hostID+"/idle-apply/"+offer.AttemptID+"/release-recovery",
		strings.NewReader(`{"acknowledge":"unverified"}`)))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "journal_reconciliation_pending") {
		t.Fatalf("release during reconciliation: %d %s, want 409", w.Code, w.Body.String())
	}
}
