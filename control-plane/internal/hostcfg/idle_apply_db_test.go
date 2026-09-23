package hostcfg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/admission"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func reviewedIdleApply(p *ApprovalPreview) ApprovalReview {
	return ApprovalReview{ExpectedRevision: p.Revision, ContentSHA256: p.ContentSHA256,
		PrerequisitesSHA256: p.PrerequisitesSHA256, Prerequisites: p.Prerequisites,
		ApprovalBootIncarnation: p.ApprovalBootIncarnation, ApprovalReviewID: p.ApprovalReviewID,
		ExpiresAt: time.Now().UTC().Add(time.Hour)}
}

func TestIdleApprovalUsesOneConnectionForLockedReview(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "hardware")
	ctx := context.Background()
	if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{
		"encoder": {Source: "deployment"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRH05Boot(ctx); err != nil {
		t.Fatal(err)
	}
	completeEmptyHostJournal(t, store, hostID)
	if err := store.ObserveDeploymentSettings(ctx, hostID, "00000000-0000-4000-8000-000000000338", json.RawMessage(`{"encoder":"va"}`)); err != nil {
		t.Fatal(err)
	}
	config := pool.Config()
	config.MaxConns = 1
	one, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(one.Close)
	limited := NewStore(one)
	deadline, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	preview, err := limited.PreviewIdleApply(deadline, hostID, "hardware")
	if err != nil || preview == nil || !preview.Available {
		t.Fatalf("single-connection preview failed: %+v %v", preview, err)
	}
	approved, err := limited.ApproveIdleApply(deadline, hostID, "hardware", reviewedIdleApply(preview))
	if err != nil || approved.Phase != "waiting" || !approved.AdmissionRestricted {
		t.Fatalf("locked review borrowed a second connection or missed the grant: %+v %v", approved, err)
	}
}

func TestIdleApplyDeploymentUsesCurrentTypedBaseline(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "hardware")
	ctx := context.Background()
	if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{
		"encoder": {Source: "deployment"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRH05Boot(ctx); err != nil {
		t.Fatal(err)
	}
	completeEmptyHostJournal(t, store, hostID)
	preview, err := store.PreviewIdleApply(ctx, hostID, "hardware")
	if err != nil || preview != nil {
		t.Fatalf("missing baseline permitted approval: %+v %v", preview, err)
	}
	connection := "00000000-0000-4000-8000-000000000338"
	if err := store.ObserveDeploymentSettings(ctx, hostID, connection, json.RawMessage(`{"encoder":"va"}`)); err != nil {
		t.Fatal(err)
	}
	preview, err = store.PreviewIdleApply(ctx, hostID, "hardware")
	if err != nil || preview == nil || !preview.Available || preview.Resolved["encoder"] != "va" {
		t.Fatalf("current typed baseline did not resolve: %+v %v", preview, err)
	}
	found := false
	for _, fact := range preview.Prerequisites {
		found = found || fact.Kind == "deployment_baseline"
	}
	if !found {
		t.Fatalf("deployment fact absent: %+v", preview.Prerequisites)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET status='offline' WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	if unavailable, err := store.PreviewIdleApply(ctx, hostID, "hardware"); err != nil || unavailable != nil {
		t.Fatalf("offline host offered an approval candidate: %+v %v", unavailable, err)
	}
	if _, err := store.ApproveIdleApply(ctx, hostID, "hardware", reviewedIdleApply(preview)); !errors.Is(err, ErrApprovalSuperseded) {
		t.Fatalf("offline host accepted a reviewed approval: %v", err)
	}
}

func TestIdleApplyAutomaticUsesFencedAccessibleProbeEvidence(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "hardware")
	ctx := context.Background()
	if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{
		"encoder": {Source: "automatic"}, "render_node": {Source: "automatic"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRH05Boot(ctx); err != nil {
		t.Fatal(err)
	}
	completeEmptyHostJournal(t, store, hostID)
	if _, err := pool.Exec(ctx, `UPDATE hosts SET last_registered_at=now()-interval '1 minute' WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	connection := "00000000-0000-4000-8000-000000000338"
	gpus := json.RawMessage(`[{"index":0,"vendor":"AMD","render_node":"/dev/dri/by-path/pci-0000:04:00.0-render","driver_identity":"vk:radv:test","encode_slots_total":1}]`)
	passing := json.RawMessage(`[{"id":"media_probe_gpu0","status":"pass","source":"host_probe","observed_at":"2026-09-23T16:00:00Z"}]`)
	if err := store.ObserveHardwareReport(ctx, hostID, connection, gpus, passing); err != nil {
		t.Fatal(err)
	}
	preview, err := store.PreviewIdleApply(ctx, hostID, "hardware")
	if err != nil || preview == nil || !preview.Available ||
		preview.Resolved["encoder"] != "vulkan" ||
		preview.Resolved["render_node"] != "/dev/dri/by-path/pci-0000:04:00.0-render" {
		t.Fatalf("accessible passing GPU did not resolve Automatic: %+v %v", preview, err)
	}
	approved, err := store.ApproveIdleApply(ctx, hostID, "hardware", reviewedIdleApply(preview))
	if err != nil {
		t.Fatal(err)
	}
	repeatedPass := json.RawMessage(`[{"id":"media_probe_gpu0","status":"pass","source":"host_probe","observed_at":"2026-09-23T16:01:00Z"}]`)
	if err := store.ObserveHardwareReport(ctx, hostID, connection, gpus, repeatedPass); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || current.Phase != "waiting" || !current.AdmissionRestricted {
		t.Fatalf("same-device repeated pass superseded waiting approval: %+v %v", current, err)
	}
	failed := json.RawMessage(`[{"id":"media_probe_gpu0","status":"fail","source":"host_probe","observed_at":"2026-09-23T16:02:00Z"}]`)
	if err := store.ObserveHardwareReport(ctx, hostID, connection, gpus, failed); err != nil {
		t.Fatal(err)
	}
	current, err = store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || current.Phase != "revoked_unstarted" || current.AdmissionRestricted {
		t.Fatalf("failed latest probe retained old approval: %+v %v", current, err)
	}
	if err := store.ObserveHardwareReport(ctx, hostID, "00000000-0000-4000-8000-000000000999", gpus, passing); err != ErrApprovalSuperseded {
		t.Fatalf("stale socket refreshed hardware authority: %v", err)
	}
	if err := store.EndJournalConnection(ctx, hostID, connection); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveHardwareReport(ctx, hostID, connection, gpus, passing); err != ErrApprovalSuperseded {
		t.Fatalf("late disconnected capacity recreated authority: %v", err)
	}
	preview, err = store.PreviewIdleApply(ctx, hostID, "hardware")
	if err != nil || preview != nil {
		t.Fatalf("disconnected socket left Automatic preview available: %+v %v", preview, err)
	}
	newConnection := "00000000-0000-4000-8000-000000000339"
	if err := store.BeginJournalReconciliation(ctx, hostID, newConnection); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveHardwareReport(ctx, hostID, connection, gpus, passing); err != ErrApprovalSuperseded {
		t.Fatalf("old in-flight capacity overwrote new gate: %v", err)
	}
	if err := store.ObserveHardwareReport(ctx, hostID, newConnection, gpus, passing); err != nil {
		t.Fatal(err)
	}
	var projectedConnection string
	if err := pool.QueryRow(ctx, `SELECT connection_incarnation::text FROM host_hardware_evidence WHERE host_id=$1::uuid`, hostID).Scan(&projectedConnection); err != nil || projectedConnection != newConnection {
		t.Fatalf("projection did not bind new authenticated connection: %q %v", projectedConnection, err)
	}
}

func TestIdleApplyOperatorGrantRefreshAndCancel(t *testing.T) {
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
	if _, err := store.StartRH05Boot(ctx); err != nil {
		t.Fatal(err)
	}
	completeEmptyHostJournal(t, store, hostID)
	view, err := store.GetPolicy(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	review := reviewedIdleApply(view.Groups["hardware"].ApprovalPreview.(*ApprovalPreview))
	request := struct {
		Group string `json:"group"`
		ApprovalReview
	}{Group: "hardware", ApprovalReview: review}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(store, &fakeDispatcher{}, nil)
	mux := http.NewServeMux()
	h.Register(mux, func(next http.Handler) http.Handler { return next }, func(next http.Handler) http.Handler { return next })
	post := httptest.NewRequest(http.MethodPost, "/v1/admin/hosts/"+hostID+"/idle-apply", bytes.NewReader(body))
	grant := httptest.NewRecorder()
	mux.ServeHTTP(grant, post)
	if grant.Code != http.StatusAccepted {
		t.Fatalf("grant HTTP %d: %s", grant.Code, grant.Body.String())
	}
	var attempt IdleApplyAttempt
	if err := json.Unmarshal(grant.Body.Bytes(), &attempt); err != nil {
		t.Fatal(err)
	}
	if attempt.Phase != "waiting" || !attempt.AdmissionRestricted || attempt.Started {
		t.Fatalf("grant incorrectly claims execution: %+v", attempt)
	}
	path := "/v1/admin/hosts/" + hostID + "/idle-apply/" + attempt.AttemptID
	read := httptest.NewRecorder()
	mux.ServeHTTP(read, httptest.NewRequest(http.MethodGet, path, nil))
	if read.Code != http.StatusOK || !bytes.Contains(read.Body.Bytes(), []byte(`"phase":"waiting"`)) {
		t.Fatalf("refresh HTTP %d: %s", read.Code, read.Body.String())
	}
	cancel := httptest.NewRecorder()
	mux.ServeHTTP(cancel, httptest.NewRequest(http.MethodPost, path+"/cancel", nil))
	if cancel.Code != http.StatusOK || !bytes.Contains(cancel.Body.Bytes(), []byte(`"phase":"revoked_unstarted"`)) {
		t.Fatalf("cancel HTTP %d: %s", cancel.Code, cancel.Body.String())
	}
	terminal := httptest.NewRecorder()
	mux.ServeHTTP(terminal, httptest.NewRequest(http.MethodGet, path, nil))
	if terminal.Code != http.StatusOK || !bytes.Contains(terminal.Body.Bytes(), []byte(`"admission_restricted":false`)) {
		t.Fatalf("terminal read HTTP %d: %s", terminal.Code, terminal.Body.String())
	}
}

func TestIdleApprovalSurvivesUnrelatedSafeEditButRelevantEditSupersedes(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "hardware", "idle_timeout_secs")
	ctx := context.Background()
	if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{
		"encoder": {Source: "explicit", Value: "openh264"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRH05Boot(ctx); err != nil {
		t.Fatal(err)
	}
	completeEmptyHostJournal(t, store, hostID)
	view, err := store.GetPolicy(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	review := reviewedIdleApply(view.Groups["hardware"].ApprovalPreview.(*ApprovalPreview))
	approved, err := store.ApproveIdleApply(ctx, hostID, "hardware", review)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SavePolicy(ctx, hostID, "1", map[string]PolicyChoice{
		"idle_timeout_secs": {Source: "explicit", Value: float64(900)},
	}, nil); err != nil {
		t.Fatal(err)
	}
	unchanged, err := store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || unchanged.Phase != "waiting" || !unchanged.AdmissionRestricted {
		t.Fatalf("unrelated safe edit superseded approval: %+v %v", unchanged, err)
	}
	if _, err := store.SavePolicy(ctx, hostID, "2", map[string]PolicyChoice{
		"encoder": {Source: "explicit", Value: "va"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	superseded, err := store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || superseded.Phase != "revoked_unstarted" || superseded.AdmissionRestricted {
		t.Fatalf("relevant edit did not supersede locally: %+v %v", superseded, err)
	}
	if _, err := store.ApproveIdleApply(ctx, hostID, "hardware", review); err != ErrApprovalSuperseded {
		t.Fatalf("old review revived after relevant edit: %v", err)
	}
}

func TestMissingAcceptedJournalRecordKeepsReconciliationProtected(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	boot, err := store.StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO host_config_attempts
		(id,host_id,group_key,approved_digest,approved_revision,scope,boot_incarnation,phase,started_at,terminal_at)
		VALUES($1::uuid,$2::uuid,'hardware',$3,1,'restart',$4::uuid,'applied',now(),now())`,
		newPolicyAttemptID(), hostID,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", boot); err != nil {
		t.Fatal(err)
	}
	completeEmptyHostJournal(t, store, hostID)
	gate, err := store.JournalGate(ctx, hostID)
	if err != nil || gate != "quarantined" {
		t.Fatalf("omitted accepted record authorized host: gate=%s err=%v", gate, err)
	}
	restrictions, err := admission.NewStore(pool).List(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, restriction := range restrictions {
		if restriction.OwnerKind == admission.Reconciliation {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing accepted record released reconciliation hold: %+v", restrictions)
	}
}

func TestCancellingOneRestartGroupRotatesOtherGroupReviewFence(t *testing.T) {
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
	if _, err := store.StartRH05Boot(ctx); err != nil {
		t.Fatal(err)
	}
	completeEmptyHostJournal(t, store, hostID)
	// A future catalog revision can add a second restart group; its unchanged
	// content must still lose a review token observed before this operation.
	if _, err := pool.Exec(ctx, `INSERT INTO host_setting_groups
		(host_id,group_key,desired_revision,desired_digest,scope,status)
		VALUES($1::uuid,'future_restart_group',1,$2,'restart','pending')`, hostID,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO host_approval_review_tokens(host_id,group_key,review_id)
		VALUES($1::uuid,'future_restart_group',gen_random_uuid())`, hostID); err != nil {
		t.Fatal(err)
	}
	var before, after string
	if err := pool.QueryRow(ctx, `SELECT review_id::text FROM host_approval_review_tokens
		WHERE host_id=$1::uuid AND group_key='future_restart_group'`, hostID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	view, err := store.GetPolicy(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := store.ApproveIdleApply(ctx, hostID, "hardware", reviewedIdleApply(view.Groups["hardware"].ApprovalPreview.(*ApprovalPreview)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CancelIdleApply(ctx, hostID, approved.AttemptID, true); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT review_id::text FROM host_approval_review_tokens
		WHERE host_id=$1::uuid AND group_key='future_restart_group'`, hostID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("other restart-group review ID survived disruptive approval exit")
	}
}

func TestOutOfOrderInventoryCannotReopenTerminalRestartAttempt(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	boot, err := store.StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := newPolicyAttemptID()
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := pool.Exec(ctx, `INSERT INTO host_config_attempts
		(id,host_id,group_key,approved_digest,approved_revision,scope,boot_incarnation,phase,journal_sequence,started_at,terminal_at)
		VALUES($1::uuid,$2::uuid,'hardware',$3,1,'restart',$4::uuid,'applied',5,now(),now())`, id, hostID, digest, boot); err != nil {
		t.Fatal(err)
	}
	connection := newPolicyAttemptID()
	if err := store.BeginJournalReconciliation(ctx, hostID, connection); err != nil {
		t.Fatal(err)
	}
	stale := JournalInventoryEntry{AttemptID: id, HostID: hostID, Group: "hardware", Digest: digest, Scope: "restart", Phase: "verifying", Sequence: "4"}
	if err := store.CompleteJournalReconciliation(ctx, hostID, connection, []JournalInventoryEntry{stale}); err != nil {
		t.Fatal(err)
	}
	gate, err := store.JournalGate(ctx, hostID)
	if err != nil || gate != "quarantined" {
		t.Fatalf("stale inventory opened gate: gate=%s err=%v", gate, err)
	}
	var phase string
	var sequence int64
	var terminal bool
	if err := pool.QueryRow(ctx, `SELECT phase,journal_sequence,terminal_at IS NOT NULL
		FROM host_config_attempts WHERE id=$1::uuid`, id).Scan(&phase, &sequence, &terminal); err != nil {
		t.Fatal(err)
	}
	if phase != "applied" || sequence != 5 || !terminal {
		t.Fatalf("stale inventory rewound terminal attempt: phase=%s seq=%d terminal=%v", phase, sequence, terminal)
	}
}

// The restart uniqueness fence is host-wide, including groups introduced by
// future catalog revisions. Two dispatchers cannot start separate groups.
func TestConcurrentRestartAttemptsAcrossGroupsHaveOneWinner(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	boot := newPolicyAttemptID()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, group := range []string{"hardware", "future_restart_group"} {
		wg.Add(1)
		go func(group string) {
			defer wg.Done()
			<-start
			_, err := pool.Exec(ctx, `INSERT INTO host_config_attempts
				(id,host_id,group_key,approved_digest,approved_revision,scope,boot_incarnation,phase)
				VALUES($1::uuid,$2::uuid,$3,$4,1,'restart',$5::uuid,'offered')`,
				newPolicyAttemptID(), hostID, group, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", boot)
			results <- err
		}(group)
	}
	close(start)
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("unexpected insert error: %v", err)
		}
		conflicts++
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("restart winners=%d conflicts=%d, want 1 each", successes, conflicts)
	}
}

func TestConcurrentWaitingApprovalsAcrossGroupsHaveOneWinner(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	boot := newPolicyAttemptID()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, group := range []string{"hardware", "future_restart_group"} {
		wg.Add(1)
		go func(group string) {
			defer wg.Done()
			<-start
			_, err := pool.Exec(ctx, `INSERT INTO host_config_approvals
				(id,host_id,group_key,revision,approved_digest,prerequisites_digest,boot_incarnation,review_id,expires_at,state)
				VALUES($1::uuid,$2::uuid,$3,1,$4,$4,$5::uuid,$6::uuid,now()+interval '1 hour','approved')`,
				newPolicyAttemptID(), hostID, group,
				"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", boot, newPolicyAttemptID())
			results <- err
		}(group)
	}
	close(start)
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("unexpected insert error: %v", err)
		}
		conflicts++
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("approval winners=%d conflicts=%d, want 1 each", successes, conflicts)
	}
}

func completeEmptyHostJournal(t *testing.T, store *Store, hostID string) {
	t.Helper()
	connection := "00000000-0000-4000-8000-000000000338"
	if err := store.BeginJournalReconciliation(context.Background(), hostID, connection); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteJournalReconciliation(context.Background(), hostID, connection, nil,
		map[string]PolicySnapshot{"hardware": {Kind: "seeded", Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}); err != nil {
		t.Fatal(err)
	}
}

func TestIdleApplyWaitingReportsCurrentSessionAndPreparationBlockers(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "hardware")
	ctx := context.Background()
	if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{"encoder": {Source: "explicit", Value: "openh264"}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRH05Boot(ctx); err != nil {
		t.Fatal(err)
	}
	completeEmptyHostJournal(t, store, hostID)
	preview, err := store.PreviewIdleApply(ctx, hostID, "hardware")
	if err != nil || !preview.Available {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	approved, err := store.ApproveIdleApply(ctx, hostID, "hardware", reviewedIdleApply(preview))
	if err != nil {
		t.Fatal(err)
	}
	status, err := store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || status.Remedy == nil || !strings.Contains(*status.Remedy, "fresh authenticated session inventory") {
		t.Fatalf("unknown heartbeat presented as idle: %+v %v", status, err)
	}
	connection := "00000000-0000-4000-8000-000000000338"
	if err := store.ObserveIdleHeartbeat(ctx, hostID, connection, []string{"local-session"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(email,username,password_hash) VALUES('idle@example.invalid','idle-user','x');
		INSERT INTO apps(name) VALUES('idle-app');`); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"assigned", "starting", "running", "stopping"} {
		if _, err := pool.Exec(ctx, `INSERT INTO sessions(user_id,app_id,host_id,state,width,height,fps,bitrate_kbps)
			SELECT u.id,a.id,$1::uuid,$2,1280,720,60,5000 FROM users u,apps a
			WHERE u.username='idle-user' AND a.name='idle-app'`, hostID, state); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET last_registered_at=now()-interval '1 minute',
		source_preparation='{"steam":{"policy_revision":"1","images":[{"state":"preparing"}]}}'::jsonb,
		source_preparation_reported_at=now() WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	status, err = store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || status.Remedy == nil || !strings.Contains(*status.Remedy, "1 assigned") ||
		!strings.Contains(*status.Remedy, "1 starting") || !strings.Contains(*status.Remedy, "1 running") ||
		!strings.Contains(*status.Remedy, "1 stopping") ||
		!strings.Contains(*status.Remedy, "agent-reported untracked") || !strings.Contains(*status.Remedy, "conflicting preparation") {
		t.Fatalf("local/preparation blockers omitted: %+v %v", status, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET state='stopped' WHERE host_id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET source_preparation='{"steam":{"policy_revision":"1","images":[{"state":"disabled"}]}}'::jsonb,
		source_preparation_reported_at=now() WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveIdleHeartbeat(ctx, hostID, connection, []string{}); err != nil {
		t.Fatal(err)
	}
	status, err = store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || status.Remedy == nil || !strings.Contains(*status.Remedy, "no active sessions or preparation") {
		t.Fatalf("confirmed cleanup did not clear wait reasons: %+v %v", status, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET source_preparation_reported_at=now()-interval '31 seconds' WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	status, err = store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || status.Remedy == nil || !strings.Contains(*status.Remedy, "preparation inventory is unknown") {
		t.Fatalf("stale preparation report was treated as idle: %+v %v", status, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET source_preparation='{"steam":{"images":{}}}'::jsonb WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	status, err = store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || status.Remedy == nil || !strings.Contains(*status.Remedy, "preparation inventory is unknown") {
		t.Fatalf("malformed preparation report was treated as idle: %+v %v", status, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE host_idle_inventory SET reported_at=now()-interval '1 minute' WHERE host_id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	status, err = store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || status.Remedy == nil || !strings.Contains(*status.Remedy, "fresh authenticated session inventory") {
		t.Fatalf("stale heartbeat was shown as idle: %+v %v", status, err)
	}
	if err := store.ObserveIdleHeartbeat(ctx, hostID, connection, nil); err != nil {
		t.Fatal(err)
	}
	status, err = store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || status.Remedy == nil || !strings.Contains(*status.Remedy, "fresh authenticated session inventory") {
		t.Fatalf("missing heartbeat list reused previous inventory: %+v %v", status, err)
	}
	if err := store.EndJournalConnection(ctx, hostID, connection); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveIdleHeartbeat(ctx, hostID, connection, []string{}); err != ErrApprovalSuperseded {
		t.Fatalf("late old heartbeat restored idle inventory: %v", err)
	}
}

// The boot fence is observed through the operator policy view and the public
// admission restriction read, using the same ephemeral Postgres as the API.
func TestIdleApprovalExpiresOnControlPlaneBootWithoutLosingSavedPolicy(t *testing.T) {
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
	firstBoot, err := store.StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if firstBoot == "" {
		t.Fatal("missing durable boot incarnation")
	}
	completeEmptyHostJournal(t, store, hostID)
	// The exact reviewed candidate is obtained from the same operator read
	// used by the console, rather than invented by the test.
	view, err := store.GetPolicy(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	preview := view.Groups["hardware"].ApprovalPreview
	if preview == nil {
		t.Fatal("restart policy has no reviewable candidate")
	}
	approved := preview.(*ApprovalPreview)
	review := ApprovalReview{
		ExpectedRevision: approved.Revision, ContentSHA256: approved.ContentSHA256,
		PrerequisitesSHA256:     approved.PrerequisitesSHA256,
		Prerequisites:           approved.Prerequisites,
		ApprovalBootIncarnation: approved.ApprovalBootIncarnation,
		ApprovalReviewID:        approved.ApprovalReviewID,
		ExpiresAt:               time.Now().UTC().Add(time.Hour),
	}
	approval, err := store.ApproveIdleApply(ctx, hostID, "hardware", review)
	if err != nil {
		t.Fatal(err)
	}
	if !approval.AdmissionRestricted || approval.Phase != "waiting" {
		t.Fatalf("approval did not wait under its own hold: %+v", approval)
	}
	secondBoot, err := store.StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if secondBoot == firstBoot {
		t.Fatal("boot incarnation was reused")
	}
	current, err := store.GetPolicy(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Choices["encoder"].Value != "openh264" || current.Groups["hardware"].Status != "pending" {
		t.Fatalf("boot lost saved intent: %+v", current)
	}
	got, err := store.GetIdleApply(ctx, hostID, approval.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != "cancel_pending" || !got.AdmissionRestricted {
		t.Fatalf("restored approval lost protection before journal proof: %+v", got)
	}
	completeEmptyHostJournal(t, store, hostID)
	got, err = store.GetIdleApply(ctx, hostID, approval.AttemptID)
	if err != nil || got.Phase != "revoked_unstarted" || got.AdmissionRestricted {
		t.Fatalf("empty journal did not settle old approval: %+v %v", got, err)
	}
	if _, err := store.ApproveIdleApply(ctx, hostID, "hardware", review); err != ErrApprovalSuperseded {
		t.Fatalf("delayed pre-boot request was authorized again: %v", err)
	}
}

func TestCancelWaitingIdleApplyKeepsOtherHoldAndFreshReviewCanReapprove(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "hardware")
	ctx := context.Background()
	if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{"encoder": {Source: "explicit", Value: "openh264"}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRH05Boot(ctx); err != nil {
		t.Fatal(err)
	}
	completeEmptyHostJournal(t, store, hostID)
	view, err := store.GetPolicy(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	firstReview := reviewedIdleApply(view.Groups["hardware"].ApprovalPreview.(*ApprovalPreview))
	first, err := store.ApproveIdleApply(ctx, hostID, "hardware", firstReview)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := store.ApproveIdleApply(ctx, hostID, "hardware", firstReview); err != nil || replay.AttemptID != first.AttemptID {
		t.Fatalf("identical replay: %+v %v", replay, err)
	}
	manual := admission.NewStore(pool)
	if _, err := manual.Acquire(ctx, hostID, admission.ManualOwner, admission.ReasonManualDrain); err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.CancelIdleApply(ctx, hostID, first.AttemptID, true)
	if err != nil || cancelled.Phase != "revoked_unstarted" || cancelled.AdmissionRestricted {
		t.Fatalf("cancelled: %+v %v", cancelled, err)
	}
	if _, err := store.ApproveIdleApply(ctx, hostID, "hardware", firstReview); err != ErrApprovalSuperseded {
		t.Fatalf("old review replayed: %v", err)
	}
	view, err = store.GetPolicy(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	secondReview := reviewedIdleApply(view.Groups["hardware"].ApprovalPreview.(*ApprovalPreview))
	if secondReview.ApprovalReviewID == firstReview.ApprovalReviewID {
		t.Fatal("review token did not change after cancellation")
	}
	second, err := store.ApproveIdleApply(ctx, hostID, "hardware", secondReview)
	if err != nil || second.AttemptID == first.AttemptID {
		t.Fatalf("fresh approval: %+v %v", second, err)
	}
	if _, err := store.CancelIdleApply(ctx, hostID, second.AttemptID, true); err != nil {
		t.Fatal(err)
	}
	thirdView, err := store.GetPolicy(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	thirdReview := thirdView.Groups["hardware"].ApprovalPreview.(*ApprovalPreview)
	if thirdReview.ApprovalReviewID == firstReview.ApprovalReviewID {
		t.Fatal("review ID was reused after two cancellations")
	}
	// Even a generator collision with an earlier issued ID must rotate again.
	if _, err := pool.Exec(ctx, `UPDATE host_approval_review_tokens SET review_id=$3::uuid
		WHERE host_id=$1::uuid AND group_key=$2`, hostID, "hardware", firstReview.ApprovalReviewID); err != nil {
		t.Fatal(err)
	}
	var guardedID string
	if err := pool.QueryRow(ctx, `SELECT review_id::text FROM host_approval_review_tokens
		WHERE host_id=$1::uuid AND group_key='hardware'`, hostID).Scan(&guardedID); err != nil {
		t.Fatal(err)
	}
	if guardedID == firstReview.ApprovalReviewID || guardedID == secondReview.ApprovalReviewID {
		t.Fatalf("issued review ID reused: %s", guardedID)
	}
	if _, err := store.ApproveIdleApply(ctx, hostID, "hardware", firstReview); err != ErrApprovalSuperseded {
		t.Fatalf("first cancelled request revived after second cancellation: %v", err)
	}
	restrictions, err := manual.List(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	foundManual := false
	for _, r := range restrictions {
		if r.OwnerKind == admission.Manual {
			foundManual = true
		}
		if r.OwnerKind == admission.IdleApply {
			t.Fatalf("idle hold survived cancellation: %+v", restrictions)
		}
	}
	if !foundManual {
		t.Fatalf("cancel released another owner's hold: %+v", restrictions)
	}
}

func TestOfferedApprovalBootAndCancelKeepProtectionUntilJournalProof(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "hardware")
	ctx := context.Background()
	if _, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{"encoder": {Source: "explicit", Value: "openh264"}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRH05Boot(ctx); err != nil {
		t.Fatal(err)
	}
	completeEmptyHostJournal(t, store, hostID)
	view, err := store.GetPolicy(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := store.ApproveIdleApply(ctx, hostID, "hardware", reviewedIdleApply(view.Groups["hardware"].ApprovalPreview.(*ApprovalPreview)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE host_config_approvals SET state='offered' WHERE id=$1::uuid`, approved.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO host_config_attempts
		(id,host_id,group_key,approved_digest,approved_revision,scope,boot_incarnation,phase)
		VALUES($1::uuid,$2::uuid,'hardware',$3,1,'restart',$4::uuid,'offered')`,
		approved.AttemptID, hostID, approved.ContentSHA256, view.Groups["hardware"].ApprovalPreview.(*ApprovalPreview).ApprovalBootIncarnation); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRH05Boot(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || current.Phase != "cancel_pending" || !current.AdmissionRestricted ||
		current.Remedy == nil || !strings.Contains(*current.Remedy, "authenticated agent journal") {
		t.Fatalf("offered boot must protect: %+v %v", current, err)
	}
	repeat, err := store.CancelIdleApply(ctx, hostID, approved.AttemptID, true)
	if err != nil || repeat.Phase != "cancel_pending" || !repeat.AdmissionRestricted {
		t.Fatalf("uncertain cancel released hold: %+v %v", repeat, err)
	}
	completeEmptyHostJournal(t, store, hostID)
	settled, err := store.GetIdleApply(ctx, hostID, approved.AttemptID)
	if err != nil || settled.Phase != "revoked_unstarted" || settled.AdmissionRestricted {
		t.Fatalf("empty journal did not prove offered nonacceptance: %+v %v", settled, err)
	}
	var phase string
	if err := pool.QueryRow(ctx, `SELECT phase FROM host_config_attempts WHERE id=$1::uuid`, approved.AttemptID).Scan(&phase); err != nil || phase != "revoked_unstarted" {
		t.Fatalf("offered attempt remained open: phase=%s err=%v", phase, err)
	}
}

func TestAcceptedThenRecoveredCannotReplayOldReview(t *testing.T) {
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
	view, err := store.GetPolicy(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	old := reviewedIdleApply(view.Groups["hardware"].ApprovalPreview.(*ApprovalPreview))
	approved, err := store.ApproveIdleApply(ctx, hostID, "hardware", old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE host_config_approvals SET state='accepted' WHERE id=$1::uuid`, approved.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO host_config_attempts(id,host_id,group_key,approved_digest,approved_revision,scope,boot_incarnation,phase,journal_sequence,started_at,terminal_at,recovery_attempted,error_code)
		VALUES($1::uuid,$2::uuid,'hardware',$3,1,'restart',$4::uuid,'recovered',4,now(),now(),true,'candidate_failed')`, approved.AttemptID, hostID, old.ContentSHA256, boot); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE host_approval_review_tokens SET review_id=gen_random_uuid() WHERE host_id=$1::uuid AND group_key='hardware'`, hostID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApproveIdleApply(ctx, hostID, "hardware", old); err != ErrApprovalSuperseded {
		t.Fatalf("recovered candidate replay: %v", err)
	}
	view, err = store.GetPolicy(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	fresh := view.Groups["hardware"].ApprovalPreview.(*ApprovalPreview)
	if fresh.ApprovalReviewID == old.ApprovalReviewID || len(fresh.Prerequisites) != 2 ||
		fresh.Prerequisites[0].Kind != "accepted_attempts" || fresh.Prerequisites[0].ID == old.Prerequisites[0].ID {
		t.Fatalf("recovery not bound into new review: %+v", fresh)
	}
}
