//! External-resolution **rungs** — the agent-side mirror of the control plane's
//! `control-plane/internal/profile/rungs.go` (adaptive-external-resolution, spec D4/D5).
//!
//! A "rung" is a legal ENCODED (external) frame size a running session may be stepped to
//! live. The ladder is derived from the session's LAUNCH size: the reduced aspect ratio of
//! the launch WxH selects a family, and the available rungs are that family filtered to
//! entries no larger than the launch size in BOTH dimensions, descending, with the launch
//! size itself always first (a launch size need not be a canonical rung; it is still,
//! trivially, a rung of itself).
//!
//! ## Why the table is duplicated
//!
//! The control plane is the validator (`PATCH /v1/sessions/{id}/display` rejects an
//! off-ladder pair before it reaches a host). The agent needs the same table to (1)
//! advertise the ladder to the guest app as `wl_output` modes (compositor `mode-ladder`
//! property), and (2) a defensive second wire check, so a CP/agent skew surfaces as
//! `ack{ok:false}` rather than a silently wrong frame size.
//!
//! [`tests::table_matches_the_control_plane`] pins the exact values against the CP's
//! `rungs_test.go` cases. If you change one table, change both.
//!
//! ## CP's stance on the agent's wire check (deliberate, tracked)
//!
//! `rungs.go` says the agent's wire validation should be the weaker structural check
//! (even, ≥ 16 px, ≤ launch) and that the rung check must never move there. This agent
//! nevertheless checks [`is_rung`] on the wire as a second gate — safe only while the two
//! tables are pinned equal, which the equality test enforces.
//!
//! **When operator-defined ("custom") rungs land, this must relax to the structural
//! check** — custom rungs are unknown to this compiled table, so `is_rung` would reject
//! every one of them. Drop the `is_rung` arm from `runner::validate_display_update` (keep
//! even/≥16/≤launch, enforced regardless by `ScaleStage::set_size`) and leave this table
//! serving only the `mode-ladder` advertisement.

/// A legal external (encoded) size.
pub type Rung = (i32, i32);

/// Canonical families, each DESCENDING. Membership is by reduced aspect ratio of the
/// launch size, so a 16:9 profile of any size draws from [`RUNGS_16X9`].
pub const RUNGS_16X9: &[Rung] = &[
    (3840, 2160),
    (2560, 1440),
    (1920, 1080),
    (1600, 900),
    (1280, 720),
];
pub const RUNGS_16X10: &[Rung] = &[
    (2560, 1600),
    (1920, 1200),
    (1680, 1050),
    (1440, 900),
    (1280, 800),
];
pub const RUNGS_21X9: &[Rung] = &[(3440, 1440), (2560, 1080)];
pub const RUNGS_4X3: &[Rung] = &[(1600, 1200), (1280, 960), (1024, 768)];

/// An aspect ratio in lowest terms.
type Ratio = (i32, i32);

/// One family: the reduced ratios that select it, and the rungs it offers.
type Family = (&'static [Ratio], &'static [Rung]);

/// Accepted reduced ratios → rung table. Membership is a SET of ratios per family, not a
/// single ratio: "21:9" is a marketing label, not an arithmetic one (3440x1440 reduces to
/// 43:18, 2560x1080 to 64:27) so a single reduced ratio would split the family's own rungs.
const FAMILIES: &[Family] = &[
    (&[(16, 9)], RUNGS_16X9),
    (&[(8, 5)], RUNGS_16X10),
    // 43:18 = 3440x1440, 64:27 = 2560x1080, 7:3 = a nominal 21:9 size.
    (&[(43, 18), (64, 27), (7, 3)], RUNGS_21X9),
    (&[(4, 3)], RUNGS_4X3),
];

/// The canonical rung family for a size's reduced aspect ratio, descending.
///
/// An aspect ratio outside the four known families has no ladder at all: the family is
/// the size itself. Deliberate — inventing rungs for an unknown ratio would hand the
/// guest app sizes the operator never certified.
pub fn family(w: i32, h: i32) -> Vec<Rung> {
    if w <= 0 || h <= 0 {
        return vec![(w, h)];
    }
    let r = reduce(w, h);
    for (ratios, rungs) in FAMILIES {
        if ratios.contains(&r) {
            return rungs.to_vec();
        }
    }
    vec![(w, h)]
}

/// The ladder a session launched at `w` x `h` may be stepped along: its [`family`]
/// filtered to entries ≤ `w` AND ≤ `h`, descending, with `(w, h)` itself guaranteed to be
/// the first entry.
///
/// The launch size is prepended rather than assumed present because a profile can carry a
/// non-canonical size (2000x1125 is exactly 16:9 but is not a rung) and the session must
/// always be able to return to it. A canonical launch size is already the largest
/// surviving entry, so no duplicate is produced.
pub fn available_rungs(w: i32, h: i32) -> Vec<Rung> {
    let mut out = vec![(w, h)];
    for (rw, rh) in family(w, h) {
        if rw > w || rh > h || (rw == w && rh == h) {
            continue;
        }
        out.push((rw, rh));
    }
    out
}

/// Whether `w` x `h` is a legal external size for a session launched at
/// `launch_w` x `launch_h`.
pub fn is_rung(w: i32, h: i32, launch_w: i32, launch_h: i32) -> bool {
    available_rungs(launch_w, launch_h).contains(&(w, h))
}

