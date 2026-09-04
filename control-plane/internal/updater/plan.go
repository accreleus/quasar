// Package updater is the per-host actor that pulls a platform release and
// recreates the containers it replaces, because a container cannot recreate
// itself (CONTEXT.md "Updater"). It ships as its own image
// (deploy/Dockerfile.updater), runs beside the stack as compose service
// `quasar-updater`, and is reached only over a unix socket in a named volume
// shared by that one host's containers.
//
// NOTHING OUTSIDE A HOST SPEAKS THIS. The socket, the request body and the
// result file are explicitly NOT a frozen interface (protocol/schema.md §"Not
// frozen: the updater's local socket"). Both ends ship in the same platform
// release. The frozen surface is agent-api.md's `release_apply` /
// `release_state`, which the agent relays from the result file — which is why
// the result file's field spellings mirror `release_state`: a relay that has to
// translate is a relay that can lie.
//
// This file is the DECISION, and it is a pure function. Given a request, the
// current `.env` bytes and the host's discovered configuration it produces
// either a plan (the exact env rewrite, the previous digests, the ordered
// commands) or a rejection carrying one identifier from the closed `reason`
// vocabulary. No I/O, no clock, no docker: exec.go runs what this decides.
package updater

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Reasons — the closed vocabulary shared with agent-api.md `release_state`.
// The updater only ever emits this subset; `updater_absent`, `updater_unreachable`,
// `timeout` and `unsupported` are observations made about the updater, never by it.
const (
	ReasonInvalid           = "invalid"
	ReasonNamespaceRejected = "namespace_rejected"
	ReasonDigestMalformed   = "digest_malformed"
	ReasonBusy              = "busy"
	ReasonPullFailed        = "pull_failed"
	ReasonRecreateFailed    = "recreate_failed"
	ReasonNeverStarted      = "never_started"
	ReasonUnhealthy         = "unhealthy"
)

// States — again exactly agent-api.md `release_state`.
const (
	StatePending    = "pending"
	StatePulling    = "pulling"
	StateRecreating = "recreating"
	StateVerifying  = "verifying"
	StateSucceeded  = "succeeded"
	StateFailed     = "failed"
)

// Terminal reports whether a state is one an apply can no longer leave.
func Terminal(state string) bool { return state == StateSucceeded || state == StateFailed }

// componentTarget maps a component name to the two things this program can
// touch for it. The table is closed on purpose: a component this build does not
// know is `invalid`, and — prototype finding 2 — the updater NEVER accepts a
// request naming itself, which falls out of `quasar-updater` simply not being
// in the table.
type componentTarget struct {
	service string // compose service name
	envVar  string // the .env variable carrying its image reference
	// aliasVar is an older spelling still honoured for READING the previous
	// value. It is never written: compose precedence makes envVar win, and
	// rewriting both would be two sources of truth.
	aliasVar string
}

var componentTargets = map[string]componentTarget{
	"control-plane": {service: "quasar-control-plane", envVar: "QUASAR_CONTROL_IMAGE"},
	"node-agent":    {service: "quasar-node-agent", envVar: "QUASAR_AGENT_IMAGE", aliasVar: "QUASAR_NODE_IMAGE"},
}

// ComponentControlPlane is the one component whose never-started failure is
// auto-restored (exec.go); named rather than spelled twice.
const ComponentControlPlane = "control-plane"

// Component is one image to move, as the request names it.
type Component struct {
	Name   string `json:"name"`
	Image  string `json:"image"` // repository reference: no tag, no digest
	Digest string `json:"digest"`
}

// Release is provenance only. ADR 0001: what is applied is the digest and only
// the digest — nothing here is resolved, matched or trusted by this program.
type Release struct {
	ID           string  `json:"id"`
	Version      *string `json:"version"`
	SourceCommit string  `json:"source_commit"`
}

// ApplyRequest is the body of POST /v1/apply.
type ApplyRequest struct {
	RequestID    string      `json:"request_id"`
	Components   []Component `json:"components"`
	Release      Release     `json:"release"`
	WaitTimeoutS int         `json:"wait_timeout_s,omitempty"`
}

// PreviousComponent is the digest a component was on BEFORE this apply.
// Digest is nil — never omitted — when the previous value was a local tag or
// there was no previous value at all, which is exactly what an install-mode
// `source` host looks like. It is what makes the manual restore recipe
// copy-paste from any observation.
type PreviousComponent struct {
	Name   string  `json:"name"`
	Digest *string `json:"digest"`
}

