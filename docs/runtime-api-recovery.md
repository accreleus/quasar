# Runtime API cutover and recovery

The node agent uses the Quasar runtime interface and Bollard for Docker operations.
The updater still owns its separate Compose workflow. This guide covers a planned
agent replacement, with sessions drained; it does not resume running sessions.
Promotion from the initiative branch requires explicit owner approval of the
[#240 acceptance evidence](https://github.com/accreleus/quasar/issues/240).

## Record the deployment before changing it

On each target, record the live control-plane source, image ID and database schema;
the agent source and image ID; Docker server and negotiated API versions; and the
stack's Compose project, files and active sessions. A checkout or an earlier report
is not evidence of what is currently running. Recheck these observations immediately
before replacement, especially on shared development hosts.

Keep a locally available known-good agent image and a schema-compatible control
plane. Retain the existing node-secret/owner state, operation journals, managed homes,
application data and NVIDIA driver volume. Preserve their exact mount identities and
the configured engine socket. Back up persistent control-plane data before applying
any previously unapplied migrations.

**A schema-80 control plane cannot replace a control plane whose database is at
schema 84.** The #251 upgrade established schema 84 on both acceptance targets. An
agent rollback does not require a control-plane rollback. The accepted #239 agent
can run with the schema-84 control plane.

## Planned cutover

1. Build a pinned source commit locally with `deploy/build-images.sh`. Retain its
   build report and require the unchanged image contract to pass. Record image IDs
   before deployment; a mutable tag alone does not identify a candidate.
2. With an admin bearer from `make admin-token HOST=gpu-test`, call
   `POST /v1/hosts/{id}/drain` with `{"force":false}`. Confirm `status=draining`;
   this excludes new placement but leaves existing sessions running. Wait for them
   to end, or explicitly stop the authorized sessions with `DELETE /v1/sessions/{id}`.
   `make session-list HOST=gpu-test` shows the remaining sessions. Verify both terminal
   session state and the independent runtime cleanup described below.
3. Confirm the target is idle. Update only the intended agent/Pulse image references
   in that stack's existing Compose configuration and recreate that agent. Preserve
   its project, environment, device/security settings, volumes and endpoint. Do not
   run a broad stack reset or prune.
4. Wait for health, registration and readiness for the required operation capabilities
   and actual encoder. Verify the actual image
   ID and reported source, unchanged host identity, and preserved mounts. Startup
   retires owned work from the previous agent; it does not adopt sessions. Unresolved
   retirement must block admission rather than permit another writer to a home.
5. Call `POST /v1/hosts/{id}/uncordon` and verify the connected host is online.
   Launch a disposable application, verify rendered content, audio and application
   input, then stop it and verify cleanup. Check a marker in its managed home before
   and after replacement. Restore temporary settings after acceptance.

For a control-plane-only correction, use
`make redeploy-cp HOST=gpu-test REF=<reviewed-commit>` (or the existing isolated local
stack wrapper). This preserves the agent container. Rebuilding the agent is not
necessary for a Go-only correction.

### Independent cleanup and identity evidence

Use scoped `docker inspect <agent-container>` to record its immutable image ID,
source label, environment and mount definitions; compare them with the saved record.
The control plane reports its source and schema at
`GET /v1/admin/platform/identity`; `GET /v1/hosts` reports the agent source.

Read the agent's persisted application/helper records beneath
`${NODE_SECRET_PATH}.runtime-images/applications` and `helpers` inside its state
mount. Ignore lock/temporary files. Before declaring cleanup complete, each relevant
operation must be `Completed`.
Inspect the recorded container IDs independently: an explicit engine “not found”
proves absence, while transport or permission errors do not. For image operations,
reconcile the recorded intent and inspect the resulting image rather than treating
cancellation as rollback. Keep these records private: they can contain host paths
and application settings. Do not delete journals to manufacture a clean result.

## Return to the recorded known-good agent

Drain and verify cleanup again. Restore the recorded agent and Pulse image
references using the same scoped Compose configuration, retaining the current
schema-compatible control plane. Verify the image IDs, online host identity and
persistent mounts, then relaunch the disposable managed-home application and read
its original marker. Repeat the same checks when returning to the candidate.

The #239 agent includes the terminal-journal endpoint fix. A rollback to an older
agent after changing the socket has an additional prerequisite documented in
[configuration](configuration.md#runtime-endpoint-migration-and-recovery). Prefer the
recorded #239-or-newer known-good image; never discard a nonterminal journal to
make an older agent start.

## Uncertainty and failure handling

- Losing observation, a log consumer or the control-plane connection does not stop
  a workload. Stop is an explicit operation.
- An unreachable engine is not a missing container. A lost create/start/remove reply
  must be reconciled under the same operation identity. Do not switch to a CLI,
  change endpoints, or submit a fresh identity to bypass uncertainty.
- Cleanup remains durable and retryable. Restore the original endpoint and allow
  startup retirement or periodic cleanup to finish. Preserve journals and backing
  storage while an outcome remains unproven.
- An explicit heartbeat omission can reconcile a previously running intentional
  stop to `stopped/host_lost`. This closes control-plane lifecycle ownership, not
  container ownership: absence is not proof of deletion. The agent's pending-operation
  and startup-retirement gates still protect managed-home reuse.
- Slow teardown remains `stopping` while listed by the current agent. The home stays
  excluded through that state. Missing heartbeat lists provide no absence evidence;
  stale connections cannot reconcile a replacement connection's sessions.
- Interrupted-session runtime directories may need scoped operator cleanup after
  boot retirement. Use the affected session's recorded mounts and prove its owned
  containers are gone first. Never remove unrelated runtime paths or user homes.

Docker is the validated engine. Intel hardware validation follows the existing
[external procedure](reports/rh01-intel-external-validation.md); unavailable hardware
is not certification. Podman/rootless certification and running-session adoption
remain separate work.