/// The compositor's `mode-ladder` wire form: `"1920x1080,1600x900,1280x720"`.
pub fn format_ladder(rungs: &[Rung]) -> String {
    rungs
        .iter()
        .map(|(w, h)| format!("{w}x{h}"))
        .collect::<Vec<_>>()
        .join(",")
}

/// A size divided by its greatest common divisor — the aspect ratio in lowest terms.
fn reduce(w: i32, h: i32) -> (i32, i32) {
    let g = gcd(w, h);
    if g == 0 {
        return (w, h);
    }
    (w / g, h / g)
}

fn gcd(mut a: i32, mut b: i32) -> i32 {
    while b != 0 {
        let t = a % b;
        a = b;
        b = t;
    }
    a.abs()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Equality gate with `control-plane/internal/profile/rungs_test.go` — cases
    /// transcribed from that file. A divergence means the CP accepts a
    /// `stream_width/height` this agent rejects, or vice versa.
    #[test]
    fn table_matches_the_control_plane() {
        // TestFamily
        for (w, h, want) in [
            (1920, 1080, RUNGS_16X9),
            (3840, 2160, RUNGS_16X9),
            // A non-canonical 16:9 size still selects the family.
            (2000, 1125, RUNGS_16X9),
            (1920, 1200, RUNGS_16X10),
            (3440, 1440, RUNGS_21X9),
            (2560, 1080, RUNGS_21X9),
            (1280, 960, RUNGS_4X3),
        ] {
            assert_eq!(family(w, h), want.to_vec(), "family({w},{h})");
        }
        // Unknown ratios get no ladder at all — only themselves.
        for (w, h) in [(1280, 1024), (1080, 1920), (3840, 1080), (0, 0)] {
            assert_eq!(family(w, h), vec![(w, h)], "family({w},{h})");
        }

        // TestAvailableRungs
        for (w, h, want) in [
            (1920, 1080, vec![(1920, 1080), (1600, 900), (1280, 720)]),
            (
                2560,
                1440,
                vec![(2560, 1440), (1920, 1080), (1600, 900), (1280, 720)],
            ),
            (
                3840,
                2160,
                vec![
                    (3840, 2160),
                    (2560, 1440),
                    (1920, 1080),
                    (1600, 900),
                    (1280, 720),
                ],
            ),
            // 720p is its own floor.
            (1280, 720, vec![(1280, 720)]),
            (
                1920,
                1200,
                vec![(1920, 1200), (1680, 1050), (1440, 900), (1280, 800)],
            ),
            (3440, 1440, vec![(3440, 1440), (2560, 1080)]),
            (1600, 1200, vec![(1600, 1200), (1280, 960), (1024, 768)]),
            // Non-canonical 16:9 launch: itself first, then the family below it.
            (
                2000,
                1125,
                vec![(2000, 1125), (1920, 1080), (1600, 900), (1280, 720)],
            ),
            // Below every canonical rung of its family ⇒ only itself.
            (640, 360, vec![(640, 360)]),
            (1280, 1024, vec![(1280, 1024)]),
        ] {
            let got = available_rungs(w, h);
            assert_eq!(got, want, "available_rungs({w},{h})");
            assert_eq!(got[0], (w, h), "the launch size must come first");
        }

        // TestIsRung
        for (w, h, lw, lh, want) in [
            (1920, 1080, 1920, 1080, true),
            (1600, 900, 1920, 1080, true),
            (1280, 720, 1920, 1080, true),
            // Above the launch size.
            (2560, 1440, 1920, 1080, false),
            // Not on the ladder.
            (1366, 768, 1920, 1080, false),
            // Another family's rung.
            (1440, 900, 1920, 1080, false),
            (1440, 900, 1920, 1200, true),
            (2000, 1125, 2000, 1125, true),
            (1280, 720, 1280, 1024, false),
            (1280, 1024, 1280, 1024, true),
            // Transposed dims.
            (1080, 1920, 1920, 1080, false),
            (0, 0, 1920, 1080, false),
        ] {
            assert_eq!(is_rung(w, h, lw, lh), want, "is_rung({w},{h},{lw},{lh})");
        }
    }

    #[test]
    fn available_rungs_are_strictly_descending_and_unique() {
        for (w, h) in [
            (3840, 2160),
            (2560, 1440),
            (1920, 1080),
            (2000, 1125),
            (1920, 1200),
            (3440, 1440),
            (1600, 1200),
        ] {
            let got = available_rungs(w, h);
            for i in 1..got.len() {
                assert!(
                    got[i].0 < got[i - 1].0 && got[i].1 < got[i - 1].1,
                    "available_rungs({w},{h}) not strictly descending at {i}: {got:?}"
                );
            }
        }
    }

    // Every rung is even in both axes — 4:2:0 chroma cannot represent an odd plane, and
    // `ScaleStage::set_size` rejects odd dimensions outright.
    #[test]
    fn every_rung_is_even() {
        for table in [RUNGS_16X9, RUNGS_16X10, RUNGS_21X9, RUNGS_4X3] {
            for (w, h) in table {
                assert_eq!((w % 2, h % 2), (0, 0), "{w}x{h} is not even");
            }
        }
    }

    #[test]
    fn ladder_formats_for_the_compositor() {
        assert_eq!(
            format_ladder(&available_rungs(1920, 1080)),
            "1920x1080,1600x900,1280x720"
        );
        assert_eq!(format_ladder(&available_rungs(1280, 720)), "1280x720");
        assert_eq!(format_ladder(&[]), "");
    }
}
