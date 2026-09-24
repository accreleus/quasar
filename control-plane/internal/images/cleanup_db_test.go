package images

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

type fakeCleanupWire struct {
	mu                sync.Mutex
	snapshots         map[string]agentws.ImageCleanupSnapshot
	cleanup           chan agentws.ImageCleanupCmd
	acks              chan agentws.ImageCleanupStateAckCmd
	inventoryRequests chan agentws.ImageInventoryReconcileCmd
	journalRequests   chan agentws.ImageCleanupJournalRequestCmd
	snapshotHook      func()
	inventorySendHook func()
	inventorySendErr  error
}

func (f *fakeCleanupWire) ImageCleanupSnapshot(hostID string) (agentws.ImageCleanupSnapshot, bool) {
	f.mu.Lock()
	hook := f.snapshotHook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	s, ok := f.snapshots[hostID]
	s.Versions = append([]agentws.ImageVersionEntry(nil), s.Versions...)
	f.mu.Unlock()
	return s, ok
}
func (f *fakeCleanupWire) SendImageCleanup(_ context.Context, _ string, c agentws.ImageCleanupCmd) (agentws.AckResult, error) {
	if f.cleanup != nil {
		f.cleanup <- c
	}
	return agentws.AckResult{OK: true}, nil
}
func (f *fakeCleanupWire) SendImageInventoryReconcile(_ string, c agentws.ImageInventoryReconcileCmd) error {
	f.mu.Lock()
	hook := f.inventorySendHook
	sendErr := f.inventorySendErr
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if sendErr != nil {
		return sendErr
	}
	if f.inventoryRequests != nil {
		f.inventoryRequests <- c
	}
	return nil
}
func (f *fakeCleanupWire) SendImageCleanupJournalRequest(_ string, c agentws.ImageCleanupJournalRequestCmd) error {
	if f.journalRequests != nil {
		f.journalRequests <- c
	}
	return nil
}
func (f *fakeCleanupWire) SendImageCleanupStateAck(_ string, ack agentws.ImageCleanupStateAckCmd) error {
	if f.acks != nil {
		f.acks <- ack
	}
	return nil
}

func (f *fakeCleanupWire) set(host string, s agentws.ImageCleanupSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshots[host] = s
}

func TestCleanupHTTPPreviewAndExactAttemptAreCurrentConnectionBound(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-http-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	path := "/v1/admin/hosts/" + host + "/images/cleanup"
	if code, body := env.do(t, http.MethodGet, path, ""); code != 200 || !strings.Contains(string(body), `"inventory_status":"offline"`) || !strings.Contains(string(body), `"images":[]`) {
		t.Fatalf("offline preview = %d %s", code, body)
	}
	entry := agentws.ImageVersionEntry{ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2, RuntimeImageID: "sha256:old-daemon", State: "present"}
	env.cleanupWire.cleanup = make(chan agentws.ImageCleanupCmd, 2)
	env.cleanupWire.acks = make(chan agentws.ImageCleanupStateAckCmd, 2)
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "connection-a", Capable: true, Complete: true, ObservedAt: time.Now().UTC(), Versions: []agentws.ImageVersionEntry{entry}})
	code, body := env.do(t, http.MethodGet, path, "")
	if code != 200 {
		t.Fatalf("current preview = %d %s", code, body)
	}
	var preview CleanupView
	if err := json.Unmarshal(body, &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.Images) != 1 || !preview.Images[0].Eligible || preview.Images[0].Generation != "0" {
		t.Fatalf("candidate = %+v", preview)
	}
	bad := `{"image_id":"` + imgID + `","version":"` + imgVer2 + `","image_ref":"other-ref","runtime_image_id":"sha256:old-daemon","expected_generation":"0"}`
	if code, body := env.do(t, http.MethodPost, path, bad); code != 409 || !strings.Contains(string(body), `"code":"stale_preview"`) {
		t.Fatalf("changed exact ref = %d %s", code, body)
	}
	request := `{"image_id":"` + imgID + `","version":"` + imgVer2 + `","image_ref":"` + imgDigest2 + `","runtime_image_id":"sha256:old-daemon","expected_generation":"0"}`
	code, body = env.do(t, http.MethodPost, path, request)
	if code != 202 {
		t.Fatalf("cleanup request = %d %s", code, body)
	}
	var attempt CleanupAttempt
	if err := json.Unmarshal(body, &attempt); err != nil {
		t.Fatal(err)
	}
	if attempt.Generation != "1" || attempt.State != "removing" {
		t.Fatalf("attempt = %+v", attempt)
	}
	select {
	case sent := <-env.cleanupWire.cleanup:
		if sent.AttemptID != attempt.AttemptID || sent.ExpectedGeneration != "1" || sent.RuntimeImageID != entry.RuntimeImageID {
			t.Fatalf("dispatch = %+v", sent)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup was not dispatched")
	}
	if code, body := env.do(t, http.MethodPost, path, request); code != 202 || !strings.Contains(string(body), attempt.AttemptID) {
		t.Fatalf("identical HTTP retry = %d %s", code, body)
	}
	select {
	case <-env.cleanupWire.cleanup:
		t.Fatal("duplicate sent a second physical command")
	default:
	}
	if code, body := env.do(t, http.MethodGet, path, ""); code != 200 || !strings.Contains(string(body), `"removing"`) {
		t.Fatalf("in-flight preview = %d %s", code, body)
	}
	entry.State = "absent"
	// The catalog's newer cached version must not demote a different ready
	// adopted version when that older cache is confirmed removed.
	if _, err := env.pool.Exec(context.Background(), `INSERT INTO host_images(host_id,image_id,version,state)
		VALUES($1::uuid,$2,$3,'ready')`, host, imgID, imgVer); err != nil {
		t.Fatal(err)
	}
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "connection-a", Capable: true, Complete: true,
		ObservedAt: time.Now().UTC(), Versions: []agentws.ImageVersionEntry{entry}})
	env.cleanup.ImageCleanupState(context.Background(), host, agentws.ImageCleanupStateMsg{
		AttemptID: attempt.AttemptID, ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2,
		RuntimeImageID: "sha256:old-daemon", Generation: "1", State: "removed",
	})
	var persistedState, fenceState string
	if err := env.pool.QueryRow(context.Background(), `SELECT a.state,f.state FROM host_image_cleanup_attempts a
		JOIN host_image_operation_fences f ON f.host_id=a.host_id AND f.image_id=a.image_id
		WHERE a.id=$1::uuid`, attempt.AttemptID).Scan(&persistedState, &fenceState); err != nil {
		t.Fatal(err)
	}
	if persistedState != "removed" || fenceState != "idle" {
		t.Fatalf("confirmed daemon absence left attempt=%s fence=%s", persistedState, fenceState)
	}
	var hostImageState string
	if err := env.pool.QueryRow(context.Background(), `SELECT state FROM host_images WHERE host_id=$1::uuid AND image_id=$2`, host, imgID).Scan(&hostImageState); err != nil {
		t.Fatal(err)
	}
	if hostImageState != "ready" {
		t.Fatalf("removed old cache demoted newer ready version: %s", hostImageState)
	}
	select {
	case ack := <-env.cleanupWire.acks:
		if ack.AttemptID != attempt.AttemptID {
			t.Fatalf("terminal ack = %+v", ack)
		}
	default:
		t.Fatal("first terminal report was not acknowledged")
	}
	// Simulate losing the ack frame. The agent repeats its terminal report after
	// the fence was released; it must receive the same ack for journal retirement.
	env.cleanup.ImageCleanupState(context.Background(), host, agentws.ImageCleanupStateMsg{
		AttemptID: attempt.AttemptID, ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2,
		RuntimeImageID: "sha256:old-daemon", Generation: "1", State: "removed",
	})
	select {
	case ack := <-env.cleanupWire.acks:
		if ack.AttemptID != attempt.AttemptID || ack.Generation != "1" {
			t.Fatalf("replayed ack = %+v", ack)
		}
	default:
		t.Fatal("terminal replay was not acknowledged")
	}
	if code, body := env.do(t, http.MethodPost, path, request); code != 200 || !strings.Contains(string(body), attempt.AttemptID) {
		t.Fatalf("confirmed-idempotent POST = %d %s", code, body)
	}
	select {
	case <-env.cleanupWire.cleanup:
		t.Fatal("confirmed duplicate dispatched again")
	default:
	}
}

