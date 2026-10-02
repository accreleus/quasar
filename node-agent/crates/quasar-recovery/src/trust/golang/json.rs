//! Go `encoding/json` v1 decoding of the two documents the verifier reads: the detached
//! signature document (`json.Decoder` + `DisallowUnknownFields` + `More`) and the signed
//! manifest (`json.Unmarshal`, unknown fields ignored).
//!
//! What has to match Go, because it decides outcomes (each pinned by a vector):
//! - the whole value is syntax-checked first, nesting capped at 10000;
//! - field names match case-insensitively, including the Kelvin sign and the long s;
//! - `null` leaves a string, int or struct untouched and resets a slice;
//! - a repeated key decodes into the existing value, and a repeated array decodes into
//!   the existing elements (re-exposing stale ones within capacity);
//! - invalid UTF-8 and lone surrogates in strings become U+FFFD, one per byte;
//! - `json.Decoder.More` is false before `]` and `}`, so those may trail the document.
//!
//! The scan and the skip are iterative: the signature document is attacker-shaped input.

use super::text::decode_rune;

const MAX_NESTING_DEPTH: usize = 10_000;

fn is_space(b: u8) -> bool {
    matches!(b, b' ' | b'\t' | b'\n' | b'\r')
}

fn skip_space(b: &[u8], mut i: usize) -> usize {
    while i < b.len() && is_space(b[i]) {
        i += 1;
    }
    i
}

fn syntax(msg: impl Into<String>) -> String {
    msg.into()
}

fn scan_string(b: &[u8], mut i: usize) -> Result<usize, String> {
    i += 1; // opening quote
    loop {
        match b.get(i) {
            None => return Err(syntax("unexpected end of JSON input")),
            Some(b'"') => return Ok(i + 1),
            Some(b'\\') => match b.get(i + 1) {
                Some(b'"' | b'\\' | b'/' | b'b' | b'f' | b'n' | b'r' | b't') => i += 2,
                Some(b'u') => {
                    for k in 0..4 {
                        match b.get(i + 2 + k) {
                            Some(c) if c.is_ascii_hexdigit() => {}
                            Some(_) => {
                                return Err(syntax(
                                    "invalid character in \\u hexadecimal character escape",
                                ))
                            }
                            None => return Err(syntax("unexpected end of JSON input")),
                        }
                    }
                    i += 6;
                }
                Some(_) => return Err(syntax("invalid character in string escape code")),
                None => return Err(syntax("unexpected end of JSON input")),
            },
            Some(c) if *c < 0x20 => return Err(syntax("invalid character in string literal")),
            Some(_) => i += 1,
        }
    }
}

fn scan_number(b: &[u8], mut i: usize) -> Result<usize, String> {
    let digit = |i: usize| b.get(i).is_some_and(u8::is_ascii_digit);
    if b.get(i) == Some(&b'-') {
        i += 1;
    }
    match b.get(i) {
        Some(b'0') => i += 1,
        Some(b'1'..=b'9') => {
            while digit(i) {
                i += 1;
            }
        }
        Some(_) => return Err(syntax("invalid character in numeric literal")),
        None => return Err(syntax("unexpected end of JSON input")),
    }
    if b.get(i) == Some(&b'.') {
        i += 1;
        if !digit(i) {
            return Err(syntax(
                "invalid character after decimal point in numeric literal",
            ));
        }
        while digit(i) {
            i += 1;
        }
    }
    if matches!(b.get(i), Some(b'e' | b'E')) {
        i += 1;
        if matches!(b.get(i), Some(b'+' | b'-')) {
            i += 1;
        }
        if !digit(i) {
            return Err(syntax("invalid character in exponent of numeric literal"));
        }
        while digit(i) {
            i += 1;
        }
    }
    Ok(i)
}

fn scan_literal(b: &[u8], i: usize, word: &[u8]) -> Result<usize, String> {
    for (k, w) in word.iter().enumerate() {
        match b.get(i + k) {
            Some(c) if c == w => {}
            Some(_) => return Err(syntax("invalid character in literal")),
            None => return Err(syntax("unexpected end of JSON input")),
        }
    }
    Ok(i + word.len())
}

