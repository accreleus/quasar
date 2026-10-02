//! `net/http.ProxyFromEnvironment` (`golang.org/x/net/http/httpproxy`) for an https
//! request: which proxy, if any, Go's default transport would route it through. Every
//! verifier request is https (the redirect policy keeps it so), so `HTTP_PROXY` and the
//! CGI guard never apply. Non-ASCII `NO_PROXY` entries are not IDNA-converted, so they
//! never match; the adapter then refuses rather than fetching around the proxy.

use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};

use super::url::{parse, Url};

/// The proxy variables, read once like Go's `envProxyFunc`.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub(crate) struct ProxyEnv {
    pub(crate) https_proxy: String,
    pub(crate) no_proxy: String,
}

impl ProxyEnv {
    pub(crate) fn from_process() -> Self {
        let any = |names: [&str; 2]| {
            names
                .iter()
                .find_map(|n| {
                    std::env::var_os(n)
                        .map(|v| v.to_string_lossy().into_owned())
                        .filter(|v| !v.is_empty())
                })
                .unwrap_or_default()
        };
        ProxyEnv {
            https_proxy: any(["HTTPS_PROXY", "https_proxy"]),
            no_proxy: any(["NO_PROXY", "no_proxy"]),
        }
    }

    /// The proxy Go would use for this https request, or `None` to connect directly.
    pub(crate) fn proxy_for(&self, request: &Url) -> Option<Url> {
        let proxy = parse_proxy(&self.https_proxy)?;
        let (host, port) = request.host_port();
        let host = String::from_utf8_lossy(&host).into_owned();
        let port = if port.is_empty() {
            "443".to_owned()
        } else {
            String::from_utf8_lossy(&port).into_owned()
        };
        use_proxy(&self.no_proxy, &host, &port).then_some(proxy)
    }
}

/// `parseProxy`: a bare `host[:port]` gets `http://`; an unparsable value is no proxy.
fn parse_proxy(raw: &str) -> Option<Url> {
    if raw.is_empty() {
        return None;
    }
    let first = parse(raw);
    match &first {
        Ok(u) if !u.scheme.is_empty() && !u.host.is_empty() => return first.ok(),
        _ => {}
    }
    if let Ok(u) = parse(&format!("http://{raw}")) {
        return Some(u);
    }
    first.ok()
}

/// `useProxy`.
fn use_proxy(no_proxy: &str, host: &str, port: &str) -> bool {
    if host == "localhost" {
        return false;
    }
    let ip = parse_addr_with_zone(host);
    if ip.is_some_and(is_loopback) {
        return false;
    }
    let addr = host.trim().to_ascii_lowercase();
    let matchers = no_proxy_matchers(no_proxy);
    !matchers.iter().any(|m| m.matches(&addr, port, ip))
}

enum Matcher {
    All,
    Cidr(IpAddr, u8),
    Ip(IpAddr, String),
    Domain {
        host: String,
        port: String,
        match_host: bool,
    },
}

impl Matcher {
    fn matches(&self, host: &str, port: &str, ip: Option<IpAddr>) -> bool {
        match self {
            Matcher::All => true,
            Matcher::Cidr(net, bits) => ip.is_some_and(|ip| cidr_contains(*net, *bits, ip)),
            Matcher::Ip(m, p) => {
                ip.is_some_and(|ip| ip_equal(*m, ip) && (p.is_empty() || p == port))
            }
            Matcher::Domain {
                host: m,
                port: p,
                match_host,
            } => {
                ip.is_none()
                    && (host.ends_with(m.as_str()) || (*match_host && host == &m[1..]))
                    && (p.is_empty() || p == port)
            }
        }
    }
}

