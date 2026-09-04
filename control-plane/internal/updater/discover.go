package updater

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// SELF-DISCOVERY (prototype finding 4). The compose invocation that survives
// every overlay — base, nvidia, console, dev, hardened, multiagent — is the one
// nobody configures: the updater reads its OWN container's compose labels and
// reconstructs `docker compose -p <project> --project-directory <dir> -f <f>...`
// from them. Whatever overlays the operator used are in those labels already,
// so there is no list to keep in step and nothing to get wrong on a host with
// an unusual stack.
//
// It FAILS CLOSED. Without the labels there is no correct compose invocation to
// guess at, and guessing would mean acting on the wrong project — so the
// program refuses to serve rather than serving something that might recreate
// somebody else's containers.

const (
	labelProject     = "com.docker.compose.project"
	labelWorkingDir  = "com.docker.compose.project.working_dir"
	labelConfigFiles = "com.docker.compose.project.config_files"
)

// Discover reads this container's compose labels through the mounted socket.
// `$HOSTNAME` inside a container is its short id unless the operator set one,
// which is exactly what `docker inspect` accepts.
func Discover(ctx context.Context, d Docker) (project, workingDir string, configFiles []string, err error) {
	self := os.Getenv("HOSTNAME")
	if self == "" {
		return "", "", nil, fmt.Errorf("HOSTNAME is empty: the updater cannot identify its own container, so it cannot discover its compose project")
	}
	// One inspect, three lines, in a fixed order — no JSON parsing, and a
	// missing label comes back as an empty line rather than a template error.
	format := fmt.Sprintf(`{{index .Config.Labels "%s"}}
{{index .Config.Labels "%s"}}
{{index .Config.Labels "%s"}}`, labelProject, labelWorkingDir, labelConfigFiles)
	out, code, err := d.Run(ctx, []string{"inspect", "--format", format, "--", self})
	if err != nil {
		return "", "", nil, fmt.Errorf("docker inspect %s: %w", self, err)
	}
	if code != 0 {
		return "", "", nil, fmt.Errorf("docker inspect %s exited %d: %s", self, code, strings.TrimSpace(out))
	}
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	for len(lines) < 3 {
		lines = append(lines, "")
	}
	project = strings.TrimSpace(lines[0])
	workingDir = strings.TrimSpace(lines[1])
	for _, f := range strings.Split(lines[2], ",") {
		if f = strings.TrimSpace(f); f != "" {
			configFiles = append(configFiles, f)
		}
	}
	if project == "" || workingDir == "" || len(configFiles) == 0 {
		return "", "", nil, fmt.Errorf(
			"container %s carries no compose labels (%s / %s / %s). The updater must run as a compose service in the stack it updates; a bare `docker run` cannot be discovered",
			self, labelProject, labelWorkingDir, labelConfigFiles)
	}
	// The label paths are HOST paths. If the stack directory is not mounted at
	// that same absolute path, every compose call would read a file that is not
	// there — so say it now, with the fix, rather than at the first apply.
	if _, statErr := os.Stat(workingDir); statErr != nil {
		return "", "", nil, fmt.Errorf(
			"the stack directory %s (from label %s) is not visible in this container: %v. Mount it at its HOST path — set QUASAR_STACK_DIR in deploy/.env",
			workingDir, labelWorkingDir, statErr)
	}
	for _, f := range configFiles {
		if _, statErr := os.Stat(f); statErr != nil {
			return "", "", nil, fmt.Errorf(
				"compose file %s (from label %s) is not visible in this container: %v. Mount the stack directory at its HOST path — set QUASAR_STACK_DIR in deploy/.env",
				f, labelConfigFiles, statErr)
		}
	}
	return project, workingDir, configFiles, nil
}
