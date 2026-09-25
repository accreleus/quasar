//! The seed contract test (ADR 0007): the current seed's decision against the machine
//! state every released recovery actor wrote (`testdata/recovery/seed/actors/<release>/`),
//! and the compiled actor profile against its frozen form. A failure here is a change of
//! seed interface 1, which needs its own ADR and owner sign-off; never edit an existing
//! fixture to make it pass.

mod support;

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use quasar_recovery::engine::Container;
use quasar_recovery::recipe::labels;
use quasar_recovery::seed::file::{self, ActorImage};
use quasar_recovery::seed::{self, profile};
use serde::Deserialize;
use support::*;

/// The fixture set the current actor writes. A release appends its own directory and
/// moves this to it.
const CURRENT: &str = "rh06-06";

fn fixtures() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/recovery/seed")
}

/// A container as a fixture records it: only what the seed may look at.
#[derive(Debug, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
struct Recorded {
    name: String,
    labels: BTreeMap<String, String>,
    status: String,
    command: Vec<String>,
}

fn container(r: &Recorded, index: usize) -> Container {
    Container {
        id: format!("{index:064x}"),
        name: r.name.clone(),
        image: String::new(),
        image_id: String::new(),
        labels: r.labels.clone(),
        status: r.status.clone(),
        running: r.status == "running",
        health: None,
        restart: None,
        mounts: Vec::new(),
        command: r.command.clone(),
        env: Vec::new(),
    }
}

fn read_json<T: serde::de::DeserializeOwned>(path: &Path) -> T {
    serde_json::from_slice(&std::fs::read(path).unwrap())
        .unwrap_or_else(|e| panic!("{}: {e}", path.display()))
}

fn sorted_dirs(dir: &Path) -> Vec<PathBuf> {
    let mut dirs: Vec<_> = std::fs::read_dir(dir)
        .unwrap()
        .map(|e| e.unwrap().path())
        .filter(|p| p.is_dir())
        .collect();
    dirs.sort();
    dirs
}

#[test]
fn the_current_seed_decides_as_it_did_for_every_released_actors_machine_state() {
    let mut cases = 0;
    for set in sorted_dirs(&fixtures().join("actors")) {
        for case in sorted_dirs(&set) {
            let at = case.strip_prefix(fixtures()).unwrap().display().to_string();
            let read = file::read(&case);
            let recorded: Vec<Recorded> = read_json(&case.join("containers.json"));
            let containers: Vec<Container> = recorded
                .iter()
                .enumerate()
                .map(|(i, r)| container(r, i))
                .collect();
            let decided = serde_json::to_value(seed::decide(&read, &containers)).unwrap();
            let expected: serde_json::Value = read_json(&case.join("expected.json"));
            // The fixture names what is decided; detail it leaves out (which container was
            // seen, the wording of a reason) is not part of the interface.
            for (key, want) in expected.as_object().unwrap() {
                assert_eq!(decided.get(key), Some(want), "{at}: {key} ({decided})");
            }
            cases += 1;
        }
    }
    assert!(cases >= 14, "only {cases} fixture cases were found");
}

/// The newest fixture set is not hand-made: it is what the current seed and actor leave on
/// a GPU host they installed.
#[test]
fn the_current_fixture_set_is_what_the_current_actor_writes() {
    let engine = std::sync::Arc::new(quasar_recovery::engine::FakeEngine::new(seeded_host(
        seed_env(),
    )));
    let dir = tempfile::tempdir().unwrap();
    seed(&engine, dir.path(), SEED_ID).step();
    let id = engine
        .state()
        .container_named(profile::NAME)
        .unwrap()
        .id
        .clone();
    seeded_actor(&engine, dir.path(), &id).resume().unwrap();

    let case = fixtures().join("actors").join(CURRENT).join("installed");
    assert_eq!(
        std::fs::read(dir.path().join(file::FILE_NAME)).unwrap(),
        std::fs::read(case.join("seed.json")).unwrap(),
        "seed.json"
    );
    let frozen = [labels::INSTALLATION, labels::PLATFORM_SERVICE];
    let written: Vec<(String, BTreeMap<String, String>, String)> = engine
        .state()
        .by_name()
        .into_iter()
        .map(|(name, (spec, status))| {
            let labels = spec
                .labels
                .into_iter()
                .filter(|(k, _)| frozen.contains(&k.as_str()))
                .collect();
            (name, labels, status)
        })
        .collect();
    let recorded: Vec<Recorded> = read_json(&case.join("containers.json"));
    let recorded: Vec<_> = recorded
        .into_iter()
        .map(|r| (r.name, r.labels, r.status))
        .collect();
    assert_eq!(written, recorded, "containers");
}

#[test]
fn the_compiled_actor_profile_is_seed_interface_1() {
    let image = ActorImage::parse(ACTOR_IMAGE).unwrap();
    let rendered = profile::actor(INSTALLATION, &image, SOCKET_HOST_PATH, SEED_ID);
    let frozen: serde_json::Value = read_json(&fixtures().join("profile-1.json"));
    assert_eq!(serde_json::to_value(&rendered).unwrap(), frozen);
}

#[test]
fn the_labels_the_seed_reads_are_the_ones_every_actor_stamps() {
    assert_eq!(seed::INSTALLATION_LABEL, labels::INSTALLATION);
    assert_eq!(seed::PLATFORM_SERVICE_LABEL, labels::PLATFORM_SERVICE);
    assert_eq!(
        seed::RECOVERY_ACTOR,
        quasar_recovery::recipe::Role::RecoveryActor.as_str()
    );
    assert_eq!(
        profile::NAME,
        quasar_recovery::recipe::names::RECOVERY_ACTOR
    );
}