// Config is the host facts the decision needs. Everything in it is discovered
// (discover.go) or an operator knob; none of it comes from the request.
type Config struct {
	Project     string   // com.docker.compose.project
	WorkingDir  string   // com.docker.compose.project.working_dir (a HOST path)
	ConfigFiles []string // com.docker.compose.project.config_files (HOST paths, in order)

	// AllowedNamespaces is the registry-namespace allowlist
	// (QUASAR_UPDATER_ALLOWED_NAMESPACES). Prefix match on host/path/
	// boundaries; entries carry no trailing slash by the time they land here.
	AllowedNamespaces []string

	// WaitTimeoutS is the default `--wait-timeout` when the request names none.
	WaitTimeoutS int

	// InFlightRequestID is the single-flight latch: the id of the request that
	// has not reached a terminal state, or "". Single-flight per host: refuse,
	// never queue (agent-api.md `busy`). It lives here rather than in a mutex
	// the decision reaches for so that "is this busy?" stays testable as a
	// table row.
	InFlightRequestID string
}

// Rejection is a refusal on the request's face, carrying one closed-vocabulary
// identifier plus an operator-readable message.
type Rejection struct {
	Reason  string
	Message string
}

func (r *Rejection) Error() string { return r.Reason + ": " + r.Message }

func reject(reason, format string, a ...any) *Rejection {
	return &Rejection{Reason: reason, Message: fmt.Sprintf(format, a...)}
}

// ApplyPlan is everything the executor needs and nothing it has to decide.
type ApplyPlan struct {
	// EnvRewrite is the COMPLETE new content of the stack's `.env`. Only the
	// mapped variables' lines differ from the input; every other byte,
	// including comments, blank lines and ordering, is preserved.
	EnvRewrite string
	Previous   []PreviousComponent
	Services   []string
	// Commands are run in order, both through `docker`.
	Commands [][]string
	// WaitTimeoutS is the value baked into the `up` command, kept here so the
	// executor can bound its own wall clock by the same number.
	WaitTimeoutS int
}

var (
	digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	uuidRe   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// DefaultAllowedNamespaces is the org's own namespace: the only place a
// platform release can come from unless an operator says otherwise.
var DefaultAllowedNamespaces = []string{"ghcr.io/accreleus/quasar"}

// ParseNamespaces splits the comma-separated knob, trimming spaces and any
// trailing slash so `a/b` and `a/b/` are one entry. An empty/blank value yields
// the default rather than an empty allowlist — an empty allowlist would reject
// everything, which reads as "the updater is broken" rather than "the operator
// meant to lock it down", and locking it down is spelled by naming a namespace
// nothing matches.
func ParseNamespaces(raw string) []string {
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimRight(strings.TrimSpace(part), "/")
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return append([]string(nil), DefaultAllowedNamespaces...)
	}
	return out
}

// namespaceAllowed matches on a path-segment boundary, never a bare string
// prefix: `ghcr.io/accreleus/quasar` must not admit
// `ghcr.io/accreleus/quasar-evil/thing`.
func namespaceAllowed(image string, allowed []string) bool {
	for _, ns := range allowed {
		if strings.HasPrefix(image, ns+"/") && len(image) > len(ns)+1 {
			return true
		}
	}
	return false
}

// imageHasTagOrDigest reports whether a repository reference carries either.
// A tag is a `:` AFTER the last `/` (so `registry:5000/repo` is a port, not a
// tag); a digest is any `@`.
func imageHasTagOrDigest(image string) bool {
	if strings.Contains(image, "@") {
		return true
	}
	last := strings.LastIndex(image, "/")
	return strings.Contains(image[last+1:], ":")
}

