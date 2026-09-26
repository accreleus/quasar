package platform

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/actorsocket"
)

// Preflight (CONTEXT.md). semantics: control-api.md §"Self-update hardening"
//
// PlanPreflight is pure: the collectors (preflight_collect.go, the agent's
// readiness report) gather facts, this decides. `unknown` never blocks — a fleet
// of agents that predate the checks must keep updating.

// The closed PreflightCheckId vocabulary. The three an agent answers about
// itself are also its readiness check ids (node-agent readiness/platform_update.rs).
const (
	CheckUpdaterSocket      = "updater_socket"
	CheckUpdaterStackDir    = "updater_stack_dir"
	CheckUpdaterOverlays    = "updater_overlays"
	CheckImageResolvable    = "image_resolvable"
	CheckAgentConnected     = "agent_connected"
	CheckHealthAddrBindable = "health_addr_bindable"
	// Amendment 14: room for the pre-update dump, on an owned control plane
	// whose database is Quasar's own.
	CheckBackupSpace = "backup_space"
	// Amendment 14: carried only by an owned target. For a host it is also the
	// agent's readiness check id (node-agent readiness/owner_conflict.rs).
	CheckOwnerConflict = "owner_conflict"
)

// Per-check status and the folded state.
const (
	CheckPass    = "pass"
	CheckFail    = "fail"
	CheckUnknown = "unknown"

	PreflightOK      = "ok"
	PreflightBlocked = "blocked"
	PreflightUnknown = "unknown"
)

// PreflightCheck is one `PlatformPreflightCheck`. Detail is operator prose that
// names the fix on a fail; never parsed.
type PreflightCheck struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Preflight is one target's `PlatformPreflight`.
type Preflight struct {
	State     string           `json:"state"`
	CheckedAt *string          `json:"checked_at"`
	Checks    []PreflightCheck `json:"checks"`
}

// SocketState is the three-way #184 diagnosis of the control plane's own
// updater socket: the mount directory, then the socket inside it.
type SocketState struct {
	DirExists    bool
	SocketExists bool
}

// UpdaterSelfFacts is what `GET /v1/self` over the socket reported. Err is
// non-empty when the socket existed but the call failed.
type UpdaterSelfFacts struct {
	Err         string
	Version     string
	StackDir    string
	ConfigFiles []string
	// Per compose service, the compose-file set its running container was
	// started with (its own labels, read by the updater). nil = not inspected.
	ServiceConfigFiles map[string][]string
}

// ReadinessFact is one of the agent's readiness checks, by id.
type ReadinessFact struct {
	Status      string
	Summary     string
	Remediation string
}

// ImageFact is the instance-wide registry check for available[0]. Err "" means
// every component manifest resolved.
type ImageFact struct {
	Err string
}

// PreflightFacts is everything the collectors found for ONE target. Every
// pointer is a tri-state: nil means nobody could look.
type PreflightFacts struct {
	CheckedAt *time.Time
	// Control plane only.
	Socket *SocketState
	Self   *UpdaterSelfFacts
	// Host only.
	AgentConnected *bool
	Readiness      map[string]ReadinessFact
	// Both; copied from the instance-wide check.
	Image *ImageFact
	// Control plane on an owned machine only; non-nil replaces Socket/Self.
	OwnedActor *OwnedActorFact
	// Control plane only: whether available[0] migrates (nil: nothing listed),
	// and the size of this control plane's database (nil: not read).
	Migrates      *bool
	DatabaseBytes *int64
	// Host only: an owned host carries owner_conflict and no Compose checks.
	OwnedHost bool
}

// OwnedActorFact is whether the recovery actor answered on the control socket,
// and the owner conflicts it reported when it did.
type OwnedActorFact struct {
	Socket    string
	Answered  bool
	Version   string
	Err       string
	Conflicts []actorsocket.Conflict
	// DatabaseMode is `owned` or `external` as the actor reported it; nil unknown.
	DatabaseMode  *string
	DumpFreeBytes *int64
}

