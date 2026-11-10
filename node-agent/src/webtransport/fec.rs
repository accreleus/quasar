//! Forward error correction on the datagram path, scheme `xor`
//! (`protocol/transport-webtransport.md` §FEC): how much the measured loss pays for, which
//! fragments each repair datagram protects, and its bytes. No I/O.

use std::time::{Duration, Instant};

use bytes::{BufMut, Bytes, BytesMut};

use super::media::{HEADER as VIDEO_HEADER, MAX_DATAGRAM};

const CHANNEL_FEC: u8 = 0x04;
const SCHEME_XOR: u8 = 0x01;
const HEADER: usize = 14;
/// §FEC: one repair datagram protects 2 … 48 fragments.
const MAX_COUNT: usize = 48;

/// Level `k` is entered at `k` × this much loss and left under `k − ½` of it.
const LOSS_STEP: f64 = 0.01;
/// Repair bytes per video byte, per level: 5 × the loss. A group then expects 0.2 losses,
/// so about four lost fragments in five are alone in their group and rebuilt.
const OVERHEAD_STEP: f64 = 0.05;
/// 25 %, groups of four. Groups of three rebuild 81 % at 10 % loss where four rebuild
/// 73 %, for another 8 % of the rate.
const MAX_LEVEL: u8 = 5;
/// A level is left one step at a time, each after this long under its floor.
const CALM: Duration = Duration::from_secs(2);

/// One repair datagram's fragments: `first + i × stride` for `i` in `0 … count − 1`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct Group {
    first: u16,
    count: u8,
    stride: u8,
}

impl Group {
    fn members(self) -> impl Iterator<Item = usize> {
        (0..self.count as usize).map(move |i| self.first as usize + i * self.stride as usize)
    }
}

/// The groups protecting a frame whose fragments carry `lens` payload bytes, spending
/// `overhead` repair bytes per video byte. `carry` is the budget (bytes) one frame leaves
/// the next: a frame too small to pay for a repair saves toward one.
///
/// The frame is cut into blocks of at most 48 fragments. A block that pays for `r` repairs
/// gets `r` interleaved groups (`stride` = `r`): they cover each fragment once, and any
/// burst of up to `r` fragments falls one to a group.
fn layout(lens: &[usize], overhead: f64, carry: &mut f64) -> Vec<Group> {
    let mut groups = Vec::new();
    if overhead <= 0.0 {
        return groups;
    }
    let blocks = lens.len().div_ceil(MAX_COUNT);
    for b in 0..blocks {
        let (start, end) = (b * lens.len() / blocks, (b + 1) * lens.len() / blocks);
        let block = &lens[start..end];
        let cost = (HEADER + block.iter().max().copied().unwrap_or(0)) as f64;
        let video: usize = block.iter().map(|len| VIDEO_HEADER + len).sum();
        let budget = video as f64 * overhead + *carry;
        // §FEC: a group has at least two fragments.
        let repairs = ((budget / cost) as usize).min(block.len() / 2);
        // At most one repair is saved: a still picture must not bank a burst.
        *carry = (budget - repairs as f64 * cost).min(MAX_DATAGRAM as f64);
        groups.extend((0..repairs).map(|j| Group {
            first: (start + j) as u16,
            count: (block.len() - j).div_ceil(repairs) as u8,
            stride: repairs as u8,
        }));
    }
    groups
}

/// The `0x04` datagram for `group` of a frame's video datagrams (§FEC).
fn repair(group: Group, fragments: &[Bytes], pkt_seq: u32) -> Bytes {
    let payloads = || group.members().map(|i| &fragments[i][VIDEO_HEADER..]);
    let longest = payloads().map(<[u8]>::len).max().unwrap_or(0);
    let mut d = BytesMut::with_capacity(HEADER + longest);
    d.put_u8(CHANNEL_FEC);
    d.put_u32_le(pkt_seq);
    d.put_u8(SCHEME_XOR);
    d.put_slice(&fragments[0][7..9]); // frame_seq
    d.put_u16_le(group.first);
    d.put_u8(group.count);
    d.put_u8(group.stride);
    d.put_u16_le(payloads().fold(0, |x, p| x ^ p.len() as u16));
    d.put_bytes(0, longest);
    for p in payloads() {
        for (out, byte) in d[HEADER..].iter_mut().zip(p) {
            *out ^= byte;
        }
    }
    d.freeze()
}

