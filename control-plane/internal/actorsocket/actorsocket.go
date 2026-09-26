// Package actorsocket holds the recovery actor's control-socket wire shapes
// (docs/rh06/2026-09-24-architecture.md §5.2), for the control plane's client.
//
// Not a frozen interface. The Rust twin is
// node-agent/crates/quasar-recovery/src/socket.rs; both decode and re-encode
// every fixture in testdata/recovery/socket to the same JSON, so a field added
// on one side only fails a test on both. Result keeps the Go updater's
// result-file spellings (internal/updater/result.go), so the agent's
// release_state relay stays a re-frame.
package actorsocket

// Component is one image to move.
type Component struct {
	Name   string `json:"name"`
	Image  string `json:"image"` // repository reference: no tag, no digest
	Digest string `json:"digest"`
}

// Release is provenance only (ADR 0001); the ADR 0003 verifier fetches the
// manifest named by Version.
type Release struct {
	ID           string  `json:"id"`
	Version      *string `json:"version"`
	SourceCommit string  `json:"source_commit"`
}

type RequestKind string

const (
	KindReplace RequestKind = "replace"
	KindRestore RequestKind = "restore"
	KindRemove  RequestKind = "remove"
)

// Request is a submit. Components are replaced in request order.
type Request struct {
	RequestID               string      `json:"request_id"`
	Kind                    RequestKind `json:"kind"`
	Components              []Component `json:"components"`
	Release                 Release     `json:"release"`
	Migrates                bool        `json:"migrates"`
	SchemaVersion           *int64      `json:"schema_version"`
	ExternalBackupConfirmed bool        `json:"external_backup_confirmed"`
	Dump                    *string     `json:"dump"` // the dump a restore restores
	Purge                   bool        `json:"purge"`
	// Zero is the actor's default.
	WaitTimeoutS int64 `json:"wait_timeout_s,omitempty"`
}

// Previous is the digest a component was on before; nil, never omitted, when
// it could not be told.
type Previous struct {
	Name   string  `json:"name"`
	Digest *string `json:"digest"`
}

// Accepted answers an admitted submit.
type Accepted struct {
	RequestID string     `json:"request_id"`
	Previous  []Previous `json:"previous"`
}

// Rejection answers a refused submit; nothing was journalled.
type Rejection struct {
	RequestID string `json:"request_id,omitempty"`
	Reason    Reason `json:"reason"`
	Message   string `json:"message"`
}

// State is agent-api.md release_state's state.
type State string

const (
	StatePending    State = "pending"
	StatePulling    State = "pulling"
	StateRecreating State = "recreating"
	StateVerifying  State = "verifying"
	StateSucceeded  State = "succeeded"
	StateFailed     State = "failed"
)

// Reason is agent-api.md release_state's closed vocabulary as the actor emits
// it, then the RH-06 identifiers of the RH06-01 draft amendment (#353). A
// reason this build does not know is kept and rendered verbatim.
type Reason string

const (
	ReasonInvalid           Reason = "invalid"
	ReasonNamespaceRejected Reason = "namespace_rejected"
	ReasonDigestMalformed   Reason = "digest_malformed"
	ReasonBusy              Reason = "busy"
	ReasonPullFailed        Reason = "pull_failed"
	ReasonRecreateFailed    Reason = "recreate_failed"
	ReasonNeverStarted      Reason = "never_started"
	ReasonUnhealthy         Reason = "unhealthy"
	ReasonSignatureMissing  Reason = "signature_missing"
	ReasonSignatureInvalid  Reason = "signature_invalid"
	ReasonRecipeUnsupported Reason = "recipe_unsupported"
	ReasonOwnerConflict     Reason = "owner_conflict"
	ReasonBackupFailed      Reason = "backup_failed"
	ReasonBackupUnconfirmed Reason = "backup_unconfirmed"
	ReasonInterrupted       Reason = "interrupted"
)