/// `config.init`'s `NO_PROXY` parsing.
fn no_proxy_matchers(no_proxy: &str) -> Vec<Matcher> {
    let mut out = Vec::new();
    for p in no_proxy.split(',') {
        let p = p.trim().to_lowercase();
        if p.is_empty() {
            continue;
        }
        if p == "*" {
            return vec![Matcher::All];
        }
        if let Some((net, bits)) = parse_cidr(&p) {
            out.push(Matcher::Cidr(net, bits));
            continue;
        }
        let (mut phost, pport) = match split_host_port(&p) {
            Some((h, port)) => {
                if h.is_empty() {
                    continue;
                }
                let h = if h.starts_with('[') && h.ends_with(']') {
                    h[1..h.len() - 1].to_owned()
                } else {
                    h
                };
                (h, port)
            }
            None => (p.clone(), String::new()),
        };
        if let Ok(ip) = phost.parse::<IpAddr>() {
            out.push(Matcher::Ip(ip, pport));
            continue;
        }
        if phost.is_empty() {
            continue;
        }
        if phost.starts_with("*.") {
            phost = phost[1..].to_owned();
        }
        let mut match_host = false;
        if !phost.starts_with('.') {
            match_host = true;
            phost = format!(".{phost}");
        }
        out.push(Matcher::Domain {
            host: phost,
            port: pport,
            match_host,
        });
    }
    out
}

/// `netip.ParseAddr`, which accepts an IPv6 zone.
fn parse_addr_with_zone(s: &str) -> Option<IpAddr> {
    if let Ok(ip) = s.parse::<IpAddr>() {
        return Some(ip);
    }
    let (addr, zone) = s.split_once('%')?;
    if zone.is_empty() {
        return None;
    }
    addr.parse::<Ipv6Addr>().ok().map(IpAddr::V6)
}

/// `net.IP.IsLoopback`, which sees an IPv4-mapped address as IPv4.
fn is_loopback(ip: IpAddr) -> bool {
    match as_v4(ip) {
        Some(v4) => v4.octets()[0] == 127,
        None => ip == IpAddr::V6(Ipv6Addr::LOCALHOST),
    }
}

fn as_v4(ip: IpAddr) -> Option<Ipv4Addr> {
    match ip {
        IpAddr::V4(v4) => Some(v4),
        IpAddr::V6(v6) => v6.to_ipv4_mapped(),
    }
}

/// `net.IP.Equal`.
fn ip_equal(a: IpAddr, b: IpAddr) -> bool {
    match (as_v4(a), as_v4(b)) {
        (Some(a), Some(b)) => a == b,
        (None, None) => a == b,
        _ => false,
    }
}

/// `net.ParseCIDR`: `addr/bits`, no zone, decimal bits within the family's width.
fn parse_cidr(s: &str) -> Option<(IpAddr, u8)> {
    let (addr, mask) = s.split_once('/')?;
    let ip = addr.parse::<IpAddr>().ok()?;
    if mask.is_empty() || !mask.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    let width = if ip.is_ipv4() { 32 } else { 128 };
    let bits: u32 = mask.parse().ok()?;
    (bits <= width).then_some((ip, bits as u8))
}

/// `net.IPNet.Contains`.
fn cidr_contains(net: IpAddr, bits: u8, ip: IpAddr) -> bool {
    let masked = |a: u128, width: u32| {
        if bits == 0 {
            0
        } else {
            a >> (width - bits as u32)
        }
    };
    match (net, ip) {
        (IpAddr::V4(n), ip) => match as_v4(ip) {
            Some(v4) => masked(u32::from(n) as u128, 32) == masked(u32::from(v4) as u128, 32),
            None => false,
        },
        (IpAddr::V6(n), IpAddr::V6(v6)) if v6.to_ipv4_mapped().is_none() => {
            masked(u128::from(n), 128) == masked(u128::from(v6), 128)
        }
        _ => false,
    }
}

