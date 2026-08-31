//! Map raw docker pull/build/rmi stderr to short operator-readable causes
//! (agent-api.md `image_state.error`: never a raw docker error blob).
//!
//! An unrecognized failure must map to a fixed string, never an echo of the raw text:
//! docker stderr carries registry hostnames, auth hints, and the literal invocation
//! (`ContainerRuntime::run_raw` embeds the command line). Raw text stays in the local log.

/// Unmapped `docker pull` failure — the raw stderr is logged locally.
pub const PULL_FALLBACK: &str = "docker pull failed; inspect node-agent logs";
/// Unmapped `docker build` failure — the raw build log is logged locally.
pub const BUILD_FALLBACK: &str = "docker build failed; inspect node-agent logs";
/// Unmapped `docker rmi` failure — the raw stderr is logged locally.
pub const RMI_FALLBACK: &str = "docker rmi failed; inspect node-agent logs";
/// Sentinel mapping: the daemon says the image is already gone, which is a
/// *success* for a best-effort remove (callers compare against this).
pub const RMI_ALREADY_ABSENT: &str = "already absent";

/// Map a failed `docker pull`'s stderr into a short cause.
pub fn map_pull_error(raw: &str) -> String {
    let lower = raw.to_lowercase();
    if lower.contains("no space left on device") {
        "insufficient disk".to_string()
    } else if lower.contains("unauthorized")
        || lower.contains("authentication required")
        || lower.contains("access denied")
        || lower.contains("denied:")
    {
        "registry auth denied".to_string()
    } else if lower.contains("manifest unknown")
        || lower.contains("manifest not found")
        || lower.contains("not found: manifest")
        || lower.contains("no such manifest")
    {
        "manifest not found".to_string()
    } else if lower.contains("timeout")
        || lower.contains("no such host")
        || lower.contains("dial tcp")
        || lower.contains("network is unreachable")
        || lower.contains("connection refused")
    {
        "network error".to_string()
    } else {
        PULL_FALLBACK.to_string()
    }
}

/// Map a failed `docker build`'s output into a short cause. Unrecognized failures
/// must never echo the raw build log (agent-api.md `image_build`) — it can carry the
/// Dockerfile, registry hostnames and secrets echoed by a `RUN`.
pub fn map_build_error(raw: &str) -> String {
    let lower = raw.to_lowercase();
    if lower.contains("no space left on device") {
        "insufficient disk".to_string()
    } else {
        BUILD_FALLBACK.to_string()
    }
}

/// Map a failed `docker rmi`'s stderr (agent-api.md `image_remove`: "never
/// force-removes an image backing a live container").
pub fn map_rmi_error(raw: &str) -> String {
    let lower = raw.to_lowercase();
    if lower.contains("no such image") {
        RMI_ALREADY_ABSENT.to_string()
    } else if lower.contains("image is being used")
        || lower.contains("has dependent child images")
        || lower.contains("must force")
    {
        "image in use".to_string()
    } else {
        RMI_FALLBACK.to_string()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn maps_disk_full() {
        assert_eq!(
            map_pull_error("write /var/lib/docker/x: no space left on device"),
            "insufficient disk"
        );
    }

    #[test]
    fn maps_auth_denied() {
        assert_eq!(
            map_pull_error(
                "Error response from daemon: pull access denied for x, repository does not \
                 exist or may require 'docker login': denied: requested access to the resource \
                 is denied"
            ),
            "registry auth denied"
        );
    }

    #[test]
    fn maps_manifest_not_found() {
        assert_eq!(
            map_pull_error("manifest unknown: manifest unknown"),
            "manifest not found"
        );
    }

    #[test]
    fn maps_network_error() {
        assert_eq!(
            map_pull_error("Get \"https://ghcr.io/v2/\": dial tcp: lookup ghcr.io: no such host"),
            "network error"
        );
    }

    #[test]
    fn unknown_pull_errors_never_leak_raw_docker_output() {
        let mapped = map_pull_error("some entirely novel docker failure text");
        assert_eq!(mapped, PULL_FALLBACK);
        // Multi-line blobs and long text are equally never echoed.
        assert_eq!(map_pull_error(&"x".repeat(500)), PULL_FALLBACK);
        assert_eq!(map_pull_error("first line\nsecond line"), PULL_FALLBACK);
        assert_eq!(map_pull_error(""), PULL_FALLBACK);
    }

    #[test]
    fn unknown_rmi_errors_never_leak_the_docker_command_line() {
        // `ContainerRuntime::run_raw`'s error embeds the full invocation — the
        // shape run_remove feeds in.
        let raw = "`docker rmi -- ghcr.io/x/steam:sha-abc1234` failed: something novel";
        let mapped = map_rmi_error(raw);
        assert_eq!(mapped, RMI_FALLBACK);
        assert!(!mapped.contains("ghcr.io"));
        assert!(!mapped.contains("novel"));
    }

    #[test]
    fn maps_build_disk_full() {
        assert_eq!(
            map_build_error("failed to solve: write /var/lib/docker/x: no space left on device"),
            "insufficient disk"
        );
    }

    #[test]
    fn unknown_build_errors_never_leak_the_raw_build_log() {
        assert_eq!(
            map_build_error(
                "Step 3/5 : RUN false\nThe command returned a non-zero code: 1\nsecret=hunter2"
            ),
            BUILD_FALLBACK
        );
        assert!(!map_build_error("secret=hunter2 in the log").contains("hunter2"));
        assert_eq!(map_build_error(""), BUILD_FALLBACK);
    }

    #[test]
    fn maps_rmi_in_use() {
        assert_eq!(
            map_rmi_error(
                "Error response from daemon: conflict: unable to remove repository reference \
                 \"x\" (must force) - container abc is using its referenced image def"
            ),
            "image in use"
        );
    }

    #[test]
    fn maps_rmi_already_absent() {
        assert_eq!(
            map_rmi_error("Error: No such image: steam:latest"),
            "already absent"
        );
    }
}
