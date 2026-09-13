package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The executor runs what plan.go decided and judges the outcome from the
// stack's post-state, not from an exit code: `up -d` without `--wait` returns 0
// for a container that starts and then dies, and the distinction that decides
// whether a restore is safe — never-started vs started-then-failed — is
// `State.StartedAt` being zero, which is what proves no migration can have run.

// Docker is every docker invocation this program makes, behind one seam so
// tests can drive a fake `docker`.
type Docker interface {
	// Run executes `docker <args...>` and returns the combined output. A
	// non-zero exit is an exit code, not an error; err means the command could
	// not be run at all.
	Run(ctx context.Context, args []string) (output string, exitCode int, err error)
}

// CLI drives the docker binary the image ships, over the mounted socket.
type CLI struct {
	Bin string // "docker" unless a test or an operator says otherwise
}

func (c CLI) Run(ctx context.Context, args []string) (string, int, error) {
	bin := c.Bin
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return string(out), ee.ExitCode(), nil
		}
		return string(out), -1, err
	}
	return string(out), 0, nil
}

// Executor applies one plan and keeps the result file current as it goes.
type Executor struct {
	Store  *Store
	Docker Docker
	Cfg    Config
	// The stack's `.env`, at its HOST path: the stack directory is mounted into
	// this container at that path so the compose labels resolve here.
	EnvPath string
	// Wall-clock bounds per step, independent of compose's `--wait-timeout`,
	// which only covers health.
	PullTimeout     time.Duration
	RecreateTimeout time.Duration
}

// `.env.prev`: the previous file kept verbatim, so a restore is a copy.
func (e *Executor) prevPath() string { return e.EnvPath + ".prev" }