/// `net.SplitHostPort`.
fn split_host_port(hostport: &str) -> Option<(String, String)> {
    let i = hostport.rfind(':')?;
    let (host, j, k) = if hostport.starts_with('[') {
        let end = hostport.find(']')?;
        if end + 1 != i {
            return None;
        }
        (hostport[1..end].to_owned(), 1, end + 1)
    } else {
        let host = &hostport[..i];
        if host.contains(':') {
            return None;
        }
        (host.to_owned(), 0, 0)
    };
    if hostport[j..].contains('[') || hostport[k..].contains(']') {
        return None;
    }
    Some((host, hostport[i + 1..].to_owned()))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn decide(https_proxy: &str, no_proxy: &str, url: &str) -> Option<String> {
        let env = ProxyEnv {
            https_proxy: https_proxy.into(),
            no_proxy: no_proxy.into(),
        };
        env.proxy_for(&parse(url).unwrap())
            .map(|p| format!("{}://{}", p.scheme, String::from_utf8_lossy(&p.host)))
    }

    #[test]
    fn matches_go_proxy_from_environment() {
        // Each row observed from Go 1.25 http.ProxyFromEnvironment in a fresh process.
        let p = Some("http://proxy.example:3128".to_string());
        let direct = None;
        assert_eq!(decide("", "", "https://mirror.example/x"), direct);
        assert_eq!(
            decide("proxy.example:3128", "", "https://mirror.example/x"),
            p
        );
        assert_eq!(
            decide("http://proxy.example:3128", "", "https://mirror.example/x"),
            p
        );
        assert_eq!(
            decide("proxy.example:3128", "", "https://localhost/x"),
            direct
        );
        assert_eq!(decide("proxy.example:3128", "", "https://LOCALHOST/x"), p);
        assert_eq!(
            decide("proxy.example:3128", "", "https://127.0.0.2/x"),
            direct
        );
        assert_eq!(decide("proxy.example:3128", "", "https://[::1]/x"), direct);
        assert_eq!(
            decide("proxy.example:3128", "", "https://[::ffff:127.0.0.1]/x"),
            direct
        );
        assert_eq!(
            decide(
                "proxy.example:3128",
                "mirror.example",
                "https://mirror.example/x"
            ),
            direct
        );
        assert_eq!(
            decide(
                "proxy.example:3128",
                "mirror.example",
                "https://a.mirror.example/x"
            ),
            direct
        );
        assert_eq!(
            decide(
                "proxy.example:3128",
                ".mirror.example",
                "https://mirror.example/x"
            ),
            p
        );
        assert_eq!(
            decide(
                "proxy.example:3128",
                "*.mirror.example",
                "https://a.mirror.example/x"
            ),
            direct
        );
        assert_eq!(
            decide(
                "proxy.example:3128",
                "mirror.example:443",
                "https://mirror.example/x"
            ),
            direct
        );
        assert_eq!(
            decide(
                "proxy.example:3128",
                "mirror.example:8443",
                "https://mirror.example/x"
            ),
            p
        );
        assert_eq!(
            decide(
                "proxy.example:3128",
                " MIRROR.example ",
                "https://mirror.example/x"
            ),
            direct
        );
        assert_eq!(
            decide("proxy.example:3128", "*", "https://mirror.example/x"),
            direct
        );
        assert_eq!(
            decide("proxy.example:3128", "192.0.2.0/24", "https://192.0.2.7/x"),
            direct
        );
        assert_eq!(
            decide("proxy.example:3128", "192.0.2.0/24", "https://192.0.3.7/x"),
            p
        );
        assert_eq!(
            decide("proxy.example:3128", "192.0.2.7", "https://192.0.2.7/x"),
            direct
        );
        assert_eq!(
            decide("proxy.example:3128", "192.0.2.7:443", "https://192.0.2.7/x"),
            direct
        );
        assert_eq!(
            decide(
                "proxy.example:3128",
                "[2001:db8::1]:443",
                "https://[2001:db8::1]/x"
            ),
            direct
        );
        assert_eq!(
            decide(
                "proxy.example:3128",
                "2001:db8::/32",
                "https://[2001:db8::1]/x"
            ),
            direct
        );
        assert_eq!(
            decide("proxy.example:3128", "example", "https://mirror.example/x"),
            direct
        );
        assert_eq!(
            decide("proxy.example:3128", "ample", "https://mirror.example/x"),
            p
        );
        assert_eq!(
            decide(
                "proxy.example:3128",
                "192.0.2.7",
                "https://mirror.example/x"
            ),
            p
        );
        assert_eq!(
            decide(
                "socks5://proxy.example:1080",
                "",
                "https://mirror.example/x"
            ),
            Some("socks5://proxy.example:1080".into())
        );
        assert_eq!(
            decide("https://proxy.example", "", "https://mirror.example/x"),
            Some("https://proxy.example".into())
        );
    }
}
