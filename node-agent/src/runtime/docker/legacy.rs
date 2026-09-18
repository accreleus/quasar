//! Boot-only retirement of LEGACY containers: pre-API siblings created by an
//! older agent that shell-launched `docker run --rm`, so they carry this
//! agent's owner label and an allowed name prefix but no durable operation
//! journal. Bollard stays here; callers see only [`LegacyRetirement`].
//!
//! Everything this agent cannot prove it owns is preserved and counted, never
//! deleted: the label alone never authorizes a removal, and neither does the
//! name. Both must hold, on an independent inspect of the immutable ID, exactly
//! as the CLI sweep this replaces required (#239).
use std::collections::HashMap;

use bollard::{
    errors::Error,
    query_parameters::{ListContainersOptions, RemoveContainerOptions},
};

use crate::runtime::{ErrorKind, LegacyRetirement, RuntimeConfig, RuntimeError};

/// Applications created through the runtime API carry their operation here.
/// Their teardown belongs to the durable journal, never to this sweep.
const OPERATION_LABEL: &str = "io.quasar.application-operation";

pub(crate) async fn retire_legacy(
    config: &RuntimeConfig,
    prefixes: &[String],
) -> Result<LegacyRetirement, RuntimeError> {
    let owner = super::application::owner(config)?;
    let (docker, _) = super::discover(config).await?;
    let mut filters = HashMap::new();
    filters.insert(
        "label".to_owned(),
        vec![format!("{}={owner}", crate::container_ownership::LABEL)],
    );
    // Listing is a hint only. Every candidate is re-inspected below, so a
    // daemon that ignores or widens the filter cannot authorize a removal.
    let listed = docker
        .list_containers(Some(ListContainersOptions {
            all: true,
            filters: Some(filters),
            ..Default::default()
        }))
        .await
        .map_err(super::classify)?;
    let prefixes = prefixes.iter().map(String::as_str).collect::<Vec<_>>();
    let mut outcome = LegacyRetirement::default();
    for summary in listed {
        let Some(id) = summary.id.filter(|value| !value.is_empty()) else {
            outcome.preserved += 1;
            continue;
        };
        let detail = match docker.inspect_container(&id, None).await {
            Ok(detail) => detail,
            // Gone between the listing and the inspection: nothing to preserve
            // and nothing this pass removed.
            Err(Error::DockerResponseServerError {
                status_code: 404, ..
            }) => continue,
            Err(error) => {
                outcome.preserved += 1;
                tracing::warn!(
                    token = "legacy-container-inspect-failed",
                    "preserving container {}: ownership could not be verified: {}",
                    short(&id),
                    super::classify(error)
                );
                continue;
            }
        };
        let Some(id) = owned_legacy_id(&detail, &owner, &prefixes) else {
            outcome.preserved += 1;
            continue;
        };
        // Pre-API containers ran `--rm` and may still be running after a SIGKILLed
        // agent; this is boot-only teardown behind the persistent owner lease,
        // exactly as the CLI sweep did. Volumes are never touched.
        match docker
            .remove_container(
                &id,
                Some(RemoveContainerOptions {
                    force: true,
                    v: false,
                    link: false,
                }),
            )
            .await
        {
            Ok(()) => {}
            Err(error) => {
                outcome.unresolved += 1;
                unresolved(&id, super::classify(error).kind);
                continue;
            }
        }
        // Absence of the exact immutable ID is the only proof of removal. A
        // second attempt is never made in this pass; the next boot retries.
        match docker.inspect_container(&id, None).await {
            Err(Error::DockerResponseServerError {
                status_code: 404, ..
            }) => outcome.removed += 1,
            Ok(_) => {
                outcome.unresolved += 1;
                unresolved(&id, ErrorKind::UnknownOutcome);
            }
            Err(error) => {
                outcome.unresolved += 1;
                unresolved(&id, super::classify(error).kind);
            }
        }
    }
    Ok(outcome)
}

fn short(id: &str) -> String {
    id.chars().take(12).collect()
}

fn unresolved(id: &str, kind: ErrorKind) {
    tracing::warn!(
        token = "legacy-container-retirement-unresolved",
        "legacy container {} was not proven removed ({kind:?}); leaving it for the next boot \
         rather than repeating the removal now",
        short(id)
    );
}

/// The ownership decision, over inspect evidence only. Identical in substance to
/// the checks the CLI sweep applied: an API-owned application and an audio
/// sidecar belong to their own durable recovery, and everything else must match
/// both the exact owner label and an allowed name prefix on a full 64-hex ID.
fn owned_legacy_id(
    detail: &bollard::models::ContainerInspectResponse,
    owner: &str,
    prefixes: &[&str],
) -> Option<String> {
    let labels = detail
        .config
        .as_ref()
        .and_then(|config| config.labels.clone())
        .unwrap_or_default();
    if labels.contains_key(OPERATION_LABEL) {
        return None;
    }
    let name = detail.name.clone().unwrap_or_default();
    let name_trimmed = name.trim_start_matches('/');
    // A GPU probe's teardown belongs to its helper journal, exactly as an audio
    // sibling's does, even when the caller asks for the probe prefix.
    if name_trimmed.starts_with(crate::session::audio::PULSE_NAME_PREFIX)
        || name_trimmed.starts_with(crate::container_ownership::PROBE_NAME_PREFIX)
    {
        return None;
    }
    let value = serde_json::json!({
        "Id": detail.id.clone().unwrap_or_default(),
        "Name": name,
        "Labels": labels,
    });
    crate::container_ownership::owned_id(&value, owner, prefixes)
}
