//! Evidence that the WebRTC media path reaches this host (RH-07 #403; `agent-api.md`
//! amendment 17, `media_reachability`).
//!
//! Reading the host's firewall rules needed `NET_ADMIN` in the host's network namespace,
//! which a rootless engine cannot grant, and even then it was a proxy. The evidence here is
//! real traffic: a remote WebRTC peer's connectivity checks arriving at one of this host's
//! candidates during a real session. Traffic from this host to itself never crosses its
//! firewall, so a peer whose every candidate is one of this host's own addresses proves
//! nothing.
//!
//! - **Reached**: ICE connected and at least one remote candidate is off this host. Behind a
//!   stateful firewall the peer's checks may be let in as replies to the host's own, so this
//!   says the peer reached the host, never that the port range is open.
//! - **Blocked**: the peer's candidates arrived over signaling, ICE ran to failure, and the
//!   session never connected: none of the peer's checks got through.
//! - Anything else (a session that ended first, one that connected and later dropped) is
//!   inconclusive and changes nothing.

use std::collections::HashMap;
use std::net::IpAddr;
use std::sync::Mutex;
use std::time::SystemTime;

/// The latest definitive observation, kept for the life of the agent process.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Evidence {
    /// A remote peer reached this host over WebRTC; `peer` is one of its off-host addresses
    /// (or an mDNS name a browser hides its address behind).
    Reached { at: SystemTime, peer: String },
    /// A remote peer offered `offered` candidates and none of its traffic arrived.
    Blocked { at: SystemTime, offered: usize },
}

/// What one ICE state change means here.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum IceOutcome {
    Connected,
    Failed,
    Other,
}

#[derive(Default)]
struct PeerConnection {
    remote: Vec<String>,
    connected: bool,
}

static PEERS: Mutex<Option<HashMap<usize, PeerConnection>>> = Mutex::new(None);
static LATEST: Mutex<Option<Evidence>> = Mutex::new(None);

/// A remote ICE candidate line arrived over signaling for the peer connection `key`
/// (the webrtcbin's identity).
pub fn note_remote_candidate(key: usize, candidate: &str) {
    let Some(address) = candidate_address(candidate) else {
        return;
    };
    if let Ok(mut peers) = PEERS.lock() {
        peers
            .get_or_insert_with(HashMap::new)
            .entry(key)
            .or_default()
            .remote
            .push(address);
    }
}

/// The ICE state of peer connection `key` changed.
pub fn note_ice_state(key: usize, outcome: IceOutcome) {
    let own = own_addresses();
    let evidence = {
        let Ok(mut peers) = PEERS.lock() else { return };
        let peers = peers.get_or_insert_with(HashMap::new);
        let peer = peers.entry(key).or_default();
        let evidence = decide(
            &peer.remote,
            peer.connected,
            outcome,
            &own,
            SystemTime::now(),
        );
        if outcome == IceOutcome::Connected {
            peer.connected = true;
        }
        if outcome == IceOutcome::Failed {
            peers.remove(&key);
        }
        evidence
    };
    if let Some(evidence) = evidence {
        if let Ok(mut latest) = LATEST.lock() {
            *latest = Some(evidence);
        }
    }
}

/// The peer connection `key` is gone; forget what it offered.
pub fn forget(key: usize) {
    if let Ok(mut peers) = PEERS.lock() {
        if let Some(peers) = peers.as_mut() {
            peers.remove(&key);
        }
    }
}

/// The latest definitive observation, if any session has produced one.
pub fn latest() -> Option<Evidence> {
    LATEST.lock().ok().and_then(|latest| latest.clone())
}

/// The rule, pure. `connected_before`: this peer connection already connected once, so a
/// later failure is a dropped session, not a blocked path.
pub fn decide(
    remote: &[String],
    connected_before: bool,
    outcome: IceOutcome,
    own: &[IpAddr],
    now: SystemTime,
) -> Option<Evidence> {
    match outcome {
        IceOutcome::Connected if !connected_before => remote
            .iter()
            .find(|address| off_host(address, own))
            .map(|peer| Evidence::Reached {
                at: now,
                peer: peer.clone(),
            }),
        IceOutcome::Failed if !connected_before && !remote.is_empty() => Some(Evidence::Blocked {
            at: now,
            offered: remote.len(),
        }),
        _ => None,
    }
}

/// Could traffic from `address` have crossed this host's firewall? Loopback, and any of
/// this host's own addresses, cannot. An mDNS name (`<uuid>.local`, how browsers hide host
/// candidates) is a remote peer's.
fn off_host(address: &str, own: &[IpAddr]) -> bool {
    match address.parse::<IpAddr>() {
        Ok(ip) => !ip.is_loopback() && !ip.is_unspecified() && !own.contains(&ip),
        Err(_) => address.ends_with(".local"),
    }
}

/// The connection address of an ICE candidate line
/// (`candidate:<foundation> <component> <transport> <priority> <address> <port> typ ...`).
fn candidate_address(line: &str) -> Option<String> {
    let line = line.trim().strip_prefix("a=").unwrap_or(line.trim());
    let mut fields = line.split_whitespace();
    let first = fields.next()?;
    if !first.starts_with("candidate:") {
        return None;
    }
    fields.nth(3).map(str::to_string).filter(|a| !a.is_empty())
}

