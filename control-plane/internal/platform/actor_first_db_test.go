// ADR 0008 against a real Postgres, through the admin API: a fleet run moves a
// host's recovery actor first, except on the control plane's own machine, which
// the control plane knows from its own configuration (MachineShape).
package platform

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

// actorRelease resolves a host target to a release naming the actor, which a
// format-1 manifest cannot yet do.
type actorRelease struct{ ManifestOrEdge }

func (actorRelease) HostComponents(context.Context, Release) ([]ComponentDigest, error) {
	return []ComponentDigest{agentDigest('a'), actorDigest('b')}, nil
}

func TestAFleetRunMovesTheActorFirstExceptOnTheControlPlanesMachine(t *testing.T) {
	both := []string{ComponentRecovery, ComponentNodeAgent}
	for _, tc := range []struct {
		name  string
		shape MachineShape
		want  []string
	}{
		{"a GPU host", MachineShape{}, both},
		{"another machine's agent", MachineShape{Role: MachineRoleCombined, NodeName: "some-other-machine"}, both},
		{"a control-only machine of the same name", MachineShape{Role: MachineRoleControlOnly, NodeName: "gpu-fleet-01"}, both},
		{"the combined host's own agent", MachineShape{Role: MachineRoleCombined, NodeName: "gpu-fleet-01"}, []string{ComponentNodeAgent}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// commitB: the control plane is on the release, so the host is the only target.
			h := newFleetHarness(t, commitB, parkedDrivers{})
			h.fleet.resolve = actorRelease{}
			h.fleet.WithMachineShape(tc.shape)
			mustExec(t, h.pool, `UPDATE hosts SET install_mode = 'owned', recovery_actor_source_commit = $2 WHERE id = $1::uuid`,
				h.hostID, commitA)

			code, raw := h.do(t, http.MethodPost, "/v1/admin/platform/apply", h.admin,
				FleetApplyRequest{ReleaseID: h.release.ID, Force: true})
			if code != http.StatusAccepted {
				t.Fatalf("POST apply = %d %s, want 202", code, raw)
			}
			run := decodeRun(t, raw)
			var attempts []Attempt
			waitFor(t, "the host attempt", func() bool {
				as, err := h.store.RunAttempts(context.Background(), run.ID)
				attempts = as
				return err == nil && len(as) == 1
			})
			var got []string
			for _, c := range attempts[0].RequestedDigests {
				got = append(got, c.Name)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("requested %v, want %v", got, tc.want)
			}
		})
	}
}
