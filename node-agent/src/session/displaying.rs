//! Whether a console session is displaying (CONTEXT.md "Displaying"): its container is
//! alive, its desktop holds DRM master on the console card, and a framebuffer is on the
//! connector's primary plane. A still desktop is displaying; there is no frame counter.

/// What the agent observed about one console session.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct DisplayFacts {
    pub container_alive: bool,
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
    Container,
    Master,
    Framebuffer,
}

impl Missing {
    pub fn describe(self) -> &'static str {
        match self {
            Missing::Container => "the console container is not running",
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
        (facts.container_alive, Missing::Container),
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

/// What the console card says about `connector` (e.g. `DP-1`): whether another client
/// holds DRM master, whether the connector's CRTC scans out a framebuffer, and its mode.
/// An unreadable card reports nothing held and nothing scanned out.
///
/// Opening a primary node while nobody holds master makes the opener master, so this is
/// read only once the desktop has had time to open the card itself; if the open did make
/// the agent master, the drop below hands the display straight back.
pub fn read_drm_facts(card_node: &std::path::Path, connector: &str) -> DisplayFacts {
    use drm::control::Device as _;
    use drm::Device as _;

    let _drm_open_guard = super::console::drm_open_lock()
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    let Ok(file) = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .open(card_node)
    else {
        return DisplayFacts::default();
    };
    let card = crate::capacity::DrmCard(file);
    // DROP_MASTER succeeds only for the current master.
    let master_held = card.release_master_lock().is_err();
    let crtc = card.resource_handles().ok().and_then(|resources| {
        resources.connectors().iter().find_map(|handle| {
            let info = card.get_connector(*handle, false).ok()?;
            let name = format!("{}-{}", info.interface().as_str(), info.interface_id());
            if name != connector {
                return None;
            }
            let encoder = card.get_encoder(info.current_encoder()?).ok()?;
            card.get_crtc(encoder.crtc()?).ok()
        })
    });
    let mode = crtc.as_ref().and_then(|crtc| crtc.mode()).map(|mode| {
        let mode = crate::capacity::drm_mode_capability(&mode);
        ScanoutMode {
            width: mode.width,
            height: mode.height,
            refresh_millihz: mode.refresh_millihz,
        }
    });
    DisplayFacts {
        container_alive: false,
        master_held,
        framebuffer: crtc.is_some_and(|crtc| crtc.framebuffer().is_some()),
        mode,
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
            container_alive: true,
            master_held: true,
            framebuffer: true,
            mode: Some(MODE),
        }
    }

    #[test]
    fn alive_master_and_framebuffer_is_displaying_at_the_mode() {
        assert_eq!(verdict(&up()), Verdict::Displaying(Some(MODE)));
    }

    #[test]
    fn each_missing_fact_names_itself() {
        let cases = [
            (
                DisplayFacts {
                    container_alive: false,
                    ..up()
                },
                vec![Missing::Container],
            ),
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
                vec![Missing::Container, Missing::Master, Missing::Framebuffer],
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
