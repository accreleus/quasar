//! #1199: which host addresses a session's ICE agent gathers candidates on.
//!
//! The agent runs on the host network, so libnice would otherwise gather on every address
//! (Docker bridges, veth pairs, link-local, libvirt bridges): about six candidate frames
//! per address, ~880 per session on a host with ~150 addresses. We tell the ICE agent
//! which local addresses to use *before* it gathers, via GstWebRTCICE's `add-local-ip-address`
//! action signal (GStreamer 1.28.8 + libnice 0.1.24 in the image): once any address is
//! added, libnice stops enumerating the host and gathers on those alone, UDP and TCP, so
//! nothing is gathered and then thrown away. STUN/TURN and mDNS are unaffected (they
//! derive from the host candidates that remain).
//!
//! Defaults: skip loopback, link-local (169.254/16, fe80::/10), and interfaces named like
//! container/VM bridges ([`DEFAULT_EXCLUDED`]). The interface carrying a default route is
//! never excluded by name. `QUASAR_ICE_INTERFACES` (comma-separated globs) replaces the
//! name rule: only matching interfaces gather (`*` = every name). If the result would be
//! empty the filter is dropped and libnice gathers everywhere, logged.

use std::net::IpAddr;

use gstreamer as gst;
use gstreamer::prelude::*;

use crate::session::signaling::PcId;

/// Interface-name globs excluded by default (`*` and `?` only).
const DEFAULT_EXCLUDED: &[&str] = &[
    "docker0", "br-*", "veth*", "virbr*", "cni*", "flannel*", "cali*", "podman*", "lxcbr*",
    "lxdbr*",
];

/// One address on one host interface.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(super) struct IfAddr {
    pub name: String,
    pub ip: IpAddr,
}

/// `QUASAR_ICE_INTERFACES`, parsed. `None` = the default name exclusions.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub(super) struct IceInterfaces(Option<Vec<String>>);

impl IceInterfaces {
    /// Unset or blank (no globs) ⇒ the default exclusions.
    pub fn parse(raw: Option<&str>) -> Self {
        let globs: Vec<String> = raw
            .unwrap_or("")
            .split(',')
            .map(str::trim)
            .filter(|g| !g.is_empty())
            .map(str::to_owned)
            .collect();
        Self((!globs.is_empty()).then_some(globs))
    }

    fn name_allowed(&self, name: &str, on_default_route: bool) -> bool {
        match &self.0 {
            Some(allow) => allow.iter().any(|g| glob_match(g, name)),
            None => on_default_route || !DEFAULT_EXCLUDED.iter().any(|g| glob_match(g, name)),
        }
    }
}

/// `*` (any run) and `?` (one char) glob match; names are short, so the recursion is cheap.
fn glob_match(pat: &str, s: &str) -> bool {
    fn go(p: &[u8], s: &[u8]) -> bool {
        match p.split_first() {
            None => s.is_empty(),
            Some((b'*', rest)) => (0..=s.len()).any(|i| go(rest, &s[i..])),
            Some((b'?', rest)) => !s.is_empty() && go(rest, &s[1..]),
            Some((c, rest)) => s.first() == Some(c) && go(rest, &s[1..]),
        }
    }
    go(pat.as_bytes(), s.as_bytes())
}

/// Addresses ICE can never use or should never offer: loopback, unspecified, multicast,
/// link-local (v4 169.254/16, v6 fe80::/10).
fn address_useless(ip: &IpAddr) -> bool {
    match ip {
        IpAddr::V4(a) => {
            a.is_loopback() || a.is_unspecified() || a.is_multicast() || a.is_link_local()
        }
        IpAddr::V6(a) => {
            a.is_loopback()
                || a.is_unspecified()
                || a.is_multicast()
                || (a.segments()[0] & 0xffc0) == 0xfe80
        }
    }
}

/// The outcome of filtering: the addresses to hand the ICE agent and the ones skipped.
#[derive(Debug, PartialEq, Eq)]
pub(super) struct Selection {
    pub kept: Vec<IfAddr>,
    pub excluded: Vec<IfAddr>,
    /// The filter would have left nothing: `kept` is empty and libnice gathers everywhere.
    pub fell_back: bool,
}

