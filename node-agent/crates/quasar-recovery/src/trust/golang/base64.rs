//! `base64.StdEncoding.DecodeString`: padding required, CR and LF ignored anywhere,
//! non-zero trailing bits accepted, and the same error offsets.

/// Decodes, or returns Go's `CorruptInputError` offset.
pub(crate) fn decode_std(src: &[u8]) -> Result<Vec<u8>, usize> {
    let mut out = Vec::with_capacity(src.len() / 4 * 3);
    let mut si = 0;
    while si < src.len() {
        let (next, err) = quantum(src, si, &mut out);
        if let Some(offset) = err {
            return Err(offset);
        }
        si = next;
    }
    Ok(out)
}

fn value(c: u8) -> Option<u8> {
    match c {
        b'A'..=b'Z' => Some(c - b'A'),
        b'a'..=b'z' => Some(c - b'a' + 26),
        b'0'..=b'9' => Some(c - b'0' + 52),
        b'+' => Some(62),
        b'/' => Some(63),
        _ => None,
    }
}

fn is_newline(c: u8) -> bool {
    c == b'\n' || c == b'\r'
}

/// Go's `decodeQuantum`.
fn quantum(src: &[u8], mut si: usize, out: &mut Vec<u8>) -> (usize, Option<usize>) {
    let mut dbuf = [0u8; 4];
    let mut j = 0;
    let mut err = None;
    while j < 4 {
        if si == src.len() {
            if j == 0 {
                return (si, None);
            }
            // StdEncoding is padded, so any partial quantum is corrupt.
            return (si, Some(si - j));
        }
        let c = src[si];
        si += 1;
        if let Some(v) = value(c) {
            dbuf[j] = v;
            j += 1;
            continue;
        }
        if is_newline(c) {
            continue;
        }
        if c != b'=' {
            return (si, Some(si - 1));
        }
        match j {
            0 | 1 => return (si, Some(si - 1)),
            2 => {
                while si < src.len() && is_newline(src[si]) {
                    si += 1;
                }
                if si == src.len() {
                    return (si, Some(src.len()));
                }
                if src[si] != b'=' {
                    return (si, Some(si - 1));
                }
                si += 1;
            }
            _ => {}
        }
        while si < src.len() && is_newline(src[si]) {
            si += 1;
        }
        if si < src.len() {
            err = Some(si);
        }
        break;
    }
    let val =
        (dbuf[0] as u32) << 18 | (dbuf[1] as u32) << 12 | (dbuf[2] as u32) << 6 | dbuf[3] as u32;
    let bytes = [(val >> 16) as u8, (val >> 8) as u8, val as u8];
    out.extend_from_slice(&bytes[..j - 1]);
    (si, err)
}

#[cfg(test)]
mod tests {
    use super::decode_std;

    #[test]
    fn matches_go_std_encoding() {
        // Each row observed from Go's base64.StdEncoding.DecodeString.
        assert_eq!(decode_std(b"AA=="), Ok(vec![0]));
        assert_eq!(decode_std(b"AA==\n"), Ok(vec![0]));
        assert_eq!(decode_std(b"A\nA=="), Ok(vec![0]));
        assert_eq!(decode_std(b"AB=="), Ok(vec![0]));
        assert_eq!(decode_std(b"AA=\n="), Ok(vec![0]));
        assert_eq!(decode_std(b"AA"), Err(0));
        assert_eq!(decode_std(b"AA==x"), Err(4));
        assert_eq!(decode_std(b"AAA="), Ok(vec![0, 0]));
        assert_eq!(decode_std(b" AA=="), Err(0));
        assert_eq!(decode_std(b"AA==\r\n"), Ok(vec![0]));
        assert_eq!(decode_std(b"AA==AA=="), Err(4));
        assert_eq!(decode_std(b"=AAA"), Err(0));
        assert_eq!(decode_std(b"AAAA\n"), Ok(vec![0, 0, 0]));
        assert_eq!(decode_std(b"nope"), Ok(vec![0x9e, 0x8a, 0x5e]));
        assert_eq!(decode_std(b""), Ok(vec![]));
    }
}
