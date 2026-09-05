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
		e.fail(res, ReasonPullFailed, outputOrErr(out, err))
		return
	}

	res.State = StateRecreating
	e.save(res)
	upOut, upCode, upErr := e.run(ctx, plan.Commands[1], e.RecreateTimeout)

	res.State = StateVerifying
	e.save(res)
	reason, detail := e.verify(ctx, plan.Services, upCode != 0 || upErr != nil)
	if reason == "" {
		res.Output = ""
		res.Reason = nil
		res.State = StateSucceeded
		e.save(res)
		return
	}

	// Report the recreate's output, not the probe's: the operator needs what
	// compose said, with the post-state as one added line.
	body := outputOrErr(upOut, upErr)
	if detail != "" {
		body = strings.TrimRight(body, "\n") + "\n" + detail + "\n"
	}

	// The only automatic restore: control-plane target, never started, so no
	// migration can have run (ADR 0002) and no console is left to click
	// "Revert". A node-agent apply is NEVER auto-restored — it carries no
	// migrations, and a host that silently reverts hides the failure.
	if reason == ReasonNeverStarted && targetsControlPlane(req.Components) {
		res.Restored = e.restore(ctx, plan)
		if res.Restored {
			body = strings.TrimRight(body, "\n") +
				"\nthe new container never started; .env restored from .env.prev and the previous digest brought back up\n"
		} else {
			body = strings.TrimRight(body, "\n") +
				"\nthe new container never started and the automatic restore ALSO failed; apply the digests in `previous` by hand\n"
		}
	}

	res.Output = TailOutput(body, OutputTailBytes)
	e.fail(res, reason, res.Output)
}

func targetsControlPlane(components []Component) bool {
	for _, c := range components {
		if c.Name == ComponentControlPlane {
			return true
		}
	}
	return false
}

// restore puts `.env.prev` back and re-runs `up` for the same services.
func (e *Executor) restore(ctx context.Context, plan *ApplyPlan) bool {
	prev, err := os.ReadFile(e.prevPath())
	if err != nil {
		log.Printf("restore: cannot read %s: %v", e.prevPath(), err)
		return false
	}
	if err := os.WriteFile(e.EnvPath, prev, 0o600); err != nil {
		log.Printf("restore: cannot write %s: %v", e.EnvPath, err)
		return false
	}
	out, code, err := e.run(ctx, plan.Commands[1], e.RecreateTimeout)
	if err != nil || code != 0 {
		log.Printf("restore: recreate failed (exit %d): %s", code, TailOutput(outputOrErr(out, err), 2048))
		return false
	}
	return true
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
// health-less, else a reason plus one line of detail.
func (e *Executor) verify(ctx context.Context, services []string, upFailed bool) (string, string) {
	args := append(ComposeArgs(e.Cfg), "ps", "-a", "--format", "json")
	args = append(args, services...)
	out, code, err := e.run(ctx, args, 60*time.Second)
	if err != nil || code != 0 {
		// Cannot see the stack, which is not evidence of success.
		if upFailed {
			return ReasonRecreateFailed, "post-state could not be read: " + strings.TrimSpace(out)
		}
		return ReasonUnhealthy, "post-state could not be read: " + strings.TrimSpace(out)
	}
	byService := map[string]composePS{}
	for _, p := range parseComposePS(out) {
		byService[p.Service] = p
	}

	for _, svc := range services {
		p, found := byService[svc]
		if !found || p.ID == "" {
			// No container at all, so there is nothing to have started.
			return ReasonRecreateFailed, fmt.Sprintf("service %s has no container after the recreate", svc)
		}
		running := strings.EqualFold(p.State, "running")
		healthy := p.Health == "" || strings.EqualFold(p.Health, "healthy")
		if running && healthy {
			continue
		}
		// Checked first: it is the one failure in which nothing the new image
		// would have done can have happened, which is what makes a restore safe.
		if e.neverStarted(ctx, p.ID) {
			return ReasonNeverStarted, fmt.Sprintf("service %s: container %s never started (State.StartedAt is zero)", svc, p.Name)
		}
		if upFailed {
			return ReasonRecreateFailed, fmt.Sprintf("service %s: state=%s health=%s", svc, p.State, p.Health)
		}
		return ReasonUnhealthy, fmt.Sprintf("service %s: state=%s health=%s", svc, p.State, p.Health)
	}
	if upFailed {
		// Everything is running and healthy but compose exited non-zero. Trust
		// the stack over the exit code — that is the whole reason post-state is
		// read — but say so.
		log.Printf("verify: compose exited non-zero yet every service is running and healthy; treating the stack as authoritative")
	}
	return "", ""
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
