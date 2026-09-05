package updater

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// The compose invocation that survives every overlay is the one nobody
// configures: read this container's own compose labels and rebuild
// `docker compose -p <project> --project-directory <dir> -f <f>...` from them,
// so the operator's overlays are already in it.
//
// Fails closed. Without the labels there is no correct invocation to guess at,
// and a guess would recreate some other project's containers.

const (
	labelProject     = "com.docker.compose.project"
	labelWorkingDir  = "com.docker.compose.project.working_dir"
	labelConfigFiles = "com.docker.compose.project.config_files"
)

// Discover reads this container's compose labels over the mounted socket.
func Discover(ctx context.Context, d Docker) (project, workingDir string, configFiles []string, err error) {
	self := SelfContainerID()
	if self == "" {
		return "", "", nil, fmt.Errorf("cannot identify this container (no id in /proc/self/mountinfo and $HOSTNAME=%q is not a container id), so the compose project cannot be discovered", os.Getenv("HOSTNAME"))
	}
	// One inspect, three lines in a fixed order: no JSON parsing, and a missing
	// label is an empty line rather than a template error.
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
	// The label paths are HOST paths: unmounted at the same absolute path,
	// every compose call reads a file that is not there. Say so now, with the
	// fix, rather than at the first apply.
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

// SelfContainerID reads this container's own id from `/proc/self/mountinfo`,
// falling back to `$HOSTNAME` only when it LOOKS like a container id. A compose
// stack that sets `hostname:` makes `$HOSTNAME` a DNS name (`quasar-dev.local`)
// and `docker inspect -- quasar-dev.local` answers "No such object" — the same
// defect that made the agent's install discovery report unknown (#107). Its
// Rust twin is `nvidia_volume::self_container_id`.
func SelfContainerID() string {
	if body, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		if id := containerIDFromMountinfo(string(body)); id != "" {
			return id
		}
	}
	h := os.Getenv("HOSTNAME")
	if len(h) >= 12 && isHex(h) {
		return h
	}
	return ""
}

func containerIDFromMountinfo(body string) string {
	for _, line := range strings.Split(body, "\n") {
		for _, token := range strings.Split(line, "/") {
			if len(token) == 64 && isHex(token) {
				return token
			}
		}
	}
	return ""
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return s != ""
}
