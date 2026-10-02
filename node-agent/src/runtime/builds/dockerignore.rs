//! Linux Docker ignore semantics, adapted from moby/patternmatcher (Apache-2.0).
//! Source: moby/patternmatcher@5a6d8429a19bb6948a372ff19e86fe83599a04b7,
//! patternmatcher.go and ignorefile/ignorefile.go (Linux path semantics).
//! See LICENSE.patternmatcher. Unlike gitignore, a bare name matches at the root.
use crate::runtime::{ErrorKind, RuntimeError};
use regex::Regex;

pub(super) struct Ignore(Vec<Pattern>);
struct Pattern {
    include: bool,
    matcher: Matcher,
}
enum Matcher {
    Exact(String),
    Prefix(String),
    Suffix(String),
    Regex(Regex),
}
impl Ignore {
    pub fn parse(contents: &str) -> Result<Self, RuntimeError> {
        let mut patterns = Vec::new();
        for raw in contents.trim_start_matches('\u{feff}').lines() {
            if raw.len() >= 65536 {
                return Err(ErrorKind::InvalidBuildContext.into());
            }
            if raw.starts_with('#') || raw.trim().is_empty() {
                continue;
            }
            let raw = raw.trim();
            let include = raw.starts_with('!');
            let raw = if include { raw[1..].trim() } else { raw };
            if raw.is_empty() {
                return Err(ErrorKind::InvalidBuildContext.into());
            }
            let rooted = raw.starts_with('/');
            let mut parts = Vec::new();
            for part in raw.split('/') {
                match part {
                    "" | "." => {}
                    ".." if parts.last().is_some_and(|v| *v != "..") => {
                        parts.pop();
                    }
                    ".." if rooted => {}
                    v => parts.push(v),
                }
            }
            let clean = parts.join("/");
            if clean.is_empty() {
                continue;
            }
            patterns.push(Pattern {
                include,
                matcher: compile(&clean)?,
            });
        }
        Ok(Self(patterns))
    }
    pub fn excluded(&self, path: &str) -> bool {
        let mut excluded = false;
        for pattern in &self.0 {
            if pattern.include != excluded {
                continue;
            }
            let mut candidate = path;
            loop {
                if pattern.matcher.matches(candidate) {
                    excluded = !pattern.include;
                    break;
                }
                let Some((parent, _)) = candidate.rsplit_once('/') else {
                    break;
                };
                candidate = parent;
            }
        }
        excluded
    }
}
impl Matcher {
    fn matches(&self, path: &str) -> bool {
        match self {
            Self::Exact(v) => path == v,
            Self::Prefix(v) => path.starts_with(v),
            Self::Suffix(v) => path.ends_with(v) || v.strip_prefix('/').is_some_and(|v| path == v),
            Self::Regex(v) => v.is_match(path),
        }
    }
}
fn compile(pattern: &str) -> Result<Matcher, RuntimeError> {
    enum Kind {
        Exact,
        Prefix,
        Suffix,
        Regex,
    }
    let mut kind = Kind::Exact;
    let mut expression = String::from("^");
    let mut chars = pattern.chars().peekable();
    let mut first = true;
    while let Some(ch) = chars.next() {
        match ch {
            '*' if chars.peek() == Some(&'*') => {
                chars.next();
                if chars.peek() == Some(&'/') {
                    chars.next();
                }
                if chars.peek().is_none() {
                    if matches!(kind, Kind::Exact) {
                        kind = Kind::Prefix;
                    } else {
                        expression.push_str(".*");
                        kind = Kind::Regex;
                    }
                } else {
                    expression.push_str("(.*/)?");
                    kind = Kind::Regex;
                }
                if first {
                    kind = Kind::Suffix;
                }
            }
            '*' => {
                expression.push_str("[^/]*");
                kind = Kind::Regex;
            }
            '?' => {
                expression.push_str("[^/]");
                kind = Kind::Regex;
            }
            '\\' => {
                let next = chars.next().ok_or(ErrorKind::InvalidBuildContext)?;
                expression.push_str(&regex::escape(&next.to_string()));
                kind = Kind::Regex;
            }
            '[' => {
                expression.push_str(&character_class(&mut chars)?);
                kind = Kind::Regex;
            }
            ']' => {
                expression.push_str("\\]");
                kind = Kind::Regex;
            }
            '.' | '+' | '(' | ')' | '|' | '{' | '}' | '$' => {
                expression.push('\\');
                expression.push(ch);
            }
            v => expression.push(v),
        }
        first = false;
    }
    Ok(match kind {
        Kind::Exact => Matcher::Exact(pattern.into()),
        Kind::Prefix => Matcher::Prefix(pattern[..pattern.len() - 2].into()),
        Kind::Suffix => Matcher::Suffix(pattern[2..].into()),
        Kind::Regex => {
            expression.push_str("\\z");
            Matcher::Regex(Regex::new(&expression).map_err(|_| ErrorKind::InvalidBuildContext)?)
        }
    })
}

/// Docker character classes have no Rust set-intersection/difference syntax.
/// Emit literal code points and explicit ranges so valid Docker patterns cannot
/// silently gain regex operators and upload files they were meant to exclude.
fn character_class(
    chars: &mut std::iter::Peekable<std::str::Chars<'_>>,
) -> Result<String, RuntimeError> {
    fn next(chars: &mut std::iter::Peekable<std::str::Chars<'_>>) -> Result<char, RuntimeError> {
        match chars.next() {
            Some('\\') => chars.next().ok_or(ErrorKind::InvalidBuildContext.into()),
            Some('-' | ']') | None => Err(ErrorKind::InvalidBuildContext.into()),
            Some(ch) => Ok(ch),
        }
    }
    let mut result = String::from("[");
    if chars.peek() == Some(&'^') {
        chars.next();
        result.push('^');
    }
    let mut count = 0;
    loop {
        if chars.peek() == Some(&']') && count > 0 {
            chars.next();
            result.push(']');
            return Ok(result);
        }
        let lo = next(chars)?;
        result.push_str(&format!("\\x{{{:x}}}", lo as u32));
        if chars.peek() == Some(&'-') {
            chars.next();
            let hi = next(chars)?;
            if hi < lo {
                return Err(ErrorKind::InvalidBuildContext.into());
            }
            result.push_str(&format!("-\\x{{{:x}}}", hi as u32));
        }
        count += 1;
    }
}