// Plan is the whole decision. It returns exactly one of a plan or a rejection.
func Plan(req ApplyRequest, env string, cfg Config) (*ApplyPlan, *Rejection) {
	if !uuidRe.MatchString(req.RequestID) {
		return nil, reject(ReasonInvalid, "request_id %q is not a uuid", req.RequestID)
	}
	// Single-flight. Checked before the component rules so a busy updater
	// answers `busy` rather than grading a request it will not run anyway.
	// Re-posting the IN-FLIGHT id is idempotent, not busy (agent-api.md).
	if cfg.InFlightRequestID != "" && cfg.InFlightRequestID != req.RequestID {
		return nil, reject(ReasonBusy, "request %s is still in flight", cfg.InFlightRequestID)
	}
	if len(req.Components) == 0 {
		return nil, reject(ReasonInvalid, "components is empty")
	}
	if cfg.Project == "" || cfg.WorkingDir == "" || len(cfg.ConfigFiles) == 0 {
		return nil, reject(ReasonInvalid, "the updater has not discovered its own compose project; it cannot act")
	}

	seen := map[string]bool{}
	targets := make([]componentTarget, 0, len(req.Components))
	for _, c := range req.Components {
		t, known := componentTargets[c.Name]
		if !known {
			// `updater` / `quasar-updater` lands here, which is the whole of
			// "the updater never accepts a request naming itself".
			return nil, reject(ReasonInvalid, "unknown component %q", c.Name)
		}
		if seen[c.Name] {
			return nil, reject(ReasonInvalid, "component %q named twice", c.Name)
		}
		seen[c.Name] = true
		if c.Image == "" || strings.ContainsAny(c.Image, " \t\n") {
			return nil, reject(ReasonInvalid, "component %q: image %q is not a repository reference", c.Name, c.Image)
		}
		if imageHasTagOrDigest(c.Image) {
			return nil, reject(ReasonInvalid, "component %q: image %q carries a tag or a digest; it must be a bare repository reference", c.Name, c.Image)
		}
		if !digestRe.MatchString(c.Digest) {
			return nil, reject(ReasonDigestMalformed, "component %q: digest %q is not sha256: + 64 lowercase hex", c.Name, c.Digest)
		}
		if !namespaceAllowed(c.Image, cfg.AllowedNamespaces) {
			return nil, reject(ReasonNamespaceRejected,
				"component %q: image %q is outside this host's platform-image namespaces (%s)",
				c.Name, c.Image, strings.Join(cfg.AllowedNamespaces, ","))
		}
		targets = append(targets, t)
	}

	// Env rewrite + previous digests, in request order so `components` and
	// `previous` line up index for index on the wire.
	newEnv := env
	previous := make([]PreviousComponent, 0, len(req.Components))
	services := make([]string, 0, len(req.Components))
	for i, c := range req.Components {
		t := targets[i]
		prevVal, found := envLookup(newEnv, t.envVar)
		if !found && t.aliasVar != "" {
			// The alias is what compose would have resolved, so it is the
			// honest "previous" even though it is never rewritten.
			prevVal, _ = envLookup(newEnv, t.aliasVar)
		}
		previous = append(previous, PreviousComponent{Name: c.Name, Digest: digestOf(prevVal)})
		newEnv = envSet(newEnv, t.envVar, c.Image+"@"+c.Digest)
		services = append(services, t.service)
	}

	wait := req.WaitTimeoutS
	if wait <= 0 {
		wait = cfg.WaitTimeoutS
	}
	if wait <= 0 {
		wait = DefaultWaitTimeoutS
	}

	base := ComposeArgs(cfg)
	pull := append(append([]string{}, base...), "pull")
	pull = append(pull, services...)
	up := append(append([]string{}, base...),
		"up", "-d", "--force-recreate", "--no-deps", "--wait", "--wait-timeout", strconv.Itoa(wait))
	up = append(up, services...)

	return &ApplyPlan{
		EnvRewrite:   newEnv,
		Previous:     previous,
		Services:     services,
		Commands:     [][]string{pull, up},
		WaitTimeoutS: wait,
	}, nil
}

// DefaultWaitTimeoutS bounds `up --wait`. Generous: a cold pull is already done
// by then, but a control plane runs migrations before it is healthy.
const DefaultWaitTimeoutS = 300

// ComposeArgs is the invocation that survives every overlay, because it is the
// one nobody configures (prototype finding 4): project, working directory and
// the ordered `-f` list all come from the updater's OWN compose labels, so
// whatever overlays the operator used are already in it.
func ComposeArgs(cfg Config) []string {
	args := []string{"compose", "-p", cfg.Project, "--project-directory", cfg.WorkingDir}
	for _, f := range cfg.ConfigFiles {
		args = append(args, "-f", f)
	}
	return args
}

// digestOf extracts the `sha256:...` half of `repo@sha256:...`. A local tag
// (`quasar-node-agent:latest`), an empty value or anything unparsable yields
// nil: "we could not determine it", never a guess.
func digestOf(value string) *string {
	i := strings.Index(value, "@")
	if i < 0 {
		return nil
	}
	d := strings.TrimSpace(value[i+1:])
	if !digestRe.MatchString(d) {
		return nil
	}
	return &d
}
