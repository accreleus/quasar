//! Every `QUASAR_*` name in the node agent's non-test code has a row in
//! `docs/configuration.md`, can be set on an owned install (`AGENT_VARIABLES`), and is passed
//! through by Compose; each exception is listed below with its reason. A list entry that is no
//! longer needed fails, so the lists only shrink.
//!
//! `recipe_compose_parity` compares Compose with the recipe, so a name missing from both
//! passes there. The site installer writes only the seed, so it carries no agent settings.

use std::collections::BTreeSet;
use std::path::Path;

use proc_macro2::{TokenStream, TokenTree};
use quasar_recovery::recipe::AGENT_VARIABLES;
use syn::visit::{self, Visit};

const OWNED: &str = "set by the recovery actor's recipe, or owned by the install";
const SEED: &str = "a seed input in its own right";
const DEV: &str = "dev and test only";
const TO_APP: &str = "set by the agent on app containers (an agent-to-image contract)";
const STAMP: &str = "compile-time stamp from build.rs, not a runtime setting";

/// Read by the agent but not an `AGENT_VARIABLES` name.
const NOT_AGENT_VARIABLE: &[(&str, &str)] = &[
    ("QUASAR_ALLOW_PLAINTEXT_AGENT", OWNED),
    ("QUASAR_APP_ENGINE_GROUPS", TO_APP),
    ("QUASAR_COMPAT_TOOLS", TO_APP),
    ("QUASAR_COMPAT_TOOLS_ROOT", OWNED),
    ("QUASAR_CONSOLE_ACCESS", OWNED),
    ("QUASAR_DIRECT_DISPLAY", TO_APP),
    ("QUASAR_ENROLLMENT", OWNED),
    ("QUASAR_ENROLLMENT_FILE", OWNED),
    ("QUASAR_GPU_NVIDIA", OWNED),
    ("QUASAR_HEALTH_ADDR", OWNED),
    ("QUASAR_HOST_FUSE", OWNED),
    ("QUASAR_HOST_SOUND", OWNED),
    ("QUASAR_HOST_UDEV_DATA", OWNED),
    ("QUASAR_NVIDIA_DRIVER_HOST_PATH", OWNED),
    ("QUASAR_NVIDIA_DRIVER_VOLUME", OWNED),
    ("QUASAR_PULSE_IMAGE", OWNED),
    ("QUASAR_RECOVERY_SOCKET", OWNED),
    ("QUASAR_RENDER_NODE", OWNED),
    ("QUASAR_STREAM_FPS", TO_APP),
    ("QUASAR_STREAM_HEIGHT", TO_APP),
    ("QUASAR_STREAM_WIDTH", TO_APP),
    ("QUASAR_APP_PGID", SEED),
    ("QUASAR_APP_PUID", SEED),
    ("QUASAR_CONTAINER_NETWORK", SEED),
    ("QUASAR_HOME_ROOT", SEED),
    ("QUASAR_TEMPLATE_ROOT", SEED),
    ("QUASAR_APP_ARGS", DEV),
    ("QUASAR_APP_EXIT_POLICY", DEV),
    ("QUASAR_APP_GPU", DEV),
    ("QUASAR_APP_IMAGE", DEV),
    ("QUASAR_BITRATE_KBPS", DEV),
    ("QUASAR_CODEC", DEV),
    ("QUASAR_DIAG_NO_OBS", DEV),
    ("QUASAR_FPS", DEV),
    ("QUASAR_H264_PROFILE", DEV),
    ("QUASAR_HEIGHT", DEV),
    ("QUASAR_MEDIA_PROBE_DUMP", DEV),
    ("QUASAR_SYNTHETIC_GPU_CAPACITY", DEV),
    ("QUASAR_USE_TEST_AUDIO", DEV),
    ("QUASAR_USE_TEST_SRC", DEV),
    ("QUASAR_WIDTH", DEV),
    ("QUASAR_BUILT_AT", "set by the image"),
    (
        "QUASAR_CONTAINER_RUNTIME",
        "retired (#239): only warned about",
    ),
    ("QUASAR_STAMP_SOURCE_COMMIT", STAMP),
    ("QUASAR_STAMP_VERSION", STAMP),
    (
        "QUASAR_SESSION_GRACE_SECS",
        "the agent's reconnect hold has never been an agent variable; it must stay below the \
         control plane's value of the same name",
    ),
];

