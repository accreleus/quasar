//! The real adapter: `quasar-runtime`'s bounded Docker client.

use std::collections::BTreeMap;
use std::time::Duration;

use quasar_runtime::{RuntimeClient, RuntimeConfig};

use super::{
    Container, ContainerSpec, EngineError, EngineHost, Image, Network, PlatformEngine,
    RestartPolicy, Volume,
};

fn refused(r: quasar_runtime::platform::Refused) -> EngineError {
    EngineError::Refused {
        status: r.status,
        message: r.message,
    }
}

pub struct DockerEngine {
    client: RuntimeClient,
}

impl DockerEngine {
    /// The engine this process's environment names (`DOCKER_HOST`, else the default
    /// socket), with every selector it cannot honour refused.
    pub fn from_environment() -> Result<Self, EngineError> {
        let config = RuntimeConfig::from_environment()?;
        Self::new(config)
    }

    pub fn new(config: RuntimeConfig) -> Result<Self, EngineError> {
        Ok(DockerEngine {
            client: RuntimeClient::new(config)?,
        })
    }

    /// The engine endpoint, for log lines.
    pub fn endpoint(&self) -> String {
        self.client.endpoint()
    }
}

impl PlatformEngine for DockerEngine {
    fn host(&self) -> Result<EngineHost, EngineError> {
        Ok(self.client.engine_host().wait()?)
    }
    fn inspect_image(&self, reference: &str) -> Result<Option<Image>, EngineError> {
        Ok(self.client.inspect_platform_image(reference).wait()?)
    }
    fn pull(&self, reference: &str) -> Result<(), EngineError> {
        Ok(self.client.pull_image(reference).wait()?)
    }
    fn inspect_container(&self, name_or_id: &str) -> Result<Option<Container>, EngineError> {
        Ok(self.client.inspect_platform_container(name_or_id).wait()?)
    }
    fn list_containers(&self) -> Result<Vec<Container>, EngineError> {
        Ok(self.client.platform_containers().wait()?)
    }
    fn create_container(&self, spec: &ContainerSpec) -> Result<String, EngineError> {
        self.client
            .create_container(spec.clone())
            .wait()?
            .map_err(refused)
    }
    fn start_container(&self, id: &str) -> Result<(), EngineError> {
        self.client.start_container(id).wait()?.map_err(refused)
    }
    fn stop_container(&self, id: &str, grace: Duration) -> Result<(), EngineError> {
        Ok(self.client.stop_container(id, grace).wait()?)
    }
    fn set_restart_policy(&self, id: &str, policy: RestartPolicy) -> Result<(), EngineError> {
        Ok(self.client.set_restart_policy(id, policy).wait()?)
    }
    fn rename_container(&self, id: &str, name: &str) -> Result<(), EngineError> {
        Ok(self.client.rename_container(id, name).wait()?)
    }
    fn remove_container(&self, id: &str) -> Result<(), EngineError> {
        Ok(self.client.remove_container(id).wait()?)
    }
    fn wait_container(&self, id: &str, timeout: Duration) -> Result<i64, EngineError> {
        Ok(self.client.wait_container(id, timeout).wait()?)
    }
    fn logs_tail(&self, id: &str, lines: usize) -> Result<String, EngineError> {
        Ok(self.client.container_logs_tail(id, lines).wait()?)
    }
    fn upload_archive(&self, id: &str, path: &str, tar: Vec<u8>) -> Result<(), EngineError> {
        Ok(self.client.upload_archive(id, path, tar).wait()?)
    }
    fn inspect_volume(&self, name: &str) -> Result<Option<Volume>, EngineError> {
        Ok(self.client.inspect_volume(name).wait()?)
    }
    fn create_volume(
        &self,
        name: &str,
        labels: &BTreeMap<String, String>,
    ) -> Result<Volume, EngineError> {
        Ok(self.client.create_volume(name, labels.clone()).wait()?)
    }
    fn remove_volume(&self, name: &str) -> Result<(), EngineError> {
        Ok(self.client.remove_volume(name).wait()?)
    }
    fn inspect_network(&self, name: &str) -> Result<Option<Network>, EngineError> {
        Ok(self.client.inspect_network(name).wait()?)
    }
    fn create_network(
        &self,
        name: &str,
        labels: &BTreeMap<String, String>,
    ) -> Result<Network, EngineError> {
        Ok(self.client.create_network(name, labels.clone()).wait()?)
    }
    fn remove_network(&self, name: &str) -> Result<(), EngineError> {
        Ok(self.client.remove_network(name).wait()?)
    }
}
