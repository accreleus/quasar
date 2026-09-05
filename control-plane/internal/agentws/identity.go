package agentws

import (
	"regexp"
	"time"
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
}

// Known reports whether all four fields are present, which is the
// `identity_known` predicate the whole eligibility model turns on
// (control-api.md §Platform releases). A host with ANY of them absent is
// identity-unknown and never eligible for a platform-release apply.
func (i HostIdentity) Known() bool {
	return i.SourceCommit != nil && i.BuiltAt != nil && i.InstallMode != nil && i.UpdaterPresent != nil
}

// 7-40 lowercase hex. A short commit is a real identity, only a less specific
// one, so it is accepted and stored EXACTLY as sent rather than rejected or
// padded.
var agentCommit = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// identityFromRegister validates the four fields off a register message.
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
		case "registry", "source":
			m := *reg.InstallMode
			id.InstallMode = &m
		default:
			// Any other value is treated as absent, per the contract — an
			// agent from the future naming a third mode must not write a value
			// this schema's CHECK would refuse.
			dropped = append(dropped, "install_mode")
		}
	}

	// A bool needs no validation: JSON gives true, false, or absent, and all
	// three are meaningful. A non-bool would have failed the message decode.
	id.UpdaterPresent = reg.UpdaterPresent

	return id, dropped
}
