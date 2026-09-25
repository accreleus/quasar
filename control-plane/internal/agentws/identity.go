package agentws

import (
	"regexp"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/semver"
)

// HostIdentity is the validated form of the four optional identity fields on
// `register` (agent-api.md §register, platform-release amendment 1). Every
// field is a pointer because ABSENT and "a value" are different facts and the
// database stores the difference — `updater_present` most sharply: NULL means
// nobody has said, `false` means an agent looked and found no updater.
type HostIdentity struct {
	SourceCommit   *string
	BuiltAt        *time.Time
	InstallMode    *string
	UpdaterPresent *bool

	// Owned-install identity (amendment 14). Not part of Known(): a host that
	// reports none of them keeps the eligibility it had.
	RecoveryActorVersion      *string
	RecoveryActorSourceCommit *string
	SeedVersion               *string
}

// Known reports whether all four fields are present, which is the
// `identity_known` predicate the whole eligibility model turns on
// (control-api.md §Platform releases). A host with ANY of them absent is
// identity-unknown and never eligible for a platform-release apply.
func (i HostIdentity) Known() bool {
	return i.SourceCommit != nil && i.BuiltAt != nil && i.InstallMode != nil && i.UpdaterPresent != nil
}

// Install modes (schema.md hosts.install_mode), the one Go definition:
// internal/platform aliases these, since platform imports agentws.
const (
	InstallRegistry = "registry"
	InstallSource   = "source"
	InstallOwned    = "owned"
)

// 7-40 lowercase hex. A short commit is a real identity, only a less specific
// one, so it is accepted and stored EXACTLY as sent rather than rejected or
// padded.
var agentCommit = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// identityFromRegister validates the identity fields off a register message.
// Nothing here can fail the registration: an unparseable or out-of-vocabulary
// value is treated as ABSENT (stored NULL), because the control plane never
// refuses a registration over these fields (agent-api.md). The second return
// value names the fields that were dropped, for a log line an operator can act
// on — a silently-ignored malformed stamp is how identity quietly stays
// unknown forever.
func identityFromRegister(reg RegisterMsg) (HostIdentity, []string) {
	var id HostIdentity
	var dropped []string

	if reg.SourceCommit != nil {
		if agentCommit.MatchString(*reg.SourceCommit) {
			c := *reg.SourceCommit
			id.SourceCommit = &c
		} else {
			dropped = append(dropped, "source_commit")
		}
	}

	if reg.BuiltAt != nil {
		if t, err := time.Parse(time.RFC3339, *reg.BuiltAt); err == nil {
			u := t.UTC()
			id.BuiltAt = &u
		} else {
			dropped = append(dropped, "built_at")
		}
	}

	if reg.InstallMode != nil {
		switch *reg.InstallMode {
		case InstallRegistry, InstallSource, InstallOwned:
			m := *reg.InstallMode
			id.InstallMode = &m
		default:
			// Any other value is treated as absent, per the contract: an
			// agent naming a newer mode must not write a value the CHECK
			// would refuse.
			dropped = append(dropped, "install_mode")
		}
	}

	// A bool needs no validation: JSON gives true, false, or absent, and all
	// three are meaningful. A non-bool would have failed the message decode.
	id.UpdaterPresent = reg.UpdaterPresent

	// Read only beside install_mode owned; ignored beside any other mode
	// (protocol/agent-api.md §register "Owned installs").
	owned := id.InstallMode != nil && *id.InstallMode == InstallOwned
	for _, f := range []struct {
		name  string
		sent  *string
		valid func(string) bool
		dst   **string
	}{
		{"recovery_actor_version", reg.RecoveryActorVersion, actorVersionOrderable, &id.RecoveryActorVersion},
		{"recovery_actor_source_commit", reg.RecoveryActorSourceCommit, agentCommit.MatchString, &id.RecoveryActorSourceCommit},
		{"seed_version", reg.SeedVersion, func(string) bool { return true }, &id.SeedVersion},
	} {
		if f.sent == nil {
			continue
		}
		if !owned || !f.valid(*f.sent) {
			dropped = append(dropped, f.name)
			continue
		}
		v := *f.sent
		*f.dst = &v
	}

	return id, dropped
}

// The contract's MAJOR.MINOR.PATCH[-prerelease]: no leading v, no build
// metadata, no leading zeros. semver.ParseFull alone is looser (it trims and
// tolerates all three), so here it only adds the component-overflow check.
var actorVersion = regexp.MustCompile(
	`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
		`(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$`)

// actorVersionOrderable: unlike agent_version (stored as sent), a
// recovery_actor_version that cannot be ordered is stored NULL.
func actorVersionOrderable(v string) bool {
	if !actorVersion.MatchString(v) {
		return false
	}
	_, ok := semver.ParseFull(v)
	return ok
}