/// Go's scanner over one value starting at `start` (whitespace allowed before it).
/// Returns (first byte of the value, one past its last byte).
fn scan_value(b: &[u8], start: usize) -> Result<(usize, usize), String> {
    let first = skip_space(b, start);
    let mut i = first;
    let mut stack: Vec<u8> = Vec::new();
    'value: loop {
        i = skip_space(b, i);
        let c = match b.get(i) {
            Some(c) => *c,
            None => return Err(syntax("unexpected end of JSON input")),
        };
        match c {
            b'{' | b'[' => {
                stack.push(c);
                if stack.len() > MAX_NESTING_DEPTH {
                    return Err(syntax("exceeded max depth"));
                }
                i = skip_space(b, i + 1);
                let close = if c == b'{' { b'}' } else { b']' };
                if b.get(i) == Some(&close) {
                    i += 1;
                    stack.pop();
                } else if c == b'{' {
                    i = scan_key(b, i)?;
                    continue 'value;
                } else {
                    continue 'value;
                }
            }
            b'"' => i = scan_string(b, i)?,
            b'-' | b'0'..=b'9' => i = scan_number(b, i)?,
            b't' => i = scan_literal(b, i, b"true")?,
            b'f' => i = scan_literal(b, i, b"false")?,
            b'n' => i = scan_literal(b, i, b"null")?,
            _ => return Err(syntax("invalid character looking for beginning of value")),
        }
        // After a complete value: close containers or move to the next element.
        loop {
            let Some(&open) = stack.last() else {
                return Ok((first, i));
            };
            i = skip_space(b, i);
            let close = if open == b'{' { b'}' } else { b']' };
            match b.get(i) {
                Some(&c) if c == close => {
                    i += 1;
                    stack.pop();
                }
                Some(b',') => {
                    i = skip_space(b, i + 1);
                    if open == b'{' {
                        i = scan_key(b, i)?;
                    }
                    continue 'value;
                }
                Some(_) => return Err(syntax("invalid character after value")),
                None => return Err(syntax("unexpected end of JSON input")),
            }
        }
    }
}

/// An object key and its colon; returns the index just past the colon.
fn scan_key(b: &[u8], i: usize) -> Result<usize, String> {
    match b.get(i) {
        Some(b'"') => {}
        Some(_) => {
            return Err(syntax(
                "invalid character looking for beginning of object key string",
            ))
        }
        None => return Err(syntax("unexpected end of JSON input")),
    }
    let i = skip_space(b, scan_string(b, i)?);
    match b.get(i) {
        Some(b':') => Ok(i + 1),
        Some(_) => Err(syntax("invalid character after object key")),
        None => Err(syntax("unexpected end of JSON input")),
    }
}

// ── decoding over an already-validated value ─────────────────────────────────

pub(crate) struct Decoder<'a> {
    b: &'a [u8],
    i: usize,
    disallow_unknown: bool,
}