func TestCleanupHTTPProtectsCurrentRequirementAndPreviousSuccess(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-protected-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	path := "/v1/admin/hosts/" + host + "/images/cleanup"
	entry := agentws.ImageVersionEntry{ImageID: imgID, Version: imgVer, ImageRef: imgRef, RuntimeImageID: "sha256:current-daemon", State: "present"}
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "current", Capable: true, Complete: true, Versions: []agentws.ImageVersionEntry{entry}})
	code, body := env.do(t, http.MethodGet, path, "")
	if code != 200 || !strings.Contains(string(body), `"required"`) {
		t.Fatalf("required preview = %d %s", code, body)
	}
	request := `{"image_id":"` + imgID + `","version":"` + imgVer + `","image_ref":"` + imgRef + `","runtime_image_id":"sha256:current-daemon","expected_generation":"0"}`
	if code, body := env.do(t, http.MethodPost, path, request); code != 409 || !strings.Contains(string(body), `"code":"required"`) {
		t.Fatalf("required removal = %d %s", code, body)
	}
	if _, err := env.pool.Exec(context.Background(), `INSERT INTO host_image_success_history
		(host_id,image_id,current_version,current_identity,previous_version,previous_identity,verified_at)
		VALUES($1::uuid,$2,$3,'{}'::jsonb,$4,jsonb_build_object('registry_ref',$5::text),now())`, host, imgID, imgVer2, imgVer, imgRef); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(context.Background(), `UPDATE apps SET enabled=false WHERE runtime_spec->>'image'=$1`, imgRef); err != nil {
		t.Fatal(err)
	}
	code, body = env.do(t, http.MethodGet, path, "")
	if code != 200 || !strings.Contains(string(body), `"retained_previous_success"`) {
		t.Fatalf("retained preview = %d %s", code, body)
	}
}

func TestCleanupProtectsOtherManagedImagesPreviousSuccessWithSharedRef(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-shared-recovery-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	entry := agentws.ImageVersionEntry{ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2, RuntimeImageID: "sha256:shared", State: "present"}
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "current", Capable: true, Complete: true, Versions: []agentws.ImageVersionEntry{entry}})
	if _, err := env.pool.Exec(context.Background(), `INSERT INTO host_image_success_history
		(host_id,image_id,current_version,current_identity,previous_version,previous_identity,verified_at)
		VALUES($1::uuid,'other-managed','new','{}'::jsonb,$2,jsonb_build_object('registry_ref',$3::text),now())`,
		host, imgVer2, imgDigest2); err != nil {
		t.Fatal(err)
	}
	path := "/v1/admin/hosts/" + host + "/images/cleanup"
	if code, body := env.do(t, http.MethodGet, path, ""); code != 200 || !strings.Contains(string(body), `"retained_previous_success"`) {
		t.Fatalf("shared-ref recovery preview = %d %s", code, body)
	}
	request := `{"image_id":"` + imgID + `","version":"` + imgVer2 + `","image_ref":"` + imgDigest2 + `","runtime_image_id":"sha256:shared","expected_generation":"0"}`
	if code, body := env.do(t, http.MethodPost, path, request); code != 409 || !strings.Contains(string(body), `"code":"retained_previous_success"`) {
		t.Fatalf("shared-ref recovery removal = %d %s", code, body)
	}
}

