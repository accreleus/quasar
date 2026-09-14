//! Read the existing Docker login configuration; never expose credentials in errors.
use super::*;
use bollard::auth::DockerCredentials;
use tokio::io::{AsyncReadExt, AsyncWriteExt};

pub(super) async fn load(
    config: &RuntimeConfig,
    image: &str,
) -> Result<Option<DockerCredentials>, RuntimeError> {
    let Some(config_json) = read_config(config).await? else {
        return Ok(None);
    };
    let first = image.split('/').next().unwrap_or(image);
    let registry = if image.contains('/')
        && (first.contains('.') || first.contains(':') || first == "localhost")
    {
        first
    } else {
        "https://index.docker.io/v1/"
    };
    let registry = if matches!(
        registry,
        "docker.io" | "index.docker.io" | "registry-1.docker.io"
    ) {
        "https://index.docker.io/v1/"
    } else {
        registry
    };
    resolve(config, &config_json, registry).await
}

async fn read_config(config: &RuntimeConfig) -> Result<Option<serde_json::Value>, RuntimeError> {
    let Some(path) = &config.registry_config_path else {
        return Ok(None);
    };
    let file = match tokio::fs::File::open(path).await {
        Ok(file) => file,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(_) => return Err(ErrorKind::RegistryDenied.into()),
    };
    let mut bytes = Vec::new();
    file.take(1024 * 1024 + 1)
        .read_to_end(&mut bytes)
        .await
        .map_err(|_| ErrorKind::RegistryDenied)?;
    if bytes.len() > 1024 * 1024 {
        return Err(ErrorKind::RegistryDenied.into());
    }
    serde_json::from_slice(&bytes)
        .map(Some)
        .map_err(|_| ErrorKind::RegistryDenied.into())
}

async fn resolve(
    config: &RuntimeConfig,
    config_json: &serde_json::Value,
    registry: &str,
) -> Result<Option<DockerCredentials>, RuntimeError> {
    let helper = config_json
        .get("credHelpers")
        .and_then(|v| v.get(registry))
        .and_then(|v| v.as_str())
        .or_else(|| config_json.get("credsStore").and_then(|v| v.as_str()))
        .filter(|v| !v.is_empty());
    if let Some(helper) = helper {
        if !helper
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"._-".contains(&b))
        {
            return Err(ErrorKind::RegistryDenied.into());
        }
        let result = tokio::time::timeout(config.deadline, async {
            let mut child = tokio::process::Command::new(format!("docker-credential-{helper}"))
                .arg("get")
                .kill_on_drop(true)
                .stdin(std::process::Stdio::piped())
                .stdout(std::process::Stdio::piped())
                .stderr(std::process::Stdio::null())
                .spawn()
                .map_err(|_| ErrorKind::RegistryDenied)?;
            let mut input = child.stdin.take().ok_or(ErrorKind::RegistryDenied)?;
            input
                .write_all(format!("{registry}\n").as_bytes())
                .await
                .map_err(|_| ErrorKind::RegistryDenied)?;
            drop(input);
            let mut output = Vec::new();
            child
                .stdout
                .take()
                .ok_or(ErrorKind::RegistryDenied)?
                .take(16385)
                .read_to_end(&mut output)
                .await
                .map_err(|_| ErrorKind::RegistryDenied)?;
            if output.len() > 16384 {
                return Err(ErrorKind::RegistryDenied);
            }
            if !child
                .wait()
                .await
                .map_err(|_| ErrorKind::RegistryDenied)?
                .success()
            {
                // Docker helpers signal an absent login using this standard message.
                if String::from_utf8_lossy(&output).trim()
                    == "credentials not found in native keychain"
                {
                    return Ok(None);
                }
                return Err(ErrorKind::RegistryDenied);
            }
            let value: serde_json::Value =
                serde_json::from_slice(&output).map_err(|_| ErrorKind::RegistryDenied)?;
            let username = value
                .get("Username")
                .and_then(|v| v.as_str())
                .ok_or(ErrorKind::RegistryDenied)?;
            let secret = value
                .get("Secret")
                .and_then(|v| v.as_str())
                .ok_or(ErrorKind::RegistryDenied)?;
            Ok(Some(if username == "<token>" {
                DockerCredentials {
                    identitytoken: Some(secret.into()),
                    serveraddress: Some(registry.into()),
                    ..Default::default()
                }
            } else {
                DockerCredentials {
                    username: Some(username.into()),
                    password: Some(secret.into()),
                    serveraddress: Some(registry.into()),
                    ..Default::default()
                }
            }))
        })
        .await
        .map_err(|_| ErrorKind::RegistryDenied)?;
        return result.map_err(Into::into);
    }
    let auths = config_json.get("auths");
    let entry = auths
        .and_then(|v| v.get(registry))
        .or_else(|| auths.and_then(|v| v.get(format!("https://{registry}").as_str())));
    let Some(entry) = entry else {
        return Ok(None);
    };
    let mut credentials: DockerCredentials =
        serde_json::from_value(entry.clone()).map_err(|_| ErrorKind::RegistryDenied)?;
    if let Some(auth) = credentials.auth.take() {
        use base64::Engine;
        let decoded = base64::engine::general_purpose::STANDARD
            .decode(auth)
            .map_err(|_| ErrorKind::RegistryDenied)?;
        let decoded = String::from_utf8(decoded).map_err(|_| ErrorKind::RegistryDenied)?;
        let (username, password) = decoded.split_once(':').ok_or(ErrorKind::RegistryDenied)?;
        credentials.username = Some(username.into());
        credentials.password = Some(password.into());
    }
    credentials.serveraddress = Some(registry.into());
    Ok(Some(credentials))
}