// PlanPreflight decides one target. Every check is evaluated (no short-circuit)
// so the card can name every fix at once; the order is the vocabulary's.
func PlanPreflight(kind string, f PreflightFacts) Preflight {
	var checks []PreflightCheck
	switch {
	case kind == TargetControlPlane && f.OwnedActor != nil:
		// An owned target carries no Compose checks (amendment 14 §"Preflight").
		checks = []PreflightCheck{
			ownedActorSocketCheck(f.OwnedActor),
			ownedConflictCheck(f.OwnedActor),
			imageCheck(f.Image),
		}
		// A silent actor has not said whose database it is; the check then reads
		// unknown rather than vanishing (amendment 14 §"Preflight").
		if db := f.OwnedActor.DatabaseMode; !f.OwnedActor.Answered || (db != nil && *db == DatabaseModeOwned) {
			checks = append(checks, backupSpaceCheck(f))
		}
	case kind == TargetControlPlane:
		checks = []PreflightCheck{
			cpSocketCheck(f.Socket, f.Self),
			cpStackDirCheck(f.Self),
			cpOverlaysCheck(f.Self),
			imageCheck(f.Image),
		}
	case f.OwnedHost:
		checks = []PreflightCheck{
			agentConnectedCheck(f.AgentConnected),
			readinessCheck(CheckUpdaterSocket, f.Readiness),
			readinessCheck(CheckHealthAddrBindable, f.Readiness),
			readinessCheck(CheckOwnerConflict, f.Readiness),
			imageCheck(f.Image),
		}
	default:
		checks = []PreflightCheck{
			agentConnectedCheck(f.AgentConnected),
			readinessCheck(CheckUpdaterSocket, f.Readiness),
			readinessCheck(CheckUpdaterStackDir, f.Readiness),
			readinessCheck(CheckUpdaterOverlays, f.Readiness),
			readinessCheck(CheckHealthAddrBindable, f.Readiness),
			imageCheck(f.Image),
		}
	}
	return Preflight{State: foldPreflight(checks), CheckedAt: rfc3339OrNil(f.CheckedAt), Checks: checks}
}

// foldPreflight: any fail → blocked; else any unknown → unknown; else ok.
func foldPreflight(checks []PreflightCheck) string {
	state := PreflightOK
	for _, c := range checks {
		switch c.Status {
		case CheckFail:
			return PreflightBlocked
		case CheckPass:
		default:
			state = PreflightUnknown
		}
	}
	return state
}

// Blocked reports whether this preflight produces the preflight_blocked reason.
func (p Preflight) Blocked() bool { return p.State == PreflightBlocked }

func cpSocketCheck(s *SocketState, self *UpdaterSelfFacts) PreflightCheck {
	switch {
	case s == nil:
		return unknown(CheckUpdaterSocket, "the updater socket was not looked for")
	case !s.DirExists:
		return fail(CheckUpdaterSocket,
			"the updater's socket volume is not mounted in this container (no "+updaterSocketDir()+
				"): the control plane was created before the volume existed. Recreate it: "+
				"docker compose up -d --force-recreate --no-deps quasar-control-plane")
	case !s.SocketExists:
		return fail(CheckUpdaterSocket,
			"the volume is mounted but the updater is not running (no socket at "+ConfiguredUpdaterSocket()+
				"). Start it: docker compose up -d quasar-updater")
	case self == nil:
		return unknown(CheckUpdaterSocket, "the socket exists but the updater was not asked")
	case self.Err != "":
		return fail(CheckUpdaterSocket, "the updater did not answer on "+ConfiguredUpdaterSocket()+": "+self.Err+
			". Check its logs: docker compose logs quasar-updater")
	}
	v := self.Version
	if v == "" {
		v = "of unknown version"
	}
	return pass(CheckUpdaterSocket, "updater "+v+" answered on "+ConfiguredUpdaterSocket())
}

// ownedActorSocketCheck keeps the updater_socket id; its detail names no
// Compose command.
func ownedActorSocketCheck(a *OwnedActorFact) PreflightCheck {
	if !a.Answered {
		detail := "the recovery actor did not answer on its control socket " + a.Socket
		if a.Err != "" {
			detail += ": " + a.Err
		}
		return fail(CheckUpdaterSocket, detail+
			". Check that it is running (docker ps --filter name=quasar-recovery) and read its log (docker logs quasar-recovery)")
	}
	v := a.Version
	if v == "" {
		v = "of unknown version"
	}
	return pass(CheckUpdaterSocket, "recovery actor "+v+" answered on "+a.Socket)
}

