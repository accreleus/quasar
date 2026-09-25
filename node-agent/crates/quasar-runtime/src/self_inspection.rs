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
    // Overlay lowerdir digests are also 64 hex characters. Only Docker's
    // per-container identity-file mounts identify THIS container.
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
        for pair in parts.windows(2) {
            if pair[0] == "containers"
                && pair[1].len() == 64
                && pair[1].chars().all(|c| c.is_ascii_hexdigit())
            {
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