// Apply is the whole detached job. There is no caller left to return an error
// to, so every outcome goes to the result file, which is all anyone reads.
func (e *Executor) Apply(ctx context.Context, req ApplyRequest, plan *ApplyPlan, priorEnv string) {
	defer e.Store.Release(req.RequestID)

	res := &Result{
		RequestID:  req.RequestID,
		State:      StatePending,
		Components: req.Components,
		Previous:   plan.Previous,
		Release:    req.Release,
		Commands:   plan.Commands,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	e.save(res)

	// `.env.prev` FIRST. The other order leaves a crash between the two writes
	// with the new digest installed and no record of the old one.
	if err := os.WriteFile(e.prevPath(), []byte(priorEnv), 0o600); err != nil {
		e.fail(res, ReasonRecreateFailed, fmt.Sprintf("could not write %s: %v", e.prevPath(), err))
		return
	}
	if err := os.WriteFile(e.EnvPath, []byte(plan.EnvRewrite), 0o600); err != nil {
		e.fail(res, ReasonRecreateFailed, fmt.Sprintf("could not write %s: %v", e.EnvPath, err))
		return
	}

	res.State = StatePulling
	e.save(res)
	out, code, err := e.run(ctx, plan.Commands[0], e.PullTimeout)
	if err != nil || code != 0 {
		// Nothing was recreated, so the old container still runs: put `.env`
		// back so it stops naming an image that never arrived. No `up`.
		body := outputOrErr(out, err)
		if e.restoreEnv() {
			body = strings.TrimRight(body, "\n") + "\n.env restored from .env.prev; nothing was recreated\n"
		}
		e.fail(res, ReasonPullFailed, body)
		return
	}

	res.State = StateRecreating
	e.save(res)
	upOut, upCode, upErr := e.run(ctx, plan.Commands[1], e.RecreateTimeout)

	res.State = StateVerifying
	e.save(res)
	reason, detail, failedID := e.verify(ctx, plan.Services, upCode != 0 || upErr != nil)
	if reason == "" {
		res.Output = ""
		res.Reason = nil
		res.State = StateSucceeded
		e.save(res)
		return
	}

	// Report the recreate's output, not the probe's: the operator needs what
	// compose said, with the post-state as one added line — and the failed
	// container's own last lines, which is where an agent's refusal to start
	// (health-bind-failed, #152) is written.
	body := outputOrErr(upOut, upErr)
	if detail != "" {
		body = strings.TrimRight(body, "\n") + "\n" + detail + "\n"
	}
	if tail := e.containerLogTail(ctx, failedID); tail != "" {
		body = strings.TrimRight(body, "\n") + "\n--- last lines of the failed container ---\n" + tail + "\n"
	}

	// The automatic restore (restoreWorthy): a never-started control plane,
	// because no migration can have run (ADR 0002) and no console is left to
	// press Revert in; and a node agent whose new container failed its health
	// wait, because the agent that would carry an operator's revert is the one
	// that is down (ADR 0004). A control plane that STARTED is never restored.
	if restoreWorthy(reason, req.Components) {
		res.Restored = e.restore(ctx, plan)
		if res.Restored {
			body = strings.TrimRight(body, "\n") +
				"\nthe new container did not come up; .env restored from .env.prev and the previous digest brought back up\n"
		} else {
			body = strings.TrimRight(body, "\n") +
				"\nthe new container did not come up and the automatic restore ALSO failed; apply the digests in `previous` by hand\n"
		}
	}

	res.Output = TailOutput(body, OutputTailBytes)
	e.fail(res, reason, res.Output)
}

// restoreWorthy is the pure restore decision. semantics: agent-api.md
// §release_state (`restored`), control-api.md §"Self-update hardening".
func restoreWorthy(reason string, components []Component) bool {
	switch reason {
	case ReasonNeverStarted:
		return true
	case ReasonRecreateFailed, ReasonUnhealthy:
		// A started control plane may have migrated; a node agent carries no
		// migration, so going back is always safe.
		return !targetsControlPlane(components)
	}
	return false
}

func targetsControlPlane(components []Component) bool {
	for _, c := range components {
		if c.Name == ComponentControlPlane {
			return true
		}
	}
	return false
}

// restoreEnv puts `.env.prev` back, and nothing else.
func (e *Executor) restoreEnv() bool {
	prev, err := os.ReadFile(e.prevPath())
	if err != nil {
		log.Printf("restore: cannot read %s: %v", e.prevPath(), err)
		return false
	}
	if err := os.WriteFile(e.EnvPath, prev, 0o600); err != nil {
		log.Printf("restore: cannot write %s: %v", e.EnvPath, err)
		return false
	}
	return true
}

// restore puts `.env.prev` back and re-runs `up` for the same services.
func (e *Executor) restore(ctx context.Context, plan *ApplyPlan) bool {
	if !e.restoreEnv() {
		return false
	}
	out, code, err := e.run(ctx, plan.Commands[1], e.RecreateTimeout)
	if err != nil || code != 0 {
		log.Printf("restore: recreate failed (exit %d): %s", code, TailOutput(outputOrErr(out, err), 2048))
		return false
	}
	return true
}

// EffectiveImages is the image reference compose would actually use for each
// component, keyed by COMPONENT name (`control-plane`, `node-agent`), not by
// service name — the caller thinks in components. A component whose service is
// absent from this stack maps to nil, which is different from an empty string.
//
// It comes from `compose config`, not from the compose file, so every layer of
// resolution is already applied: an unset `${QUASAR_CONTROL_IMAGE}` shows as the
// default `quasar-control-plane:latest`, an `.env` pin shows as
// `repo@sha256:…`, and an overlay that replaces the image shows the override.
// That is what makes the answer usable for classifying install mode.
func (e *Executor) EffectiveImages(ctx context.Context) map[string]*string {
	out := map[string]*string{}
	for name := range componentTargets {
		out[name] = nil
	}
	args := append(ComposeArgs(e.Cfg), "config", "--format", "json")
	body, code, err := e.run(ctx, args, 60*time.Second)
	if err != nil || code != 0 {
		log.Printf("compose config: exit %d: %s", code, TailOutput(body, 512))
		return out
	}
	var doc struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		log.Printf("compose config: unparsable output: %v", err)
		return out
	}
	for name, t := range componentTargets {
		if svc, ok := doc.Services[t.service]; ok && svc.Image != "" {
			img := svc.Image
			out[name] = &img
		}
	}
	return out
}

// ServiceConfigFiles is, per compose service this program may recreate, the
// compose-file set its running container was started with (its own
// com.docker.compose.project.config_files label). A service with no running
// container maps to nil. Preflight compares each against this updater's own set
// (`updater_overlays`): a recreate uses the updater's, so a service brought up
// with an overlay the updater does not know would silently lose it.
func (e *Executor) ServiceConfigFiles(ctx context.Context) map[string][]string {
	out := map[string][]string{}
	for _, t := range componentTargets {
		out[t.service] = nil
	}
	args := []string{"ps", "--filter", "label=" + labelProject + "=" + e.Cfg.Project,
		"--format", `{{.Label "com.docker.compose.service"}}` + "\t" + `{{.Label "` + labelConfigFiles + `"}}`}
	body, code, err := e.run(ctx, args, 30*time.Second)
	if err != nil || code != 0 {
		log.Printf("docker ps (service labels): exit %d: %s", code, TailOutput(body, 512))
		return out
	}
	for _, line := range strings.Split(body, "\n") {
		svc, files, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		if _, known := out[svc]; !known {
			continue
		}
		var list []string
		for _, f := range strings.Split(files, ",") {
			if f = strings.TrimSpace(f); f != "" {
				list = append(list, f)
			}
		}
		if list == nil {
			list = []string{}
		}
		out[svc] = list
	}
	return out
}

// composePS is one service's post-state as `docker compose ps --format json`
// reports it. Only the fields that decide the verdict are named.
type composePS struct {
	ID      string `json:"ID"`
	Name    string `json:"Name"`
	Service string `json:"Service"`
	State   string `json:"State"`
	Health  string `json:"Health"`
}