/// Read by the agent but with no table row in `docs/configuration.md`.
const NO_DOCS_ROW: &[(&str, &str)] = &[
    ("QUASAR_COMPAT_TOOLS", TO_APP),
    ("QUASAR_DIRECT_DISPLAY", TO_APP),
    ("QUASAR_STAMP_SOURCE_COMMIT", STAMP),
    ("QUASAR_STAMP_VERSION", STAMP),
    ("QUASAR_STREAM_FPS", TO_APP),
    ("QUASAR_STREAM_HEIGHT", TO_APP),
    ("QUASAR_STREAM_WIDTH", TO_APP),
];

/// Backlog: agent variables the Compose files (contributor tooling) do not pass through, so a
/// `deploy/.env` line for one does nothing. Do not add to it: pass a new variable through.
const NOT_IN_COMPOSE: &[&str] = &[
    "QUASAR_ABR_DISABLED",
    "QUASAR_ABR_LADDER",
    "QUASAR_ABR_LADDER_ENGAGE_DWELL",
    "QUASAR_ABR_LADDER_FLOOR_FOLLOWS_RUNG",
    "QUASAR_ABR_LADDER_FPS",
    "QUASAR_ABR_LADDER_MAX_BIAS",
    "QUASAR_ABR_LADDER_ORDER",
    "QUASAR_ABR_LADDER_RECOVER_DWELL",
    "QUASAR_ABR_LADDER_RESOLUTION",
    "QUASAR_ABR_LADDER_RES_ENGAGE_DWELL",
    "QUASAR_ABR_LADDER_RES_ENGAGE_FRAC",
    "QUASAR_ABR_LADDER_RES_EXPONENT",
    "QUASAR_ABR_LADDER_RES_MIN_HEIGHT",
    "QUASAR_ABR_LADDER_RES_MIN_STEP_S",
    "QUASAR_ABR_LADDER_RES_RECOVER_DWELL",
    "QUASAR_ABR_LADDER_RES_RECOVER_FRAC",
    "QUASAR_ADAPT_ENCODE_BUDGET_FRAC",
    "QUASAR_ADAPT_FPS_STEADY_FRAC",
    "QUASAR_ADAPT_GCC_BELOW_FRAC",
    "QUASAR_ADAPT_SEND_AT_CAP_FRAC",
    "QUASAR_APP_APPARMOR_PROFILE",
    "QUASAR_APP_BOOT_TIMEOUT_SECS",
    "QUASAR_APP_DISPLAY_ENV",
    "QUASAR_APP_GAMESCOPE_ENV",
    "QUASAR_APP_MOUNT_ALLOW",
    "QUASAR_APP_PIDS_LIMIT",
    "QUASAR_APP_PRIVILEGE_OPTOUT",
    "QUASAR_APP_READ_ONLY",
    "QUASAR_APP_SECCOMP",
    "QUASAR_CAPTURE_BITSTREAM",
    "QUASAR_CONSOLE_DDC",
    "QUASAR_CONSOLE_INPUT_SOURCE",
    "QUASAR_CUDA_CONTEXT_SHARED",
    "QUASAR_CUDA_RUNTIME_DIR",
    "QUASAR_GOP",
    "QUASAR_ICE_INTERFACES",
    "QUASAR_IDLE_TIMEOUT_SECS",
    "QUASAR_IMAGE_SOURCE_HOSTS",
    "QUASAR_INTEL_VULKAN_VIDEO",
    "QUASAR_JOB_POLL_SECS",
    "QUASAR_LOG_FORMAT",
    "QUASAR_MIC",
    "QUASAR_MIC_DISABLED",
    "QUASAR_MIC_JITTER_MS",
    "QUASAR_NVENC_DEFER_TEARDOWN",
    "QUASAR_NVENC_MAX_SESSIONS",
    "QUASAR_NVIDIA_DRIVER_RUN",
    "QUASAR_NVIDIA_DRIVER_TRUST_ON_FIRST_USE",
    "QUASAR_NV_LIB32_PATH",
    "QUASAR_REQUIRE_HW_RENDER",
    "QUASAR_RTP_MTU",
    "QUASAR_RTP_MTU_SHRINK",
    "QUASAR_STEAM_REVEAL_CAP_SECS",
    "QUASAR_SWAP_APP_READY_TIMEOUT_MS",
    "QUASAR_VULKAN_MAX_SESSIONS",
    "QUASAR_VULKAN_RING",
];

