package platform

import (
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// The revert decision, as a table over the contract's rules rather than over
// the function's branches (control-api.md §revert).

const (
	digestOld = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	digestNew = "sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"
	agentRepo = "ghcr.io/accreleus/quasar/quasar-node-agent"
)

// succeededApply is a host's last succeeded attempt: it asked for digestNew and
// the updater reported the host had been on digestOld.
func succeededApply(previous *string) *Attempt {
	prev := PreviousDigest{Name: ComponentNodeAgent, Digest: previous}
	return &Attempt{
		ID:               "11111111-1111-4111-8111-111111111111",
		Kind:             KindApply,
		Target:           TargetHost,
		State:            AttemptSucceeded,
		RequestedDigests: []ComponentDigest{{Name: ComponentNodeAgent, Image: agentRepo, Digest: digestNew}},
		PreviousDigests:  []PreviousDigest{prev},
	}
}

func TestPlanRevert(t *testing.T) {
	old := digestOld
	empty := ""
	cpID := cp(commitB, 40)

	tests := []struct {
		name    string
		in      RevertInputs
		wantOK  bool
		code    string
		reason  string
		release string // the attempt's release_id, "" for null
	}{
		{
			name: "a host that has never succeeded an attempt",
			in:   RevertInputs{ControlPlane: cpID},
			code: CodeNothingToRevert,
		},
		{
			name: "the updater never determined what was there",
			in:   RevertInputs{LastSucceeded: succeededApply(nil), ControlPlane: cpID},
			code: CodeNothingToRevert,
		},
		{
			name: "an empty previous digest is not a digest",
			in:   RevertInputs{LastSucceeded: succeededApply(&empty), ControlPlane: cpID},
			code: CodeNothingToRevert,
		},
		{
			name: "the previous build is known and at or below the control plane",
			in: RevertInputs{
				LastSucceeded:       succeededApply(&old),
				PreviousRelease:     relPtr(rel("rel-old", "0.1.0", commitA, 39, at(1))),
				ControlPlaneRelease: relPtr(rel("rel-cp", "0.2.0", commitB, 40, at(4))),
				ControlPlane:        cpID,
			},
			wantOK:  true,
			release: "rel-old",
		},
		{
			// Only reachable when the control plane was moved backwards by
			// hand; refused rather than performed (ADR 0002).
			name: "the previous build orders above the control plane",
			in: RevertInputs{
				LastSucceeded:       succeededApply(&old),
				PreviousRelease:     relPtr(rel("rel-old", "0.3.0", commitA, 41, at(5))),
				ControlPlaneRelease: relPtr(rel("rel-cp", "0.2.0", commitB, 40, at(4))),
				ControlPlane:        cpID,
			},
			code:   CodeHostNotEligible,
			reason: ReasonReleaseAboveControlPlane,
		},
		{
			// Same schema, newer build: the built_at tiebreak is the ordering
			// `available` uses.
			name: "the previous build is newer at the same schema",
			in: RevertInputs{
				LastSucceeded:       succeededApply(&old),
				PreviousRelease:     relPtr(rel("rel-old", "0.3.0", commitA, 40, at(9))),
				ControlPlaneRelease: relPtr(rel("rel-cp", "0.2.0", commitB, 40, at(4))),
				ControlPlane:        cpID,
			},
			code:   CodeHostNotEligible,
			reason: ReasonReleaseAboveControlPlane,
		},
		{
			// Provenance lost, not a refusal: the host demonstrably ran that
			// digest, so it cannot be above this control plane.
			name:   "the previous build cannot be named",
			in:     RevertInputs{LastSucceeded: succeededApply(&old), ControlPlane: cpID},
			wantOK: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PlanRevert(tc.in)
			if got.OK != tc.wantOK {
				t.Fatalf("OK = %v (%s/%s), want %v", got.OK, got.Code, got.Reason, tc.wantOK)
			}
			if !tc.wantOK {
				if got.Code != tc.code || got.Reason != tc.reason {
					t.Fatalf("refusal = %s/%s, want %s/%s", got.Code, got.Reason, tc.code, tc.reason)
				}
				return
			}
			if len(got.Requested) != 1 || got.Requested[0].Name != ComponentNodeAgent {
				t.Fatalf("requested = %+v, want exactly the node-agent component", got.Requested)
			}
			if got.Requested[0].Digest != digestOld {
				t.Errorf("requested digest = %s, want the previous one", got.Requested[0].Digest)
			}
			if got.Requested[0].Image != agentRepo {
				t.Errorf("image = %q, want the repository the host was last sent", got.Requested[0].Image)
			}
			// A revert is itself revertible: what it replaces is recorded.
			if len(got.Previous) != 1 || got.Previous[0].Digest == nil || *got.Previous[0].Digest != digestNew {
				t.Errorf("previous = %+v, want the reverted-from digest", got.Previous)
			}
			switch {
			case tc.release == "" && got.ReleaseID != nil:
				t.Errorf("release_id = %v, want null", *got.ReleaseID)
			case tc.release != "" && (got.ReleaseID == nil || *got.ReleaseID != tc.release):
				t.Errorf("release_id = %v, want %s", got.ReleaseID, tc.release)
			}
		})
	}
}

