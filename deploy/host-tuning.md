# Host tuning for Quasar nodes

One-time host-level settings a node needs beyond the compose stack itself.
These cannot live in `docker-compose.yml`: they are non-namespaced kernel
sysctls (global, not per-container), so Docker refuses them via `sysctls:`.

## UDP send buffer (`net.core.wmem_default`) — required on every media node

The WebRTC media path sends via libnice/`nicesink`, which never calls
`setsockopt(SO_SNDBUF)` — its UDP sockets inherit the **kernel default**
(`net.core.wmem_default`, 208 KB on stock Linux), not `wmem_max`. An IDR
keyframe burst at 8 Mbps+ overflows 208 KB; the kernel silently drops the
excess, and the `rtpgccbwe` ABR estimator misreads the loss as network
congestion and cuts the bitrate (the Wolf-research "UDP pacing" gap;
#149/F-2).

Set 2 MB on every host that runs a node-agent:

```
sysctl -w net.core.wmem_default=2097152
```

Persist it:

- **Standard Linux** (e.g. the hermes dev box): drop a file in
  `/etc/sysctl.d/99-quasar.conf` containing
  `net.core.wmem_default = 2097152`.
- **unraid** (e.g. Tower): the rootfs is a ramdisk — append the `sysctl -w`
  line to `/boot/config/go` instead (it runs at boot).

Both dev hosts were set + persisted on 2026-06-12. Phase 5+ deployment
images/installers should apply this automatically.

## Already-documented host requirements (for completeness)

- Chrome's mDNS (`.local`) ICE candidates resolve inside the node-agent
  container (in-image avahi + nss-mdns, started by the entrypoint), so no
  host-side avahi install is needed (CLAUDE.md "WebRTC / browser testing
  gotchas").
- `device_cgroup_rules: ['c 13:* rmw']` stays in the compose file (uinput
  devices born at runtime) — that one *is* per-container.
