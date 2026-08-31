# control-plane (Go)

Accounts, authentication/authorization, the public + control API, WebRTC signaling, and the session scheduler / resource governor. Backed by Postgres. Holds no per-host GPU state — it directs node agents.

Built in Phase 1+. The Kubernetes story (DaemonSet node agents + Deployment control plane) falls out of this split; see `../docs/architecture-and-plan.md`.

Update the module path in `go.mod` (REPLACE_ME) to your repo path.
