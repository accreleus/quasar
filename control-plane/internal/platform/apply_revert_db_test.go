// The revert endpoint against a real Postgres, behind the real
// RequireAuth→RequireAdmin chain.
package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// seedSucceeded writes one terminal attempt: what it asked for, and what the
// updater said was there before. The endpoint's whole input.
func seedSucceeded(t *testing.T, h *applyHarness, kind, requested, previous string, releaseID *string) {
	t.Helper()
	req, err := json.Marshal([]ComponentDigest{{
		Name: ComponentNodeAgent, Image: "ghcr.io/accreleus/quasar/quasar-node-agent", Digest: requested,
	}})
	if err != nil {
		t.Fatal(err)
	}
	prev := []PreviousDigest{{Name: ComponentNodeAgent}}
	if previous != "" {
		p := previous
		prev[0].Digest = &p
	}
	prevRaw, err := json.Marshal(prev)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(context.Background(), `
		INSERT INTO platform_apply_attempts
		    (kind, target, host_id, release_id, requested_digests, previous_digests, state, finished_at)
		VALUES ($1, 'host', $2::uuid, $3::uuid, $4::jsonb, $5::jsonb, 'succeeded', now())
	`, kind, h.hostID, releaseID, req, prevRaw); err != nil {
		t.Fatalf("seed succeeded attempt: %v", err)
	}
}

func (h *applyHarness) revertURL() string {
	return "/v1/admin/platform/hosts/" + h.hostID + "/revert"
}

func decodeAttempt(t *testing.T, body []byte) Attempt {
	t.Helper()
	var env AttemptEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return env.Attempt
}

func TestRevertIsAdminOnly(t *testing.T) {
	h := newApplyHarness(t)
	if code, _ := h.post(t, h.revertURL(), "", nil); code != http.StatusUnauthorized {
		t.Errorf("anonymous revert = %d, want 401", code)
	}
	if code, _ := h.post(t, h.revertURL(), h.userToken, nil); code != http.StatusForbidden {
		t.Errorf("non-admin revert = %d, want 403", code)
	}
}

func TestRevertWithNoSucceededAttempt(t *testing.T) {
	h := newApplyHarness(t)
	code, body := h.post(t, h.revertURL(), h.adminToken, map[string]any{})
	if code != http.StatusConflict || errCode(t, body) != CodeNothingToRevert {
		t.Fatalf("revert = %d %s, want 409 nothing_to_revert", code, body)
	}
	missing := "44444444-4444-4444-8444-444444444444"
	if code, _ := h.post(t, "/v1/admin/platform/hosts/"+missing+"/revert", h.adminToken, nil); code != http.StatusNotFound {
		t.Errorf("unknown host = %d, want 404", code)
	}
	// An attempt whose previous digest the updater never determined is nothing
	// to revert to either.
	seedSucceeded(t, h, KindApply, digestNew, "", &h.release.ID)
	code, body = h.post(t, h.revertURL(), h.adminToken, map[string]any{})
	if code != http.StatusConflict || errCode(t, body) != CodeNothingToRevert {
		t.Fatalf("revert with a null previous digest = %d %s, want 409 nothing_to_revert", code, body)
	}
}

