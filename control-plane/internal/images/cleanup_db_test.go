package images

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

type fakeCleanupWire struct {
	mu        sync.Mutex
	snapshots map[string]agentws.ImageCleanupSnapshot
	cleanup   chan agentws.ImageCleanupCmd
	acks      chan agentws.ImageCleanupStateAckCmd
}

func (f *fakeCleanupWire) ImageCleanupSnapshot(hostID string) (agentws.ImageCleanupSnapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.snapshots[hostID]
	s.Versions = append([]agentws.ImageVersionEntry(nil), s.Versions...)
	return s, ok
}
func (f *fakeCleanupWire) SendImageCleanup(_ context.Context, _ string, c agentws.ImageCleanupCmd) (agentws.AckResult, error) {
	if f.cleanup != nil {
		f.cleanup <- c
	}
	return agentws.AckResult{OK: true}, nil
}
func (f *fakeCleanupWire) SendImageInventoryReconcile(string, agentws.ImageInventoryReconcileCmd) error {
	return nil
}
func (f *fakeCleanupWire) SendImageCleanupJournalRequest(string, agentws.ImageCleanupJournalRequestCmd) error {
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

func TestCleanupFailedPresentPreservesReadyHostImage(t *testing.T) {
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
	if _, err := env.pool.Exec(context.Background(), `INSERT INTO host_images(host_id,image_id,version,state)
		VALUES($1::uuid,$2,$3,'ready')`, host, imgID, imgVer2); err != nil {
		t.Fatal(err)
	}
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
	if imageState != "ready" || attemptState != "failed" || fenceState != "idle" {
		t.Fatalf("failed-present transition: host_image=%s attempt=%s fence=%s", imageState, attemptState, fenceState)
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
