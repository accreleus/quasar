//! T1–T3: the only URLs the agent fetches, the redirect allowlist and the connect-time address
//! check. semantics: control-api.md §Compatibility tools, "Trust rules".

use std::io::{Read, Write};
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use sha2::{Digest, Sha512};
use ureq::unversioned::resolver::{DefaultResolver, ResolvedSocketAddrs, Resolver};
use ureq::unversioned::transport::{Connector, NextTimeout, RustlsConnector, TcpConnector};

use super::Fail;

const PROJECT: &str = "/GloriousEggroll/proton-ge-custom/releases/download/";
const ASSET_HOSTS: [&str; 2] = [
    "release-assets.githubusercontent.com",
    "objects.githubusercontent.com",
];
pub const MAX_REDIRECTS: usize = 5;

/// `^GE-Proton[0-9]{1,3}-[0-9]{1,4}$`
pub fn valid_tag(tag: &str) -> bool {
    let Some((major, minor)) = tag
        .strip_prefix("GE-Proton")
        .and_then(|r| r.split_once('-'))
    else {
        return false;
    };
    let digits =
        |s: &str, max: usize| (1..=max).contains(&s.len()) && s.bytes().all(|c| c.is_ascii_digit());
    digits(major, 3) && digits(minor, 4)
}

/// The asset name for `asset` (`x86_64` or `plain`), `None` for anything else (T1).
pub fn asset_name(tag: &str, asset: &str) -> Option<String> {
    match asset {
        "x86_64" => Some(format!("{tag}-x86_64.tar.gz")),
        "plain" => Some(format!("{tag}.tar.gz")),
        _ => None,
    }
}

pub fn asset_url(tag: &str, name: &str) -> String {
    format!("https://github.com{PROJECT}{tag}/{name}")
}

/// Checks a raw `Location` value, before any URL library sees it (a normalizing parser would
/// clean `..` or `%2e` into a match). `https`, port 443, no userinfo, an allowlisted host, and on
/// github.com only the run's own release path.
pub fn redirect_allowed(raw: &str, tag: &str) -> Result<(), String> {
    if raw
        .bytes()
        .any(|c| c.is_ascii_control() || c == b' ' || c == b'\\' || c == b'#')
    {
        return Err("Location holds a control character, space, backslash or fragment".into());
    }
    let rest = raw
        .strip_prefix("https://")
        .ok_or("Location is not an absolute https URL")?;
    let end = rest.find(['/', '?']).unwrap_or(rest.len());
    let (authority, tail) = rest.split_at(end);
    if authority.contains('@') {
        return Err("Location carries userinfo".into());
    }
    let host = match authority.split_once(':') {
        Some((h, "443")) => h,
        Some(_) => return Err("Location names a port other than 443".into()),
        None => authority,
    };
    if ASSET_HOSTS.contains(&host) {
        return Ok(());
    }
    if host != "github.com" {
        return Err(format!("Location host {host:?} is not allowed"));
    }
    let path = tail.split('?').next().unwrap_or("");
    let lower = path.to_ascii_lowercase();
    if lower.contains("%2e") || lower.contains("%2f") {
        return Err("Location path holds an encoded dot or slash".into());
    }
    let segments: Vec<&str> = path.strip_prefix('/').unwrap_or("").split('/').collect();
    if !path.starts_with('/')
        || segments
            .iter()
            .any(|s| s.is_empty() || *s == "." || *s == "..")
    {
        return Err("Location path has an empty, `.` or `..` segment".into());
    }
    if !path.starts_with(&format!("{PROJECT}{tag}/")) {
        return Err("Location path is outside the run's release".into());
    }
    Ok(())
}

/// Loopback, private, link-local, CGNAT, NAT64 and every other special-purpose range (T2).
pub fn address_allowed(ip: IpAddr) -> bool {
    match ip {
        IpAddr::V4(v4) => v4_allowed(v4),
        IpAddr::V6(v6) => match v6.to_ipv4_mapped() {
            Some(v4) => v4_allowed(v4),
            None => v6_allowed(v6),
        },
    }
}

fn v4_allowed(ip: Ipv4Addr) -> bool {
    let [a, b, c, _] = ip.octets();
    !(a == 0
        || a == 10
        || a == 127
        || (a == 100 && (64..128).contains(&b))
        || (a == 169 && b == 254)
        || (a == 172 && (16..32).contains(&b))
        || (a == 192 && b == 0 && (c == 0 || c == 2))
        || (a == 192 && b == 88 && c == 99)
        || (a == 192 && b == 168)
        || (a == 198 && (b == 18 || b == 19))
        || (a == 198 && b == 51 && c == 100)
        || (a == 203 && b == 0 && c == 113)
        || a >= 224)
}