/// This host's own addresses (the agent shares the host's network namespace).
fn own_addresses() -> Vec<IpAddr> {
    let mut out = Vec::new();
    let mut head: *mut libc::ifaddrs = std::ptr::null_mut();
    // SAFETY: getifaddrs allocates a list we only read and then free with freeifaddrs.
    if unsafe { libc::getifaddrs(&mut head) } != 0 {
        return out;
    }
    let mut cursor = head;
    while !cursor.is_null() {
        // SAFETY: cursor walks the list getifaddrs returned, until its null terminator.
        let entry = unsafe { &*cursor };
        if !entry.ifa_addr.is_null() {
            // SAFETY: ifa_addr is non-null and its family says which sockaddr it is.
            let family = unsafe { (*entry.ifa_addr).sa_family } as i32;
            if family == libc::AF_INET {
                let sin = unsafe { &*(entry.ifa_addr as *const libc::sockaddr_in) };
                // s_addr is in network byte order, so its in-memory bytes are the address.
                out.push(IpAddr::from(sin.sin_addr.s_addr.to_ne_bytes()));
            } else if family == libc::AF_INET6 {
                let sin6 = unsafe { &*(entry.ifa_addr as *const libc::sockaddr_in6) };
                out.push(IpAddr::from(sin6.sin6_addr.s6_addr));
            }
        }
        cursor = entry.ifa_next;
    }
    // SAFETY: head came from a successful getifaddrs.
    unsafe { libc::freeifaddrs(head) };
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn now() -> SystemTime {
        SystemTime::UNIX_EPOCH + std::time::Duration::from_secs(1_800_000_000)
    }

    fn own() -> Vec<IpAddr> {
        vec!["192.0.2.10".parse().unwrap(), "127.0.0.1".parse().unwrap()]
    }

    #[test]
    fn candidate_lines_yield_their_connection_address() {
        assert_eq!(
            candidate_address("candidate:1 1 udp 2122260223 198.51.100.7 51234 typ host")
                .as_deref(),
            Some("198.51.100.7")
        );
        assert_eq!(
            candidate_address(
                "a=candidate:9 1 UDP 1686052607 203.0.113.9 61000 typ srflx raddr 0.0.0.0 rport 0"
            )
            .as_deref(),
            Some("203.0.113.9")
        );
        assert_eq!(
            candidate_address("candidate:2 1 udp 2122260223 4b2c-11.local 50000 typ host")
                .as_deref(),
            Some("4b2c-11.local")
        );
        assert_eq!(candidate_address(""), None);
        assert_eq!(candidate_address("garbage here"), None);
    }

    #[test]
    fn a_connected_off_host_peer_reached_this_host() {
        let remote = vec!["198.51.100.7".to_string()];
        assert_eq!(
            decide(&remote, false, IceOutcome::Connected, &own(), now()),
            Some(Evidence::Reached {
                at: now(),
                peer: "198.51.100.7".into()
            })
        );
        // Browsers hide host candidates behind mDNS names: still a remote peer.
        let mdns = vec!["4b2c-11.local".to_string()];
        assert!(matches!(
            decide(&mdns, false, IceOutcome::Connected, &own(), now()),
            Some(Evidence::Reached { .. })
        ));
    }

    /// Traffic from this host to itself never crosses its firewall: no evidence.
    #[test]
    fn a_peer_on_this_host_proves_nothing() {
        let local = vec![
            "192.0.2.10".to_string(),
            "127.0.0.1".to_string(),
            "::1".to_string(),
        ];
        assert_eq!(
            decide(&local, false, IceOutcome::Connected, &own(), now()),
            None
        );
        assert_eq!(
            decide(&[], false, IceOutcome::Connected, &own(), now()),
            None
        );
    }

    #[test]
    fn a_failure_after_candidates_arrived_is_blocked() {
        let remote = vec!["198.51.100.7".to_string(), "203.0.113.9".to_string()];
        assert_eq!(
            decide(&remote, false, IceOutcome::Failed, &own(), now()),
            Some(Evidence::Blocked {
                at: now(),
                offered: 2
            })
        );
    }

    /// Amendment 17: a session that ended before ICE finished, or one that connected and
    /// later dropped, is inconclusive for `fail`.
    #[test]
    fn inconclusive_endings_change_nothing() {
        let remote = vec!["198.51.100.7".to_string()];
        assert_eq!(decide(&[], false, IceOutcome::Failed, &own(), now()), None);
        assert_eq!(
            decide(&remote, true, IceOutcome::Failed, &own(), now()),
            None
        );
        assert_eq!(
            decide(&remote, true, IceOutcome::Connected, &own(), now()),
            None
        );
        assert_eq!(
            decide(&remote, false, IceOutcome::Other, &own(), now()),
            None
        );
    }

    #[test]
    fn this_hosts_own_addresses_include_loopback() {
        assert!(own_addresses().iter().any(|ip| ip.is_loopback()));
    }
}