impl Decoder<'_> {
    fn space(&mut self) {
        self.i = skip_space(self.b, self.i);
    }

    fn peek(&mut self) -> Option<u8> {
        self.space();
        self.b.get(self.i).copied()
    }

    fn eat(&mut self, c: u8) -> bool {
        if self.peek() == Some(c) {
            self.i += 1;
            true
        } else {
            false
        }
    }

    /// Go's `unquote`: escapes decoded, invalid UTF-8 and bad surrogates to U+FFFD.
    fn string(&mut self) -> String {
        let b = self.b;
        let mut i = self.i + 1;
        let mut out = String::new();
        while let Some(&c) = b.get(i) {
            match c {
                b'"' => {
                    i += 1;
                    break;
                }
                b'\\' => {
                    let e = b.get(i + 1).copied().unwrap_or(b'"');
                    i += 2;
                    match e {
                        b'b' => out.push('\u{8}'),
                        b'f' => out.push('\u{c}'),
                        b'n' => out.push('\n'),
                        b'r' => out.push('\r'),
                        b't' => out.push('\t'),
                        b'u' => {
                            let r = hex4(b, i);
                            i += 4;
                            if (0xd800..0xe000).contains(&r) {
                                let pair = (b.get(i) == Some(&b'\\')
                                    && b.get(i + 1) == Some(&b'u'))
                                .then(|| hex4(b, i + 2))
                                .filter(|r2| {
                                    (0xd800..0xdc00).contains(&r) && (0xdc00..0xe000).contains(r2)
                                });
                                match pair {
                                    Some(r2) => {
                                        let cp = 0x10000 + ((r - 0xd800) << 10) + (r2 - 0xdc00);
                                        out.push(char::from_u32(cp).unwrap_or('\u{fffd}'));
                                        i += 6;
                                    }
                                    None => out.push('\u{fffd}'),
                                }
                            } else {
                                out.push(char::from_u32(r).unwrap_or('\u{fffd}'));
                            }
                        }
                        other => out.push(other as char),
                    }
                }
                0x00..=0x7f => {
                    out.push(c as char);
                    i += 1;
                }
                _ => {
                    let (r, n) = decode_rune(&b[i..]);
                    out.push(r);
                    i += n.max(1);
                }
            }
        }
        self.i = i;
        out
    }

    /// A number, `true`, `false` or `null` token.
    fn literal(&mut self) -> &[u8] {
        self.space();
        let start = self.i;
        while let Some(&c) = self.b.get(self.i) {
            if is_space(c) || matches!(c, b',' | b']' | b'}' | b':') {
                break;
            }
            self.i += 1;
        }
        &self.b[start..self.i]
    }

    /// Skips one value without recursion.
    fn skip(&mut self) {
        match self.peek() {
            Some(b'"') => {
                self.string();
            }
            Some(b'{' | b'[') => {
                let mut depth = 0usize;
                while let Some(&c) = self.b.get(self.i) {
                    match c {
                        b'"' => {
                            self.string();
                            continue;
                        }
                        b'{' | b'[' => depth += 1,
                        b'}' | b']' => {
                            depth = depth.saturating_sub(1);
                            if depth == 0 {
                                self.i += 1;
                                return;
                            }
                        }
                        _ => {}
                    }
                    self.i += 1;
                }
            }
            _ => {
                self.literal();
            }
        }
    }

    fn type_error(&mut self, into: &str) -> String {
        let what = match self.peek() {
            Some(b'"') => "string",
            Some(b'{') => "object",
            Some(b'[') => "array",
            Some(b't' | b'f') => "bool",
            _ => "number",
        };
        self.skip();
        format!("json: cannot unmarshal {what} into Go value of type {into}")
    }
}

fn hex4(b: &[u8], i: usize) -> u32 {
    let mut v = 0u32;
    for k in 0..4 {
        let d = b
            .get(i + k)
            .and_then(|c| (*c as char).to_digit(16))
            .unwrap_or(0);
        v = v << 4 | d;
    }
    v
}

/// `foldName` equality against an ASCII field name.
fn field_matches(key: &str, field: &str) -> bool {
    let mut k = key.chars();
    let mut f = field.chars();
    loop {
        match (k.next(), f.next()) {
            (None, None) => return true,
            (Some(kc), Some(fc)) => {
                let kc = match kc {
                    '\u{17f}' => 'S',
                    '\u{212a}' => 'K',
                    c => c.to_ascii_uppercase(),
                };
                if kc != fc.to_ascii_uppercase() {
                    return false;
                }
            }
            _ => return false,
        }
    }
}

