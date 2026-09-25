//! `net/url` (Go 1.25) as the verifier uses it: `Parse` (the base URL and every
//! `Location`), `ResolveReference`, `EscapedPath`/`RequestURI` (the request line), and
//! `Hostname`/`Port`. Fragments are validated and dropped: no request carries one.
//! Errors carry Go's text (`parse "<url>": <cause>`), except that an IP-literal Go's
//! `net/netip` refuses is described by Rust's parser after the shared `invalid host: `.

use std::net::{Ipv4Addr, Ipv6Addr};

use super::text::quote_bytes;

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub(crate) struct Userinfo {
    pub(crate) username: Vec<u8>,
    pub(crate) password: Option<Vec<u8>>,
}

/// Go's `url.URL` without the fragment.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub(crate) struct Url {
    /// Lowercased, as `Parse` leaves it.
    pub(crate) scheme: String,
    pub(crate) opaque: Vec<u8>,
    pub(crate) user: Option<Userinfo>,
    /// `host` or `host:port`, unescaped.
    pub(crate) host: Vec<u8>,
    pub(crate) path: Vec<u8>,
    pub(crate) raw_path: Vec<u8>,
    pub(crate) force_query: bool,
    pub(crate) raw_query: Vec<u8>,
}

pub(crate) fn parse(raw: &str) -> Result<Url, String> {
    let raw = raw.as_bytes();
    let (u, frag) = match raw.iter().position(|&c| c == b'#') {
        Some(i) => (&raw[..i], Some(&raw[i + 1..])),
        None => (raw, None),
    };
    let url = parse_without_fragment(u).map_err(|e| format!("parse {}: {e}", quote_bytes(u)))?;
    if let Some(frag) = frag.filter(|f| !f.is_empty()) {
        unescape(frag, Mode::Fragment).map_err(|e| format!("parse {}: {e}", quote_bytes(raw)))?;
    }
    Ok(url)
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Mode {
    Path,
    Host,
    Zone,
    UserPassword,
    Fragment,
}

fn parse_without_fragment(raw: &[u8]) -> Result<Url, String> {
    if raw.iter().any(|&b| b < b' ' || b == 0x7f) {
        return Err("net/url: invalid control character in URL".into());
    }
    let mut url = Url::default();
    if raw == b"*" {
        url.path = b"*".to_vec();
        return Ok(url);
    }
    let (scheme, rest) = scheme(raw)?;
    url.scheme = String::from_utf8_lossy(scheme).to_ascii_lowercase();
    let mut rest = rest;
    if rest.last() == Some(&b'?') && rest.iter().filter(|&&c| c == b'?').count() == 1 {
        url.force_query = true;
        rest = &rest[..rest.len() - 1];
    } else if let Some(i) = rest.iter().position(|&c| c == b'?') {
        url.raw_query = rest[i + 1..].to_vec();
        rest = &rest[..i];
    }
    if !rest.starts_with(b"/") {
        if !url.scheme.is_empty() {
            url.opaque = rest.to_vec();
            return Ok(url);
        }
        let segment = rest.split(|&c| c == b'/').next().unwrap_or_default();
        if segment.contains(&b':') {
            return Err("first path segment in URL cannot contain colon".into());
        }
    }
    if (!url.scheme.is_empty() || !rest.starts_with(b"///")) && rest.starts_with(b"//") {
        let after = &rest[2..];
        let (authority, tail) = match after.iter().position(|&c| c == b'/') {
            Some(i) => (&after[..i], &after[i..]),
            None => (after, &b""[..]),
        };
        let (user, host) = parse_authority(authority)?;
        url.user = user;
        url.host = host;
        rest = tail;
    }
    url.set_path(rest)?;
    Ok(url)
}

impl Url {
    /// `setPath`: `raw_path` is kept only when it differs from the default escaping.
    fn set_path(&mut self, p: &[u8]) -> Result<(), String> {
        let path = unescape(p, Mode::Path)?;
        self.raw_path = if escape_path(&path) == p {
            Vec::new()
        } else {
            p.to_vec()
        };
        self.path = path;
        Ok(())
    }

    /// `EscapedPath`.
    pub(crate) fn escaped_path(&self) -> Vec<u8> {
        if !self.raw_path.is_empty()
            && valid_encoded_path(&self.raw_path)
            && unescape(&self.raw_path, Mode::Path).is_ok_and(|p| p == self.path)
        {
            return self.raw_path.clone();
        }
        if self.path == b"*" {
            return b"*".to_vec();
        }
        escape_path(&self.path)
    }

    /// `RequestURI`: the request-line target.
    pub(crate) fn request_uri(&self) -> Vec<u8> {
        let mut out = self.opaque.clone();
        if out.is_empty() {
            out = self.escaped_path();
            if out.is_empty() {
                out = b"/".to_vec();
            }
        } else if out.starts_with(b"//") {
            let mut prefixed = format!("{}:", self.scheme).into_bytes();
            prefixed.extend(out);
            out = prefixed;
        }
        if self.force_query || !self.raw_query.is_empty() {
            out.push(b'?');
            out.extend_from_slice(&self.raw_query);
        }
        out
    }

    /// `(*url.URL).Parse(reference)`.
    pub(crate) fn join(&self, reference: &str) -> Result<Url, String> {
        Ok(self.resolve_reference(&parse(reference)?))
    }

    /// `ResolveReference` (RFC 3986 §5.2).
    fn resolve_reference(&self, r: &Url) -> Url {
        let mut url = r.clone();
        if r.scheme.is_empty() {
            url.scheme = self.scheme.clone();
        }
        if !r.scheme.is_empty() || !r.host.is_empty() || r.user.is_some() {
            let p = resolve_path(&r.escaped_path(), b"");
            let _ = url.set_path(&p);
            return url;
        }
        if !r.opaque.is_empty() {
            url.user = None;
            url.host.clear();
            url.path.clear();
            return url;
        }
        if r.path.is_empty() && !r.force_query && r.raw_query.is_empty() {
            url.raw_query = self.raw_query.clone();
        }
        if r.path.is_empty() && !self.opaque.is_empty() {
            url.opaque = self.opaque.clone();
            url.user = None;
            url.host.clear();
            url.path.clear();
            return url;
        }
        url.host = self.host.clone();
        url.user = self.user.clone();
        let p = resolve_path(&self.escaped_path(), &r.escaped_path());
        let _ = url.set_path(&p);
        url
    }

    /// `Hostname` and `Port` (Go's `url.splitHostPort`).
    pub(crate) fn host_port(&self) -> (Vec<u8>, Vec<u8>) {
        let mut host = self.host.as_slice();
        let mut port: &[u8] = b"";
        if let Some(colon) = host.iter().rposition(|&c| c == b':') {
            if valid_optional_port(&host[colon..]) {
                port = &host[colon + 1..];
                host = &host[..colon];
            }
        }
        if host.starts_with(b"[") && host.ends_with(b"]") && host.len() >= 2 {
            host = &host[1..host.len() - 1];
        }
        (host.to_vec(), port.to_vec())
    }
}

/// `resolvePath`: merge and remove dot segments.
fn resolve_path(base: &[u8], reference: &[u8]) -> Vec<u8> {
    let full: Vec<u8> = if reference.is_empty() {
        base.to_vec()
    } else if reference[0] != b'/' {
        let i = base
            .iter()
            .rposition(|&c| c == b'/')
            .map(|i| i + 1)
            .unwrap_or(0);
        [&base[..i], reference].concat()
    } else {
        reference.to_vec()
    };
    if full.is_empty() {
        return Vec::new();
    }
    let mut dst: Vec<u8> = vec![b'/'];
    let mut first = true;
    let mut last: &[u8] = b"";
    let mut segments = full.split(|&c| c == b'/').peekable();
    while let Some(elem) = segments.next() {
        last = elem;
        match elem {
            b"." => first = false,
            b".." => {
                match dst[1..].iter().rposition(|&c| c == b'/') {
                    Some(i) => dst.truncate(i + 1),
                    None => dst.truncate(1),
                }
                first = dst.len() == 1;
            }
            _ => {
                if !first {
                    dst.push(b'/');
                }
                dst.extend_from_slice(elem);
                first = false;
            }
        }
        if segments.peek().is_none() {
            break;
        }
    }
    if last == b"." || last == b".." {
        dst.push(b'/');
    }
    if dst.len() > 1 && dst[1] == b'/' {
        dst.remove(0);
    }
    dst
}

/// `getScheme`.
fn scheme(raw: &[u8]) -> Result<(&[u8], &[u8]), String> {
    for (i, &c) in raw.iter().enumerate() {
        match c {
            b'a'..=b'z' | b'A'..=b'Z' => {}
            b'0'..=b'9' | b'+' | b'-' | b'.' if i != 0 => {}
            b':' if i == 0 => return Err("missing protocol scheme".into()),
            b':' => return Ok((&raw[..i], &raw[i + 1..])),
            _ => return Ok((&b""[..], raw)),
        }
    }
    Ok((&b""[..], raw))
}

fn parse_authority(authority: &[u8]) -> Result<(Option<Userinfo>, Vec<u8>), String> {
    let at = authority.iter().rposition(|&c| c == b'@');
    let host = parse_host(match at {
        Some(i) => &authority[i + 1..],
        None => authority,
    })?;
    let Some(i) = at else {
        return Ok((None, host));
    };
    let userinfo = &authority[..i];
    if !valid_userinfo(userinfo) {
        return Err("net/url: invalid userinfo".into());
    }
    let user = match userinfo.iter().position(|&c| c == b':') {
        None => Userinfo {
            username: unescape(userinfo, Mode::UserPassword)?,
            password: None,
        },
        Some(k) => Userinfo {
            username: unescape(&userinfo[..k], Mode::UserPassword)?,
            password: Some(unescape(&userinfo[k + 1..], Mode::UserPassword)?),
        },
    };
    Ok((Some(user), host))
}

fn parse_host(host: &[u8]) -> Result<Vec<u8>, String> {
    match host.iter().rposition(|&c| c == b'[') {
        Some(open) if open > 0 => return Err("invalid IP-literal".into()),
        Some(_) => {
            let close = host
                .iter()
                .rposition(|&c| c == b']')
                .ok_or_else(|| "missing ']' in host".to_string())?;
            let colon_port = &host[close + 1..];
            if !valid_optional_port(colon_port) {
                return Err(format!(
                    "invalid port {} after host",
                    quote_bytes(colon_port)
                ));
            }
            let port = unescape(colon_port, Mode::Host)?;
            let hostname = &host[1..close];
            let unescaped = match find(hostname, b"%25") {
                Some(z) => {
                    let mut h = unescape(&hostname[..z], Mode::Host)?;
                    h.extend(unescape(&hostname[z..], Mode::Zone)?);
                    h
                }
                None => unescape(hostname, Mode::Host)?,
            };
            check_ip_literal(&unescaped)?;
            let mut out = b"[".to_vec();
            out.extend(unescaped);
            out.push(b']');
            out.extend(port);
            return Ok(out);
        }
        None => {}
    }
    if let Some(i) = host.iter().rposition(|&c| c == b':') {
        if !valid_optional_port(&host[i..]) {
            return Err(format!(
                "invalid port {} after host",
                quote_bytes(&host[i..])
            ));
        }
    }
    unescape(host, Mode::Host)
}

/// `netip.ParseAddr` then "not IPv4": only an IPv6 address may sit in brackets.
fn check_ip_literal(addr: &[u8]) -> Result<(), String> {
    let s = String::from_utf8_lossy(addr);
    if s.contains(':') {
        let (ip, zone) = match s.split_once('%') {
            Some((ip, zone)) => (ip, Some(zone)),
            None => (&*s, None),
        };
        if zone == Some("") {
            return Err(format!(
                "invalid host: ParseAddr({}): zone must be a non-empty string",
                quote_bytes(addr)
            ));
        }
        return ip
            .parse::<Ipv6Addr>()
            .map(|_| ())
            .map_err(|e| format!("invalid host: ParseAddr({}): {e}", quote_bytes(addr)));
    }
    if s.contains('.') && s.parse::<Ipv4Addr>().is_ok() {
        return Err("invalid IP-literal".into());
    }
    Err(format!(
        "invalid host: ParseAddr({}): unable to parse IP",
        quote_bytes(addr)
    ))
}

fn find(hay: &[u8], needle: &[u8]) -> Option<usize> {
    hay.windows(needle.len()).position(|w| w == needle)
}

fn valid_optional_port(port: &[u8]) -> bool {
    match port.split_first() {
        None => true,
        Some((b':', digits)) => digits.iter().all(u8::is_ascii_digit),
        Some(_) => false,
    }
}

fn valid_userinfo(s: &[u8]) -> bool {
    // Go ranges runes, so any non-ASCII byte fails as its rune would.
    s.iter()
        .all(|&c| c.is_ascii_alphanumeric() || b"-._:~!$&'()*+,;=%@".contains(&c))
}

/// `shouldEscape(c, encodeHost)`.
fn host_should_escape(c: u8) -> bool {
    !(c.is_ascii_alphanumeric() || b"!$&'()*+,;=:[]<>\"-_.~".contains(&c))
}

/// `shouldEscape(c, encodePath)`: only unreserved and `$&+,/:;=@` stay.
fn path_should_escape(c: u8) -> bool {
    !(c.is_ascii_alphanumeric() || b"-_.~$&+,/:;=@".contains(&c))
}

/// `escape(s, encodePath)`, uppercase hex.
fn escape_path(s: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(s.len());
    for &c in s {
        if path_should_escape(c) {
            out.extend_from_slice(format!("%{c:02X}").as_bytes());
        } else {
            out.push(c);
        }
    }
    out
}

/// `validEncoded(s, encodePath)`.
fn valid_encoded_path(s: &[u8]) -> bool {
    s.iter()
        .all(|&c| b"!$&'()*+,;=:@[]%".contains(&c) || !path_should_escape(c))
}

fn unhex(c: u8) -> u8 {
    (c as char).to_digit(16).unwrap_or(0) as u8
}

/// `unescape`: validates `%XX` escapes and, in a host, the characters; returns the bytes.
fn unescape(s: &[u8], mode: Mode) -> Result<Vec<u8>, String> {
    let escape_error = |e: &[u8]| format!("invalid URL escape {}", quote_bytes(e));
    let mut i = 0;
    while i < s.len() {
        match s[i] {
            b'%' => {
                if i + 2 >= s.len()
                    || !s[i + 1].is_ascii_hexdigit()
                    || !s[i + 2].is_ascii_hexdigit()
                {
                    return Err(escape_error(&s[i..(i + 3).min(s.len())]));
                }
                let esc = &s[i..i + 3];
                if mode == Mode::Host && unhex(s[i + 1]) < 8 && esc != b"%25" {
                    return Err(escape_error(esc));
                }
                if mode == Mode::Zone {
                    let v = unhex(s[i + 1]) << 4 | unhex(s[i + 2]);
                    if esc != b"%25" && v != b' ' && host_should_escape(v) {
                        return Err(escape_error(esc));
                    }
                }
                i += 3;
            }
            c => {
                if matches!(mode, Mode::Host | Mode::Zone) && c < 0x80 && host_should_escape(c) {
                    return Err(format!(
                        "invalid character {} in host name",
                        quote_bytes(&s[i..i + 1])
                    ));
                }
                i += 1;
            }
        }
    }
    let mut out = Vec::with_capacity(s.len());
    let mut i = 0;
    while i < s.len() {
        if s[i] == b'%' {
            out.push(unhex(s[i + 1]) << 4 | unhex(s[i + 2]));
            i += 3;
        } else {
            out.push(s[i]);
            i += 1;
        }
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn resolve(base: &str, reference: &str) -> String {
        let u = parse(base).unwrap().join(reference).unwrap();
        format!(
            "{}://{}{}",
            u.scheme,
            String::from_utf8_lossy(&u.host),
            String::from_utf8_lossy(&u.request_uri())
        )
    }

    #[test]
    fn references_resolve_as_go_resolves_them() {
        // Each row observed from Go 1.25 (*url.URL).Parse + RequestURI.
        let base = "https://a.example/b/c/d?q";
        assert_eq!(resolve(base, "g"), "https://a.example/b/c/g");
        assert_eq!(resolve(base, "../g"), "https://a.example/b/g");
        assert_eq!(resolve(base, "../../../../g"), "https://a.example/g");
        assert_eq!(resolve(base, "/x/./y/../z"), "https://a.example/x/z");
        assert_eq!(resolve(base, ""), "https://a.example/b/c/d?q");
        assert_eq!(resolve(base, "?r"), "https://a.example/b/c/d?r");
        assert_eq!(resolve(base, "."), "https://a.example/b/c/");
        assert_eq!(
            resolve(base, "//other.example/p"),
            "https://other.example/p"
        );
        assert_eq!(
            resolve(base, "HTTPS://Other.example/p"),
            "https://Other.example/p"
        );
        assert_eq!(
            resolve(base, "http://x.example/p%2fq"),
            "http://x.example/p%2fq"
        );
        assert_eq!(resolve(base, "/a b"), "https://a.example/a%20b");
        assert_eq!(resolve(base, "/a!b"), "https://a.example/a!b");
    }

    #[test]
    fn host_and_port_split_like_go() {
        let hp = |s: &str| {
            let (h, p) = parse(s).unwrap().host_port();
            (String::from_utf8(h).unwrap(), String::from_utf8(p).unwrap())
        };
        assert_eq!(
            hp("https://a.example:8443/x"),
            ("a.example".into(), "8443".into())
        );
        assert_eq!(hp("https://a.example/x"), ("a.example".into(), "".into()));
        assert_eq!(hp("https://[::1]:9/x"), ("::1".into(), "9".into()));
        assert_eq!(hp("https://a.example:/x"), ("a.example".into(), "".into()));
    }
}
