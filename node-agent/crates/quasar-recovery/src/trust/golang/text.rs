//! `strings.TrimSpace`, `strings.ToLower` (for matching ASCII keywords) and
//! `strconv.Quote` (the `%q` in every refusal message).

/// `strings.TrimSpace`: Go's `unicode.IsSpace` and Rust's `char::is_whitespace` are both
/// the Unicode White_Space property.
pub(crate) fn trim_space(s: &str) -> &str {
    s.trim()
}

/// `strings.ToLower`, as far as it can produce ASCII: Go maps each rune by its simple
/// lowercase, so U+0130 becomes `i` and the Kelvin sign `k`. Any other non-ASCII rune
/// lowers to non-ASCII in both languages and so can never equal an ASCII keyword.
pub(crate) fn to_lower_ascii_keyword(s: &str) -> String {
    s.chars()
        .map(|c| match c {
            '\u{130}' => 'i',
            '\u{212a}' => 'k',
            c => c.to_ascii_lowercase(),
        })
        .collect()
}

/// `strconv.Quote` of a byte string. Exact for ASCII and invalid UTF-8; for other runes
/// `is_print` approximates `unicode.IsPrint` (see there).
pub(crate) fn quote_bytes(b: &[u8]) -> String {
    let mut out = String::with_capacity(b.len() + 2);
    out.push('"');
    let mut i = 0;
    while i < b.len() {
        let (r, n) = decode_rune(&b[i..]);
        if r == '\u{fffd}' && n == 1 {
            out.push_str(&format!("\\x{:02x}", b[i]));
            i += 1;
            continue;
        }
        i += n;
        match r {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            r if is_print(r) => out.push(r),
            '\u{7}' => out.push_str("\\a"),
            '\u{8}' => out.push_str("\\b"),
            '\u{c}' => out.push_str("\\f"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{b}' => out.push_str("\\v"),
            r if (r as u32) < 0x20 || r == '\u{7f}' => {
                out.push_str(&format!("\\x{:02x}", r as u32))
            }
            r if (r as u32) < 0x10000 => out.push_str(&format!("\\u{:04x}", r as u32)),
            r => out.push_str(&format!("\\U{:08x}", r as u32)),
        }
    }
    out.push('"');
    out
}

pub(crate) fn quote(s: &str) -> String {
    quote_bytes(s.as_bytes())
}

/// `utf8.DecodeRune`: one rune and its width, or (U+FFFD, 1) for any invalid sequence.
pub(crate) fn decode_rune(b: &[u8]) -> (char, usize) {
    let n = match b.first() {
        None => return ('\u{fffd}', 0),
        Some(0x00..=0x7f) => 1,
        Some(0xc2..=0xdf) => 2,
        Some(0xe0..=0xef) => 3,
        Some(0xf0..=0xf4) => 4,
        Some(_) => return ('\u{fffd}', 1),
    };
    match b.get(..n).map(std::str::from_utf8) {
        Some(Ok(s)) => (s.chars().next().unwrap_or('\u{fffd}'), n),
        _ => ('\u{fffd}', 1),
    }
}

/// `unicode.IsPrint`: letters, marks, numbers, punctuation, symbols and the ASCII space.
/// Exact for ASCII; beyond it, control, space-separator and the common format and
/// private-use runes are the non-printing ones. Only a refusal message's spelling of an
/// exotic rune can differ, never an outcome.
fn is_print(c: char) -> bool {
    if c.is_ascii() {
        return (' '..='~').contains(&c);
    }
    let u = c as u32;
    let format = matches!(u,
        0xad | 0x600..=0x605 | 0x61c | 0x6dd | 0x70f | 0x180e | 0x200b..=0x200f
        | 0x202a..=0x202e | 0x2060..=0x2064 | 0x2066..=0x206f | 0xfeff | 0xfff9..=0xfffb
        | 0x110bd | 0x1bca0..=0x1bca3 | 0x1d173..=0x1d17a | 0xe0001 | 0xe0020..=0xe007f);
    let private = matches!(u, 0xe000..=0xf8ff | 0xf0000..=0x10ffff);
    let noncharacter = (0xfdd0..=0xfdef).contains(&u) || (u & 0xfffe) == 0xfffe;
    !c.is_control() && !c.is_whitespace() && !format && !private && !noncharacter
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn quote_matches_strconv_for_what_the_messages_carry() {
        assert_eq!(quote("nope"), r#""nope""#);
        assert_eq!(quote("a\"b\\c"), r#""a\"b\\c""#);
        assert_eq!(quote("x\r\n\t\u{7f}\u{0}"), r#""x\r\n\t\x7f\x00""#);
        assert_eq!(quote("c2hvcnQ=c2hv\u{2026}"), "\"c2hvcnQ=c2hv\u{2026}\"");
        assert_eq!(quote("\u{a0}\u{feff}"), r#""\u00a0\ufeff""#);
        assert_eq!(quote_bytes(b"k\xff"), r#""k\xff""#);
    }

    #[test]
    fn decode_rune_consumes_one_byte_per_invalid_byte() {
        assert_eq!(decode_rune(b"\xe2\x82\""), ('\u{fffd}', 1));
        assert_eq!(decode_rune("é".as_bytes()), ('é', 2));
        assert_eq!(decode_rune(b"\xed\xa0\x80"), ('\u{fffd}', 1));
    }
}
