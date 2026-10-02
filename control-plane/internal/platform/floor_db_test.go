// below_floor through the admin API against a real Postgres, behind the real
// RequireAuth→RequireAdmin chain: the release view serves it, and a revert from or to
// a release below the declared floor is refused (control-api.md amendment 14).
package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// A version that orders below this build's declared floor (0.4.0-0).
const belowFloorVersion = "0.3.9"

func notEligibleReason(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return e.Reason
}

func TestReleaseViewServesBelowFloor(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)
	below := seedHost(t, pool, "gpu-below", commitA, "online")
	actorBelow := seedHost(t, pool, "gpu-actor-below", commitA, "online")
	fine := seedHost(t, pool, "gpu-fine", commitA, "online")
	mustExec(t, pool, `UPDATE hosts SET agent_version = $1 WHERE id = $2::uuid`, belowFloorVersion, below)
	mustExec(t, pool, `UPDATE hosts SET install_mode = 'owned', agent_version = '0.4.0',
		recovery_actor_version = $1 WHERE id = $2::uuid`, belowFloorVersion, actorBelow)
	mustExec(t, pool, `UPDATE hosts SET agent_version = 'dev' WHERE id = $1::uuid`, fine)

	deps := &Deps{
		Channel:   func(context.Context) (string, string, error) { return ChannelStable, "develop", nil },
		Hosts:     store.Hosts,
		Releases:  store.Releases,
		Detection: func(context.Context) (DetectionStatus, error) { return DetectionStatus{}, nil },
	}
	adminToken, _, url := newViewHarness(t, pool, deps)
	code, body := get(t, url, adminToken)
	if code != http.StatusOK {
		t.Fatalf("view = %d (%s)", code, body)
	}
	var view struct {
		Installed struct {
			Hosts []struct {
				HostID     string `json:"host_id"`
				BelowFloor *bool  `json:"below_floor"`
			} `json:"hosts"`
		} `json:"installed"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]bool{below: true, actorBelow: true, fine: false}
	for _, h := range view.Installed.Hosts {
		if h.BelowFloor == nil {
			t.Errorf("host %s: below_floor not served", h.HostID)
			continue
		}
		if *h.BelowFloor != want[h.HostID] {
			t.Errorf("host %s: below_floor = %v, want %v", h.HostID, *h.BelowFloor, want[h.HostID])
		}
	}
	_ = ctx
}

func TestRevertOfAHostBelowTheFloorIsRefused(t *testing.T) {
	h := newApplyHarness(t)
	seedSucceeded(t, h, KindApply, digestNew, digestOld, &h.release.ID)
	mustExec(t, h.pool, `UPDATE hosts SET agent_version = $1 WHERE id = $2::uuid`, belowFloorVersion, h.hostID)

	code, body := h.post(t, h.revertURL(), h.adminToken, map[string]any{"force": true})
	if code != http.StatusConflict || errCode(t, body) != CodeHostNotEligible || notEligibleReason(t, body) != ReasonBelowFloor {
		t.Fatalf("revert = %d %s, want 409 host_not_eligible/below_floor", code, body)
	}
	if h.agent.sentCount() != 0 {
		t.Error("a refused revert sent release_apply")
	}
}

// The digest the revert would restore belongs to a release this instance knows whose
// version is below the floor.
func TestRevertOntoAReleaseBelowTheFloorIsRefused(t *testing.T) {
	h := newApplyHarness(t)
	schema := buildinfo.Get().SchemaVersion
	old := Release{
		Channel: ChannelStable, Version: str(belowFloorVersion), SourceCommit: commitC,
		BuiltAt: at(1), SchemaVersion: schema,
		Manifest: json.RawMessage(fmt.Sprintf(`{
		  "format_version": 1, "version": %q, "prerelease": false,
		  "source_commit": %q, "built_at": "2026-09-01T12:00:00Z", "schema_version": %d,
		  "components": [
		    { "name": "control-plane", "image": "ghcr.io/accreleus/quasar/quasar-control-plane", "digest": "sha256:%s" },
		    { "name": "node-agent",    "image": "ghcr.io/accreleus/quasar/quasar-node-agent",    "digest": %q }
		  ]
		}`, belowFloorVersion, commitC, schema, hex64, digestOld)),
	}
	if _, err := h.store.UpsertRelease(context.Background(), old); err != nil {
		t.Fatalf("seed release: %v", err)
	}
	seedSucceeded(t, h, KindApply, digestNew, digestOld, &h.release.ID)

	code, body := h.post(t, h.revertURL(), h.adminToken, map[string]any{"force": true})
	if code != http.StatusConflict || notEligibleReason(t, body) != ReasonBelowFloor {
		t.Fatalf("revert = %d %s, want 409 below_floor", code, body)
	}
}

// A developer apply whose images are a known release below the floor is refused.
func TestDeveloperApplyOfAReleaseBelowTheFloorIsRefused(t *testing.T) {
	h := newDevHarness(t)
	h.images.commit = commitC
	seedRelease(t, h.store, commitC, buildinfo.Get().SchemaVersion, withVersion(belowFloorVersion),
		func(r *Release) { r.BuiltAt = at(1) })

	code, body := h.post(t, devURL, h.adminToken, h.body(agentComponent()))
	if code != http.StatusConflict || errCode(t, body) != CodeHostNotEligible || notEligibleReason(t, body) != ReasonBelowFloor {
		t.Fatalf("developer apply = %d %s, want 409 host_not_eligible/below_floor", code, body)
	}
	if h.agent.sentCount() != 0 {
		t.Error("a refused developer apply reached the host")
	}
}