/// Pure core: classify `addrs`. `default_route_ifaces` are exempt from the default name rule.
pub(super) fn select(
    addrs: &[IfAddr],
    policy: &IceInterfaces,
    default_route_ifaces: &[String],
) -> Selection {
    let (mut kept, mut excluded) = (Vec::new(), Vec::new());
    for a in addrs {
        let on_route = default_route_ifaces.contains(&a.name);
        if !address_useless(&a.ip) && policy.name_allowed(&a.name, on_route) {
            if !kept.contains(a) {
                kept.push(a.clone());
            }
        } else {
            excluded.push(a.clone());
        }
    }
    let fell_back = kept.is_empty();
    if fell_back {
        excluded.clear();
    }
    Selection {
        kept,
        excluded,
        fell_back,
    }
}

/// Interfaces with a default route, from the host's route tables. `lo` carries only
/// unreachable/blackhole defaults in the IPv6 table, so it is ignored.
fn default_route_ifaces() -> Vec<String> {
    let mut out = Vec::new();
    if let Ok(t) = std::fs::read_to_string("/proc/net/route") {
        // Iface Destination Gateway Flags RefCnt Use Metric Mask ...
        for l in t.lines().skip(1) {
            let f: Vec<&str> = l.split_whitespace().collect();
            if f.len() > 7 && f[1] == "00000000" && f[7] == "00000000" {
                out.push(f[0].to_owned());
            }
        }
    }
    if let Ok(t) = std::fs::read_to_string("/proc/net/ipv6_route") {
        // dest(32) plen(2) src(32) splen(2) nexthop(32) metric refcnt use flags iface
        for l in t.lines() {
            let f: Vec<&str> = l.split_whitespace().collect();
            if f.len() > 9 && f[0] == "0".repeat(32) && f[1] == "00" && f[9] != "lo" {
                out.push(f[9].to_owned());
            }
        }
    }
    out
}

/// Every address on every UP, non-loopback host interface (`getifaddrs`).
fn host_addrs() -> Vec<IfAddr> {
    let mut out = Vec::new();
    let mut head: *mut libc::ifaddrs = std::ptr::null_mut();
    // SAFETY: getifaddrs fills `head` with a list we only read and then free.
    if unsafe { libc::getifaddrs(&mut head) } != 0 {
        return out;
    }
    let mut cur = head;
    while !cur.is_null() {
        // SAFETY: `cur` is a live node of the list getifaddrs returned.
        let ifa = unsafe { &*cur };
        cur = ifa.ifa_next;
        let up = ifa.ifa_flags & libc::IFF_UP as u32 != 0;
        let lo = ifa.ifa_flags & libc::IFF_LOOPBACK as u32 != 0;
        if !up || lo || ifa.ifa_addr.is_null() {
            continue;
        }
        // SAFETY: ifa_addr is non-null and its sa_family says which sockaddr it is.
        let ip = unsafe {
            match (*ifa.ifa_addr).sa_family as i32 {
                libc::AF_INET => {
                    let sa = &*(ifa.ifa_addr as *const libc::sockaddr_in);
                    IpAddr::from(sa.sin_addr.s_addr.to_ne_bytes())
                }
                libc::AF_INET6 => {
                    let sa = &*(ifa.ifa_addr as *const libc::sockaddr_in6);
                    IpAddr::from(sa.sin6_addr.s6_addr)
                }
                _ => continue,
            }
        };
        // SAFETY: ifa_name is a NUL-terminated C string.
        let name = unsafe { std::ffi::CStr::from_ptr(ifa.ifa_name) }
            .to_string_lossy()
            .into_owned();
        out.push(IfAddr { name, ip });
    }
    // SAFETY: `head` came from a successful getifaddrs and is freed once.
    unsafe { libc::freeifaddrs(head) };
    out
}

fn summary(addrs: &[IfAddr]) -> String {
    let mut names: Vec<&str> = addrs.iter().map(|a| a.name.as_str()).collect();
    names.sort_unstable();
    names.dedup();
    let shown = names.iter().take(8).copied().collect::<Vec<_>>().join(",");
    let more = names.len().saturating_sub(8);
    format!(
        "{} addresses on {} interfaces [{shown}{}]",
        addrs.len(),
        names.len(),
        if more > 0 {
            format!(",+{more} more")
        } else {
            String::new()
        }
    )
}

