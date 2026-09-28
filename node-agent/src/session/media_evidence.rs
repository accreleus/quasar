//! Evidence that the WebRTC media path reaches this host (RH-07 #403; `agent-api.md`
//! amendment 17, `media_reachability`).
//!
//! Reading the host's firewall rules needed `NET_ADMIN` in the host's network namespace,
//! which a rootless engine cannot grant, and even then it was a proxy. The evidence here is
//! real traffic, read from the session's own `get-stats` when its ICE state settles:
//!
//! - **Reached**: ICE connected and the selected pair's remote address is off this host.
//!   Behind a stateful firewall the peer's checks may be let in as replies to the host's
//!   own, so this says the peer reached the host, never that the port range is open.
//! - **Blocked**: ICE failed before ever connecting, the peer offered at least one off-host
//!   candidate, and no peer-reflexive remote candidate exists (one would mean a check from
//!   the peer did arrive).
//! - Anything else is inconclusive and changes nothing: a session that ended first, one
//!   that connected and later dropped, a peer on this host or on one of its container
//!   bridges, a remote address still hidden behind an mDNS name.

use gstreamer::prelude::*;
use std::net::IpAddr;
use std::sync::Mutex;
use std::time::SystemTime;

/// The latest definitive observation, kept for the life of the agent process.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Evidence {
    /// A remote peer reached this host over WebRTC; `peer` is its address on the selected
    /// candidate pair.
    Reached { at: SystemTime, peer: String },
    /// A remote peer offered `offered` candidates and none of its checks arrived.
    Blocked { at: SystemTime, offered: usize },
}

/// What one ICE state change means here.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum IceOutcome {
    Connected,
    Failed,
}

/// One remote ICE candidate, as `get-stats` reports it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Candidate {
    pub address: String,
    /// `host`, `srflx`, `prflx` or `relay`.
    pub kind: String,
}

/// The remote side of one peer connection's ICE, from `get-stats`.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct IceStats {
    /// The remote candidate of each selected pair (one per transport).
    pub selected: Vec<Candidate>,
    /// Every remote candidate the ICE agent knows.
    pub remote: Vec<Candidate>,
}

/// Addresses whose traffic never crossed this host's firewall: its own, and the subnets of
/// its container bridges.
#[derive(Debug, Clone, Default)]
pub struct LocalNet {
    pub own: Vec<IpAddr>,
    pub bridges: Vec<(IpAddr, u8)>,
}

static LATEST: Mutex<Option<Evidence>> = Mutex::new(None);

/// The latest definitive observation, if any session has produced one.
pub fn latest() -> Option<Evidence> {
    LATEST.lock().ok().and_then(|latest| latest.clone())
}

/// Record what one settled ICE state proves, if anything.
pub fn record(stats: &IceStats, connected_before: bool, outcome: IceOutcome) {
    let Some(evidence) = decide(
        stats,
        connected_before,
        outcome,
        &LocalNet::live(),
        SystemTime::now(),
    ) else {
        return;
    };
    tracing::info!(token = "media-reachability-observed", evidence = ?evidence, "media path evidence");
    if let Ok(mut latest) = LATEST.lock() {
        *latest = Some(evidence);
    }
}

/// The rule, pure. `connected_before`: this peer connection already connected once, so a
/// later failure (after an ICE restart too) is a dropped session, not a blocked path.
pub fn decide(
    stats: &IceStats,
    connected_before: bool,
    outcome: IceOutcome,
    local: &LocalNet,
    now: SystemTime,
) -> Option<Evidence> {
    if connected_before {
        return None;
    }
    match outcome {
        IceOutcome::Connected => stats
            .selected
            .iter()
            .find(|c| local.off_host(&c.address))
            .map(|c| Evidence::Reached {
                at: now,
                peer: c.address.clone(),
            }),
        IceOutcome::Failed => {
            let a_check_arrived = stats.remote.iter().any(|c| c.kind == "prflx");
            let offered_off_host = stats.remote.iter().any(|c| local.off_host(&c.address));
            (!a_check_arrived && offered_off_host).then_some(Evidence::Blocked {
                at: now,
                offered: stats.remote.len(),
            })
        }
    }
}

impl LocalNet {
    /// Could traffic from `address` have crossed this host's firewall? Not from loopback,
    /// this host's own addresses or its container bridges; and an unresolved name (a
    /// browser's mDNS `.local`) cannot be placed, so it is not counted either way.
    fn off_host(&self, address: &str) -> bool {
        let Ok(ip) = address.parse::<IpAddr>() else {
            return false;
        };
        !ip.is_loopback()
            && !ip.is_unspecified()
            && !self.own.contains(&ip)
            && !self
                .bridges
                .iter()
                .any(|(net, prefix)| same_subnet(ip, *net, *prefix))
    }

    /// This host's addresses and container-bridge subnets (the agent shares the host's
    /// network namespace).
    pub fn live() -> Self {
        let mut out = LocalNet::default();
        for (name, ip, prefix) in interfaces() {
            out.own.push(ip);
            if is_bridge(&name) {
                out.bridges.push((ip, prefix));
            }
        }
        out
    }
}

