//! `seed.json`, format 1 (ADR 0007, seed interface 1). Written only by a recovery actor
//! (atomically, through `DurableFile`), read only by the seed. Frozen: a field is never
//! added, renamed or re-typed under format 1, and this module depends on no type that
//! may evolve. The fixtures in `testdata/recovery/seed/` pin its bytes.

use std::io;
use std::path::Path;

use serde::{Deserialize, Serialize};

pub const FILE_NAME: &str = "seed.json";
pub const FORMAT_VERSION: u64 = 1;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SeedFile {
    pub format_version: u64,
    pub installation_id: String,
    /// The last verified recovery-actor image: what the seed re-creates the actor from.
    pub recovery_actor_image: ActorImage,
    pub state: SeedState,
}

/// A repository and a `sha256:` digest, never a tag (ADR 0001).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ActorImage {
    pub repository: String,
    pub digest: String,
}

impl ActorImage {
    /// `repository@sha256:<64 lowercase hex>`, the only shape the seed pulls or creates from.
    pub fn parse(reference: &str) -> Option<ActorImage> {
        let (repository, digest) = reference.trim().split_once('@')?;
        let hex = digest.strip_prefix("sha256:")?;
        let last = repository.rsplit('/').next().unwrap_or("");
        let ok = !repository.is_empty()
            && !last.contains(':')
            && hex.len() == 64
            && hex
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b));
        ok.then(|| ActorImage {
            repository: repository.into(),
            digest: digest.into(),
        })
    }

    pub fn reference(&self) -> String {
        format!("{}@{}", self.repository, self.digest)
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SeedState {
    Active,
    /// The operator's `uninstall` or a console "remove host" took this machine's services
    /// away: the seed must not bring them back.
    Uninstalled,
}

/// What the seed makes of the machine's `seed.json`. It never guesses: anything but a
/// whole, valid format-1 file (or no file at all) keeps it idle.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SeedRead {
    /// No file: a first install, or a machine no actor has recorded yet.
    Missing,
    Found(SeedFile),
    /// A `format_version` other than 1 (or none), as written.
    UnknownFormat(String),
    /// Format 1, but not a valid one.
    Unreadable(String),
}

pub fn read(machine_dir: &Path) -> SeedRead {
    match std::fs::read(machine_dir.join(FILE_NAME)) {
        Ok(bytes) => parse(&bytes),
        Err(e) if e.kind() == io::ErrorKind::NotFound => SeedRead::Missing,
        Err(e) => SeedRead::Unreadable(format!("cannot read {FILE_NAME}: {e}")),
    }
}

pub fn parse(bytes: &[u8]) -> SeedRead {
    let value: serde_json::Value = match serde_json::from_slice(bytes) {
        Ok(v) => v,
        Err(e) => return SeedRead::Unreadable(format!("{FILE_NAME} is not JSON: {e}")),
    };
    match value.get("format_version") {
        Some(v) if v.as_u64() == Some(FORMAT_VERSION) => {}
        Some(v) => return SeedRead::UnknownFormat(v.to_string()),
        None => return SeedRead::UnknownFormat("absent".into()),
    }
    let file: SeedFile = match serde_json::from_value(value) {
        Ok(f) => f,
        Err(e) => return SeedRead::Unreadable(format!("{FILE_NAME} format 1: {e}")),
    };
    if file.installation_id.trim().is_empty() {
        return SeedRead::Unreadable(format!("{FILE_NAME} names no installation"));
    }
    if ActorImage::parse(&file.recovery_actor_image.reference()).as_ref()
        != Some(&file.recovery_actor_image)
    {
        return SeedRead::Unreadable(format!(
            "{FILE_NAME} names {:?}, which is not repository@sha256:<64 lowercase hex>",
            file.recovery_actor_image.reference()
        ));
    }
    SeedRead::Found(file)
}