// With no release row for the control plane there is no built_at to compare, so
// the bound falls back to schema_version, the key that always exists.
func TestPlanRevertFallsBackToSchemaVersionWithNoControlPlaneRow(t *testing.T) {
	old := digestOld
	in := RevertInputs{
		LastSucceeded:   succeededApply(&old),
		PreviousRelease: relPtr(rel("rel-old", "0.3.0", commitA, 41, at(1))),
		ControlPlane:    buildinfo.Identity{SchemaVersion: 40},
	}
	if got := PlanRevert(in); got.OK || got.Reason != ReasonReleaseAboveControlPlane {
		t.Fatalf("got %+v, want release_above_control_plane", got)
	}
	in.PreviousRelease = relPtr(rel("rel-old", "0.1.0", commitA, 40, at(9)))
	if got := PlanRevert(in); !got.OK {
		t.Fatalf("equal schema with no control-plane row = %+v, want ok", got)
	}
}

// A revert honours the durable facts about an install and the two single-flight
// reasons; the reasons that describe the NEWEST release say nothing about going
// backwards.
func TestRevertBlockingReasonIgnoresForwardOnlyReasons(t *testing.T) {
	hostID := "22222222-2222-4222-8222-222222222222"
	view := func(reason string) View {
		t := Target{Kind: TargetHost, HostID: &hostID, NodeName: str("gpu-01")}
		if reason == "" {
			t.Eligible = true
		} else {
			t.Reason = str(reason)
		}
		return View{Targets: []Target{t}}
	}
	for _, reason := range []string{ReasonUpToDate, ReasonControlPlaneNotFirst, ReasonReleaseAboveControlPlane, ReasonNoRelease, ""} {
		if got := revertBlockingReason(view(reason), hostID); got != "" {
			t.Errorf("%s blocked a revert with %q, want it ignored", reason, got)
		}
	}
	for _, reason := range []string{ReasonIdentityUnknown, ReasonInstallModeSource, ReasonUpdaterAbsent,
		ReasonHostOffline, ReasonAttemptInFlight, ReasonRunActive} {
		if got := revertBlockingReason(view(reason), hostID); got != reason {
			t.Errorf("%s = %q, want it to block the revert", reason, got)
		}
	}
	// A host the view has no row for is not one a revert may be aimed at.
	if got := revertBlockingReason(view(""), "77777777-7777-4777-8777-777777777777"); got != ReasonIdentityUnknown {
		t.Errorf("unknown host = %q, want identity_unknown", got)
	}
}

func relPtr(r Release) *Release { return &r }