// RejectionReasons refuse a submit before anything is journalled.
var RejectionReasons = []Reason{
	ReasonInvalid, ReasonBusy, ReasonNamespaceRejected, ReasonDigestMalformed,
	ReasonSignatureMissing, ReasonSignatureInvalid, ReasonBackupUnconfirmed, ReasonOwnerConflict,
}

// FailureReasons end an admitted attempt in state failed.
var FailureReasons = []Reason{
	ReasonPullFailed, ReasonRecreateFailed, ReasonNeverStarted, ReasonUnhealthy,
	ReasonRecipeUnsupported, ReasonBackupFailed, ReasonInterrupted,
}

// KnownReasons is every reason this build emits.
var KnownReasons = append(append([]Reason{}, RejectionReasons...), FailureReasons...)

// Result is one attempt's observable state. Reason is non-nil exactly when
// State is failed; an interrupted attempt is failed/interrupted, Restored false.
type Result struct {
	RequestID  string      `json:"request_id"`
	State      State       `json:"state"`
	Reason     *Reason     `json:"reason"`
	Components []Component `json:"components"`
	Previous   []Previous  `json:"previous"`
	Output     string      `json:"output"`
	StartedAt  string      `json:"started_at"`
	UpdatedAt  string      `json:"updated_at"`
	FinishedAt *string     `json:"finished_at"`
	Restored   bool        `json:"restored"`
	Release    Release     `json:"release"`
}

type ActorIdentity struct {
	Version string  `json:"version"`
	Commit  string  `json:"commit"`
	Image   string  `json:"image"`
	Digest  *string `json:"digest"`
}

type SeedIdentity struct {
	Version string  `json:"version"`
	Digest  *string `json:"digest"`
}

// Service is one platform service's container as last inspected.
type Service struct {
	Role      string  `json:"role"` // control-plane | node-agent | postgres | recovery-actor
	Container string  `json:"container"`
	Image     string  `json:"image"`
	Digest    *string `json:"digest"`
	State     string  `json:"state"`  // the engine's container state
	Health    *string `json:"health"` // nil with no healthcheck
}

// Conflict is a look-alike container without this installation's labels:
// reported, never acted on.
type Conflict struct {
	Container string `json:"container"`
	// ID is the engine's first 12 characters.
	ID string `json:"id"`
	// Image is the configured reference, tag or digest included.
	Image string `json:"image"`
	// Role is the role it looks like: control-plane | node-agent | postgres |
	// recovery-actor | updater (the Go updater of a Compose install).
	Role string `json:"role"`
	Why  string `json:"why"`
}

type Dump struct {
	Name          string `json:"name"`
	SchemaVersion int64  `json:"schema_version"`
	CreatedAt     string `json:"created_at"`
	SizeBytes     int64  `json:"size_bytes"`
}

// Role is what a machine runs (CONTEXT.md "Combined host").
type Role string

const (
	RoleCombined    Role = "combined"
	RoleGPU         Role = "gpu"
	RoleControlOnly Role = "control_only"
)

// Database is who owns the control plane's database.
type Database string

const (
	DatabaseOwned    Database = "owned"
	DatabaseExternal Database = "external"
	DatabaseNone     Database = "none"
)

// Status is the machine inventory, plus one attempt's result when a request
// id was asked for. Stale means the engine was slow and this is the last one.
type Status struct {
	Actor ActorIdentity `json:"actor"`
	Seed  *SeedIdentity `json:"seed"`
	Role  Role          `json:"role"`
	// NodeName is the machine's node name: on a combined host, the node name
	// its own agent enrolls under. Nil before the machine is installed.
	NodeName  *string    `json:"node_name"`
	Database  Database   `json:"database"`
	Services  []Service  `json:"services"`
	Conflicts []Conflict `json:"conflicts"`
	InFlight  *string    `json:"in_flight"`
	Dumps     []Dump     `json:"dumps"`
	Result    *Result    `json:"result"`
	Stale     bool       `json:"stale"`
}
