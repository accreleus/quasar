//! PyroWave frames as WebTransport datagram payloads (`protocol/codec-pyrowave.md` §Video
//! datagrams): the 12-byte PyroWave header, whole packets packed greedily, an oversized
//! block split into parts, and each datagram's coverage range.

/// The PyroWave header in front of every PyroWave datagram payload.
pub(crate) const HEADER: usize = 12;
const KIND_PACKETS: u8 = 0;
const KIND_PART: u8 = 1;
/// Wavelet levels at the bitstream pin (`Themaister/pyrowave@7fe0e266`).
const LEVELS: u32 = 5;

/// `N`: the block indices of a 4:2:0 frame (bitstream.md "Block index ordering"). The
/// browser's twin is `blockLayout` in `web/src/pyrowave/bitstream.ts`.
pub(crate) fn block_count(width: u32, height: u32) -> u32 {
    let align = |v: u32| v.div_ceil(32).max(4) * 32;
    let (aw, ah) = (align(width), align(height));
    let mut n = 0;
    for level in (0..LEVELS).rev() {
        let blocks = (aw >> (level + 1)).div_ceil(32) * (ah >> (level + 1)).div_ceil(32);
        let comps = if level == 0 { 1 } else { 3 };
        let bands = if level == LEVELS - 1 { 4 } else { 3 };
        n += comps * bands * blocks;
    }
    n
}

/// The 24-byte `PWV1Header` of `hello_ack.video.description_b64` (codec-pyrowave.md
/// §`hello_ack`): sRGB primaries and transfer, BT.709, centre siting, 4:2:0.
pub(crate) fn pwv1_header(width: u32, height: u32, fps: u32, full_range: bool) -> [u8; 24] {
    let mut h = [0u8; 24];
    h[0..4].copy_from_slice(b"PWV1");
    let w0 = (width - 1) & 0x3fff | ((height - 1) & 0x3fff) << 14;
    h[4..8].copy_from_slice(&w0.to_le_bytes());
    h[8..12].copy_from_slice(&(u32::from(!full_range) << 30).to_le_bytes());
    h[12..16].copy_from_slice(&fps.to_le_bytes());
    h[16..20].copy_from_slice(&1u32.to_le_bytes());
    h[20] = 8; // reference_bit_depth
    h[21] = 1; // header_version
    h
}

struct Packet<'a> {
    bytes: &'a [u8],
    /// `None` for the SOF.
    block: Option<u32>,
}

/// The frame's packets in order. Refuses what the packing rules cannot carry: a truncated
/// packet, a block index outside `[0, n)` or not after the one before, a SOF not first.
fn packets(frame: &[u8], n: u32) -> Result<Vec<Packet<'_>>, &'static str> {
    let word = |at: usize| u32::from_le_bytes(frame[at..at + 4].try_into().unwrap());
    let mut out = Vec::new();
    let mut at = 0;
    let mut last: Option<u32> = None;
    while at < frame.len() {
        if frame.len() - at < 8 {
            return Err("a truncated packet");
        }
        let w0 = word(at);
        if w0 >> 31 == 1 {
            if at != 0 {
                return Err("a start-of-frame header that is not first");
            }
            out.push(Packet {
                bytes: &frame[at..at + 8],
                block: None,
            });
            at += 8;
            continue;
        }
        let len = ((w0 >> 16) & 0xfff) as usize * 4;
        if len < 8 || frame.len() - at < len {
            return Err("a truncated block");
        }
        let index = word(at + 4) >> 8;
        if index >= n || last.is_some_and(|l| index <= l) {
            return Err("a block index out of range or out of order");
        }
        last = Some(index);
        out.push(Packet {
            bytes: &frame[at..at + len],
            block: Some(index),
        });
        at += len;
    }
    if out.first().is_none_or(|p| p.block.is_some()) {
        return Err("a frame without a start-of-frame header");
    }
    Ok(out)
}

fn header(kind: u8, part: (u8, u8), first: u32, end: u32) -> [u8; HEADER] {
    let mut h = [0u8; HEADER];
    h[0] = kind;
    h[1] = part.0;
    h[2] = part.1;
    h[4..8].copy_from_slice(&first.to_le_bytes());
    h[8..12].copy_from_slice(&end.to_le_bytes());
    h
}

