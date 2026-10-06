//! Whether a console session is displaying (CONTEXT.md "Displaying"): its container is
//! alive, its desktop holds DRM master on the console card, and a framebuffer is on the
//! connector's primary plane. A still desktop is displaying; there is no frame counter.
//! The container's exit ends the session before any reading, so the facts here are the
//! card's.

/// What the agent observed about one console session.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct DisplayFacts {
    /// Some client other than the agent holds DRM master on the console card.
    pub master_held: bool,
    /// The connector's CRTC scans out a framebuffer.
    pub framebuffer: bool,
    /// The connector's current mode, when it has one.
    pub mode: Option<ScanoutMode>,
}

/// The connector's current mode, as the CRTC reports it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ScanoutMode {
    pub width: u16,
    pub height: u16,
    pub refresh_millihz: u32,
}

/// A fact the verdict needed and did not see.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Missing {
    Master,
    Framebuffer,
}

impl Missing {
    pub fn describe(self) -> &'static str {
        match self {
            Missing::Master => "the desktop does not hold the display (no DRM master)",
            Missing::Framebuffer => "nothing is scanned out on the console connector",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Verdict {
    Displaying(Option<ScanoutMode>),
    NotDisplaying(Vec<Missing>),
}

pub fn verdict(facts: &DisplayFacts) -> Verdict {
    let missing: Vec<Missing> = [
        (facts.master_held, Missing::Master),
        (facts.framebuffer, Missing::Framebuffer),
    ]
    .into_iter()
    .filter_map(|(seen, missing)| (!seen).then_some(missing))
    .collect();
    if missing.is_empty() {
        Verdict::Displaying(facts.mode)
    } else {
        Verdict::NotDisplaying(missing)
    }
}

/// The display facts of `connector` on `card_node` (see `capacity::connector_scanout`
/// for when it is safe to read).
pub fn read_drm_facts(card_node: &std::path::Path, connector: &str) -> DisplayFacts {
    let scanout = crate::capacity::connector_scanout(card_node, connector);
    DisplayFacts {
        master_held: scanout.master_held,
        framebuffer: scanout.framebuffer,
        mode: scanout.mode.map(|mode| ScanoutMode {
            width: mode.width,
            height: mode.height,
            refresh_millihz: mode.refresh_millihz,
        }),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const MODE: ScanoutMode = ScanoutMode {
        width: 3840,
        height: 2160,
        refresh_millihz: 239_990,
    };

    fn up() -> DisplayFacts {
        DisplayFacts {
            master_held: true,
            framebuffer: true,
            mode: Some(MODE),
        }
    }

    #[test]
    fn master_and_framebuffer_is_displaying_at_the_mode() {
        assert_eq!(verdict(&up()), Verdict::Displaying(Some(MODE)));
    }

    #[test]
    fn each_missing_fact_names_itself() {
        let cases = [
            (
                DisplayFacts {
                    master_held: false,
                    ..up()
                },
                vec![Missing::Master],
            ),
            (
                DisplayFacts {
                    framebuffer: false,
                    ..up()
                },
                vec![Missing::Framebuffer],
            ),
            (
                DisplayFacts::default(),
                vec![Missing::Master, Missing::Framebuffer],
            ),
        ];
        for (facts, missing) in cases {
            assert_eq!(
                verdict(&facts),
                Verdict::NotDisplaying(missing),
                "{facts:?}"
            );
        }
    }

    #[test]
    fn a_framebuffer_without_a_reported_mode_still_displays() {
        let facts = DisplayFacts { mode: None, ..up() };
        assert_eq!(verdict(&facts), Verdict::Displaying(None));
    }
}
