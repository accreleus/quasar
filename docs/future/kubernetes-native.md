# Kubernetes-native (DEFERRED)

**Status:** deferred indefinitely. Was the architecture doc's original "Phase 4"; **dropped
from the active sequence** while the focus is single-host → small multi-host. No milestone
until revived.

## Why deferred
The current and near-term focus is a single host (expanding to a small multi-host fleet
later). Kubernetes is a *packaging + scale* concern, not a capability the product needs to
be useful for self-hosters at this stage. Deferring it costs nothing **because the
control-plane / node-agent split has existed since Phase 1** — when K8s is revived it is
packaging, not re-architecture.

## What it would entail (when revived)
- **node-agent → DaemonSet** on GPU nodes.
- **control-plane → Deployment.**
- **sessions → scheduled workloads** integrated with GPU device plugins.
- The scheduler's multi-host placement (Phase 3) maps onto the K8s scheduler / a custom
  scheduler; the storage-provider abstraction (Phase 5) maps onto PVs/PVCs + CSI.

## Revival triggers
Operating at a scale where hand-rolled multi-host deployment hurts, or a deployment target
that is K8s-native. Until then, `docker compose` (single + small multi-host) is the
supported deployment.