/// One whole PyroWave frame (`pyrowaveenc` output: the SOF, then blocks by index) as the
/// payloads of its datagrams, in `frag_index` order. `room` is the payload room after the
/// 25-byte video header; `n` the frame's block count (`block_count`).
pub(crate) fn pack(frame: &[u8], n: u32, room: usize) -> Result<Vec<Vec<u8>>, &'static str> {
    let room = room
        .checked_sub(HEADER)
        .filter(|r| *r >= 8)
        .ok_or("no room")?;
    let packets = packets(frame, n)?;

    // A datagram before its range's end is known: the next datagram's first block sets it.
    enum Draft<'a> {
        Packets(u32, Vec<&'a [u8]>, usize),
        Parts(u32, &'a [u8]),
    }
    let mut drafts: Vec<Draft> = Vec::new();
    for p in &packets {
        let start = p.block.unwrap_or(0);
        let len = p.bytes.len();
        if len > room {
            drafts.push(Draft::Parts(start, p.bytes));
            continue;
        }
        match drafts.last_mut() {
            Some(Draft::Packets(_, list, used)) if *used + len <= room => {
                list.push(p.bytes);
                *used += len;
            }
            // Datagram 0 always starts at 0: it covers the zero blocks before its first.
            _ => drafts.push(Draft::Packets(
                if drafts.is_empty() { 0 } else { start },
                vec![p.bytes],
                len,
            )),
        }
    }

    let firsts: Vec<u32> = drafts
        .iter()
        .map(|d| match d {
            Draft::Packets(first, ..) | Draft::Parts(first, _) => *first,
        })
        .collect();
    let mut out = Vec::new();
    for (i, draft) in drafts.iter().enumerate() {
        let end = firsts.get(i + 1).copied().unwrap_or(n);
        match draft {
            Draft::Packets(first, list, used) => {
                let mut d = Vec::with_capacity(HEADER + used);
                d.extend_from_slice(&header(KIND_PACKETS, (0, 0), *first, end));
                list.iter().for_each(|b| d.extend_from_slice(b));
                out.push(d);
            }
            Draft::Parts(first, bytes) => {
                let count = u8::try_from(bytes.len().div_ceil(room))
                    .map_err(|_| "a block over 255 parts")?;
                for (index, chunk) in bytes.chunks(room).enumerate() {
                    let mut d = Vec::with_capacity(HEADER + chunk.len());
                    d.extend_from_slice(&header(KIND_PART, (index as u8, count), *first, end));
                    d.extend_from_slice(chunk);
                    out.push(d);
                }
            }
        }
    }
    Ok(out)
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    /// A SOF for `w`x`h`; `total_blocks` is informational and left 0.
    pub(crate) fn sof(w: u32, h: u32) -> Vec<u8> {
        let w0 = (1u32 << 31) | (w - 1) | (h - 1) << 14;
        let mut v = w0.to_le_bytes().to_vec();
        v.extend(0u32.to_le_bytes());
        v
    }

    /// A block of `words` u32 words (at least 2) at `index`, its body a counting pattern.
    pub(crate) fn block(index: u32, words: u32) -> Vec<u8> {
        let mut v = (words << 16 | 1).to_le_bytes().to_vec();
        v.extend((index << 8).to_le_bytes());
        v.extend((0..(words - 2) * 4).map(|i| (i ^ index) as u8));
        v
    }

    pub(crate) fn frame(w: u32, h: u32, blocks: &[(u32, u32)]) -> Vec<u8> {
        let mut f = sof(w, h);
        for &(i, words) in blocks {
            f.extend(block(i, words));
        }
        f
    }

    fn le(d: &[u8], at: usize) -> u32 {
        u32::from_le_bytes(d[at..at + 4].try_into().unwrap())
    }

    /// Joins every datagram back into the frame and checks the coverage partitions [0, n).
    fn rejoin(dgrams: &[Vec<u8>], n: u32) -> Vec<u8> {
        let mut out = Vec::new();
        let mut next = 0;
        for d in dgrams {
            let (first, end) = (le(d, 4), le(d, 8));
            if d[0] == KIND_PART && d[1] > 0 {
                assert_eq!(next, end, "a later part repeats its block's range");
            } else {
                assert_eq!(first, next, "contiguous");
                next = end;
            }
            assert!(first <= end && end <= n);
            out.extend_from_slice(&d[HEADER..]);
        }
        assert_eq!(next, n, "the last range ends at N");
        out
    }

    #[test]
    fn n_matches_the_browser_layout() {
        // The same table as web/src/pyrowave/bitstream.test.ts "blockLayout".
        for (w, h, n) in [(200, 114, 72), (320, 192, 138), (1920, 1080, 3261)] {
            assert_eq!(block_count(w, h), n, "{w}x{h}");
        }
        assert_eq!(
            block_count(2, 2),
            block_count(128, 128),
            "aligned up to 128"
        );
    }

    #[test]
    fn packets_fill_datagrams_greedily_and_cover_zero_blocks_between() {
        let n = block_count(320, 192);
        let f = frame(320, 192, &[(3, 100), (4, 100), (9, 100), (20, 2)]);
        let room = HEADER + 900;
        let d = pack(&f, n, room).unwrap();
        // SOF (8) + 400 + 400 fits 900; block 9 (400) starts the next with block 20.
        assert_eq!(d.len(), 2);
        assert_eq!(
            (le(&d[0], 4), le(&d[0], 8)),
            (0, 9),
            "zeros 0..3 and 5..9 are covered"
        );
        assert_eq!((le(&d[1], 4), le(&d[1], 8)), (9, n));
        assert!(d.iter().all(|d| d.len() <= room && d[0] == KIND_PACKETS));
        assert_eq!(rejoin(&d, n), f);
    }

    #[test]
    fn a_block_that_fits_an_empty_datagram_is_never_split() {
        let n = block_count(320, 192);
        let room = HEADER + 400;
        let f = frame(320, 192, &[(0, 50), (1, 100)]);
        let d = pack(&f, n, room).unwrap();
        assert_eq!(
            d.len(),
            2,
            "8 + 200 then 400 alone: exactly fills the second"
        );
        assert_eq!(d[1].len(), room);
        assert_eq!(rejoin(&d, n), f);
    }

    #[test]
    fn an_oversized_block_goes_alone_in_filled_parts_with_its_own_range() {
        let n = block_count(320, 192);
        let room = HEADER + 400;
        let f = frame(320, 192, &[(2, 10), (5, 250), (6, 10)]);
        let d = pack(&f, n, room).unwrap();
        // [SOF + block 2] [part 0: 400][part 1: 400][part 2: 200] [block 6]
        assert_eq!(d.len(), 5);
        assert_eq!((le(&d[0], 4), le(&d[0], 8)), (0, 5));
        for (j, part) in d[1..4].iter().enumerate() {
            assert_eq!((part[0], part[1], part[2]), (KIND_PART, j as u8, 3));
            assert_eq!(
                (le(part, 4), le(part, 8)),
                (5, 6),
                "every part: the same range"
            );
        }
        assert_eq!(d[1].len(), room);
        assert_eq!(d[2].len(), room);
        assert_eq!(d[3].len(), HEADER + 200);
        assert_eq!((le(&d[4], 4), le(&d[4], 8)), (6, n));
        assert_eq!(rejoin(&d, n), f);
    }

    #[test]
    fn block_zero_oversized_leaves_datagram_zero_holding_only_the_sof() {
        let n = block_count(320, 192);
        let f = frame(320, 192, &[(0, 250)]);
        let d = pack(&f, n, HEADER + 400).unwrap();
        assert_eq!(d[0].len(), HEADER + 8);
        assert_eq!((le(&d[0], 4), le(&d[0], 8)), (0, 0), "the one empty range");
        assert_eq!((le(&d[1], 4), le(&d[1], 8)), (0, n));
        assert_eq!(rejoin(&d, n), f);
    }

    #[test]
    fn an_all_zero_frame_is_one_sof_covering_everything() {
        let n = block_count(320, 192);
        let d = pack(&sof(320, 192), n, 1123).unwrap();
        assert_eq!(d.len(), 1);
        assert_eq!((le(&d[0], 4), le(&d[0], 8)), (0, n));
    }

    #[test]
    fn frames_the_rules_cannot_carry_are_refused() {
        let n = block_count(320, 192);
        assert!(pack(&frame(320, 192, &[(4, 2), (4, 2)]), n, 1123).is_err());
        assert!(pack(&frame(320, 192, &[(4, 2), (3, 2)]), n, 1123).is_err());
        assert!(pack(&frame(320, 192, &[(n, 2)]), n, 1123).is_err());
        assert!(pack(&block(0, 2), n, 1123).is_err(), "no SOF");
        let mut cut = frame(320, 192, &[(0, 10)]);
        cut.truncate(cut.len() - 4);
        assert!(pack(&cut, n, 1123).is_err());
    }

    #[test]
    fn the_golden_fixture_packs_and_rejoins_byte_for_byte() {
        let raw = std::fs::read(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../testdata/pyrowave/detail_320x192_xhi.pwv1"
        ))
        .unwrap();
        let f = &raw[28..];
        let n = block_count(320, 192);
        // The 1200-byte budget's room, and one small enough to split the largest blocks.
        for room in [1123, 200] {
            let d = pack(f, n, room).unwrap();
            assert!(d.iter().all(|d| d.len() <= room));
            assert_eq!(rejoin(&d, n), f, "room {room}");
        }
    }

    #[test]
    fn the_description_is_the_fixtures_header_for_the_same_picture() {
        let raw = std::fs::read(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../testdata/pyrowave/detail_320x192_hi.pwv1"
        ))
        .unwrap();
        assert_eq!(pwv1_header(320, 192, 60, false), raw[..24]);
    }
}
