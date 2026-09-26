package buildinfo

// The floor this control plane declares: the oldest node-agent and recovery-actor release
// it still manages (semantics: control-api.md amendment 14 §"below_floor"). The planner
// judges below_floor against it, never against a manifest.
//
// scripts/release/generate-platform-release-manifest.sh reads the two lines below to
// publish the same floor in the release's format-2 manifest: keep their exact shape.
//
// 0.4.0-0 orders below every 0.4.0 prerelease, so the release candidates of the first
// release with owned installs stay managed and can be cut from this tree (a floor may
// never order above its release's version).
const FloorNodeAgent = "0.4.0-0"
const FloorRecoveryActor = "0.4.0-0"

// Floor is the declared floor, one version per managed component.
type Floor struct {
	NodeAgent     string
	RecoveryActor string
}

// DeclaredFloor returns this build's floor.
func DeclaredFloor() Floor {
	return Floor{NodeAgent: FloorNodeAgent, RecoveryActor: FloorRecoveryActor}
}