pub(crate) trait Target {
    fn decode(&mut self, d: &mut Decoder<'_>) -> Result<(), String>;
}

impl Target for String {
    fn decode(&mut self, d: &mut Decoder<'_>) -> Result<(), String> {
        match d.peek() {
            Some(b'"') => {
                *self = d.string();
                Ok(())
            }
            Some(b'n') => {
                d.literal();
                Ok(())
            }
            _ => Err(d.type_error("string")),
        }
    }
}

impl Target for i64 {
    fn decode(&mut self, d: &mut Decoder<'_>) -> Result<(), String> {
        match d.peek() {
            Some(b'-' | b'0'..=b'9') => {
                let lit = String::from_utf8_lossy(d.literal()).into_owned();
                // strconv.ParseInt(lit, 10, 64): no fraction, no exponent, no overflow.
                *self = lit.parse::<i64>().map_err(|_| {
                    format!("json: cannot unmarshal number {lit} into Go value of type int")
                })?;
                Ok(())
            }
            Some(b'n') => {
                d.literal();
                Ok(())
            }
            _ => Err(d.type_error("int")),
        }
    }
}

/// A Go slice: `backing` keeps every element ever exposed, `len` is the visible length.
#[derive(Debug, Default)]
pub(crate) struct GoSlice<T> {
    backing: Vec<T>,
    len: usize,
}

impl<T> GoSlice<T> {
    pub(crate) fn as_slice(&self) -> &[T] {
        &self.backing[..self.len]
    }
}

impl<T: Target + Default> Target for GoSlice<T> {
    fn decode(&mut self, d: &mut Decoder<'_>) -> Result<(), String> {
        match d.peek() {
            Some(b'[') => {
                d.i += 1;
                let mut n = 0;
                while !d.eat(b']') {
                    if n >= self.backing.len() {
                        self.backing.push(T::default());
                    }
                    self.backing[n].decode(d)?;
                    n += 1;
                    d.eat(b',');
                }
                self.len = n;
                if n == 0 {
                    self.backing = Vec::new();
                }
                Ok(())
            }
            Some(b'n') => {
                d.literal();
                *self = GoSlice {
                    backing: Vec::new(),
                    len: 0,
                };
                Ok(())
            }
            _ => Err(d.type_error("slice")),
        }
    }
}

/// Decodes a JSON object into a struct whose fields are `fields`, in place.
fn object(
    d: &mut Decoder<'_>,
    fields: &[&str],
    mut field: impl FnMut(usize, &mut Decoder<'_>) -> Result<(), String>,
) -> Result<(), String> {
    match d.peek() {
        Some(b'{') => {}
        Some(b'n') => {
            d.literal();
            return Ok(());
        }
        _ => return Err(d.type_error("struct")),
    }
    d.i += 1;
    while !d.eat(b'}') {
        d.space();
        let key = d.string();
        d.eat(b':');
        match fields.iter().position(|f| field_matches(&key, f)) {
            Some(k) => field(k, d)?,
            None if d.disallow_unknown => {
                return Err(format!("json: unknown field {}", super::text::quote(&key)))
            }
            None => d.skip(),
        }
        d.eat(b',');
    }
    Ok(())
}

#[derive(Debug, Default)]
pub(crate) struct SignatureEntry {
    pub(crate) algorithm: String,
    pub(crate) key_id: String,
    pub(crate) signature: String,
}

impl Target for SignatureEntry {
    fn decode(&mut self, d: &mut Decoder<'_>) -> Result<(), String> {
        object(d, &["algorithm", "key_id", "signature"], |k, d| match k {
            0 => self.algorithm.decode(d),
            1 => self.key_id.decode(d),
            _ => self.signature.decode(d),
        })
    }
}

#[derive(Debug, Default)]
pub(crate) struct SignatureDocument {
    pub(crate) format_version: i64,
    pub(crate) signatures: GoSlice<SignatureEntry>,
}

impl Target for SignatureDocument {
    fn decode(&mut self, d: &mut Decoder<'_>) -> Result<(), String> {
        object(d, &["format_version", "signatures"], |k, d| match k {
            0 => self.format_version.decode(d),
            _ => self.signatures.decode(d),
        })
    }
}

#[derive(Debug, Default)]
pub(crate) struct ManifestComponent {
    pub(crate) name: String,
    pub(crate) image: String,
    pub(crate) digest: String,
}

impl Target for ManifestComponent {
    fn decode(&mut self, d: &mut Decoder<'_>) -> Result<(), String> {
        object(d, &["name", "image", "digest"], |k, d| match k {
            0 => self.name.decode(d),
            1 => self.image.decode(d),
            _ => self.digest.decode(d),
        })
    }
}

#[derive(Debug, Default)]
pub(crate) struct SignedManifest {
    pub(crate) version: String,
    pub(crate) components: GoSlice<ManifestComponent>,
}

impl Target for SignedManifest {
    fn decode(&mut self, d: &mut Decoder<'_>) -> Result<(), String> {
        object(d, &["version", "components"], |k, d| match k {
            0 => self.version.decode(d),
            _ => self.components.decode(d),
        })
    }
}

/// Why a signature document did not decode.
#[derive(Debug, PartialEq, Eq)]
pub(crate) enum DocumentError {
    /// Not JSON in the documented shape (Go's `Decode` error).
    Json(String),
    /// A well-formed document followed by more than `]`, `}` or space (`More()`).
    TrailingContent,
}

/// `json.NewDecoder(b)` + `DisallowUnknownFields` + `Decode` + `More`.
pub(crate) fn decode_signature_document(b: &[u8]) -> Result<SignatureDocument, DocumentError> {
    let first = skip_space(b, 0);
    if first == b.len() {
        return Err(DocumentError::Json("EOF".into()));
    }
    // The Decoder stops at the value's last byte, scalar or not: what follows is More's.
    let (start, end) = scan_value(b, first).map_err(DocumentError::Json)?;
    let mut doc = SignatureDocument::default();
    let mut d = Decoder {
        b: &b[start..end],
        i: 0,
        disallow_unknown: true,
    };
    doc.decode(&mut d).map_err(DocumentError::Json)?;
    let next = skip_space(b, end);
    if next < b.len() && b[next] != b']' && b[next] != b'}' {
        return Err(DocumentError::TrailingContent);
    }
    Ok(doc)
}

/// `json.Unmarshal(b, &manifest)`.
pub(crate) fn unmarshal_signed_manifest(b: &[u8]) -> Result<SignedManifest, String> {
    let (start, end) = scan_value(b, 0)?;
    if skip_space(b, end) != b.len() {
        return Err("invalid character after top-level value".into());
    }
    let mut m = SignedManifest::default();
    let mut d = Decoder {
        b: &b[start..end],
        i: 0,
        disallow_unknown: false,
    };
    m.decode(&mut d)?;
    Ok(m)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn scan_accepts_valid_and_refuses_invalid_json() {
        for ok in [
            r#"{}"#,
            r#"[]"#,
            r#"{"a":[1,-0,2.5e-3,true,false,null,"x\u00e9"]}"#,
            " null ",
            r#""s""#,
        ] {
            assert!(scan_value(ok.as_bytes(), 0).is_ok(), "{ok}");
        }
        for bad in [
            r#"{"a"}"#,
            r#"[1,]"#,
            "[01]",
            "1.",
            "-",
            r#"{"a":1,}"#,
            "tru",
            "\"\u{1}\"",
            r#""\x""#,
            r#"{1:2}"#,
            "[",
            "",
        ] {
            assert!(scan_value(bad.as_bytes(), 0).is_err(), "{bad:?}");
        }
    }

    #[test]
    fn depth_is_capped_like_go() {
        let ok = format!(
            "{}{}",
            "[".repeat(MAX_NESTING_DEPTH),
            "]".repeat(MAX_NESTING_DEPTH)
        );
        assert!(scan_value(ok.as_bytes(), 0).is_ok());
        let deep = format!(
            "{}{}",
            "[".repeat(MAX_NESTING_DEPTH + 1),
            "]".repeat(MAX_NESTING_DEPTH + 1)
        );
        assert!(scan_value(deep.as_bytes(), 0).is_err());
    }

    #[test]
    fn surrogate_pairs_decode_and_lone_ones_are_replaced() {
        let m = unmarshal_signed_manifest(br#"{"version":"\ud83d\ude00|\udc00|\ud800x"}"#).unwrap();
        assert_eq!(m.version, "\u{1f600}|\u{fffd}|\u{fffd}x");
    }
}