/// `#[test]` (also `#[tokio::test]`), `#[cfg(test)]` or `#[cfg(all(test, ..))]`.
fn is_test(attrs: &[syn::Attribute]) -> bool {
    attrs.iter().any(|a| {
        let path = a.path();
        path.segments.last().is_some_and(|s| s.ident == "test")
            || path.is_ident("cfg")
                && a.meta.require_list().is_ok_and(|list| {
                    let cfg = list.tokens.to_string().replace(' ', "");
                    cfg == "test" || cfg.starts_with("all(test,")
                })
    })
}

struct Names<'a>(&'a mut BTreeSet<String>);

impl Names<'_> {
    fn literal(&mut self, value: String) {
        if value.starts_with("QUASAR_")
            && !value.ends_with('_')
            && value
                .bytes()
                .all(|b| b.is_ascii_uppercase() || b.is_ascii_digit() || b == b'_')
        {
            self.0.insert(value);
        }
    }

    /// `syn` does not look inside a macro's arguments (`env!`, `info!`, `format!`).
    fn tokens(&mut self, tokens: TokenStream) {
        for token in tokens {
            match token {
                TokenTree::Group(group) => self.tokens(group.stream()),
                TokenTree::Literal(literal) => {
                    if let syn::Lit::Str(s) = syn::Lit::new(literal) {
                        self.literal(s.value());
                    }
                }
                _ => {}
            }
        }
    }
}

macro_rules! skip_test_items {
    ($($method:ident: $node:ident),*) => {$(
        fn $method(&mut self, node: &'ast syn::$node) {
            if !is_test(&node.attrs) {
                visit::$method(self, node);
            }
        }
    )*};
}

impl<'ast> Visit<'ast> for Names<'_> {
    skip_test_items!(
        visit_item_mod: ItemMod,
        visit_item_fn: ItemFn,
        visit_item_const: ItemConst,
        visit_item_static: ItemStatic,
        visit_item_impl: ItemImpl,
        visit_impl_item_const: ImplItemConst,
        visit_impl_item_fn: ImplItemFn,
        visit_item_macro: ItemMacro
    );

    fn visit_lit_str(&mut self, s: &'ast syn::LitStr) {
        self.literal(s.value());
    }

    fn visit_macro(&mut self, m: &'ast syn::Macro) {
        self.tokens(m.tokens.clone());
    }
}

/// The exact `"QUASAR_*"` string literals of one file, outside test items. A name built with
/// `format!` is not seen.
fn names_in(source: &str, out: &mut BTreeSet<String>) -> syn::Result<()> {
    Names(out).visit_file(&syn::parse_file(source)?);
    Ok(())
}

fn is_test_path(name: &str) -> bool {
    let stem = name.trim_end_matches(".rs");
    stem == "tests"
        || stem.ends_with("_tests")
        || stem.starts_with("tests_")
        || stem.starts_with("test_")
}

fn walk(dir: &Path, out: &mut BTreeSet<String>) {
    for entry in std::fs::read_dir(dir).unwrap_or_else(|e| panic!("{}: {e}", dir.display())) {
        let path = entry.unwrap().path();
        let name = path.file_name().unwrap().to_string_lossy().into_owned();
        if is_test_path(&name) {
            continue;
        }
        if path.is_dir() {
            walk(&path, out);
        } else if name.ends_with(".rs") {
            names_in(&std::fs::read_to_string(&path).unwrap(), out)
                .unwrap_or_else(|e| panic!("{}: {e}", path.display()));
        }
    }
}

fn repo(path: &str) -> std::path::PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../..")
        .join(path)
}

fn read_by_the_agent() -> BTreeSet<String> {
    let mut out = BTreeSet::new();
    walk(&repo("node-agent/src"), &mut out);
    walk(&repo("node-agent/crates/quasar-runtime/src"), &mut out);
    out
}

fn compose_environment() -> BTreeSet<String> {
    let mut out = BTreeSet::new();
    for file in [
        "docker-compose.yml",
        "docker-compose.nvidia.yml",
        "overlays/docker-compose.console.yml",
    ] {
        let text = std::fs::read_to_string(repo("deploy").join(file)).unwrap();
        let doc: serde_yaml::Value = serde_yaml::from_str(&text).unwrap();
        let env = doc["services"]["quasar-node-agent"]["environment"]
            .as_mapping()
            .unwrap_or_else(|| panic!("{file}: no node-agent environment"));
        out.extend(env.keys().map(|k| k.as_str().unwrap().to_owned()));
    }
    out
}

