// Package mountpolicy is the shared vocabulary for what a runtime bind mount may
// name, written once because the same `mounts` column has two doors (catalog
// materialization in internal/images, admin CRUD in internal/crud) and a second
// copy would drift until the laxer door won.
//
// This is NOT the enforcement boundary. The node agent owns that
// (node-agent/src/session/mount_policy.rs): it spawns the container, the wire is
// untrusted, and only the host knows which of its own paths a session may see. What
// lives here is a deny list — the sources that are an escape on every host — so a
// bad manifest is refused at install time and a bad admin write at 400, rather than
// silently at launch.
package mountpolicy

import (
	"fmt"
	"path"
	"strings"
)

// deniedRoots are host trees no session may bind, whatever host it lands on.
// Mirrors DENIED_ROOTS in node-agent/src/session/mount_policy.rs.
var deniedRoots = []string{
	"/proc",
	"/sys",
	"/dev",
	"/boot",
	"/etc",
	"/root",
	"/run",
	"/var/run",
	"/var/lib/docker",
	"/var/lib/containerd",
	"/var/lib/containers",
	"/var/lib/kubelet",
	"/var/lib/rancher",
	"/lib/modules",
	"/usr/lib/modules",
}

// runtimeSockets: a source equal to, or an ANCESTOR of, any of these is refused.
// `/var/run/docker.sock` alone was never the hole — `/var/run` carries the same
// socket one directory up.
var runtimeSockets = []string{
	"/var/run/docker.sock",
	"/run/docker.sock",
	"/run/containerd/containerd.sock",
	"/var/run/containerd/containerd.sock",
	"/run/crio/crio.sock",
	"/run/podman/podman.sock",
	"/var/run/podman/podman.sock",
	"/var/run/crio/crio.sock",
}

// Error is the caller-facing summary for a 400 or a rolled-back install.
const Error = "each mount must be src:dst[:opts] with absolute paths, and may not name the host root, " +
	"a container-runtime socket or its parent directory, or /proc, /sys, /dev, /etc, /root, /run or /var/lib/docker"

// Validate checks one `src:dst[:opts]` entry. A nil return does not mean the mount
// will launch: the target host's allowlist decides that.
func Validate(mount string) error {
	parts := strings.SplitN(mount, ":", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("mount %q is not in src:dst[:opts] form", mount)
	}
	src, dst := parts[0], parts[1]
	if !strings.HasPrefix(src, "/") {
		return fmt.Errorf("mount %q: the source must be an absolute host path", mount)
	}
	if !strings.HasPrefix(dst, "/") {
		return fmt.Errorf("mount %q: the destination must be an absolute container path", mount)
	}
	if dst == "/" {
		return fmt.Errorf("mount %q: a destination of %q (the container root) is not allowed", mount, "/")
	}
	if strings.ContainsAny(mount, "\x00\n") {
		return fmt.Errorf("mount %q: control characters are not allowed", mount)
	}
	for _, seg := range strings.Split(src, "/") {
		if seg == ".." {
			return fmt.Errorf("mount %q: a source containing %q is not allowed", mount, "..")
		}
	}

	clean := path.Clean(src)
	if clean == "/" {
		return fmt.Errorf("mount %q: a source of %q (the whole host) is not allowed", mount, "/")
	}
	for _, denied := range deniedRoots {
		if under(denied, clean) {
			return fmt.Errorf("mount %q: the source is under %s, which a session may never bind", mount, denied)
		}
	}
	for _, sock := range runtimeSockets {
		if under(clean, sock) {
			return fmt.Errorf("mount %q: the source contains the container-runtime socket %s — "+
				"binding it hands the session control of the host daemon", mount, sock)
		}
	}
	return nil
}

// ValidateAll returns the first rejection.
func ValidateAll(mounts []string) error {
	for _, m := range mounts {
		if err := Validate(m); err != nil {
			return err
		}
	}
	return nil
}

// under is component-wise, so /var/lib/dockerfoo is not under /var/lib/docker.
func under(root, p string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}