/// How much FEC one client is sent, from the loss its feedback reports.
#[derive(Default)]
pub(crate) struct Policy {
    level: u8,
    calm_since: Option<Instant>,
    carry: f64,
}

impl Policy {
    /// The path's loss fraction, as the estimator measured it over its window. More loss
    /// raises the level at once; less lowers it a step per [`CALM`], so a spell between two
    /// bursts does not switch it off.
    pub(crate) fn observe(&mut self, loss: f64, now: Instant) {
        let steps = loss / LOSS_STEP;
        let enter = (steps as u8).min(MAX_LEVEL);
        let keep = ((steps + 0.5) as u8).min(MAX_LEVEL);
        if enter > self.level {
            self.level = enter;
            self.calm_since = None;
        } else if keep < self.level {
            let since = *self.calm_since.get_or_insert(now);
            if now.saturating_duration_since(since) >= CALM {
                self.level -= 1;
                self.calm_since = Some(now);
            }
        } else {
            self.calm_since = None;
        }
    }

    /// Repair bytes sent per video byte; 0 is off.
    pub(crate) fn overhead(&self) -> f64 {
        f64::from(self.level) * OVERHEAD_STEP
    }

    /// The repair datagrams for one frame's video datagrams (in `frag_index` order),
    /// numbered from `*pkt_seq`: sent after them, never retransmitted.
    pub(crate) fn protect(&mut self, fragments: &[Bytes], pkt_seq: &mut u32) -> Vec<Bytes> {
        let lens: Vec<usize> = fragments
            .iter()
            .map(|d| d.len().saturating_sub(VIDEO_HEADER))
            .collect();
        layout(&lens, self.overhead(), &mut self.carry)
            .into_iter()
            .map(|group| {
                let d = repair(group, fragments, *pkt_seq);
                *pkt_seq = pkt_seq.wrapping_add(1);
                d
            })
            .collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A frame's `0x01` datagrams: `n` fragments of `room` payload bytes, the last of `last`.
    fn frame(frame_seq: u16, n: usize, room: usize, last: usize) -> Vec<Bytes> {
        (0..n)
            .map(|i| {
                let len = if i + 1 == n { last } else { room };
                let mut d = BytesMut::with_capacity(VIDEO_HEADER + len);
                d.put_u8(0x01);
                d.put_u32_le(i as u32);
                d.put_u8(1); // a keyframe: a client starts at one
                d.put_u8(0);
                d.put_u16_le(frame_seq);
                d.put_u16_le(i as u16);
                d.put_u16_le(n as u16);
                d.put_bytes(0, 12);
                d.extend((0..len).map(|b| (b * 31 + i * 17 + frame_seq as usize) as u8));
                d.freeze()
            })
            .collect()
    }

    fn group_of(repair: &[u8]) -> Group {
        Group {
            first: u16::from_le_bytes([repair[8], repair[9]]),
            count: repair[10],
            stride: repair[11],
        }
    }

    /// §FEC's rule, written from the contract and not from `repair`: fragment `k`'s payload
    /// is the repair payload XOR every other protected payload (zero-padded), truncated to
    /// `length_xor` XOR every other protected length.
    fn rebuild(repair: &[u8], fragments: &[Bytes], k: usize) -> Vec<u8> {
        let mut payload = repair[HEADER..].to_vec();
        let mut len = u16::from_le_bytes([repair[12], repair[13]]);
        for i in group_of(repair).members().filter(|&i| i != k) {
            let other = &fragments[i][VIDEO_HEADER..];
            len ^= other.len() as u16;
            for (out, byte) in payload.iter_mut().zip(other) {
                *out ^= byte;
            }
        }
        payload.truncate(len as usize);
        payload
    }

    #[test]
    fn every_group_rebuilds_any_one_of_its_fragments_byte_for_byte() {
        let mut groups = 0;
        for n in [2, 3, 5, 9, 48, 49, 97, 200] {
            // A shorter last fragment, a one-byte one, and a full one.
            for last in [300, 1, 1123] {
                for level in 1..=MAX_LEVEL {
                    let fragments = frame(n as u16, n, 1123, last);
                    let mut policy = Policy {
                        level,
                        carry: 1137.0, // one repair saved: the small frames get theirs
                        ..Policy::default()
                    };
                    let mut pkt_seq = 900;
                    let repairs = policy.protect(&fragments, &mut pkt_seq);
                    assert_eq!(pkt_seq, 900 + repairs.len() as u32, "one pkt_seq each");
                    let mut covered = vec![0; n];
                    for (i, r) in repairs.iter().enumerate() {
                        assert_eq!(r[0], 0x04);
                        assert_eq!(r[1..5], (900 + i as u32).to_le_bytes());
                        assert_eq!(r[5], 0x01, "scheme xor");
                        assert_eq!(r[6..8], (n as u16).to_le_bytes(), "frame_seq");
                        assert!(r.len() <= 1148 - 11, "no longer than a fragment");
                        let g = group_of(r);
                        assert!((2..=48).contains(&g.count) && g.stride >= 1, "{g:?}");
                        for k in g.members() {
                            covered[k] += 1;
                            assert_eq!(
                                rebuild(r, &fragments, k),
                                fragments[k][VIDEO_HEADER..],
                                "n={n} last={last} {g:?} fragment {k}"
                            );
                        }
                        groups += 1;
                    }
                    assert!(covered.iter().all(|&c| c <= 1), "a fragment in two groups");
                }
            }
        }
        assert!(groups > 200, "the cases produced groups ({groups})");
    }

    #[test]
    fn a_blocks_groups_cover_every_fragment_once_and_split_a_burst_of_their_number() {
        // 20 full fragments at 15 %: 3444 bytes of budget pay for three repairs.
        let groups = layout(&[1123; 20], 0.15, &mut 0.0);
        assert_eq!(
            groups,
            [(0, 7), (1, 7), (2, 6)].map(|(first, count)| Group {
                first,
                count,
                stride: 3
            })
        );
        let owner = |k: usize| groups.iter().position(|g| g.members().any(|m| m == k));
        assert!((0..20).all(|k| owner(k).is_some()));
        for burst in 0..18 {
            let hit: std::collections::HashSet<_> = (burst..burst + 3).map(owner).collect();
            assert_eq!(hit.len(), 3, "a burst of three at {burst} shares a group");
        }
        // A frame over 48 fragments is cut into even blocks, each with its own groups.
        let groups = layout(&[1123; 100], 0.05, &mut 0.0);
        assert!(groups.iter().all(|g| g.count <= 48));
        let covered: usize = groups.iter().map(|g| g.count as usize).sum();
        assert_eq!(covered, 100);
    }

    #[test]
    fn repairs_cost_no_more_than_the_overhead_allows_plus_one_datagram() {
        for level in 1..=MAX_LEVEL {
            let mut policy = Policy {
                level,
                ..Policy::default()
            };
            let (mut video, mut fec, mut pkt_seq) = (0usize, 0usize, 0);
            // Small frames, the common delta sizes, and a keyframe; the last fragment short.
            for (i, n) in [1, 2, 4, 9, 40, 3, 300, 2, 1, 17]
                .into_iter()
                .cycle()
                .take(500)
                .enumerate()
            {
                let fragments = frame(i as u16, n, 1123, 200);
                video += fragments.iter().map(Bytes::len).sum::<usize>();
                let repairs = policy.protect(&fragments, &mut pkt_seq);
                assert!(n > 1 || repairs.is_empty(), "one fragment has no group");
                fec += repairs.iter().map(Bytes::len).sum::<usize>();
            }
            let allowed = video as f64 * policy.overhead();
            // Never over by more than the one saved repair, and the budget is spent.
            assert!(
                (fec as f64) <= allowed + MAX_DATAGRAM as f64 && fec as f64 >= 0.9 * allowed,
                "level {level}: {fec} repair bytes for {allowed:.0} allowed"
            );
        }
        let mut off = Policy::default();
        assert!(off.protect(&frame(0, 40, 1123, 200), &mut 0).is_empty());
    }

    #[test]
    fn the_level_follows_the_loss_up_at_once_and_down_a_step_per_calm_spell() {
        let start = Instant::now();
        let mut policy = Policy::default();
        // (ms, loss, overhead after)
        for (ms, loss, overhead) in [
            (0, 0.0, 0.0),
            (50, 0.009, 0.0),    // under 1 %: off
            (100, 0.012, 0.05),  // on at 1 %
            (150, 0.006, 0.05),  // over half a step: held
            (200, 0.034, 0.15),  // up at once, past a level
            (250, 0.5, 0.25),    // capped
            (300, 0.046, 0.25),  // inside the hold band
            (350, 0.02, 0.25),   // under it, but not for long enough
            (2300, 0.02, 0.25),  //
            (2350, 0.02, 0.20),  // 2 s under: one step
            (2400, 0.02, 0.20),  //
            (3000, 0.036, 0.20), // back inside the band: the spell ends
            (4400, 0.0, 0.20),   //
            (6399, 0.0, 0.20),   //
            (6400, 0.0, 0.15),   // 2 s from the spell's start
            (8400, 0.0, 0.10),
            (10400, 0.0, 0.05),
            (12400, 0.0, 0.0),
            (14400, 0.0, 0.0),
        ] {
            policy.observe(loss, start + Duration::from_millis(ms));
            assert!(
                (policy.overhead() - overhead).abs() < 1e-9,
                "at {ms} ms, loss {loss}: {} not {overhead}",
                policy.overhead()
            );
        }
    }

    /// The vectors the web client's decoder is held to (`pipeline.test.ts`, "the agent's
    /// vectors"): one frame and its repair datagrams at three overheads, as hex.
    /// `QUASAR_UPDATE_GOLDEN=1` rewrites the file.
    #[test]
    fn the_shared_vectors_are_what_this_module_sends() {
        let hex = |d: &Bytes| d.iter().map(|b| format!("{b:02x}")).collect::<String>();
        // Seven fragments of 40 payload bytes, the last of 13.
        let fragments = frame(4711, 7, 40, 13);
        let cases: Vec<_> = [0.15, 0.26, 0.40]
            .into_iter()
            .map(|overhead| {
                let lens: Vec<usize> = fragments.iter().map(|d| d.len() - VIDEO_HEADER).collect();
                let repairs: Vec<_> = layout(&lens, overhead, &mut 0.0)
                    .into_iter()
                    .enumerate()
                    .map(|(i, g)| hex(&repair(g, &fragments, 7 + i as u32)))
                    .collect();
                serde_json::json!({"overhead": overhead, "repairs": repairs})
            })
            .collect();
        let made = serde_json::to_string_pretty(&serde_json::json!({
            "about": "transport-webtransport.md §FEC, scheme xor: one frame's 0x01 datagrams and \
                      the 0x04 repair datagrams the node agent sends for them, hex. Written by \
                      node-agent/src/webtransport/fec.rs (QUASAR_UPDATE_GOLDEN=1), read by \
                      web/src/webtransport/pipeline.test.ts.",
            "fragments": fragments.iter().map(hex).collect::<Vec<_>>(),
            "cases": cases,
        }))
        .unwrap()
            + "\n";
        let path = concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../testdata/webtransport/fec-xor.json"
        );
        if std::env::var_os("QUASAR_UPDATE_GOLDEN").is_some() {
            std::fs::write(path, &made).unwrap();
        }
        let held = std::fs::read_to_string(path)
            .unwrap_or_else(|e| panic!("{path}: {e} (QUASAR_UPDATE_GOLDEN=1 writes it)"));
        assert_eq!(
            held, made,
            "{path} is stale (QUASAR_UPDATE_GOLDEN=1 rewrites it)"
        );
        let counts: Vec<usize> = cases
            .iter()
            .map(|c| c["repairs"].as_array().unwrap().len())
            .collect();
        assert_eq!(
            counts,
            [1, 2, 3],
            "one group, then two and three interleaved"
        );
    }
}
