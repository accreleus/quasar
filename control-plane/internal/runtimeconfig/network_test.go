package runtimeconfig

import (
	"strings"
	"testing"
)

// TestValidNetwork pins the app-facing vocabulary. This is the single list both
// write paths (admin CRUD and catalog-image materialization) consult, so it is
// also the single place the policy can be accidentally widened — hence an
// explicit assertion per value rather than a loop over the set itself, which
// would happily agree with whatever the set said.
func TestValidNetwork(t *testing.T) {
	for _, ok := range []string{"", "none", "bridge"} {
		if !ValidNetwork(ok) {
			t.Errorf("ValidNetwork(%q) = false, want true", ok)
		}
	}

	// `host` is the load-bearing rejection (review, Alice round 2 on PR #464):
	// it removes the container's network namespace, and every value this function
	// guards is portable — an app's runtime_spec, a preset column, an admin API
	// write, or a catalog manifest authored on another machine entirely. An
	// operator selects host networking through the node agent's
	// QUASAR_CONTAINER_NETWORK knob, which never travels.
	//
	// If a future change makes this pass, it is not a test to update: it is the
	// boundary this package exists to hold.
	for _, bad := range []string{
		"host",
		"container:quasar-control",
		"my-net",
		"Bridge", "NONE", "Host",
		" none", "none ",
		"none;rm -rf /",
	} {
		if ValidNetwork(bad) {
			t.Errorf("ValidNetwork(%q) = true, want false", bad)
		}
	}
}

// The rejection message must both explain the refusal and name the supported
// alternative. An operator told only "invalid value" reasonably concludes they
// have hit a bug and looks for a way around the check.
func TestNetworkErrorNamesTheOperatorKnob(t *testing.T) {
	if !strings.Contains(NetworkError, "QUASAR_CONTAINER_NETWORK") {
		t.Error("NetworkError must point at the host-level knob")
	}
	if !strings.Contains(NetworkError, "isolation") {
		t.Error("NetworkError must say why host is refused")
	}
	if strings.Contains(NetworkError, `"host" is available`) {
		t.Error("NetworkError must not read as though host were an app-facing option")
	}
}

// NetworkValues is what operator-facing copy and API enum docs are built from,
// so it must agree with ValidNetwork exactly — a drift here would publish a set
// the validator does not actually accept (or hide one it does).
func TestNetworkValuesMatchesValidNetwork(t *testing.T) {
	vals := NetworkValues()
	if len(vals) != 3 {
		t.Fatalf("NetworkValues() = %v, want exactly 3 entries", vals)
	}
	for _, v := range vals {
		if !ValidNetwork(v) {
			t.Errorf("NetworkValues() advertises %q, which ValidNetwork rejects", v)
		}
	}
	if vals[0] != NetworkInherit {
		t.Errorf("NetworkValues()[0] = %q, want the inherit sentinel first", vals[0])
	}
	for _, v := range vals {
		if v == "host" {
			t.Fatal("NetworkValues() must never advertise host")
		}
	}
}