/// A table row whose first cell names it, alone or beside others.
fn has_row(docs: &str, name: &str) -> bool {
    let name = format!("`{name}`");
    docs.lines()
        .filter_map(|l| l.strip_prefix('|'))
        .any(|row| row.split('|').next().unwrap().contains(&name))
}

fn listed(list: &[(&str, &str)], name: &str) -> bool {
    list.iter().any(|(n, _)| *n == name)
}

fn findings(read: &BTreeSet<String>, docs: &str, compose: &BTreeSet<String>) -> Vec<String> {
    let mut out = Vec::new();
    for name in read {
        let name = name.as_str();
        if !has_row(docs, name) && !listed(NO_DOCS_ROW, name) {
            out.push(format!(
                "{name}: no row in docs/configuration.md (add one, or list it in NO_DOCS_ROW)"
            ));
        }
        let variable = AGENT_VARIABLES.contains(&name);
        if !variable && !listed(NOT_AGENT_VARIABLE, name) {
            out.push(format!(
                "{name}: an owned install cannot set it (add it to AGENT_VARIABLES in \
                 quasar-recovery's recipe, or list it in NOT_AGENT_VARIABLE)"
            ));
        }
        if variable && !compose.contains(name) && !NOT_IN_COMPOSE.contains(&name) {
            out.push(format!(
                "{name}: not passed through by deploy/docker-compose.yml (add it to the \
                 node-agent environment as a bare `{name}:`)"
            ));
        }
    }
    out
}

/// An entry that is no longer needed, repeated, or in two lists that cannot both hold it.
/// `NOT_AGENT_VARIABLE` and `NO_DOCS_ROW` may share a name: they excuse different checks.
fn list_problems(
    read: &BTreeSet<String>,
    docs: &str,
    compose: &BTreeSet<String>,
    not_variable: &[&str],
    no_row: &[&str],
    not_in_compose: &[&str],
) -> Vec<String> {
    let mut out = Vec::new();
    type Needed<'a> = &'a dyn Fn(&str) -> bool;
    let lists: [(&str, &[&str], Needed); 3] = [
        ("NOT_AGENT_VARIABLE", not_variable, &|n| {
            !AGENT_VARIABLES.contains(&n)
        }),
        ("NO_DOCS_ROW", no_row, &|n| !has_row(docs, n)),
        ("NOT_IN_COMPOSE", not_in_compose, &|n| {
            AGENT_VARIABLES.contains(&n) && !compose.contains(n)
        }),
    ];
    for (list, entries, needed) in lists {
        let mut seen = BTreeSet::new();
        for &name in entries {
            if !seen.insert(name) {
                out.push(format!("{list} {name}: listed twice"));
            } else if !read.contains(name) || !needed(name) {
                out.push(format!("{list} {name}: no longer needed"));
            }
        }
    }
    for name in not_in_compose {
        for (list, entries) in [
            ("NOT_AGENT_VARIABLE", not_variable),
            ("NO_DOCS_ROW", no_row),
        ] {
            if entries.contains(name) {
                out.push(format!("{list} and NOT_IN_COMPOSE both list {name}"));
            }
        }
    }
    out
}

