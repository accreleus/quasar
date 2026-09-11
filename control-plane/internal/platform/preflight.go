package platform

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
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
}

// PlanPreflight decides one target. Every check is evaluated (no short-circuit)
// so the card can name every fix at once; the order is the vocabulary's.
func PlanPreflight(kind string, f PreflightFacts) Preflight {
	var checks []PreflightCheck
	switch kind {
	case TargetControlPlane:
		checks = []PreflightCheck{
			cpSocketCheck(f.Socket, f.Self),
			cpStackDirCheck(f.Self),
			cpOverlaysCheck(f.Self),
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
	}
}