/// Classic builds need a registry configuration, including private FROM images.
/// Match Docker CLI's login discovery without invoking its engine CLI.
pub(super) async fn load_all(
    config: &RuntimeConfig,
) -> Result<Option<std::collections::HashMap<String, DockerCredentials>>, RuntimeError> {
    let Some(value) = read_config(config).await? else {
        return Ok(None);
    };
    let mut registries = std::collections::BTreeSet::new();
    for field in ["auths", "credHelpers"] {
        if let Some(entries) = value.get(field).and_then(|v| v.as_object()) {
            registries.extend(entries.keys().cloned());
        }
    }
    if let Some(helper) = value
        .get("credsStore")
        .and_then(|v| v.as_str())
        .filter(|v| !v.is_empty())
    {
        if !helper
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"._-".contains(&b))
        {
            return Err(ErrorKind::RegistryDenied.into());
        }
        let output = tokio::time::timeout(config.deadline, async {
            let mut child = tokio::process::Command::new(format!("docker-credential-{helper}"))
                .arg("list")
                .kill_on_drop(true)
                .stdin(std::process::Stdio::null())
                .stdout(std::process::Stdio::piped())
                .stderr(std::process::Stdio::null())
                .spawn()
                .map_err(|_| ErrorKind::RegistryDenied)?;
            let mut output = Vec::new();
            child
                .stdout
                .take()
                .ok_or(ErrorKind::RegistryDenied)?
                .take(1024 * 1024 + 1)
                .read_to_end(&mut output)
                .await
                .map_err(|_| ErrorKind::RegistryDenied)?;
            if output.len() > 1024 * 1024
                || !child
                    .wait()
                    .await
                    .map_err(|_| ErrorKind::RegistryDenied)?
                    .success()
            {
                return Err(ErrorKind::RegistryDenied);
            }
            serde_json::from_slice::<std::collections::BTreeMap<String, String>>(&output)
                .map_err(|_| ErrorKind::RegistryDenied)
        })
        .await
        .map_err(|_| ErrorKind::RegistryDenied)??;
        registries.extend(output.into_keys());
    }
    let mut credentials = std::collections::HashMap::new();
    for registry in registries {
        if let Some(auth) = resolve(config, &value, &registry).await? {
            credentials.insert(registry, auth);
        }
    }
    Ok(Some(credentials))
}