// ownedConflictCheck lifts the race guard's report (recovery-actor
// race_guard.rs): any container that looks like a Quasar service without this
// installation's labels blocks the target.
func ownedConflictCheck(a *OwnedActorFact) PreflightCheck {
	if !a.Answered {
		return unknown(CheckOwnerConflict, "not evaluated: the recovery actor did not answer")
	}
	if len(a.Conflicts) == 0 {
		return pass(CheckOwnerConflict, "no container on this machine looks like a Quasar service without being this installation's")
	}
	names := make([]string, 0, len(a.Conflicts))
	each := make([]string, 0, len(a.Conflicts))
	for _, c := range a.Conflicts {
		names = append(names, c.Container)
		line := c.Container + " (" + c.Image + ")"
		if c.Why != "" {
			line += ": " + c.Why
		}
		each = append(each, line)
	}
	return fail(CheckOwnerConflict, "the recovery actor never acts on a container it did not create, and these look like Quasar services: "+
		strings.Join(each, "; ")+". Remove them and the stack or manager definition that recreates them: docker rm -f "+strings.Join(names, " "))
}

func cpStackDirCheck(self *UpdaterSelfFacts) PreflightCheck {
	if self == nil || self.Err != "" {
		return unknown(CheckUpdaterStackDir, "not evaluated: the updater did not answer")
	}
	if self.StackDir == "" || len(self.ConfigFiles) == 0 {
		return fail(CheckUpdaterStackDir,
			"the updater has not discovered the stack it sits beside. Set QUASAR_STACK_DIR in deploy/.env "+
				"to the stack directory's absolute host path and recreate quasar-updater")
	}
	return pass(CheckUpdaterStackDir, fmt.Sprintf("%s, %d compose file(s)", self.StackDir, len(self.ConfigFiles)))
}

// cpOverlaysCheck compares the compose-file set each running service was
// started with against the updater's own. A mismatch means the next recreate
// drops (or adds) an overlay the operator did not expect.
func cpOverlaysCheck(self *UpdaterSelfFacts) PreflightCheck {
	if self == nil || self.Err != "" {
		return unknown(CheckUpdaterOverlays, "not evaluated: the updater did not answer")
	}
	if self.ServiceConfigFiles == nil {
		return unknown(CheckUpdaterOverlays, "the updater predates the overlay check")
	}
	want := strings.Join(self.ConfigFiles, ", ")
	services := make([]string, 0, len(self.ServiceConfigFiles))
	for svc := range self.ServiceConfigFiles {
		services = append(services, svc)
	}
	sort.Strings(services)
	for _, svc := range services {
		got := self.ServiceConfigFiles[svc]
		if got == nil {
			continue // no container for this service: nothing to compare
		}
		if !slices.Equal(got, self.ConfigFiles) {
			return fail(CheckUpdaterOverlays, fmt.Sprintf(
				"%s was started with [%s] but the updater with [%s]; an apply would recreate it with the updater's set. "+
					"Bring both up with the same -f list, or recreate quasar-updater with the service's",
				svc, strings.Join(got, ", "), want))
		}
	}
	return pass(CheckUpdaterOverlays, "every running service was started with the updater's compose files")
}

func agentConnectedCheck(c *bool) PreflightCheck {
	switch {
	case c == nil:
		return unknown(CheckAgentConnected, "no agent registry is wired")
	case !*c:
		return fail(CheckAgentConnected, "the host's agent is not connected to this control plane")
	}
	return pass(CheckAgentConnected, "the agent is connected")
}

// readinessCheck lifts one of the agent's own checks. `warn` reads as pass with
// its summary (advisory), anything else — including a missing id, which is an
// agent predating the check — is unknown.
func readinessCheck(id string, r map[string]ReadinessFact) PreflightCheck {
	f, ok := r[id]
	if !ok {
		return unknown(id, "the agent has not reported this check (it predates it, or has not reported readiness yet)")
	}
	switch f.Status {
	case "pass", "warn":
		return pass(id, f.Summary)
	case "fail":
		detail := f.Summary
		if f.Remediation != "" {
			detail += " — " + f.Remediation
		}
		return fail(id, detail)
	case "skip":
		return pass(id, "not applicable: "+f.Summary)
	}
	return unknown(id, f.Summary)
}