func TestRevertRestoresThePreviousDigestAndIsItselfRevertible(t *testing.T) {
	h := newApplyHarness(t)
	seedSucceeded(t, h, KindApply, digestNew, digestOld, &h.release.ID)

	code, body := h.post(t, h.revertURL(), h.adminToken, map[string]any{"force": true})
	if code != http.StatusAccepted {
		t.Fatalf("revert = %d (%s), want 202", code, body)
	}
	a := decodeAttempt(t, body)
	if a.Kind != KindRevert || a.Target != TargetHost || a.RunID != nil {
		t.Errorf("attempt = %+v, want a standalone host revert", a)
	}
	if len(a.RequestedDigests) != 1 || a.RequestedDigests[0].Digest != digestOld {
		t.Fatalf("requested = %+v, want the previous digest", a.RequestedDigests)
	}
	if len(a.PreviousDigests) != 1 || a.PreviousDigests[0].Digest == nil || *a.PreviousDigests[0].Digest != digestNew {
		t.Fatalf("previous = %+v, want the reverted-from digest", a.PreviousDigests)
	}
	if !a.Force {
		t.Error("force was not recorded")
	}

	// The wire message is the ordinary release_apply, carrying the older digest.
	waitFor(t, "release_apply to be sent", func() bool { return h.agent.sentCount() == 1 })
	if got := h.agent.sent[0].Components; len(got) != 1 || got[0].Digest != digestOld {
		t.Errorf("sent components = %+v, want the previous digest", got)
	}

	// Resolve it, then revert again: the second revert walks back to where the
	// first one started.
	ctx := context.Background()
	if _, err := h.store.SucceedAttempt(ctx, a.ID); err != nil {
		t.Fatalf("succeed the revert: %v", err)
	}
	code, body = h.post(t, h.revertURL(), h.adminToken, map[string]any{"force": true})
	if code != http.StatusAccepted {
		t.Fatalf("second revert = %d (%s), want 202", code, body)
	}
	back := decodeAttempt(t, body)
	if back.Kind != KindRevert || len(back.RequestedDigests) != 1 || back.RequestedDigests[0].Digest != digestNew {
		t.Fatalf("second revert requested %+v, want the digest the first one replaced", back.RequestedDigests)
	}

	// And the history says which button was pressed.
	attempts, err := h.store.ListAttempts(ctx, h.hostID, 10)
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attempts) != 3 || attempts[0].Kind != KindRevert || attempts[2].Kind != KindApply {
		t.Fatalf("history = %d rows, kinds %v/%v", len(attempts), attempts[0].Kind, attempts[2].Kind)
	}
}

func TestRevertIsRefusedWhileAnAttemptIsInFlight(t *testing.T) {
	h := newApplyHarness(t)
	seedSucceeded(t, h, KindApply, digestNew, digestOld, &h.release.ID)
	if code, body := h.post(t, h.revertURL(), h.adminToken, map[string]any{"force": true}); code != http.StatusAccepted {
		t.Fatalf("first revert = %d (%s)", code, body)
	}
	code, body := h.post(t, h.revertURL(), h.adminToken, map[string]any{"force": true})
	if code != http.StatusConflict || errCode(t, body) != CodeAttemptInFlight {
		t.Fatalf("second revert = %d %s, want 409 attempt_in_flight", code, body)
	}
	// An apply cannot slip past an open revert either — the same index.
	if code, body := h.post(t, h.applyURL(), h.adminToken, HostApplyRequest{ReleaseID: h.release.ID}); code != http.StatusConflict {
		t.Fatalf("apply during a revert = %d %s, want 409", code, body)
	}
}

// The release row is provenance: found when the digest is still pinned by a
// manifest, null when the build can no longer be named.
func TestRevertNamesTheReleaseWhenTheDigestIsStillKnown(t *testing.T) {
	h := newApplyHarness(t)
	// hex64 is the node-agent digest in the seeded release's manifest.
	seedSucceeded(t, h, KindApply, digestNew, "sha256:"+hex64, &h.release.ID)
	code, body := h.post(t, h.revertURL(), h.adminToken, map[string]any{"force": true})
	if code != http.StatusAccepted {
		t.Fatalf("revert = %d (%s)", code, body)
	}
	a := decodeAttempt(t, body)
	if a.ReleaseID == nil || *a.ReleaseID != h.release.ID {
		t.Fatalf("release_id = %v, want the release whose manifest pins that digest", a.ReleaseID)
	}
	// The provenance the agent is sent is that release's, so a register on its
	// commit resolves the revert exactly as an apply's does.
	waitFor(t, "release_apply to be sent", func() bool { return h.agent.sentCount() == 1 })
	if got := h.agent.sent[0].Release.SourceCommit; got != h.release.SourceCommit {
		t.Errorf("release provenance = %q, want the restored release's commit", got)
	}
}

func TestRevertToAnUnnameableBuildIsAllowedAndCarriesNoRelease(t *testing.T) {
	h := newApplyHarness(t)
	seedSucceeded(t, h, KindApply, digestNew, digestOld, nil)
	code, body := h.post(t, h.revertURL(), h.adminToken, map[string]any{"force": true})
	if code != http.StatusAccepted {
		t.Fatalf("revert = %d (%s), want 202", code, body)
	}
	if a := decodeAttempt(t, body); a.ReleaseID != nil {
		t.Errorf("release_id = %v, want null when no release row pins the digest", *a.ReleaseID)
	}
}