// verify returns "" when every named service is running and healthy-or-
// health-less, else a reason, one line of detail, and the failed container's
// id ("" when there is none to read logs from).
func (e *Executor) verify(ctx context.Context, services []string, upFailed bool) (string, string, string) {
	args := append(ComposeArgs(e.Cfg), "ps", "-a", "--format", "json")
	args = append(args, services...)
	out, code, err := e.run(ctx, args, 60*time.Second)
	if err != nil || code != 0 {
		// Cannot see the stack, which is not evidence of success.
		if upFailed {
			return ReasonRecreateFailed, "post-state could not be read: " + strings.TrimSpace(out), ""
		}
		return ReasonUnhealthy, "post-state could not be read: " + strings.TrimSpace(out), ""
	}
	byService := map[string]composePS{}
	for _, p := range parseComposePS(out) {
		byService[p.Service] = p
	}

	for _, svc := range services {
		p, found := byService[svc]
		if !found || p.ID == "" {
			// No container at all, so there is nothing to have started.
			return ReasonRecreateFailed, fmt.Sprintf("service %s has no container after the recreate", svc), ""
		}
		running := strings.EqualFold(p.State, "running")
		healthy := p.Health == "" || strings.EqualFold(p.Health, "healthy")
		if running && healthy {
			continue
		}
		// Checked first: it is the one failure in which nothing the new image
		// would have done can have happened, which is what makes a restore safe.
		if e.neverStarted(ctx, p.ID) {
			return ReasonNeverStarted, fmt.Sprintf("service %s: container %s never started (State.StartedAt is zero)", svc, p.Name), p.ID
		}
		if upFailed {
			return ReasonRecreateFailed, fmt.Sprintf("service %s: state=%s health=%s", svc, p.State, p.Health), p.ID
		}
		return ReasonUnhealthy, fmt.Sprintf("service %s: state=%s health=%s", svc, p.State, p.Health), p.ID
	}
	if upFailed {
		// Everything is running and healthy but compose exited non-zero. Trust
		// the stack over the exit code — that is the whole reason post-state is
		// read — but say so.
		log.Printf("verify: compose exited non-zero yet every service is running and healthy; treating the stack as authoritative")
	}
	return "", "", ""
}

// containerLogTail is the failed container's last lines, "" when there is no
// container or docker cannot read it. Bounded: it lands inside the 8 KiB
// output the wire carries.
func (e *Executor) containerLogTail(ctx context.Context, containerID string) string {
	if containerID == "" {
		return ""
	}
	out, code, err := e.run(ctx, []string{"logs", "--tail", "40", "--", containerID}, 30*time.Second)
	if err != nil || code != 0 {
		return ""
	}
	return TailOutput(strings.TrimRight(out, "\n"), 3072)
}

// zeroStartedAt is what docker prints for a container that has never run.
const zeroStartedAt = "0001-01-01T00:00:00Z"

func (e *Executor) neverStarted(ctx context.Context, containerID string) bool {
	out, code, err := e.run(ctx, []string{"inspect", "--format", "{{.State.StartedAt}}", "--", containerID}, 30*time.Second)
	if err != nil || code != 0 {
		// Unknown is not "never started": never trigger a restore on a guess.
		return false
	}
	s := strings.TrimSpace(out)
	return s == "" || strings.HasPrefix(s, "0001-01-01")
}

// Both shapes compose emits for `--format json`: a JSON array and NDJSON. Which
// one depends on the compose version, and this image pins its own.
func parseComposePS(out string) []composePS {
	trimmed := strings.TrimSpace(out)
	if strings.HasPrefix(trimmed, "[") {
		var arr []composePS
		if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
			return arr
		}
	}
	var res []composePS
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var p composePS
		if err := json.Unmarshal([]byte(line), &p); err == nil {
			res = append(res, p)
		}
	}
	return res
}

func (e *Executor) run(ctx context.Context, args []string, timeout time.Duration) (string, int, error) {
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	log.Printf("docker %s", strings.Join(args, " "))
	return e.Docker.Run(cctx, args)
}

func outputOrErr(out string, err error) string {
	if err != nil {
		return strings.TrimRight(out, "\n") + "\n" + err.Error() + "\n"
	}
	return out
}

func (e *Executor) save(r *Result) {
	if err := e.Store.Write(r); err != nil {
		log.Printf("could not write result for %s: %v", r.RequestID, err)
	}
}

func (e *Executor) fail(r *Result, reason, output string) {
	r.State = StateFailed
	r.Reason = &reason
	r.Output = TailOutput(output, OutputTailBytes)
	e.save(r)
	log.Printf("apply %s FAILED: %s", r.RequestID, reason)
}

// Where a discovered project keeps its env file; named so the server and the
// executor cannot disagree.
func EnvPathFor(cfg Config) string { return filepath.Join(cfg.WorkingDir, ".env") }