func TestCleanupFailedPresentDemotesReadyAndReEnsures(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-failed-present-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	entry := agentws.ImageVersionEntry{ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2, RuntimeImageID: "sha256:still-present", State: "present"}
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "current", Capable: true, Complete: true, Versions: []agentws.ImageVersionEntry{entry}})
	path := "/v1/admin/hosts/" + host + "/images/cleanup"
	request := `{"image_id":"` + imgID + `","version":"` + imgVer2 + `","image_ref":"` + imgDigest2 + `","runtime_image_id":"sha256:still-present","expected_generation":"0"}`
	code, body := env.do(t, http.MethodPost, path, request)
	if code != 202 {
		t.Fatalf("cleanup request = %d %s", code, body)
	}
	var attempt CleanupAttempt
	if err := json.Unmarshal(body, &attempt); err != nil {
		t.Fatal(err)
	}
	// Older agents can report a versionless ready row for the adopted ref.
	if _, err := env.pool.Exec(context.Background(), `INSERT INTO host_images(host_id,image_id,version,state)
		VALUES($1::uuid,$2,'','ready')`, host, imgID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(context.Background(), `UPDATE installed_images SET version=$2,registry_ref=$3
		WHERE image_id=$1`, imgID, imgVer2, imgDigest2); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(context.Background(), `INSERT INTO apps(name,runtime_spec)
		VALUES('cleanup selected fixture',jsonb_build_object('image',$1::text))`, imgDigest2); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(context.Background(), `UPDATE app_placement SET mode='fixed'`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(context.Background(), `INSERT INTO app_placement_hosts(app_id,host_id)
		SELECT id,$1::uuid FROM apps`, host); err != nil {
		t.Fatal(err)
	}
	if adopted, required, err := requiredAdoptionForHost(context.Background(), env.pool, host, imgID); err != nil || !required {
		t.Fatalf("test fixture missing current requirement: adoption=%+v required=%t err=%v", adopted, required, err)
	}
	env.cleanup.SetEnsurer(env.ens)
	reason := "reference_present"
	env.cleanup.ImageCleanupState(context.Background(), host, agentws.ImageCleanupStateMsg{
		AttemptID: attempt.AttemptID, ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2,
		RuntimeImageID: entry.RuntimeImageID, Generation: "1", State: "failed", Reason: &reason,
	})
	var imageState, attemptState, fenceState string
	if err := env.pool.QueryRow(context.Background(), `SELECT h.state,a.state,f.state FROM host_images h
		JOIN host_image_cleanup_attempts a ON a.host_id=h.host_id AND a.image_id=h.image_id
		JOIN host_image_operation_fences f ON f.host_id=h.host_id AND f.image_id=h.image_id
		WHERE a.id=$1::uuid`, attempt.AttemptID).Scan(&imageState, &attemptState, &fenceState); err != nil {
		t.Fatal(err)
	}
	if imageState != "failed" || attemptState != "failed" || fenceState != "idle" {
		t.Fatalf("failed-present transition: host_image=%s attempt=%s fence=%s", imageState, attemptState, fenceState)
	}
	if got := env.fleet.waitEnsure(t); got.HostID != host || got.ImageID != imgID || got.Version != imgVer2 {
		t.Fatalf("failed cleanup did not dispatch current requirement: %+v", got)
	}
	env.ens.Wait()
	env.fleet.noMoreEnsures(t, 0)
}

func TestCleanupDuplicateUnresolvedAttemptRequestsRecoveryWithoutSecondDelete(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-duplicate-recovery-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	env.cleanupWire.cleanup = make(chan agentws.ImageCleanupCmd, 2)
	env.cleanupWire.inventoryRequests = make(chan agentws.ImageInventoryReconcileCmd, 3)
	env.cleanupWire.journalRequests = make(chan agentws.ImageCleanupJournalRequestCmd, 1)
	entry := agentws.ImageVersionEntry{ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2, RuntimeImageID: "sha256:lost-dispatch", State: "present"}
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "current", Capable: true, Complete: true,
		ReconciledRevision: 1, Versions: []agentws.ImageVersionEntry{entry}})
	path := "/v1/admin/hosts/" + host + "/images/cleanup"
	request := `{"image_id":"` + imgID + `","version":"` + imgVer2 + `","image_ref":"` + imgDigest2 + `","runtime_image_id":"sha256:lost-dispatch","expected_generation":"0"}`
	code, body := env.do(t, http.MethodPost, path, request)
	if code != 202 {
		t.Fatalf("initial cleanup = %d %s", code, body)
	}
	var first CleanupAttempt
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-env.cleanupWire.cleanup:
	case <-time.After(5 * time.Second):
		t.Fatal("initial dispatch missing")
	}
	var firstReconcile agentws.ImageInventoryReconcileCmd
	select {
	case firstReconcile = <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("initial post-dispatch reconcile missing")
	}
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "current", Capable: true, Complete: true,
		ReconciledRevision: 2, ReconciledRequestID: firstReconcile.ID, Versions: []agentws.ImageVersionEntry{entry}})
	// Catalog/adoption may disappear while the attempt remains durable. The
	// duplicate still needs to reconcile that exact managed identity.
	if _, err := env.pool.Exec(context.Background(), `DELETE FROM installed_images`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(context.Background(), `DELETE FROM image_catalog`); err != nil {
		t.Fatal(err)
	}
	// An older journal response may still be in flight when the duplicate
	// begins a new daemon scan. It cannot settle against the old inventory.
	env.cleanup.requestCleanupJournal(context.Background(), host)
	var oldJournal agentws.ImageCleanupJournalRequestCmd
	select {
	case oldJournal = <-env.cleanupWire.journalRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("old journal request missing")
	}
	code, body = env.do(t, http.MethodPost, path, request)
	if code != 202 {
		t.Fatalf("duplicate cleanup = %d %s", code, body)
	}
	var duplicate CleanupAttempt
	if err := json.Unmarshal(body, &duplicate); err != nil {
		t.Fatal(err)
	}
	if duplicate.AttemptID != first.AttemptID || duplicate.Generation != first.Generation {
		t.Fatalf("duplicate created a new attempt: first=%+v duplicate=%+v", first, duplicate)
	}
	var duplicateReconcile agentws.ImageInventoryReconcileCmd
	select {
	case inventory := <-env.cleanupWire.inventoryRequests:
		duplicateReconcile = inventory
		found := false
		for _, identity := range inventory.Identities {
			if identity.ImageID == imgID && identity.Version == imgVer2 && identity.ImageRef == imgDigest2 {
				found = true
			}
		}
		if !found {
			t.Fatalf("pruned unresolved identity omitted from reconciliation: %+v", inventory.Identities)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("duplicate did not request fresh inventory")
	}
	env.cleanup.ImageCleanupJournal(context.Background(), host, agentws.ImageCleanupJournalMsg{
		RequestID: oldJournal.ID, RetiredAttemptIDs: []string{first.AttemptID},
	})
	var state string
	if err := env.pool.QueryRow(context.Background(), `SELECT state FROM host_image_cleanup_attempts WHERE id=$1::uuid`, first.AttemptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "removing" {
		t.Fatalf("old journal settled attempt before fresh scan: %s", state)
	}
	select {
	case journal := <-env.cleanupWire.journalRequests:
		t.Fatalf("journal requested before fresh reconcile revision: %+v", journal)
	default:
	}
	entry.State = "absent"
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "current", Capable: true, Complete: true,
		ReconciledRevision: 3, ReconciledRequestID: duplicateReconcile.ID, Versions: []agentws.ImageVersionEntry{entry}})
	env.cleanup.ImageVersionsChanged(context.Background(), host)
	select {
	case journal := <-env.cleanupWire.journalRequests:
		if len(journal.AttemptIDs) != 1 || journal.AttemptIDs[0] != first.AttemptID {
			t.Fatalf("duplicate requested wrong journal: %+v", journal)
		}
		env.cleanup.ImageCleanupJournal(context.Background(), host, agentws.ImageCleanupJournalMsg{
			RequestID: journal.ID, RetiredAttemptIDs: []string{first.AttemptID},
		})
	case <-time.After(5 * time.Second):
		t.Fatal("duplicate did not request attempt journal")
	}
	if err := env.pool.QueryRow(context.Background(), `SELECT state FROM host_image_cleanup_attempts WHERE id=$1::uuid`, first.AttemptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "removed" {
		t.Fatalf("fresh retirement plus absent scan left attempt %s", state)
	}
	select {
	case second := <-env.cleanupWire.cleanup:
		t.Fatalf("duplicate dispatched second delete: %+v", second)
	default:
	}
}

func TestCleanupSecondAttemptRequiresItsOwnFreshInventoryBeforeRetiredJournal(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-second-attempt-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	env.cleanupWire.cleanup = make(chan agentws.ImageCleanupCmd, 2)
	env.cleanupWire.inventoryRequests = make(chan agentws.ImageInventoryReconcileCmd, 2)
	env.cleanupWire.journalRequests = make(chan agentws.ImageCleanupJournalRequestCmd, 2)
	entry := agentws.ImageVersionEntry{ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2, RuntimeImageID: "sha256:retry-same-connection", State: "present"}
	snapshot := agentws.ImageCleanupSnapshot{ConnectionID: "same", Capable: true, Complete: true, Versions: []agentws.ImageVersionEntry{entry}}
	env.cleanupWire.set(host, snapshot)
	path := "/v1/admin/hosts/" + host + "/images/cleanup"
	request := func(generation string) string {
		return `{"image_id":"` + imgID + `","version":"` + imgVer2 + `","image_ref":"` + imgDigest2 + `","runtime_image_id":"sha256:retry-same-connection","expected_generation":"` + generation + `"}`
	}
	code, body := env.do(t, http.MethodPost, path, request("0"))
	if code != 202 {
		t.Fatalf("first attempt = %d %s", code, body)
	}
	var first CleanupAttempt
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-env.cleanupWire.cleanup:
	case <-time.After(5 * time.Second):
		t.Fatal("first dispatch missing")
	}
	var firstReconcile agentws.ImageInventoryReconcileCmd
	select {
	case firstReconcile = <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("first post-dispatch reconcile missing")
	}
	// The first scan is complete, then its attempt receives a definite refusal.
	snapshot.ReconciledRevision = 1
	snapshot.ReconciledRequestID = firstReconcile.ID
	env.cleanupWire.set(host, snapshot)
	reason := "operation_busy"
	env.cleanup.ImageCleanupState(context.Background(), host, agentws.ImageCleanupStateMsg{
		AttemptID: first.AttemptID, ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2,
		RuntimeImageID: entry.RuntimeImageID, Generation: first.Generation, State: "failed", Reason: &reason,
	})
	code, body = env.do(t, http.MethodPost, path, request("1"))
	if code != 202 {
		t.Fatalf("second attempt = %d %s", code, body)
	}
	var second CleanupAttempt
	if err := json.Unmarshal(body, &second); err != nil {
		t.Fatal(err)
	}
	if second.AttemptID == first.AttemptID || second.Generation != "2" {
		t.Fatalf("second attempt identity = %+v", second)
	}
	// A late state from the prior reconcile may advance the revision after the
	// second attempt commits. Its request ID still cannot authorize retirement.
	snapshot.ReconciledRevision = 2
	snapshot.ReconciledRequestID = firstReconcile.ID
	env.cleanupWire.set(host, snapshot)
	env.cleanup.ImageVersionsChanged(context.Background(), host)
	// A retained complete snapshot from the first scan cannot authorize a
	// retired-ID settlement of the second attempt.
	env.cleanup.requestCleanupJournal(context.Background(), host)
	select {
	case stale := <-env.cleanupWire.journalRequests:
		t.Fatalf("second attempt used first scan for journal: %+v", stale)
	default:
	}
	select {
	case <-env.cleanupWire.cleanup:
	case <-time.After(5 * time.Second):
		t.Fatal("second dispatch missing")
	}
	var secondReconcile agentws.ImageInventoryReconcileCmd
	select {
	case secondReconcile = <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("second post-dispatch reconcile missing")
	}
	select {
	case stale := <-env.cleanupWire.journalRequests:
		t.Fatalf("second attempt requested journal before second scan: %+v", stale)
	default:
	}
	snapshot.ReconciledRevision = 3
	snapshot.ReconciledRequestID = secondReconcile.ID
	snapshot.Versions[0].State = "absent"
	env.cleanupWire.set(host, snapshot)
	env.cleanup.ImageVersionsChanged(context.Background(), host)
	select {
	case journal := <-env.cleanupWire.journalRequests:
		env.cleanup.ImageCleanupJournal(context.Background(), host, agentws.ImageCleanupJournalMsg{
			RequestID: journal.ID, RetiredAttemptIDs: []string{second.AttemptID},
		})
	case <-time.After(5 * time.Second):
		t.Fatal("second scan did not request journal")
	}
	var state string
	if err := env.pool.QueryRow(context.Background(), `SELECT state FROM host_image_cleanup_attempts WHERE id=$1::uuid`, second.AttemptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "removed" {
		t.Fatalf("second attempt after own fresh scan = %s", state)
	}
}

func TestCleanupOlderRegistrationCannotReplaceNewAttemptReconcile(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-stale-registration-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	env.cleanupWire.inventoryRequests = make(chan agentws.ImageInventoryReconcileCmd, 2)
	snapshot := agentws.ImageCleanupSnapshot{ConnectionID: "same", Capable: true, Complete: true,
		Versions: []agentws.ImageVersionEntry{{ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2,
			RuntimeImageID: "sha256:registration-race", State: "present"}}}
	env.cleanupWire.set(host, snapshot)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var paused atomic.Bool
	env.cleanupWire.mu.Lock()
	env.cleanupWire.snapshotHook = func() {
		if paused.CompareAndSwap(false, true) {
			close(started)
			<-release
		}
	}
	env.cleanupWire.mu.Unlock()
	go func() {
		env.cleanup.ImageCleanupRegistered(context.Background(), host)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("older registration did not pause")
	}
	// A new durable attempt invalidates the old authority while the first
	// registration is still reading its managed identities.
	env.cleanup.mu.Lock()
	env.cleanup.invalidateReconcileLocked(host, snapshot, "")
	env.cleanup.mu.Unlock()
	env.cleanup.ImageCleanupRegistered(context.Background(), host)
	select {
	case <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("new reconcile not sent")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("older registration did not finish")
	}
	select {
	case older := <-env.cleanupWire.inventoryRequests:
		t.Fatalf("older registration replaced newer scan: %+v", older)
	default:
	}
}

func TestCleanupGateAndReconcileSendStayOrderedAcrossNewAttempt(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-send-order-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	env.cleanupWire.cleanup = make(chan agentws.ImageCleanupCmd, 1)
	env.cleanupWire.inventoryRequests = make(chan agentws.ImageInventoryReconcileCmd, 3)
	entry := agentws.ImageVersionEntry{ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2,
		RuntimeImageID: "sha256:send-order", State: "present"}
	snapshot := agentws.ImageCleanupSnapshot{ConnectionID: "same", Capable: true, Complete: true,
		Versions: []agentws.ImageVersionEntry{entry}}
	env.cleanupWire.set(host, snapshot)
	entered, release, registered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var paused atomic.Bool
	env.cleanupWire.mu.Lock()
	env.cleanupWire.inventorySendHook = func() {
		if paused.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	env.cleanupWire.mu.Unlock()
	go func() { env.cleanup.ImageCleanupRegistered(context.Background(), host); close(registered) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first reconcile did not reach send")
	}
	path := "/v1/admin/hosts/" + host + "/images/cleanup"
	request := `{"image_id":"` + imgID + `","version":"` + imgVer2 + `","image_ref":"` + imgDigest2 + `","runtime_image_id":"sha256:send-order","expected_generation":"0"}`
	type answer struct {
		code int
		body []byte
	}
	result := make(chan answer, 1)
	go func() { code, body := env.do(t, http.MethodPost, path, request); result <- answer{code, body} }()
	select {
	case got := <-result:
		t.Fatalf("new attempt overtook paused gate/send: %d %s", got.code, got.body)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	var first agentws.ImageInventoryReconcileCmd
	select {
	case first = <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("first reconcile not sent")
	}
	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("first registration did not finish")
	}
	select {
	case got := <-result:
		if got.code != 202 {
			t.Fatalf("new attempt = %d %s", got.code, got.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("new attempt did not commit")
	}
	select {
	case <-env.cleanupWire.cleanup:
	case <-time.After(5 * time.Second):
		t.Fatal("new cleanup did not dispatch")
	}
	select {
	case extra := <-env.cleanupWire.inventoryRequests:
		t.Fatalf("started second scan before first completed: %+v", extra)
	default:
	}
	snapshot.ReconciledRevision = 1
	snapshot.ReconciledRequestID = first.ID
	env.cleanupWire.set(host, snapshot)
	env.cleanup.ImageVersionsChanged(context.Background(), host)
	select {
	case second := <-env.cleanupWire.inventoryRequests:
		if second.ID == first.ID {
			t.Fatal("follow-up reused first reconcile ID")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("new attempt's follow-up scan missing")
	}
}

func TestCleanupRepeatedDuplicatePOSTsBoundStalledReconcile(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-stalled-reconcile-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	env.cleanupWire.cleanup = make(chan agentws.ImageCleanupCmd, 1)
	env.cleanupWire.inventoryRequests = make(chan agentws.ImageInventoryReconcileCmd, 3)
	entry := agentws.ImageVersionEntry{ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2,
		RuntimeImageID: "sha256:stalled", State: "present"}
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "same", Capable: true, Complete: true,
		Versions: []agentws.ImageVersionEntry{entry}})
	path := "/v1/admin/hosts/" + host + "/images/cleanup"
	request := `{"image_id":"` + imgID + `","version":"` + imgVer2 + `","image_ref":"` + imgDigest2 + `","runtime_image_id":"sha256:stalled","expected_generation":"0"}`
	if code, body := env.do(t, http.MethodPost, path, request); code != 202 {
		t.Fatalf("initial POST = %d %s", code, body)
	}
	select {
	case <-env.cleanupWire.cleanup:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup not dispatched")
	}
	select {
	case <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("first scan missing")
	}
	for range 50 {
		if code, body := env.do(t, http.MethodPost, path, request); code != 202 {
			t.Fatalf("duplicate POST = %d %s", code, body)
		}
	}
	select {
	case extra := <-env.cleanupWire.inventoryRequests:
		t.Fatalf("stalled scan multiplied: %+v", extra)
	default:
	}
	// One timed retry is allowed. A second stalled request cannot create an
	// unbounded sequence of agent scan workers on this connection.
	env.cleanup.mu.Lock()
	flight := env.cleanup.reconcileFlights[host]
	flight.startedAt = time.Now().Add(-31 * time.Second)
	env.cleanup.reconcileFlights[host] = flight
	env.cleanup.mu.Unlock()
	if code, body := env.do(t, http.MethodPost, path, request); code != 202 {
		t.Fatalf("timed retry POST = %d %s", code, body)
	}
	select {
	case <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("one bounded retry missing")
	}
	env.cleanup.mu.Lock()
	flight = env.cleanup.reconcileFlights[host]
	flight.startedAt = time.Now().Add(-31 * time.Second)
	env.cleanup.reconcileFlights[host] = flight
	env.cleanup.mu.Unlock()
	for range 50 {
		if code, body := env.do(t, http.MethodPost, path, request); code != 202 {
			t.Fatalf("exhausted duplicate POST = %d %s", code, body)
		}
	}
	select {
	case extra := <-env.cleanupWire.inventoryRequests:
		t.Fatalf("unbounded retry sent: %+v", extra)
	default:
	}
	var state string
	var reason *string
	if err := env.pool.QueryRow(context.Background(), `SELECT state,reason FROM host_image_cleanup_attempts
		WHERE host_id=$1::uuid`, host).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "unknown" || reason == nil || *reason != "inventory_unknown" {
		t.Fatalf("exhausted scan should surface uncertain recovery remedy: state=%s reason=%v", state, reason)
	}
}

func TestCleanupHistoryIdentityChangeDuringAndAfterFlightGetsFreshScan(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-history-change-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	env.cleanupWire.inventoryRequests = make(chan agentws.ImageInventoryReconcileCmd, 3)
	snapshot := agentws.ImageCleanupSnapshot{ConnectionID: "same", Capable: true, Complete: true,
		Versions: []agentws.ImageVersionEntry{{ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2,
			RuntimeImageID: "sha256:history-current", State: "present"}}}
	env.cleanupWire.set(host, snapshot)
	env.cleanup.ImageCleanupRegistered(context.Background(), host)
	var first agentws.ImageInventoryReconcileCmd
	select {
	case first = <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("first scan missing")
	}
	oldRef := "sha256:history-old"
	if _, err := env.pool.Exec(context.Background(), `INSERT INTO host_image_success_history
		(host_id,image_id,current_version,current_identity,previous_version,previous_identity,verified_at)
		VALUES($1::uuid,$2,$3,jsonb_build_object('registry_ref',$4::text),$5,jsonb_build_object('registry_ref',$6::text),now())`,
		host, imgID, imgVer2, imgDigest2, imgVer, oldRef); err != nil {
		t.Fatal(err)
	}
	env.cleanup.ImageCleanupRegistered(context.Background(), host)
	select {
	case extra := <-env.cleanupWire.inventoryRequests:
		t.Fatalf("changed history started concurrent scan: %+v", extra)
	default:
	}
	snapshot.ReconciledRevision, snapshot.ReconciledRequestID = 1, first.ID
	env.cleanupWire.set(host, snapshot)
	env.cleanup.ImageVersionsChanged(context.Background(), host)
	var second agentws.ImageInventoryReconcileCmd
	select {
	case second = <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("changed history was dropped during flight")
	}
	contains := func(cmd agentws.ImageInventoryReconcileCmd, version, ref string) bool {
		for _, identity := range cmd.Identities {
			if identity.ImageID == imgID && identity.Version == version && identity.ImageRef == ref {
				return true
			}
		}
		return false
	}
	if !contains(second, imgVer, oldRef) {
		t.Fatalf("follow-up omitted previous success: %+v", second.Identities)
	}
	snapshot.ReconciledRevision, snapshot.ReconciledRequestID = 2, second.ID
	env.cleanupWire.set(host, snapshot)
	env.cleanup.ImageVersionsChanged(context.Background(), host)
	newOldRef := "sha256:history-older"
	if _, err := env.pool.Exec(context.Background(), `UPDATE host_image_success_history
		SET previous_version='v0',previous_identity=jsonb_build_object('registry_ref',$3::text)
		WHERE host_id=$1::uuid AND image_id=$2`, host, imgID, newOldRef); err != nil {
		t.Fatal(err)
	}
	env.cleanup.ImageCleanupRegistered(context.Background(), host)
	select {
	case third := <-env.cleanupWire.inventoryRequests:
		if !contains(third, "v0", newOldRef) {
			t.Fatalf("post-completion scan omitted changed history: %+v", third.Identities)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("post-completion history change did not scan")
	}
}

func TestCleanupAdoptionWritersReconcileManagedIdentitySet(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-adoption-hook-host")
	host := hosts[0]
	seedCatalogDigest(t, env.pool, imgVer, imgRef)
	env.store.SetCleanupService(env.cleanup)
	env.cleanupWire.inventoryRequests = make(chan agentws.ImageInventoryReconcileCmd, 2)
	snapshot := agentws.ImageCleanupSnapshot{ConnectionID: "same", Capable: true, Complete: true,
		Versions: []agentws.ImageVersionEntry{}}
	env.cleanupWire.set(host, snapshot)
	if _, err := env.store.Install(context.Background(), imgID, true); err != nil {
		t.Fatal(err)
	}
	var installed agentws.ImageInventoryReconcileCmd
	select {
	case installed = <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("install did not reconcile")
	}
	found := false
	for _, identity := range installed.Identities {
		if identity.ImageID == imgID && identity.Version == imgVer && identity.ImageRef == imgRef {
			found = true
		}
	}
	if !found {
		t.Fatalf("install identity missing: %+v", installed.Identities)
	}
	snapshot.ReconciledRevision, snapshot.ReconciledRequestID = 1, installed.ID
	env.cleanupWire.set(host, snapshot)
	env.cleanup.ImageVersionsChanged(context.Background(), host)
	if _, err := env.pool.Exec(context.Background(), `UPDATE image_catalog SET version=$2,registry_digest=$3 WHERE id=$1`,
		imgID, imgVer2, imgDigest2); err != nil {
		t.Fatal(err)
	}
	if applied, _, err := env.store.Update(context.Background(), imgID); err != nil || !applied {
		t.Fatalf("update adoption: applied=%t err=%v", applied, err)
	}
	var updated agentws.ImageInventoryReconcileCmd
	select {
	case updated = <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("update did not reconcile")
	}
	found = false
	for _, identity := range updated.Identities {
		if identity.ImageID == imgID && identity.Version == imgVer2 && identity.ImageRef == imgDigest2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("updated adoption identity missing: %+v", updated.Identities)
	}
	snapshot.ReconciledRevision, snapshot.ReconciledRequestID = 2, updated.ID
	env.cleanupWire.set(host, snapshot)
	env.cleanup.ImageVersionsChanged(context.Background(), host)
	if err := env.store.Uninstall(context.Background(), imgID); err != nil {
		t.Fatal(err)
	}
	select {
	case uninstalled := <-env.cleanupWire.inventoryRequests:
		for _, identity := range uninstalled.Identities {
			if identity.ImageID == imgID {
				t.Fatalf("uninstall retained adoption identity: %+v", uninstalled.Identities)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("uninstall did not reconcile")
	}
}

func TestCleanupReadyHistoryWriterQueuesFollowupDuringScan(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-ready-history-hook-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	env.ens.AgentImageState(context.Background(), host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "ready"})
	env.ens.SetCleanupService(env.cleanup)
	env.cleanupWire.inventoryRequests = make(chan agentws.ImageInventoryReconcileCmd, 2)
	snapshot := agentws.ImageCleanupSnapshot{ConnectionID: "same", Capable: true, Complete: true,
		Versions: []agentws.ImageVersionEntry{{ImageID: imgID, Version: imgVer, ImageRef: imgRef,
			RuntimeImageID: "sha256:ready-old", State: "present"}}}
	env.cleanupWire.set(host, snapshot)
	env.cleanup.ImageCleanupRegistered(context.Background(), host)
	var first agentws.ImageInventoryReconcileCmd
	select {
	case first = <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("first scan missing")
	}
	if _, err := env.pool.Exec(context.Background(), `UPDATE installed_images SET version=$2,registry_ref=$3 WHERE image_id=$1`,
		imgID, imgVer2, imgDigest2); err != nil {
		t.Fatal(err)
	}
	env.ens.AgentImageState(context.Background(), host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer2, State: "ready"})
	select {
	case extra := <-env.cleanupWire.inventoryRequests:
		t.Fatalf("ready report started concurrent scan: %+v", extra)
	default:
	}
	snapshot.ReconciledRevision, snapshot.ReconciledRequestID = 1, first.ID
	env.cleanupWire.set(host, snapshot)
	env.cleanup.ImageVersionsChanged(context.Background(), host)
	var second agentws.ImageInventoryReconcileCmd
	select {
	case second = <-env.cleanupWire.inventoryRequests:
		old, current := false, false
		for _, identity := range second.Identities {
			if identity.ImageID == imgID && identity.Version == imgVer && identity.ImageRef == imgRef {
				old = true
			}
			if identity.ImageID == imgID && identity.Version == imgVer2 && identity.ImageRef == imgDigest2 {
				current = true
			}
		}
		if !old || !current {
			t.Fatalf("history follow-up missing old/current identities: %+v", second.Identities)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ready history change did not trigger follow-up")
	}
	snapshot.ReconciledRevision, snapshot.ReconciledRequestID = 2, second.ID
	env.cleanupWire.set(host, snapshot)
	env.cleanup.ImageVersionsChanged(context.Background(), host)
	env.ens.AgentImageState(context.Background(), host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer2, State: "ready"})
	select {
	case extra := <-env.cleanupWire.inventoryRequests:
		t.Fatalf("unchanged ready replay triggered new scan: %+v", extra)
	default:
	}
}

func TestCleanupRegistrationHistoryWriterQueuesFollowupDuringScan(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-register-history-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	ctx := context.Background()
	if err := env.ens.reconcile(ctx, host, []agentws.RegisterImage{{ImageID: imgID, Version: imgVer, State: "ready"}}); err != nil {
		t.Fatal(err)
	}
	env.ens.SetCleanupService(env.cleanup)
	env.cleanupWire.inventoryRequests = make(chan agentws.ImageInventoryReconcileCmd, 2)
	snapshot := agentws.ImageCleanupSnapshot{ConnectionID: "same", Capable: true, Complete: true}
	env.cleanupWire.set(host, snapshot)
	env.cleanup.ImageCleanupRegistered(ctx, host)
	var first agentws.ImageInventoryReconcileCmd
	select {
	case first = <-env.cleanupWire.inventoryRequests:
	case <-time.After(3 * time.Second):
		t.Fatal("initial registration scan was not sent")
	}
	if _, err := env.pool.Exec(ctx, `UPDATE installed_images SET version=$2,registry_ref=$3 WHERE image_id=$1`, imgID, imgVer2, imgDigest2); err != nil {
		t.Fatal(err)
	}
	if err := env.ens.reconcile(ctx, host, []agentws.RegisterImage{{ImageID: imgID, Version: imgVer2, State: "ready"}}); err != nil {
		t.Fatal(err)
	}
	snapshot.ReconciledRevision, snapshot.ReconciledRequestID = 1, first.ID
	env.cleanupWire.set(host, snapshot)
	env.cleanup.ImageVersionsChanged(ctx, host)
	select {
	case second := <-env.cleanupWire.inventoryRequests:
		found := false
		for _, identity := range second.Identities {
			if identity.ImageID == imgID && identity.Version == imgVer && identity.ImageRef == imgRef {
				found = true
			}
		}
		if !found {
			t.Fatalf("registration follow-up omitted retained identity: %+v", second.Identities)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("registration history change did not schedule a follow-up scan")
	}
}

func TestCleanupManagedIdentityTriggerRetriesFailedSend(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-trigger-retry-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	env.cleanupWire.inventoryRequests = make(chan agentws.ImageInventoryReconcileCmd, 1)
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "same", Capable: true, Complete: true})
	env.cleanupWire.mu.Lock()
	env.cleanupWire.inventorySendErr = errors.New("temporary send failure")
	env.cleanupWire.mu.Unlock()
	failed := make(chan struct{}, 1)
	env.cleanupWire.inventorySendHook = func() {
		select {
		case failed <- struct{}{}:
		default:
		}
	}
	env.cleanup.ManagedIdentityChanged(host)
	select {
	case <-failed:
	case <-time.After(3 * time.Second):
		t.Fatal("initial send did not run")
	}
	env.cleanupWire.mu.Lock()
	env.cleanupWire.inventorySendErr = nil
	env.cleanupWire.mu.Unlock()
	select {
	case <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("transient send failure discarded identity trigger")
	}
}

func TestCleanupFleetIdentityTriggerRetriesFailedHostQuery(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-fleet-query-retry-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	env.cleanupWire.inventoryRequests = make(chan agentws.ImageInventoryReconcileCmd, 1)
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "same", Capable: true, Complete: true})
	ctx := context.Background()
	lock, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(ctx) }()
	if _, err := lock.Exec(ctx, `LOCK TABLE hosts IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	env.cleanup.ManagedIdentitiesChanged()
	// The worker's first host query has a ten-second deadline. Once it fails,
	// releasing the lock must let the same trigger reach the connected host.
	time.Sleep(11 * time.Second)
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("transient fleet query failure discarded identity trigger")
	}
}

func TestCleanupIncompleteRepliesKeepBoundedSameAttemptFlight(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-incomplete-flight-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	env.cleanupWire.inventoryRequests = make(chan agentws.ImageInventoryReconcileCmd, 3)
	snapshot := agentws.ImageCleanupSnapshot{ConnectionID: "same", Capable: true, Complete: true,
		Versions: []agentws.ImageVersionEntry{{ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2,
			RuntimeImageID: "sha256:incomplete", State: "present"}}}
	env.cleanupWire.set(host, snapshot)
	path := "/v1/admin/hosts/" + host + "/images/cleanup"
	request := `{"image_id":"` + imgID + `","version":"` + imgVer2 + `","image_ref":"` + imgDigest2 + `","runtime_image_id":"sha256:incomplete","expected_generation":"0"}`
	if code, body := env.do(t, http.MethodPost, path, request); code != 202 {
		t.Fatalf("initial POST = %d %s", code, body)
	}
	var first agentws.ImageInventoryReconcileCmd
	select {
	case first = <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("first scan missing")
	}
	snapshot.Complete, snapshot.ReconciledRevision, snapshot.ReconciledRequestID = false, 1, first.ID
	env.cleanupWire.set(host, snapshot)
	env.cleanup.ImageVersionsChanged(context.Background(), host)
	for range 50 {
		if code, body := env.do(t, http.MethodPost, path, request); code != 202 {
			t.Fatalf("incomplete duplicate POST = %d %s", code, body)
		}
	}
	select {
	case extra := <-env.cleanupWire.inventoryRequests:
		t.Fatalf("incomplete reply multiplied scan: %+v", extra)
	default:
	}
	env.cleanup.mu.Lock()
	flight := env.cleanup.reconcileFlights[host]
	flight.startedAt = time.Now().Add(-31 * time.Second)
	env.cleanup.reconcileFlights[host] = flight
	env.cleanup.mu.Unlock()
	if code, body := env.do(t, http.MethodPost, path, request); code != 202 {
		t.Fatalf("bounded retry = %d %s", code, body)
	}
	var second agentws.ImageInventoryReconcileCmd
	select {
	case second = <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("bounded retry missing")
	}
	snapshot.ReconciledRevision, snapshot.ReconciledRequestID = 2, second.ID
	env.cleanupWire.set(host, snapshot)
	env.cleanup.ImageVersionsChanged(context.Background(), host)
	env.cleanup.mu.Lock()
	flight = env.cleanup.reconcileFlights[host]
	flight.startedAt = time.Now().Add(-31 * time.Second)
	env.cleanup.reconcileFlights[host] = flight
	env.cleanup.mu.Unlock()
	for range 50 {
		if code, body := env.do(t, http.MethodPost, path, request); code != 202 {
			t.Fatalf("exhausted incomplete POST = %d %s", code, body)
		}
	}
	select {
	case extra := <-env.cleanupWire.inventoryRequests:
		t.Fatalf("incomplete replies escaped retry bound: %+v", extra)
	default:
	}
}

func TestCleanupUnadoptedReadyReportDoesNotStartManagedScan(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-unadopted-ready-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	env.ens.SetCleanupService(env.cleanup)
	env.cleanupWire.inventoryRequests = make(chan agentws.ImageInventoryReconcileCmd, 1)
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "same", Capable: true, Complete: true,
		Versions: []agentws.ImageVersionEntry{}})
	env.ens.AgentImageState(context.Background(), host, agentws.ImageStateMsg{ImageID: imgID, Version: imgVer, State: "ready"})
	select {
	case scan := <-env.cleanupWire.inventoryRequests:
		t.Fatalf("unadopted ready report started managed scan: %+v", scan)
	default:
	}
}

func TestCleanupReconcileSendFailureKeepsProofClosedUntilRetry(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-send-failure-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	env.cleanupWire.inventoryRequests = make(chan agentws.ImageInventoryReconcileCmd, 1)
	env.cleanupWire.journalRequests = make(chan agentws.ImageCleanupJournalRequestCmd, 1)
	entry := agentws.ImageVersionEntry{ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2,
		RuntimeImageID: "sha256:send-failure", State: "present"}
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "same", Capable: true, Complete: true,
		Versions: []agentws.ImageVersionEntry{entry}})
	failedSend := make(chan struct{})
	var once sync.Once
	env.cleanupWire.mu.Lock()
	env.cleanupWire.inventorySendErr = errors.New("queue unavailable")
	env.cleanupWire.inventorySendHook = func() { once.Do(func() { close(failedSend) }) }
	env.cleanupWire.mu.Unlock()
	path := "/v1/admin/hosts/" + host + "/images/cleanup"
	request := `{"image_id":"` + imgID + `","version":"` + imgVer2 + `","image_ref":"` + imgDigest2 + `","runtime_image_id":"sha256:send-failure","expected_generation":"0"}`
	if code, body := env.do(t, http.MethodPost, path, request); code != 202 {
		t.Fatalf("initial POST = %d %s", code, body)
	}
	select {
	case <-failedSend:
	case <-time.After(5 * time.Second):
		t.Fatal("initial send did not fail")
	}
	env.cleanupWire.mu.Lock()
	env.cleanupWire.inventorySendErr = nil
	env.cleanupWire.mu.Unlock()
	env.cleanup.requestCleanupJournal(context.Background(), host)
	select {
	case journal := <-env.cleanupWire.journalRequests:
		t.Fatalf("failed send admitted stale journal: %+v", journal)
	default:
	}
	if code, body := env.do(t, http.MethodPost, path, request); code != 202 {
		t.Fatalf("retry POST = %d %s", code, body)
	}
	select {
	case <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not resend reconcile")
	}
}

func TestCleanupPOSTRechecksAppReferenceAfterFenceLock(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-reference-race-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	entry := agentws.ImageVersionEntry{ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2, RuntimeImageID: "sha256:old-daemon", State: "present"}
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "current", Capable: true, Complete: true, Versions: []agentws.ImageVersionEntry{entry}})
	path := "/v1/admin/hosts/" + host + "/images/cleanup"
	if code, body := env.do(t, http.MethodGet, path, ""); code != 200 || !strings.Contains(string(body), `"eligible":true`) {
		t.Fatalf("pre-race preview = %d %s", code, body)
	}
	tx, err := env.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(context.Background(), `INSERT INTO host_image_operation_fences(host_id,image_id,state)
		VALUES($1::uuid,$2,'idle') ON CONFLICT DO NOTHING`, host, imgID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(context.Background(), `UPDATE host_image_operation_fences SET generation=generation+1
		WHERE host_id=$1::uuid AND image_id=$2`, host, imgID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(context.Background(), `INSERT INTO apps(name,runtime_spec)
		VALUES('cleanup race selected app',jsonb_build_object('image',$1::text))`, imgDigest2); err != nil {
		t.Fatal(err)
	}
	request := `{"image_id":"` + imgID + `","version":"` + imgVer2 + `","image_ref":"` + imgDigest2 + `","runtime_image_id":"sha256:old-daemon","expected_generation":"0"}`
	type result struct {
		code int
		body []byte
	}
	answer := make(chan result, 1)
	go func() { code, body := env.do(t, http.MethodPost, path, request); answer <- result{code, body} }()
	select {
	case got := <-answer:
		t.Fatalf("POST escaped uncommitted reference: %d %s", got.code, got.body)
	case <-time.After(50 * time.Millisecond):
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-answer:
		if got.code != 409 || !strings.Contains(string(got.body), `"stale_preview"`) {
			t.Fatalf("stale reference race = %d %s", got.code, got.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("POST remained blocked after writer committed")
	}
}

func TestCleanupJournalLostRepliesKeepOneCurrentRequest(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-journal-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	env.cleanupWire.inventoryRequests = make(chan agentws.ImageInventoryReconcileCmd, 1)
	entry := agentws.ImageVersionEntry{ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2, RuntimeImageID: "sha256:journal", State: "present"}
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "connection-a", Capable: true, Complete: true, Versions: []agentws.ImageVersionEntry{entry}})
	path := "/v1/admin/hosts/" + host + "/images/cleanup"
	request := `{"image_id":"` + imgID + `","version":"` + imgVer2 + `","image_ref":"` + imgDigest2 + `","runtime_image_id":"sha256:journal","expected_generation":"0"}`
	if code, body := env.do(t, http.MethodPost, path, request); code != 202 {
		t.Fatalf("cleanup request = %d %s", code, body)
	}
	var reconcile agentws.ImageInventoryReconcileCmd
	select {
	case reconcile = <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("post-dispatch reconcile missing")
	}
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "connection-a", Capable: true, Complete: true,
		ReconciledRevision: 1, ReconciledRequestID: reconcile.ID, Versions: []agentws.ImageVersionEntry{entry}})
	for range 5 {
		env.cleanup.requestCleanupJournal(context.Background(), host)
	}
	env.cleanup.mu.Lock()
	outstanding := len(env.cleanup.journalRequests)
	env.cleanup.mu.Unlock()
	if outstanding != 1 {
		t.Fatalf("lost journal replies accumulated %d requests, want one current request", outstanding)
	}
}

func TestCleanupAgentReasonCannotExposeRawDaemonText(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-safe-reason-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	entry := agentws.ImageVersionEntry{ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2, RuntimeImageID: "sha256:reason", State: "present"}
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "current", Capable: true, Complete: true, Versions: []agentws.ImageVersionEntry{entry}})
	path := "/v1/admin/hosts/" + host + "/images/cleanup"
	request := `{"image_id":"` + imgID + `","version":"` + imgVer2 + `","image_ref":"` + imgDigest2 + `","runtime_image_id":"sha256:reason","expected_generation":"0"}`
	code, body := env.do(t, http.MethodPost, path, request)
	if code != 202 {
		t.Fatalf("cleanup request = %d %s", code, body)
	}
	var attempt CleanupAttempt
	if err := json.Unmarshal(body, &attempt); err != nil {
		t.Fatal(err)
	}
	raw := "daemon read /operator/path/credential=[redacted] failed"
	env.cleanup.ImageCleanupState(context.Background(), host, agentws.ImageCleanupStateMsg{
		AttemptID: attempt.AttemptID, ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2,
		RuntimeImageID: entry.RuntimeImageID, Generation: "1", State: "unknown", Reason: &raw,
	})
	code, body = env.do(t, http.MethodPost, path, request)
	if code != 202 || strings.Contains(string(body), raw) || !strings.Contains(string(body), `"reason":null`) {
		t.Fatalf("raw daemon reason reached HTTP response: %d %s", code, body)
	}
	var persisted *string
	if err := env.pool.QueryRow(context.Background(), `SELECT reason FROM host_image_cleanup_attempts WHERE id=$1::uuid`, attempt.AttemptID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != nil {
		t.Fatalf("raw daemon reason persisted as %q", *persisted)
	}
}

func TestCleanupAttemptReadIsDurableHostScopedAndSafe(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-attempt-host", "cleanup-other-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	entry := agentws.ImageVersionEntry{ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2, RuntimeImageID: "sha256:status", State: "present"}
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "current", Capable: true, Complete: true, Versions: []agentws.ImageVersionEntry{entry}})
	path := "/v1/admin/hosts/" + host + "/images/cleanup"
	request := `{"image_id":"` + imgID + `","version":"` + imgVer2 + `","image_ref":"` + imgDigest2 + `","runtime_image_id":"sha256:status","expected_generation":"0"}`
	code, body := env.do(t, http.MethodPost, path, request)
	if code != 202 {
		t.Fatalf("cleanup request = %d %s", code, body)
	}
	var attempt CleanupAttempt
	if err := json.Unmarshal(body, &attempt); err != nil {
		t.Fatal(err)
	}
	readPath := path + "/attempts/" + attempt.AttemptID
	if code, body := env.do(t, http.MethodGet, readPath, ""); code != 200 || !strings.Contains(string(body), `"state":"removing"`) {
		t.Fatalf("persisted pending read = %d %s", code, body)
	}
	if code, body := env.do(t, http.MethodGet, "/v1/admin/hosts/"+hosts[1]+"/images/cleanup/attempts/"+attempt.AttemptID, ""); code != 404 {
		t.Fatalf("other-host attempt read = %d %s", code, body)
	}
	if code, body := env.do(t, http.MethodGet, path+"/attempts/not-a-uuid", ""); code != 400 {
		t.Fatalf("malformed attempt ID = %d %s", code, body)
	}
	if code, body := env.do(t, http.MethodGet, "/v1/admin/hosts/not-a-uuid/images/cleanup/attempts/"+attempt.AttemptID, ""); code != 400 {
		t.Fatalf("malformed host ID = %d %s", code, body)
	}
	reason := "reference_in_use"
	env.cleanup.ImageCleanupState(context.Background(), host, agentws.ImageCleanupStateMsg{
		AttemptID: attempt.AttemptID, ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2,
		RuntimeImageID: entry.RuntimeImageID, Generation: "1", State: "failed", Reason: &reason,
	})
	if code, body := env.do(t, http.MethodGet, readPath, ""); code != 200 || !strings.Contains(string(body), `"state":"failed"`) || !strings.Contains(string(body), `"reason":"reference_in_use"`) {
		t.Fatalf("persisted terminal read = %d %s", code, body)
	}
	// Even a legacy/corrupt row cannot forward raw agent text through the new
	// read or the existing idempotent POST response.
	if _, err := env.pool.Exec(context.Background(), `UPDATE host_image_cleanup_attempts SET reason=$2 WHERE id=$1::uuid`,
		attempt.AttemptID, "daemon /operator/path/credential=[redacted]"); err != nil {
		t.Fatal(err)
	}
	if code, body := env.do(t, http.MethodGet, readPath, ""); code != 200 || !strings.Contains(string(body), `"reason":null`) || strings.Contains(string(body), "/operator/path") {
		t.Fatalf("raw reason from read = %d %s", code, body)
	}
}

func TestCleanupMissingJournalRequiresCurrentConnectionRetirement(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-retirement-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	env.cleanupWire.inventoryRequests = make(chan agentws.ImageInventoryReconcileCmd, 2)
	entry := agentws.ImageVersionEntry{ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2, RuntimeImageID: "sha256:retired", State: "present"}
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "old", Capable: true, Complete: true, Versions: []agentws.ImageVersionEntry{entry}})
	path := "/v1/admin/hosts/" + host + "/images/cleanup"
	request := `{"image_id":"` + imgID + `","version":"` + imgVer2 + `","image_ref":"` + imgDigest2 + `","runtime_image_id":"sha256:retired","expected_generation":"0"}`
	code, body := env.do(t, http.MethodPost, path, request)
	if code != 202 {
		t.Fatalf("cleanup request = %d %s", code, body)
	}
	var attempt CleanupAttempt
	if err := json.Unmarshal(body, &attempt); err != nil {
		t.Fatal(err)
	}
	var oldReconcile agentws.ImageInventoryReconcileCmd
	select {
	case oldReconcile = <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("initial reconcile missing")
	}
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "old", Capable: true, Complete: true,
		ReconciledRevision: 1, ReconciledRequestID: oldReconcile.ID, Versions: []agentws.ImageVersionEntry{entry}})
	env.cleanup.requestCleanupJournal(context.Background(), host)
	env.cleanup.mu.Lock()
	var oldRequest string
	for id := range env.cleanup.journalRequests {
		oldRequest = id
	}
	env.cleanup.mu.Unlock()
	entry.State = "absent"
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "new", Capable: true, Complete: true,
		Versions: []agentws.ImageVersionEntry{entry}})
	env.cleanup.ImageCleanupJournal(context.Background(), host, agentws.ImageCleanupJournalMsg{
		RequestID: oldRequest, RetiredAttemptIDs: []string{attempt.AttemptID},
	})
	var state string
	if err := env.pool.QueryRow(context.Background(), `SELECT state FROM host_image_cleanup_attempts WHERE id=$1::uuid`, attempt.AttemptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "removing" {
		t.Fatalf("old-connection journal resolved attempt as %s", state)
	}
	env.cleanup.ImageCleanupRegistered(context.Background(), host)
	var newReconcile agentws.ImageInventoryReconcileCmd
	select {
	case newReconcile = <-env.cleanupWire.inventoryRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("new-connection reconcile missing")
	}
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "new", Capable: true, Complete: true,
		ReconciledRevision: 1, ReconciledRequestID: newReconcile.ID, Versions: []agentws.ImageVersionEntry{entry}})
	env.cleanup.ImageVersionsChanged(context.Background(), host)
	env.cleanup.mu.Lock()
	var newRequest string
	for id := range env.cleanup.journalRequests {
		if id != oldRequest {
			newRequest = id
		}
	}
	env.cleanup.mu.Unlock()
	if newRequest == "" {
		t.Fatal("current connection did not get a fresh journal request")
	}
	env.cleanup.ImageCleanupJournal(context.Background(), host, agentws.ImageCleanupJournalMsg{
		RequestID: newRequest, RetiredAttemptIDs: []string{attempt.AttemptID},
	})
	if err := env.pool.QueryRow(context.Background(), `SELECT state FROM host_image_cleanup_attempts WHERE id=$1::uuid`, attempt.AttemptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "removed" {
		t.Fatalf("current-connection retirement plus exact absence left attempt %s", state)
	}
}

func TestCleanupRetiredDuplicateNeverReleasesUncertainFence(t *testing.T) {
	env, hosts := newActionsEnv(t, "cleanup-retired-duplicate-host")
	host := hosts[0]
	seedCatalog(t, env.pool)
	install(t, env.pool, false)
	entry := agentws.ImageVersionEntry{ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2, RuntimeImageID: "sha256:uncertain", State: "present"}
	env.cleanupWire.set(host, agentws.ImageCleanupSnapshot{ConnectionID: "old", Capable: true, Complete: true, Versions: []agentws.ImageVersionEntry{entry}})
	path := "/v1/admin/hosts/" + host + "/images/cleanup"
	request := `{"image_id":"` + imgID + `","version":"` + imgVer2 + `","image_ref":"` + imgDigest2 + `","runtime_image_id":"sha256:uncertain","expected_generation":"0"}`
	code, body := env.do(t, http.MethodPost, path, request)
	if code != 202 {
		t.Fatalf("cleanup request = %d %s", code, body)
	}
	var attempt CleanupAttempt
	if err := json.Unmarshal(body, &attempt); err != nil {
		t.Fatal(err)
	}
	env.cleanupWire.mu.Lock()
	delete(env.cleanupWire.snapshots, host)
	env.cleanupWire.mu.Unlock()
	reason := "retired_attempt"
	env.cleanup.ImageCleanupState(context.Background(), host, agentws.ImageCleanupStateMsg{
		AttemptID: attempt.AttemptID, ImageID: imgID, Version: imgVer2, ImageRef: imgDigest2,
		RuntimeImageID: entry.RuntimeImageID, Generation: "1", State: "failed", Reason: &reason,
	})
	var state, fence string
	if err := env.pool.QueryRow(context.Background(), `SELECT a.state,f.state FROM host_image_cleanup_attempts a
		JOIN host_image_operation_fences f ON f.host_id=a.host_id AND f.image_id=a.image_id
		WHERE a.id=$1::uuid`, attempt.AttemptID).Scan(&state, &fence); err != nil {
		t.Fatal(err)
	}
	if state != "unknown" || fence != "removing" {
		t.Fatalf("retired duplicate released uncertain removal: attempt=%s fence=%s", state, fence)
	}
}
