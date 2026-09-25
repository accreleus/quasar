//! The names an owned install's node agent and its recovery actor must agree on. Both
//! crates depend on this one, so a rename cannot reach only one side.

/// Set in the agent's environment by the actor's recipe: the agent socket. Its presence is
/// what makes the agent an owned install.
pub const AGENT_SOCKET_ENV: &str = "QUASAR_RECOVERY_SOCKET";

/// The file twin of `QUASAR_ENROLLMENT`, which the actor points at the agent's secrets volume.
pub const ENROLLMENT_FILE_ENV: &str = "QUASAR_ENROLLMENT_FILE";

/// Where the agent-socket volume is mounted, in the actor (read-write) and the agent
/// (read-only).
pub const AGENT_SOCKET_DIR: &str = "/run/quasar-recovery";

/// The agent socket inside [`AGENT_SOCKET_DIR`].
pub const AGENT_SOCKET: &str = "/run/quasar-recovery/agent.sock";

/// Where a per-service secrets volume is mounted, read-only.
pub const SECRETS_DIR: &str = "/run/quasar-secrets";
