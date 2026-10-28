//! The session's keyboard layout, settled once when an assign becomes a container spec.
//!
//! The control plane checks only the shape of a layout and variant (amendment 49), so a
//! value like `zz` reaches the agent. The compositor falls back to US on a keymap that does
//! not compile, but the app's own compositor (gamescope) reads the same `XKB_DEFAULT_LAYOUT`
//! and segfaults in libxkbcommon on its first key (#902). So the pair is checked here,
//! before the app or the compositor sees it; a pair that does not compile becomes US for both.

use std::collections::BTreeMap;
use std::path::Path;

const LAYOUT: &str = "XKB_DEFAULT_LAYOUT";
const VARIANT: &str = "XKB_DEFAULT_VARIANT";
/// Where libxkbcommon (the compositor's, in this image) reads xkeyboard-config.
const DATA_ROOT: &str = "/usr/share/X11/xkb";

/// The control plane's rules (`ValidXKBLayout`/`ValidXKBVariant`): one name, no comma list
/// for xkbcommon to compile into many groups. It also keeps a name a single path component.
pub(crate) fn xkb_name(v: &str, max: usize) -> bool {
    v.len() <= max
        && v.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
}

/// Whether the data has the layout's symbols file and, for a variant, its section.
// shortcut: a file check, not the evdev rules; it misses a pair the rules map elsewhere. The
// exact answer is a libxkbcommon call (a dlopen) if that ever shows up.
fn in_data(root: &Path, layout: &str, variant: &str) -> bool {
    std::fs::read_to_string(root.join("symbols").join(layout))
        .is_ok_and(|s| variant.is_empty() || s.contains(&format!("xkb_symbols \"{variant}\"")))
}

/// Replace an app env's keyboard pair with US unless it is a real layout. Unset or empty
/// stays as is (the compositor keeps US). A missing data directory cannot decide, so only
/// the shape rules apply there.
pub(crate) fn settle(env: &mut BTreeMap<String, String>) {
    settle_in(env, Path::new(DATA_ROOT));
}

fn settle_in(env: &mut BTreeMap<String, String>, root: &Path) {
    let Some(layout) = env.get(LAYOUT).filter(|l| !l.is_empty()) else {
        return;
    };
    let variant = env.get(VARIANT).map_or("", String::as_str);
    if xkb_name(layout, 32)
        && xkb_name(variant, 64)
        && (!root.is_dir() || in_data(root, layout, variant))
    {
        return;
    }
    tracing::warn!(
        token = "keyboard-layout-unavailable",
        layout,
        variant,
        "not a keyboard layout xkbcommon can compile; the app and the compositor use the US keymap"
    );
    env.insert(LAYOUT.into(), "us".into());
    env.insert(VARIANT.into(), String::new());
}

#[cfg(test)]
mod tests {
    use super::*;

    fn env(layout: &str, variant: &str) -> BTreeMap<String, String> {
        [(LAYOUT, layout), (VARIANT, variant)]
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .into()
    }

    /// A data directory with `de` (and its `nodeadkeys` section) and `us`.
    fn data() -> tempfile::TempDir {
        let d = tempfile::tempdir().unwrap();
        let sym = d.path().join("symbols");
        std::fs::create_dir(&sym).unwrap();
        std::fs::write(
            sym.join("de"),
            "default partial alphanumeric_keys\nxkb_symbols \"basic\" {\n};\n\
             partial alphanumeric_keys\nxkb_symbols \"nodeadkeys\" {\n};\n",
        )
        .unwrap();
        std::fs::write(sym.join("us"), "default xkb_symbols \"basic\" {\n};\n").unwrap();
        d
    }

    #[test]
    fn a_pair_that_does_not_compile_becomes_us() {
        let d = data();
        for (layout, variant) in [
            ("zz", ""),            // no such layout (#902)
            ("de", "nosuchvar"),   // a layout without that variant
            ("us,de", ""),         // a list, not one layout
            ("de", ","),           // a list in the variant
            ("../de", ""),         // not a name at all
            ("level5", ""),        // a directory in symbols/, not a layout
            (&"a".repeat(33), ""), // over the control plane's length
        ] {
            let mut e = env(layout, variant);
            settle_in(&mut e, d.path());
            assert_eq!(e, env("us", ""), "{layout:?}/{variant:?}");
        }
    }

    #[test]
    fn a_real_layout_or_no_layout_passes_unchanged() {
        let d = data();
        std::fs::create_dir(d.path().join("symbols").join("level5")).unwrap();
        for (layout, variant) in [("de", ""), ("de", "nodeadkeys"), ("us", "basic"), ("", "")] {
            let mut e = env(layout, variant);
            settle_in(&mut e, d.path());
            assert_eq!(e, env(layout, variant), "{layout:?}/{variant:?}");
        }
        let mut unset = BTreeMap::from([("TZ".to_string(), "UTC".to_string())]);
        settle_in(&mut unset, d.path());
        assert_eq!(unset.len(), 1, "an env without the keys gets none");
    }

    #[test]
    fn without_the_data_directory_only_the_shape_rules_apply() {
        let none = Path::new("/nonexistent/xkb");
        let mut e = env("zz", "");
        settle_in(&mut e, none);
        assert_eq!(e, env("zz", ""), "cannot tell: left to the compositor");
        let mut e = env("us,de", "");
        settle_in(&mut e, none);
        assert_eq!(e, env("us", ""));
    }
}
