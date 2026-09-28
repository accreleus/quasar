//! Which container is this process? The engine-side identity a caller then
//! inspects (through [`crate::RuntimeClient::inspect_container`]) to learn its own
//! mounts, image and network.

/// Our own container id, from `/proc/self/mountinfo` with `$HOSTNAME` as fallback. Both
/// best-effort; a miss blocks dependent app launches unless an explicit host path
/// is validated. The agent remains available to explain the failed inspection.
pub fn self_container_id() -> Option<String> {
    if let Ok(body) = std::fs::read_to_string("/proc/self/mountinfo") {
        if let Some(id) = parse_container_id_from_mountinfo(&body) {
            return Some(id);
        }
    }
    std::env::var("HOSTNAME")
        .ok()
        .filter(|h| hostname_is_container_id(h))
}

/// Whether `$HOSTNAME` may stand in for the container id. A compose stack that
/// sets `hostname:` makes it a DNS name (`quasar-dev.local`), which docker
/// answers "No such object" for — so the shape is checked, never assumed.
pub fn hostname_is_container_id(hostname: &str) -> bool {
    hostname.len() >= 12 && hostname.chars().all(|c| c.is_ascii_hexdigit())
}

/// Pull the 64-hex container id out of a mountinfo body.
pub fn parse_container_id_from_mountinfo(body: &str) -> Option<String> {
    // Overlay lowerdir digests are also 64 hex characters. Only the engine's
    // per-container identity-file mounts identify THIS container. Two layouts carry
    // them (RH-07 #405, measured on Podman 5.8.4 rootful and rootless):
    //   Docker:  .../containers/<64 hex>/hosts
    //   Podman:  .../overlay-containers/<64 hex>/userdata/hosts
    // Podman's host-networked containers also carry the HOST's name in $HOSTNAME, so
    // the fallback below cannot stand in for it there.
    for line in body.lines() {
        let fields: Vec<_> = line.split_whitespace().collect();
        if fields.len() < 6
            || !matches!(
                fields[4],
                "/etc/hosts" | "/etc/hostname" | "/etc/resolv.conf"
            )
        {
            continue;
        }
        let parts: Vec<_> = fields[3].split('/').collect();
        let id_shaped = |s: &str| s.len() == 64 && s.chars().all(|c| c.is_ascii_hexdigit());
        for (i, pair) in parts.windows(2).enumerate() {
            let docker = pair[0] == "containers";
            let podman = pair[0] == "overlay-containers" && parts.get(i + 2) == Some(&"userdata");
            if (docker || podman) && id_shaped(pair[1]) {
                return Some(pair[1].to_owned());
            }
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn overlay_digests_are_not_container_ids() {
        let layer = "b".repeat(64);
        let id = "a".repeat(64);
        let body = format!("1 0 0:1 / / rw - overlay overlay rw,lowerdir=/layers/{layer}/diff\n2 1 8:1 /var/lib/docker/containers/{id}/hosts /etc/hosts rw - ext4 /dev/sda rw\n");
        assert_eq!(parse_container_id_from_mountinfo(&body), Some(id));
        assert_eq!(
            parse_container_id_from_mountinfo(&format!(
                "1 0 0:1 /layers/{layer}/diff /opt/data rw - ext4 /dev/sda rw"
            )),
            None
        );
    }

    /// RH-07 #405: the lines real Podman containers carry (rootless, then rootful), with
    /// host networking, where `$HOSTNAME` is the host's own name.
    #[test]
    fn podman_container_ids_are_recovered_from_mountinfo() {
        let id = "89a71e3f44e796f7dc9d6ccc726d06b4a6470234d3b2b887135813d17a834c19";
        let rootless = format!(
            "1032 1000 0:70 / / rw - overlay overlay rw\n\
             1026 1032 0:63 /containers/overlay-containers/{id}/userdata/hosts /etc/hosts rw,nosuid,nodev,relatime - tmpfs tmpfs rw,seclabel,mode=700,uid=1001,gid=1001\n"
        );
        assert_eq!(
            parse_container_id_from_mountinfo(&rootless).as_deref(),
            Some(id)
        );
        let rootful = format!(
            "1048 1037 0:29 /containers/storage/overlay-containers/{id}/userdata/resolv.conf /etc/resolv.conf rw - tmpfs tmpfs rw,seclabel,mode=755\n"
        );
        assert_eq!(
            parse_container_id_from_mountinfo(&rootful).as_deref(),
            Some(id)
        );
        // The same shape without Podman's `userdata` is not an identity mount.
        let other = format!(
            "1 0 0:1 /overlay-containers/{id}/other/hosts /etc/hosts rw - tmpfs tmpfs rw\n"
        );
        assert_eq!(parse_container_id_from_mountinfo(&other), None);
    }

    #[test]
    fn container_id_is_recovered_from_mountinfo() {
        let id = "a".repeat(64);
        let body = format!(
            "1234 1200 0:59 / / rw - overlay overlay rw\n\
             1240 1234 0:60 /var/lib/docker/containers/{id}/resolv.conf /etc/resolv.conf rw - ext4 /dev/sda1 rw\n"
        );
        assert_eq!(
            parse_container_id_from_mountinfo(&body).as_deref(),
            Some(id.as_str())
        );
        assert_eq!(parse_container_id_from_mountinfo("1 2 0:1 / / rw\n"), None);
    }
}