/// Engine and hypervisor bridges, whose peers are containers or VMs on this host.
fn is_bridge(name: &str) -> bool {
    [
        "docker", "br-", "podman", "cni", "virbr", "veth", "lxcbr", "cali", "flannel",
    ]
    .iter()
    .any(|p| name.starts_with(p))
}

fn same_subnet(a: IpAddr, b: IpAddr, prefix: u8) -> bool {
    match (a, b) {
        (IpAddr::V4(a), IpAddr::V4(b)) => {
            let mask = u32::MAX
                .checked_shl(32 - u32::from(prefix.min(32)))
                .unwrap_or(0);
            u32::from(a) & mask == u32::from(b) & mask
        }
        (IpAddr::V6(a), IpAddr::V6(b)) => {
            let mask = u128::MAX
                .checked_shl(128 - u32::from(prefix.min(128)))
                .unwrap_or(0);
            u128::from(a) & mask == u128::from(b) & mask
        }
        _ => false,
    }
}

/// Read a webrtcbin `get-stats` reply into the remote side of its ICE.
pub fn ice_stats(reply: &gstreamer::StructureRef) -> IceStats {
    let mut remote = std::collections::HashMap::new();
    let mut selected_ids = Vec::new();
    for (id, value) in reply.iter() {
        let Ok(stat) = value.get::<gstreamer::Structure>() else {
            continue;
        };
        let kind = stat
            .value("type")
            .ok()
            .and_then(|v| v.serialize().ok())
            .map(|s| s.to_string());
        match kind.as_deref() {
            Some("remote-candidate") => {
                if let Ok(address) = stat.get::<String>("address") {
                    let kind = stat.get::<String>("candidate-type").unwrap_or_default();
                    remote.insert(id.to_string(), Candidate { address, kind });
                }
            }
            Some("candidate-pair") => {
                if let Ok(remote_id) = stat.get::<String>("remote-candidate-id") {
                    selected_ids.push(remote_id);
                }
            }
            _ => {}
        }
    }
    let selected = selected_ids
        .iter()
        .filter_map(|id| remote.get(id).cloned())
        .collect();
    let mut remote: Vec<Candidate> = remote.into_values().collect();
    remote.sort_by(|a, b| a.address.cmp(&b.address));
    IceStats { selected, remote }
}