func imageCheck(i *ImageFact) PreflightCheck {
	switch {
	case i == nil:
		return unknown(CheckImageResolvable, "no release is listed to resolve")
	case i.Err != "":
		return fail(CheckImageResolvable, i.Err)
	}
	return pass(CheckImageResolvable, "every component manifest resolves at the registry")
}

func pass(id, detail string) PreflightCheck {
	return PreflightCheck{ID: id, Status: CheckPass, Detail: detail}
}
func fail(id, detail string) PreflightCheck {
	return PreflightCheck{ID: id, Status: CheckFail, Detail: detail}
}
func unknown(id, detail string) PreflightCheck {
	return PreflightCheck{ID: id, Status: CheckUnknown, Detail: detail}
}

func updaterSocketDir() string { return filepath.Dir(ConfiguredUpdaterSocket()) }

// backupSpaceCheck: room for the pre-update dump when available[0] migrates.
// The recovery actor measures again before it dumps and is the enforcement.
func backupSpaceCheck(f PreflightFacts) PreflightCheck {
	if f.Migrates == nil || !*f.Migrates {
		return pass(CheckBackupSpace, "This release does not change the database, so no dump is taken.")
	}
	if f.OwnedActor == nil || f.OwnedActor.DumpFreeBytes == nil {
		return unknown(CheckBackupSpace, "Free space on this machine has not been reported, so it is checked just before the dump.")
	}
	if f.DatabaseBytes == nil {
		return unknown(CheckBackupSpace, "The database's size could not be read, so the free space is checked just before the dump.")
	}
	free, size := uint64(max(*f.OwnedActor.DumpFreeBytes, 0)), uint64(max(*f.DatabaseBytes, 0))
	need := dumpSpaceNeeded(size)
	if free < need {
		return fail(CheckBackupSpace, fmt.Sprintf(
			"The pre-update dump needs about %s and %s is free on this machine. Free some space there, then check again.",
			humanBytes(need), humanBytes(free)))
	}
	return pass(CheckBackupSpace, fmt.Sprintf(
		"Quasar's database is about %s; %s is free for the pre-update dump.", humanBytes(size), humanBytes(free)))
}

// dumpSpaceNeeded is the recovery actor's rule, the database's size plus a
// tenth, at least 64 MiB. Rust twin: quasar_recovery::dump::space_needed; keep
// the two equal.
func dumpSpaceNeeded(databaseBytes uint64) uint64 {
	return databaseBytes + max(databaseBytes/10, 64<<20)
}

// humanBytes reads a size as operator prose does ("1.4 GB"). Rust twin:
// quasar_recovery::dump::human.
func humanBytes(b uint64) string {
	const gb, mb = 1_000_000_000.0, 1_000_000.0
	if f := float64(b); f >= gb {
		return fmt.Sprintf("%.1f GB", f/gb)
	}
	return fmt.Sprintf("%.0f MB", max(float64(b)/mb, 1))
}

// readinessCheckWire is agent-api.md `readiness[]` as stored in hosts.readiness.
type readinessCheckWire struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Summary     string `json:"summary"`
	Remediation string `json:"remediation"`
}

// ReadinessFacts indexes a stored readiness report by check id. A malformed
// report reads as "nothing reported": preflight then says unknown, never blocked.
func ReadinessFacts(raw json.RawMessage) map[string]ReadinessFact {
	if len(raw) == 0 {
		return nil
	}
	var checks []readinessCheckWire
	if err := json.Unmarshal(raw, &checks); err != nil {
		return nil
	}
	out := make(map[string]ReadinessFact, len(checks))
	for _, c := range checks {
		out[c.ID] = ReadinessFact{Status: c.Status, Summary: c.Summary, Remediation: c.Remediation}
	}
	return out
}

// HostPreflightFacts derives a host's facts from its identity row, plus the
// instance-wide image fact.
func HostPreflightFacts(h HostIdentity, image *ImageFact) PreflightFacts {
	return PreflightFacts{
		CheckedAt:      h.ReadinessReportedAt,
		AgentConnected: h.AgentConnected,
		Readiness:      ReadinessFacts(h.Readiness),
		Image:          image,
		OwnedHost:      h.InstallMode != nil && *h.InstallMode == InstallOwned,
	}
}
