package agentws

import (
	"context"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/hostcfg"
)

func TestNewEnrollmentDefaultsSupportedHardwareToAutomatic(t *testing.T) {
	pool := testPool(t)
	store := &agentStore{pool: pool}
	ctx := context.Background()
	newHost, err := store.enrollHost(ctx, "automatic-new-host", "0.3.0", "token", "token")
	if err != nil {
		t.Fatal(err)
	}
	view, err := hostcfg.NewStore(pool).GetPolicy(ctx, newHost.HostID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Choices["encoder"].Source != "deployment" || view.Groups["hardware"].Status != "upgrade_required" || view.Revision != "0" {
		t.Fatalf("unconfirmed host claimed automatic intent: %+v", view)
	}
	// Re-enrollment before capability confirmation must preserve the marker.
	if _, err := pool.Exec(ctx, `UPDATE hosts SET status='offline',agent_disconnected_at=now() WHERE id=$1::uuid`, newHost.HostID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.enrollHost(ctx, "automatic-new-host", "0.3.1", "token", "token"); err != nil {
		t.Fatal(err)
	}
	policy := hostcfg.NewStore(pool)
	connection := "00000000-0000-4000-8000-000000000340"
	if _, err := policy.BeginPolicyConnection(ctx, newHost.HostID, connection, map[string]int{"typed_settings": 2}, []string{"hardware"}, true); err != nil {
		t.Fatal(err)
	}
	if confirmed, err := policy.ConfirmPolicyGroups(ctx, newHost.HostID, "00000000-0000-4000-8000-000000000349", []string{"hardware"}); err != nil || confirmed {
		t.Fatalf("stale connection initialized policy: confirmed=%v err=%v", confirmed, err)
	}
	if confirmed, err := policy.ConfirmPolicyGroups(ctx, newHost.HostID, connection, []string{"hardware"}); err != nil || !confirmed {
		t.Fatalf("hardware capability echo: confirmed=%v err=%v", confirmed, err)
	}
	view, err = policy.GetPolicy(ctx, newHost.HostID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Choices["encoder"].Source != "automatic" || view.Choices["render_node"].Source != "automatic" ||
		view.Choices["cuda_device"].Source != "deployment" || view.Revision != "0" || view.Groups["hardware"].Status != "pending" {
		t.Fatalf("confirmed new host policy = %+v", view)
	}
	// Re-enrollment after an operator edit does not restore the install choice.
	if _, err := hostcfg.NewStore(pool).SavePolicy(ctx, newHost.HostID, "0", map[string]hostcfg.PolicyChoice{
		"encoder": {Source: "deployment"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET status='offline',agent_disconnected_at=now() WHERE id=$1::uuid`, newHost.HostID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.enrollHost(ctx, "automatic-new-host", "0.3.2", "token", "token"); err != nil {
		t.Fatal(err)
	}
	view, err = hostcfg.NewStore(pool).GetPolicy(ctx, newHost.HostID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Choices["encoder"].Source != "deployment" || view.Choices["render_node"].Source != "automatic" || view.Revision != "1" {
		t.Fatalf("re-enrollment rewrote policy: %+v", view)
	}
}

func TestCapabilityEchoDoesNotInferAutomaticOnExistingOrEditedHost(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	policy := hostcfg.NewStore(pool)
	store := &agentStore{pool: pool}
	for _, tc := range []struct {
		name, hostID, want string
	}{
		{name: "legacy revision zero", hostID: seedHost(t, pool), want: "deployment"},
		{name: "fresh with legacy edit", want: "explicit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostID := tc.hostID
			if hostID == "" {
				result, err := store.enrollHost(ctx, "automatic-edited-host", "0.3.0", "token", "token")
				if err != nil {
					t.Fatal(err)
				}
				hostID = result.HostID
				if _, err := policy.SaveLegacyPatch(ctx, hostID, map[string]any{"encoder": "va"}, nil); err != nil {
					t.Fatal(err)
				}
			}
			connection := "00000000-0000-4000-8000-000000000341"
			if _, err := policy.BeginPolicyConnection(ctx, hostID, connection, map[string]int{"typed_settings": 2}, []string{"hardware"}, true); err != nil {
				t.Fatal(err)
			}
			if confirmed, err := policy.ConfirmPolicyGroups(ctx, hostID, connection, []string{"hardware"}); err != nil || !confirmed {
				t.Fatalf("hardware capability echo: confirmed=%v err=%v", confirmed, err)
			}
			view, err := policy.GetPolicy(ctx, hostID)
			if err != nil {
				t.Fatal(err)
			}
			if view.Choices["encoder"].Source != tc.want || view.Choices["render_node"].Source != "deployment" {
				t.Fatalf("capability echo inferred intent for %s: %+v", tc.name, view)
			}
		})
	}
}