fn v6_allowed(ip: Ipv6Addr) -> bool {
    let s = ip.segments();
    // Only global unicast 2000::/3 is reachable, minus its special-purpose blocks.
    (s[0] & 0xe000) == 0x2000
        && !(s[0] == 0x2001 && s[1] < 0x0200)
        && !(s[0] == 0x2001 && s[1] == 0x0db8)
        && s[0] != 0x2002
        && !(s[0] == 0x3fff && s[1] < 0x1000)
        && !(s[0] == 0x0064 && s[1] == 0xff9b)
}

/// One response, no redirect followed.
pub struct Hop {
    pub status: u16,
    pub location: Option<String>,
    pub content_encoding: Option<String>,
    pub body: Box<dyn Read + Send>,
}

pub enum HopError {
    /// The address check refused every address the name resolved to.
    AddressRefused(String),
    Other(String),
}

/// A GET with no redirect followed. Production is [`HttpsSource`]; tests serve canned hops.
pub trait Source: Send + Sync {
    fn get(&self, url: &str) -> Result<Hop, HopError>;
}

/// Downloads the asset into `dest`, following only allowed redirects, reading at most
/// `size + 1` bytes and hashing as it writes. Returns the lowercase hex SHA-512.
pub fn download(
    src: &dyn Source,
    tag: &str,
    name: &str,
    size: u64,
    dest: &mut dyn Write,
    aborted: &dyn Fn() -> bool,
) -> Result<String, Fail> {
    let mut url = asset_url(tag, name);
    let mut hops = 0;
    let hop = loop {
        let hop = src.get(&url).map_err(|e| match e {
            HopError::AddressRefused(d) => Fail::new("redirect_refused", d),
            HopError::Other(d) => Fail::new("download_failed", d),
        })?;
        match hop.status {
            200 => break hop,
            301 | 302 | 303 | 307 | 308 => {
                hops += 1;
                if hops > MAX_REDIRECTS {
                    return Err(Fail::new("redirect_refused", "more than 5 redirects"));
                }
                let loc = hop
                    .location
                    .ok_or_else(|| Fail::new("redirect_refused", "a redirect with no Location"))?;
                redirect_allowed(&loc, tag).map_err(|d| Fail::new("redirect_refused", d))?;
                url = loc;
            }
            s => return Err(Fail::new("download_failed", format!("HTTP {s}"))),
        }
    };
    if hop
        .content_encoding
        .as_deref()
        .is_some_and(|e| !e.eq_ignore_ascii_case("identity"))
    {
        return Err(Fail::new("download_failed", "the asset was sent encoded"));
    }
    let mut body = hop.body.take(size + 1);
    let mut hasher = Sha512::new();
    let (mut read, mut buf) = (0u64, vec![0u8; 1 << 16]);
    loop {
        if aborted() {
            return Err(Fail::new("io_error", "the run was aborted"));
        }
        let n = body
            .read(&mut buf)
            .map_err(|e| Fail::new("download_failed", e.to_string()))?;
        if n == 0 {
            break;
        }
        read += n as u64;
        hasher.update(&buf[..n]);
        dest.write_all(&buf[..n])
            .map_err(|e| Fail::new("io_error", format!("write download: {e}")))?;
    }
    if read != size {
        return Err(Fail::new(
            "size_mismatch",
            format!("read {read} bytes, expected {size}"),
        ));
    }
    Ok(crate::artifact::hex(&hasher.finalize()))
}

/// Resolves, then drops every address [`address_allowed`] refuses. The TCP connector connects only
/// to what this returns, so the address checked is the address connected to.
#[derive(Debug)]
struct CheckedResolver {
    inner: DefaultResolver,
    refused: Arc<Mutex<Option<String>>>,
}

impl Resolver for CheckedResolver {
    fn resolve(
        &self,
        uri: &ureq::http::Uri,
        config: &ureq::config::Config,
        timeout: NextTimeout,
    ) -> Result<ResolvedSocketAddrs, ureq::Error> {
        let all = self.inner.resolve(uri, config, timeout)?;
        let mut out = self.empty();
        for a in all.iter().filter(|a| address_allowed(a.ip())) {
            out.push(SocketAddr::new(a.ip(), a.port()));
        }
        if out.is_empty() {
            let shown: Vec<String> = all.iter().map(|a| a.ip().to_string()).collect();
            let msg = format!(
                "{} resolves only to refused addresses ({})",
                uri.host().unwrap_or("?"),
                shown.join(", ")
            );
            *self.refused.lock().unwrap_or_else(|e| e.into_inner()) = Some(msg);
            return Err(ureq::Error::HostNotFound);
        }
        Ok(out)
    }
}

/// The real source: https only, WebPKI roots, no redirect followed, no proxy (the connector chain
/// has no proxy connector, and the config sets none), and the address check above.
pub struct HttpsSource {
    agent: ureq::Agent,
    refused: Arc<Mutex<Option<String>>>,
}