#[test]
fn every_variable_the_agent_reads_is_documented_and_settable() {
    let docs = std::fs::read_to_string(repo("docs/configuration.md")).unwrap();
    let (read, compose) = (read_by_the_agent(), compose_environment());
    let found = findings(&read, &docs, &compose);
    assert!(found.is_empty(), "{found:#?}");

    let names = |list: &[(&'static str, &str)]| list.iter().map(|e| e.0).collect::<Vec<_>>();
    let problems = list_problems(
        &read,
        &docs,
        &compose,
        &names(NOT_AGENT_VARIABLE),
        &names(NO_DOCS_ROW),
        NOT_IN_COMPOSE,
    );
    assert!(problems.is_empty(), "fix these list entries: {problems:#?}");
}

/// The check on planted source: a new knob with no row and no list entry fails, and names in
/// comments, test modules and longer strings are not counted.
#[test]
fn a_planted_undocumented_variable_fails() {
    let source = r#"
// std::env::var("QUASAR_IN_A_COMMENT")
const K: &str = "QUASAR_FAKE_KNOB";
const SET: &str = "QUASAR_IN_AN_ASSIGNMENT=1";
#[cfg(test)]
#[allow(dead_code)]
mod tests {
    fn f() {
        let _ = "QUASAR_ONLY_IN_TESTS";
    }
}
const AFTER: &str = "QUASAR_AFTER_THE_TESTS";
"#;
    let mut read = BTreeSet::new();
    names_in(source, &mut read).unwrap();
    assert_eq!(
        read.iter().collect::<Vec<_>>(),
        ["QUASAR_AFTER_THE_TESTS", "QUASAR_FAKE_KNOB"]
    );
    read.remove("QUASAR_AFTER_THE_TESTS");
    let found = findings(&read, "", &BTreeSet::new());
    assert_eq!(found.len(), 2, "{found:#?}");
    assert!(found[0].contains("no row"), "{found:#?}");
    assert!(found[1].contains("AGENT_VARIABLES"), "{found:#?}");

    // Documented and an agent variable, but not in Compose.
    let read = BTreeSet::from(["QUASAR_WT_PORT".to_string()]);
    let found = findings(&read, "| `QUASAR_WT_PORT` | 4433 |", &BTreeSet::new());
    assert_eq!(found.len(), 1, "{found:#?}");
    assert!(found[0].contains("docker-compose"), "{found:#?}");
}

#[test]
fn a_brace_in_a_raw_string_does_not_end_a_test_module() {
    let source = r##"
#[cfg(test)]
mod tests {
    const VDF: &str = r#"
"compatibilitytools"
{
}
"#;
    #[test]
    fn f() {
        let _ = "QUASAR_ONLY_IN_TESTS";
    }
}
"##;
    let mut read = BTreeSet::new();
    names_in(source, &mut read).unwrap();
    assert!(read.is_empty(), "{read:?}");
}

#[test]
fn production_code_after_a_commented_test_module_is_read() {
    let source = r#"
#[cfg(test)]
mod tests {
} // end of tests
fn f() {
    let _ = env!("QUASAR_IN_A_MACRO");
    info!(knob = "QUASAR_AFTER_THE_TESTS", "{{}}");
}
"#;
    let mut read = BTreeSet::new();
    names_in(source, &mut read).unwrap();
    assert_eq!(
        read.iter().collect::<Vec<_>>(),
        ["QUASAR_AFTER_THE_TESTS", "QUASAR_IN_A_MACRO"]
    );
}

#[test]
fn a_repeated_misplaced_or_unneeded_list_entry_fails() {
    let read = BTreeSet::from(["QUASAR_FAKE".to_string(), "QUASAR_WT_PORT".to_string()]);
    let none = BTreeSet::new();
    let check = |a: &[&str], b: &[&str], c: &[&str]| list_problems(&read, "", &none, a, b, c);
    let (fake, port) = (&["QUASAR_FAKE"][..], &["QUASAR_WT_PORT"][..]);
    assert_eq!(check(fake, fake, port), Vec::<String>::new());

    let twice = ["QUASAR_FAKE", "QUASAR_FAKE"];
    assert_eq!(
        check(&twice, &twice, &["QUASAR_WT_PORT", "QUASAR_WT_PORT"]),
        [
            "NOT_AGENT_VARIABLE QUASAR_FAKE: listed twice",
            "NO_DOCS_ROW QUASAR_FAKE: listed twice",
            "NOT_IN_COMPOSE QUASAR_WT_PORT: listed twice",
        ]
    );
    // Not an agent variable, so Compose has nothing to excuse.
    assert_eq!(
        check(&[], &[], fake),
        ["NOT_IN_COMPOSE QUASAR_FAKE: no longer needed"]
    );
    assert_eq!(
        check(fake, &[], fake),
        [
            "NOT_IN_COMPOSE QUASAR_FAKE: no longer needed",
            "NOT_AGENT_VARIABLE and NOT_IN_COMPOSE both list QUASAR_FAKE",
        ]
    );
    assert_eq!(
        check(&[], port, port),
        ["NO_DOCS_ROW and NOT_IN_COMPOSE both list QUASAR_WT_PORT"]
    );
}