/// `(interface, address, prefix length)` for every configured address.
fn interfaces() -> Vec<(String, IpAddr, u8)> {
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
        cursor = entry.ifa_next;
        if entry.ifa_addr.is_null() {
            continue;
        }
        // SAFETY: ifa_name is a NUL-terminated string owned by the list.
        let name = unsafe { std::ffi::CStr::from_ptr(entry.ifa_name) }
            .to_string_lossy()
            .into_owned();
        // SAFETY: ifa_addr is non-null and its family says which sockaddr it is; the
        // netmask, when present, has the same family.
        let family = unsafe { (*entry.ifa_addr).sa_family } as i32;
        let (ip, prefix) = if family == libc::AF_INET {
            let sin = unsafe { &*(entry.ifa_addr as *const libc::sockaddr_in) };
            // s_addr is in network byte order, so its in-memory bytes are the address.
            let ip = IpAddr::from(sin.sin_addr.s_addr.to_ne_bytes());
            let prefix = (!entry.ifa_netmask.is_null())
                .then(|| unsafe { &*(entry.ifa_netmask as *const libc::sockaddr_in) })
                .map_or(32, |m| {
                    u32::from_be_bytes(m.sin_addr.s_addr.to_ne_bytes()).count_ones()
                });
            (ip, prefix as u8)
        } else if family == libc::AF_INET6 {
            let sin6 = unsafe { &*(entry.ifa_addr as *const libc::sockaddr_in6) };
            let prefix = (!entry.ifa_netmask.is_null())
                .then(|| unsafe { &*(entry.ifa_netmask as *const libc::sockaddr_in6) })
                .map_or(128, |m| {
                    u128::from_be_bytes(m.sin6_addr.s6_addr).count_ones()
                });
            (IpAddr::from(sin6.sin6_addr.s6_addr), prefix as u8)
        } else {
            continue;
        };
        out.push((name, ip, prefix));
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

    fn local() -> LocalNet {
        LocalNet {
            own: vec!["192.0.2.10".parse().unwrap(), "172.17.0.1".parse().unwrap()],
            bridges: vec![("172.17.0.1".parse().unwrap(), 16)],
        }
    }

    fn c(address: &str, kind: &str) -> Candidate {
        Candidate {
            address: address.into(),
            kind: kind.into(),
        }
    }

    #[test]
    fn a_connected_off_host_selected_pair_reached_this_host() {
        let stats = IceStats {
            selected: vec![c("198.51.100.7", "srflx")],
            remote: vec![c("198.51.100.7", "srflx")],
        };
        assert_eq!(
            decide(&stats, false, IceOutcome::Connected, &local(), now()),
            Some(Evidence::Reached {
                at: now(),
                peer: "198.51.100.7".into()
            })
        );
    }

    /// Review finding: a same-host browser can offer an off-host candidate (a reflexive
    /// or `.local` one) while the pair actually selected is local. Only the selected pair
    /// counts.
    #[test]
    fn an_off_host_offer_with_a_local_selected_pair_proves_nothing() {
        let stats = IceStats {
            selected: vec![c("192.0.2.10", "host")],
            remote: vec![c("192.0.2.10", "host"), c("198.51.100.7", "srflx")],
        };
        assert_eq!(
            decide(&stats, false, IceOutcome::Connected, &local(), now()),
            None
        );
    }

    #[test]
    fn peers_on_this_host_or_its_bridges_or_unresolved_prove_nothing() {
        for address in [
            "127.0.0.1",
            "::1",
            "192.0.2.10",
            "172.17.0.5",
            "4b2c-11.local",
        ] {
            let stats = IceStats {
                selected: vec![c(address, "host")],
                remote: vec![c(address, "host")],
            };
            assert_eq!(
                decide(&stats, false, IceOutcome::Connected, &local(), now()),
                None,
                "{address}"
            );
        }
    }

    #[test]
    fn a_failure_with_off_host_candidates_and_no_arrived_check_is_blocked() {
        let stats = IceStats {
            selected: vec![],
            remote: vec![c("198.51.100.7", "host"), c("203.0.113.9", "srflx")],
        };
        assert_eq!(
            decide(&stats, false, IceOutcome::Failed, &local(), now()),
            Some(Evidence::Blocked {
                at: now(),
                offered: 2
            })
        );
    }

    /// A peer-reflexive remote candidate exists only because a check from the peer
    /// arrived, so the path was not blocked.
    #[test]
    fn a_failure_after_a_check_arrived_is_inconclusive() {
        let stats = IceStats {
            selected: vec![],
            remote: vec![c("198.51.100.7", "host"), c("198.51.100.8", "prflx")],
        };
        assert_eq!(
            decide(&stats, false, IceOutcome::Failed, &local(), now()),
            None
        );
    }

    /// Amendment 17: a session that ended before ICE finished, one that connected and
    /// later dropped, or one whose peer offered nothing off this host is inconclusive.
    #[test]
    fn inconclusive_endings_change_nothing() {
        let off = IceStats {
            selected: vec![c("198.51.100.7", "host")],
            remote: vec![c("198.51.100.7", "host")],
        };
        assert_eq!(
            decide(&off, true, IceOutcome::Failed, &local(), now()),
            None
        );
        assert_eq!(
            decide(&off, true, IceOutcome::Connected, &local(), now()),
            None
        );
        let none = IceStats::default();
        assert_eq!(
            decide(&none, false, IceOutcome::Failed, &local(), now()),
            None
        );
        let local_only = IceStats {
            selected: vec![],
            remote: vec![c("172.17.0.5", "host")],
        };
        assert_eq!(
            decide(&local_only, false, IceOutcome::Failed, &local(), now()),
            None
        );
    }

    #[test]
    fn subnets_match_by_prefix() {
        let a = |s: &str| s.parse::<IpAddr>().unwrap();
        assert!(same_subnet(a("172.17.5.4"), a("172.17.0.1"), 16));
        assert!(!same_subnet(a("172.18.0.4"), a("172.17.0.1"), 16));
        assert!(same_subnet(a("fd00::5"), a("fd00::1"), 64));
        assert!(!same_subnet(a("fd01::5"), a("fd00::1"), 64));
        assert!(!same_subnet(a("10.0.0.1"), a("fd00::1"), 8));
    }

    #[test]
    fn get_stats_is_read_through_the_selected_pair() {
        use gstreamer_webrtc::WebRTCStatsType as T;
        gstreamer::init().unwrap();
        let reply = gstreamer::Structure::builder("application/x-webrtc-stats")
            .field(
                "ice-candidate-pair_1",
                gstreamer::Structure::builder("candidate-pair")
                    .field("type", T::CandidatePair)
                    .field("remote-candidate-id", "ice-candidate-remote_b")
                    .build(),
            )
            .field(
                "ice-candidate-remote_a",
                gstreamer::Structure::builder("remote-candidate")
                    .field("type", T::RemoteCandidate)
                    .field("address", "192.0.2.10")
                    .field("candidate-type", "host")
                    .build(),
            )
            .field(
                "ice-candidate-remote_b",
                gstreamer::Structure::builder("remote-candidate")
                    .field("type", T::RemoteCandidate)
                    .field("address", "198.51.100.7")
                    .field("candidate-type", "prflx")
                    .build(),
            )
            .build();
        let stats = ice_stats(&reply);
        assert_eq!(stats.selected, vec![c("198.51.100.7", "prflx")]);
        assert_eq!(
            stats.remote,
            vec![c("192.0.2.10", "host"), c("198.51.100.7", "prflx")]
        );
    }

    #[test]
    fn this_hosts_own_addresses_include_loopback() {
        assert!(LocalNet::live().own.iter().any(|ip| ip.is_loopback()));
    }
}