impl HttpsSource {
    pub fn new(body_timeout: Duration) -> Self {
        let refused = Arc::new(Mutex::new(None));
        let config = ureq::Agent::config_builder()
            .https_only(true)
            .max_redirects(0)
            .proxy(None)
            .http_status_as_error(false)
            .user_agent("quasar-node-agent")
            .timeout_connect(Some(Duration::from_secs(30)))
            .timeout_recv_response(Some(Duration::from_secs(60)))
            .timeout_recv_body(Some(body_timeout))
            .build();
        let agent = ureq::Agent::with_parts(
            config,
            TcpConnector::default().chain(RustlsConnector::default()),
            CheckedResolver {
                inner: DefaultResolver::default(),
                refused: refused.clone(),
            },
        );
        Self { agent, refused }
    }
}

impl Source for HttpsSource {
    fn get(&self, url: &str) -> Result<Hop, HopError> {
        *self.refused.lock().unwrap_or_else(|e| e.into_inner()) = None;
        let resp = self
            .agent
            .get(url)
            .header("Accept-Encoding", "identity")
            .call()
            .map_err(|e| {
                match self
                    .refused
                    .lock()
                    .unwrap_or_else(|e| e.into_inner())
                    .take()
                {
                    Some(why) => HopError::AddressRefused(why),
                    None => HopError::Other(e.to_string()),
                }
            })?;
        let header = |k: &str| {
            resp.headers()
                .get(k)
                .and_then(|v| v.to_str().ok())
                .map(str::to_owned)
        };
        let (status, location, content_encoding) = (
            resp.status().as_u16(),
            header("location"),
            header("content-encoding"),
        );
        let body = resp.into_body().into_reader();
        Ok(Hop {
            status,
            location,
            content_encoding,
            body: Box::new(body),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const TAG: &str = "GE-Proton11-12";

    #[test]
    fn tags_follow_the_grammar() {
        for ok in ["GE-Proton11-12", "GE-Proton9-1", "GE-Proton100-1234"] {
            assert!(valid_tag(ok), "{ok}");
        }
        for bad in [
            "GE-Proton1000-1",
            "GE-Proton11-12345",
            "GE-Proton11",
            "ge-proton11-12",
            "GE-Proton11-1a",
            "GE-Proton-11-12",
            "GE-Proton11-12/../x",
        ] {
            assert!(!valid_tag(bad), "{bad}");
        }
    }

    #[test]
    fn redirects_only_to_the_allowlist() {
        let ok = [
            "https://release-assets.githubusercontent.com/github-production-release-asset/1?sp=r&sig=x",
            "https://objects.githubusercontent.com/a/b",
            "https://objects.githubusercontent.com:443/a",
            "https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/GE-Proton11-12-x86_64.tar.gz",
        ];
        for u in ok {
            assert!(redirect_allowed(u, TAG).is_ok(), "{u}");
        }
        let bad = [
            "http://objects.githubusercontent.com/a",
            "https://evil.example/a",
            "https://objects.githubusercontent.com.evil.example/a",
            "https://x.objects.githubusercontent.com/a",
            "https://objects.githubusercontent.com:8443/a",
            "https://user@objects.githubusercontent.com/a",
            "/relative/path",
            "https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-13/x",
            "https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/../GE-Proton11-13/x",
            "https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/%2e%2e/x",
            "https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/%2F/x",
            "https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12//x",
            "https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/./x",
            "https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/a\\b",
            "https://github.com/evil/proton-ge-custom/releases/download/GE-Proton11-12/x",
            "https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/x#f",
        ];
        for u in bad {
            assert!(redirect_allowed(u, TAG).is_err(), "{u}");
        }
    }

    #[test]
    fn special_purpose_addresses_are_refused() {
        for bad in [
            "127.0.0.1",
            "10.1.2.3",
            "172.16.0.1",
            "192.168.1.1",
            "169.254.1.1",
            "100.64.0.1",
            "100.127.255.255",
            "0.0.0.0",
            "224.0.0.1",
            "255.255.255.255",
            "198.18.0.1",
            "::1",
            "::",
            "fe80::1",
            "fc00::1",
            "fd12::1",
            "64:ff9b::1.2.3.4",
            "ff02::1",
            "::ffff:127.0.0.1",
            "::ffff:10.0.0.1",
            "2001:db8::1",
        ] {
            assert!(!address_allowed(bad.parse().unwrap()), "{bad}");
        }
        for ok in [
            "140.82.112.3",
            "185.199.108.133",
            "2606:50c0:8000::154",
            "100.128.0.1",
        ] {
            assert!(address_allowed(ok.parse().unwrap()), "{ok}");
        }
    }
}
