//! Every `QUASAR_*` name in the node agent's non-test code has a row in
//! `docs/configuration.md`, can be set on an owned install (`AGENT_VARIABLES`), and is passed
//! through by Compose; each exception is listed below with its reason. A list entry that is no
//! longer needed fails, so the lists only shrink.
//!
//! `recipe_compose_parity` compares Compose with the recipe, so a name missing from both
//! passes there. The site installer writes only the seed, so it carries no agent settings.

use std::collections::BTreeSet;
use std::path::Path;

use quasar_recovery::recipe::AGENT_VARIABLES;

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

/// The exact `"QUASAR_*"` string literals of one file, outside comments and inline test
/// modules. A name built with `format!` is not seen.
fn names_in(source: &str, out: &mut BTreeSet<String>) {
    let mut lines = source.lines();
    let mut test_attr = false;
    while let Some(line) = lines.next() {
        let code = line.trim_start();
        if code.starts_with("//") {
            continue;
        }
        if code.starts_with("#[cfg(test)]") || code.starts_with("#[cfg(all(test") {
            test_attr = true;
            continue;
        }
        if test_attr && code.starts_with("#[") {
            continue;
        }
        let is_mod = code.starts_with("mod ") || code.contains(" mod ");
        if std::mem::take(&mut test_attr) && is_mod && code.ends_with('{') {
            // rustfmt closes the module at the indentation that opened it.
            let close = format!("{}}}", &line[..line.len() - code.len()]);
            lines.by_ref().find(|l| *l == close);
            continue;
        }
        for (i, _) in line.match_indices("\"QUASAR_") {
            let rest = &line[i + 1..];
            let end = rest
                .find(|c: char| !(c.is_ascii_uppercase() || c.is_ascii_digit() || c == '_'))
                .unwrap_or(rest.len());
            if rest[end..].starts_with('"') && !rest[..end].ends_with('_') {
                out.insert(rest[..end].to_owned());
            }
        }
    }
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
            names_in(&std::fs::read_to_string(&path).unwrap(), out);
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

#[test]
fn every_variable_the_agent_reads_is_documented_and_settable() {
    let docs = std::fs::read_to_string(repo("docs/configuration.md")).unwrap();
    let (read, compose) = (read_by_the_agent(), compose_environment());
    let found = findings(&read, &docs, &compose);
    assert!(found.is_empty(), "{found:#?}");

    let stale: Vec<_> = NOT_AGENT_VARIABLE
        .iter()
        .filter(|(n, _)| !read.contains(*n) || AGENT_VARIABLES.contains(n))
        .map(|(n, _)| format!("NOT_AGENT_VARIABLE {n}"))
        .chain(
            NO_DOCS_ROW
                .iter()
                .filter(|(n, _)| !read.contains(*n) || has_row(&docs, n))
                .map(|(n, _)| format!("NO_DOCS_ROW {n}")),
        )
        .chain(
            NOT_IN_COMPOSE
                .iter()
                .filter(|n| !read.contains(**n) || compose.contains(**n))
                .map(|n| format!("NOT_IN_COMPOSE {n}")),
        )
        .collect();
    assert!(stale.is_empty(), "remove these entries: {stale:#?}");
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
    names_in(source, &mut read);
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
