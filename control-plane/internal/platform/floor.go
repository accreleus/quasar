package platform

import (
	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
	"github.com/accreleus/quasar/control-plane/internal/semver"
)

// The floor (control-api.md amendment 14 §"below_floor"; CONTEXT.md "Floor"): the oldest
// node-agent and recovery-actor release the installed control plane still manages. It is
// judged against THIS build's declared floor (buildinfo.DeclaredFloor), never against a
// manifest, so it holds on edge and for a developer apply alike.
//
// Pure: every function here compares versions and nothing else.

// HostBelowFloor is `PlatformHostIdentity.below_floor`: the host's reported agent_version
// or recovery_actor_version orders below the floor for that component. A version that is
// absent or is not MAJOR.MINOR.PATCH[-prerelease] (an unstamped developer build) is never
// below — the floor judges only what it can order.
func HostBelowFloor(h HostIdentity, floor buildinfo.Floor) bool {
	return versionBelow(h.AgentVersion, floor.NodeAgent) ||
		versionBelow(h.RecoveryActorVersion, floor.RecoveryActor)
}

// releaseBelowFloor reports whether a known release's version orders below the floor of
// any of the named components: what a revert or a developer apply would put back. A
// release with no version (edge) is never below.
func releaseBelowFloor(r Release, components []string, floor buildinfo.Floor) bool {
	for _, name := range components {
		switch name {
		case ComponentNodeAgent:
			if versionBelow(r.Version, floor.NodeAgent) {
				return true
			}
		case ComponentRecovery:
			if versionBelow(r.Version, floor.RecoveryActor) {
				return true
			}
		}
	}
	return false
}

// versionBelow: v orders strictly below floor by SemVer precedence. An empty floor (a
// caller that wired none) and an unorderable v are both "not below".
func versionBelow(v *string, floor string) bool {
	if v == nil || floor == "" {
		return false
	}
	have, ok := strictVersion(*v)
	if !ok {
		return false
	}
	want, ok := strictVersion(floor)
	if !ok {
		return false
	}
	return semver.ComparePrecedence(have, want) < 0
}