/// Restrict `webrtc`'s ICE agent to the selected host addresses. Call right after the
/// webrtcbin is built and before any offer/answer (gathering starts at
/// set-local-description). Fails open: any problem leaves libnice gathering everywhere.
/// Logged once per video PC, i.e. once per session (the audio PC applies the same set).
pub(super) fn restrict_ice_gathering(webrtc: &gst::Element, pc: PcId) {
    let raw = std::env::var("QUASAR_ICE_INTERFACES").ok();
    let sel = select(
        &host_addrs(),
        &IceInterfaces::parse(raw.as_deref()),
        &default_route_ifaces(),
    );
    if sel.fell_back {
        if pc == PcId::Video {
            tracing::warn!(
                token = "ice-interfaces-fallback",
                "ICE interface filter (QUASAR_ICE_INTERFACES={:?}) would leave no address — \
                 gathering on every host address",
                raw.as_deref().unwrap_or("")
            );
        }
        return;
    }
    let Some(ice) = webrtc.property::<Option<gst::Object>>("ice-agent") else {
        tracing::warn!(
            token = "ice-interfaces-unavailable",
            "webrtcbin has no ice-agent — gathering on every host address"
        );
        return;
    };
    let mut refused = 0;
    for a in &sel.kept {
        if !ice.emit_by_name::<bool>("add-local-ip-address", &[&a.ip.to_string()]) {
            refused += 1;
        }
    }
    if pc == PcId::Video {
        tracing::info!(
            token = "ice-interfaces",
            "ICE gathers on {}; excluded {} (QUASAR_ICE_INTERFACES={:?}); libnice refused {refused}",
            summary(&sel.kept),
            summary(&sel.excluded),
            raw.as_deref().unwrap_or("")
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn a(name: &str, ip: &str) -> IfAddr {
        IfAddr {
            name: name.into(),
            ip: ip.parse().unwrap(),
        }
    }

    fn names(v: &[IfAddr]) -> Vec<&str> {
        v.iter().map(|x| x.name.as_str()).collect()
    }

    #[test]
    fn default_policy_drops_virtual_loopback_and_link_local() {
        let addrs = [
            a("eth0", "192.0.2.10"),
            a("eth0", "2001:db8::10"),
            a("eth0", "fe80::1"),
            a("eth0", "169.254.7.7"),
            a("lo", "127.0.0.1"),
            a("lo", "::1"),
            a("docker0", "172.17.0.1"),
            a("br-1a2b3c", "172.18.0.1"),
            a("veth9f", "fe80::2"),
            a("virbr0", "192.168.122.1"),
            a("cni0", "10.244.0.1"),
            a("flannel.1", "10.244.1.0"),
            a("br0", "192.0.2.11"),
        ];
        let s = select(&addrs, &IceInterfaces::default(), &[]);
        assert_eq!(names(&s.kept), ["eth0", "eth0", "br0"]);
        assert_eq!(s.excluded.len(), addrs.len() - 3);
        assert!(!s.fell_back);
    }

    #[test]
    fn default_route_interface_survives_a_bridge_name() {
        let addrs = [a("br-lan", "192.0.2.5"), a("br-other", "172.18.0.1")];
        let s = select(&addrs, &IceInterfaces::default(), &["br-lan".into()]);
        assert_eq!(names(&s.kept), ["br-lan"]);
    }

    #[test]
    fn allowlist_replaces_the_name_rule_but_not_the_address_rule() {
        let addrs = [
            a("eth0", "192.0.2.10"),
            a("docker0", "172.17.0.1"),
            a("docker0", "fe80::1"),
        ];
        let p = IceInterfaces::parse(Some(" docker0 , eth1 "));
        let s = select(&addrs, &p, &[]);
        assert_eq!(s.kept, [a("docker0", "172.17.0.1")]);
        let all = select(&addrs, &IceInterfaces::parse(Some("*")), &[]);
        assert_eq!(names(&all.kept), ["eth0", "docker0"]);
    }

    #[test]
    fn a_filter_that_removes_everything_falls_back() {
        let addrs = [a("docker0", "172.17.0.1"), a("lo", "127.0.0.1")];
        let s = select(&addrs, &IceInterfaces::default(), &[]);
        assert!(s.fell_back && s.kept.is_empty() && s.excluded.is_empty());
        let s = select(
            &[a("eth0", "192.0.2.1")],
            &IceInterfaces::parse(Some("nope*")),
            &[],
        );
        assert!(s.fell_back);
    }

    #[test]
    fn duplicate_addresses_are_added_once() {
        let s = select(
            &[a("eth0", "192.0.2.1"), a("eth0", "192.0.2.1")],
            &IceInterfaces::default(),
            &[],
        );
        assert_eq!(s.kept.len(), 1);
    }

    #[test]
    fn knob_parsing() {
        assert_eq!(IceInterfaces::parse(None), IceInterfaces(None));
        assert_eq!(IceInterfaces::parse(Some("")), IceInterfaces(None));
        assert_eq!(IceInterfaces::parse(Some(" , ,")), IceInterfaces(None));
        assert_eq!(
            IceInterfaces::parse(Some("eth*,br0")),
            IceInterfaces(Some(vec!["eth*".into(), "br0".into()]))
        );
    }

    #[test]
    fn globs() {
        assert!(glob_match("br-*", "br-1a2b"));
        assert!(!glob_match("br-*", "br0"));
        assert!(glob_match("eth?", "eth0"));
        assert!(!glob_match("eth?", "eth10"));
        assert!(glob_match("*", ""));
        assert!(glob_match("docker0", "docker0"));
        assert!(!glob_match("docker0", "docker01"));
    }

    /// The real GStreamer + libnice action signal: with one local address added, every
    /// candidate the agent gathers for an offer sits on that address (not on the rest of
    /// the host).
    #[test]
    fn libnice_gathers_only_on_the_added_address() {
        use std::sync::mpsc;
        use std::time::Duration;
        gst::init().unwrap();
        let pipeline = gst::Pipeline::new();
        let webrtc = crate::session::pipeline::webrtc::make_webrtcbin(None).unwrap();
        pipeline.add(&webrtc).unwrap();
        let ice = webrtc
            .property::<Option<gst::Object>>("ice-agent")
            .expect("ice-agent");
        assert!(ice.emit_by_name::<bool>("add-local-ip-address", &[&"127.0.0.1"]));
        let (tx, rx) = mpsc::channel::<String>();
        webrtc.connect("on-ice-candidate", false, move |v| {
            let _ = tx.send(v[2].get::<String>().unwrap());
            None
        });
        pipeline.set_state(gst::State::Playing).unwrap();
        let _chan = webrtc.emit_by_name::<Option<gst::glib::Object>>(
            "create-data-channel",
            &[&"probe", &None::<gst::Structure>],
        );
        let wc = webrtc.clone();
        let offer = gst::Promise::with_change_func(move |reply| {
            let offer = reply
                .unwrap()
                .unwrap()
                .get::<gstreamer_webrtc::WebRTCSessionDescription>("offer")
                .unwrap();
            wc.emit_by_name::<()>("set-local-description", &[&offer, &None::<gst::Promise>]);
        });
        webrtc.emit_by_name::<()>("create-offer", &[&None::<gst::Structure>, &offer]);
        let first = rx
            .recv_timeout(Duration::from_secs(5))
            .expect("no ICE candidate gathered");
        let rest: Vec<String> =
            std::iter::from_fn(|| rx.recv_timeout(Duration::from_millis(500)).ok()).collect();
        pipeline.set_state(gst::State::Null).unwrap();
        let mut seen = 0;
        // An empty string is webrtcbin's end-of-candidates marker.
        for c in std::iter::once(first).chain(rest).filter(|c| !c.is_empty()) {
            seen += 1;
            assert!(
                c.contains(" 127.0.0.1 "),
                "candidate off the allowed address: {c}"
            );
        }
        assert!(seen > 0, "no ICE candidate gathered");
    }
}
