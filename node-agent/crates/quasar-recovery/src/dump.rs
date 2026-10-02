//! The final dump an `uninstall --purge` takes of a Quasar-owned database before it deletes
//! anything (#352 R1-Q2: "D7's dump, not a bundle").
//!
//! A plain `pg_dump`, nothing more: this is not the pre-update dump of a migrating update
//! (`crate::dump_dir`, #364, with its retention and `restore`); the two share one helper
//! runner (`crate::database::run_helper`). The database is dumped **offline**,
//! from its data volume, by a one-shot helper on the machine's own Postgres image that
//! starts a private postmaster with no TCP listener, runs `pg_dump` over its local socket,
//! and stops it. So the dump does not depend on the Postgres service still running (an
//! earlier `uninstall` may have removed it), needs no password (the image's local
//! connections are `trust`), and cannot race a writer: the control plane is removed first.
//!
//! The dump is written as `<name>.partial` and renamed only once `pg_dump` succeeded, so a
//! file under the final name is always whole.

use std::collections::BTreeMap;
use std::time::Duration;

use crate::engine::{ContainerSpec, PlatformEngine, RestartPolicy};
use crate::recipe::{self, control, labels, names, paths, Bind, ImageRef};

/// What the helper container is, on its `io.quasar.helper` label.
pub const HELPER: &str = "final-dump";

/// How long a dump may take. Generous: a household database is small, but the helper also
/// replays WAL when the database was stopped uncleanly.
pub const DUMP_TIMEOUT: Duration = Duration::from_secs(30 * 60);

/// Where the dump goes.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Dest {
    /// A named volume, created if missing, that a purge never deletes.
    Volume(String),
    /// An absolute directory on the engine host.
    HostDir(String),
}

impl Dest {
    /// Where the operator finds the dump.
    pub fn describe(&self, name: &str) -> String {
        match self {
            Dest::Volume(v) => format!("{name} in the {v} volume (copy it out with: docker run --rm -v {v}:/dump -v \"$PWD\":/out alpine cp /dump/{name} /out/)"),
            Dest::HostDir(d) => format!("{}/{name} on this machine", d.trim_end_matches('/')),
        }
    }
}

/// The dump's file name for a dump taken at `now` (RFC 3339): `quasar-final-20260926T101500Z.dump`.
pub fn file_name(now: &str) -> String {
    let stamp: String = now.chars().filter(|c| c.is_ascii_alphanumeric()).collect();
    format!("quasar-final-{stamp}.dump")
}

/// Runs the helper to completion. `Ok` is the dump's file name; `Err` says why there is none.
/// The helper is removed on every path.
pub fn take(
    engine: &dyn PlatformEngine,
    postgres_image: &ImageRef,
    installation_id: &str,
    dest: &Dest,
    name: &str,
) -> Result<String, String> {
    let dump_mount = match dest {
        Dest::Volume(volume) => {
            let exists = engine
                .inspect_volume(volume)
                .map_err(|e| format!("inspect the {volume} volume: {e}"))?
                .is_some();
            if !exists {
                let labels = BTreeMap::from([
                    (
                        labels::INSTALLATION.to_string(),
                        installation_id.to_string(),
                    ),
                    (labels::HELPER.to_string(), HELPER.to_string()),
                ]);
                engine
                    .create_volume(volume, &labels)
                    .map_err(|e| format!("create the {volume} volume: {e}"))?;
            }
            volume.clone()
        }
        Dest::HostDir(dir) => {
            if !dir.starts_with('/') {
                return Err(format!(
                    "the dump directory {dir:?} must be an absolute path on this machine"
                ));
            }
            dir.clone()
        }
    };
    let spec = helper_spec(postgres_image, &dump_mount, name);
    // The runner `crate::database` shares: a stale helper swept, this one always removed.
    let (code, logs) =
        crate::database::run_helper(engine, &spec, DUMP_TIMEOUT).map_err(|e| match e {
            crate::database::DbError::Crashed => "the recovery actor stopped".to_string(),
            crate::database::DbError::Failed(why) => format!("the dump helper: {why}"),
        })?;
    if code != 0 {
        let tail = logs.trim_end();
        return Err(if tail.is_empty() {
            format!("pg_dump exited {code}")
        } else {
            format!("the dump helper exited {code}:\n{tail}")
        });
    }
    Ok(name.to_string())
}

/// The script reads its inputs from the environment, so nothing is interpolated into it.
const SCRIPT: &str = r#"set -eu
umask 077
as_pg() { if command -v su-exec >/dev/null 2>&1; then su-exec postgres "$@"; else gosu postgres "$@"; fi; }
as_pg pg_ctl -D "$PGDATA" -w -t 300 -o "-c listen_addresses=''" start
rc=0
as_pg pg_dump -U "$DUMP_USER" -d "$DUMP_DB" --format=custom > "/dump/$DUMP_NAME.partial" || rc=$?
as_pg pg_ctl -D "$PGDATA" -m fast -w stop || true
if [ "$rc" -eq 0 ]; then mv "/dump/$DUMP_NAME.partial" "/dump/$DUMP_NAME"; else rm -f "/dump/$DUMP_NAME.partial"; fi
exit "$rc"
"#;

fn helper_spec(image: &ImageRef, dump_mount: &str, name: &str) -> ContainerSpec {
    ContainerSpec {
        name: names::FINAL_DUMP.into(),
        image: image.reference(),
        entrypoint: Some(vec!["/bin/sh".into(), "-c".into(), SCRIPT.into()]),
        cmd: None,
        env: BTreeMap::from([
            ("PGDATA".to_string(), paths::POSTGRES_DATA_DIR.to_string()),
            ("DUMP_USER".into(), control::OWNED_DATABASE_USER.into()),
            ("DUMP_DB".into(), control::OWNED_DATABASE.into()),
            ("DUMP_NAME".into(), name.into()),
        ]),
        labels: BTreeMap::from([(labels::HELPER.to_string(), HELPER.to_string())]),
        network_mode: Some("none".into()),
        binds: vec![
            recipe::bind(names::POSTGRES_DATA_VOLUME, paths::POSTGRES_DATA_DIR, false),
            Bind {
                source: dump_mount.into(),
                target: "/dump".into(),
                read_only: false,
            },
        ],
        devices: Vec::new(),
        device_cgroup_rules: Vec::new(),
        gpus: Vec::new(),
        cap_add: Vec::new(),
        security_opt: Vec::new(),
        init: false,
        restart: RestartPolicy::No,
        ports: Vec::new(),
        healthcheck: None,
    }
}
