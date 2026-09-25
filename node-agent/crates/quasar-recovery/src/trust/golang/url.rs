//! `net/url.Parse` (Go 1.25), reduced to what `ParseManifestBaseURL` and the redirect
//! policy read: whether it parses, the lowercased scheme, and whether there is a host.
//! Errors carry Go's text (`parse "<url>": <cause>`), except that an IP-literal Go's
//! `net/netip` refuses is described by Rust's parser after the shared `invalid host: `.

use std::net::{Ipv4Addr, Ipv6Addr};

use super::text::quote_bytes;

pub(crate) struct Parsed {
    pub(crate) scheme: String,
    pub(crate) host: Vec<u8>,
}

pub(crate) fn parse(raw: &str) -> Result<Parsed, String> {
    let raw = raw.as_bytes();
    let (u, frag) = match raw.iter().position(|&c| c == b'#') {
        Some(i) => (&raw[..i], Some(&raw[i + 1..])),
        None => (raw, None),
    };
    let parsed = parse_without_fragment(u).map_err(|e| format!("parse {}: {e}", quote_bytes(u)))?;
    if let Some(frag) = frag.filter(|f| !f.is_empty()) {
        unescape(frag, Mode::Fragment).map_err(|e| format!("parse {}: {e}", quote_bytes(raw)))?;
    }
    Ok(parsed)
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Mode {
    Path,
    Host,
    Zone,
    UserPassword,
    Fragment,
}

fn parse_without_fragment(raw: &[u8]) -> Result<Parsed, String> {
    if raw.iter().any(|&b| b < b' ' || b == 0x7f) {
        return Err("net/url: invalid control character in URL".into());
    }
    if raw == b"*" {
        return Ok(Parsed {
            scheme: String::new(),
            host: Vec::new(),
        });
    }
    let (scheme, rest) = scheme(raw)?;
    let scheme = String::from_utf8_lossy(scheme).to_ascii_lowercase();
    let rest = if rest.last() == Some(&b'?') && rest.iter().filter(|&&c| c == b'?').count() == 1 {
        &rest[..rest.len() - 1]
    } else {
        match rest.iter().position(|&c| c == b'?') {
            Some(i) => &rest[..i],
            None => rest,
        }
    };
    if !rest.starts_with(b"/") {
        if !scheme.is_empty() {
            return Ok(Parsed {
                scheme,
                host: Vec::new(),
            }); // opaque
        }
        let segment = rest.split(|&c| c == b'/').next().unwrap_or_default();
        if segment.contains(&b':') {
            return Err("first path segment in URL cannot contain colon".into());
        }
    }
    let mut host = Vec::new();
    let mut path = rest;
    if (!scheme.is_empty() || !rest.starts_with(b"///")) && rest.starts_with(b"//") {
        let after = &rest[2..];
        let (authority, tail) = match after.iter().position(|&c| c == b'/') {
            Some(i) => (&after[..i], &after[i..]),
            None => (after, &b""[..]),
        };
        host = parse_authority(authority)?;
        path = tail;
    }
    unescape(path, Mode::Path)?;
    Ok(Parsed { scheme, host })
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

fn parse_authority(authority: &[u8]) -> Result<Vec<u8>, String> {
    let at = authority.iter().rposition(|&c| c == b'@');
    let host = parse_host(match at {
        Some(i) => &authority[i + 1..],
        None => authority,
    })?;
    if let Some(i) = at {
        let userinfo = &authority[..i];
        if !valid_userinfo(userinfo) {
            return Err("net/url: invalid userinfo".into());
        }
        match userinfo.iter().position(|&c| c == b':') {
            None => {
                unescape(userinfo, Mode::UserPassword)?;
            }
            Some(k) => {
                unescape(&userinfo[..k], Mode::UserPassword)?;
                unescape(&userinfo[k + 1..], Mode::UserPassword)?;
            }
        }
    }
    Ok(host)
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

/// `shouldEscape(c, encodeHost)`, the only mode `unescape` consults it for.
fn host_should_escape(c: u8) -> bool {
    !(c.is_ascii_alphanumeric() || b"!$&'()*+,;=:[]<>\"-_.~".contains(&c))
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
