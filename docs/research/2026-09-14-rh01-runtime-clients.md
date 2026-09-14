# Rust client options for the runtime API adapter

Research for [#227](https://github.com/accreleus/quasar/issues/227), under [map #224](https://github.com/accreleus/quasar/issues/224). Checked 2026-09-14 against repository commit `5998fc5002cca6d138d30e170b1cfd313d313336`. This is decision input, not an approved dependency choice or implementation. No dependencies, runtime code, tests, or live operations were changed or run.

## Recommendation

Prefer **Bollard behind Quasar-owned request, result, event, and error types** for Docker-first implementation. Direct HTTP with reqwest is a credible alternative if a bounded implementation experiment exposes an SDK schema or cancellation limitation. Choose direct Hyper only when connection/upgraded-stream control demonstrably requires it. Neither choice establishes Podman or rootless support; that requires a separate capability and behavior contract.

This recommendation is engineering judgment based on the existing call surface and the protocol work compared below. The human decision ticket must choose the client, supported daemon/API floor, endpoint policy, and async ownership boundary before implementation.

## Repository fit

The agent is Rust 2021 and already uses Tokio, futures-util, serde, and serde_json. Its HTTP dependency is blocking ureq 3.4, intentionally used on blocking workers. Bollard, reqwest, and Hyper are absent from the inspected lockfile. The image pins Rust 1.94.0; the manifest warns that transitive dependency updates can exceed that compiler and preserves kstring 2.0.2 for this reason. Adding an async client therefore introduces a new HTTP dependency tree even though the executor and serialization ecosystem already fit. [Manifest](https://github.com/accreleus/quasar/blob/5998fc5002cca6d138d30e170b1cfd313d313336/node-agent/Cargo.toml), [lockfile](https://github.com/accreleus/quasar/blob/5998fc5002cca6d138d30e170b1cfd313d313336/node-agent/Cargo.lock), [image toolchain](https://github.com/accreleus/quasar/blob/5998fc5002cca6d138d30e170b1cfd313d313336/deploy/Dockerfile.vulkan#L113).

`ContainerRuntime` is currently a synchronous CLI facade selected by `QUASAR_CONTAINER_RUNTIME`. It has 30-second ordinary-command and 600-second pull deadlines, bounded retained output, a live log follower because auto-removed containers lose their logs, and inspect-before-pull behavior for locally built images. `pull_command`, `build_command`, and `run_raw` also expose CLI mechanics to callers. Image operations run on dedicated threads and survive control-plane disconnects. This rules out treating an SDK migration as a mechanical replacement of `Command::output()`. [Runtime facade](https://github.com/accreleus/quasar/blob/5998fc5002cca6d138d30e170b1cfd313d313336/node-agent/src/session/container.rs), [image ownership](https://github.com/accreleus/quasar/blob/5998fc5002cca6d138d30e170b1cfd313d313336/node-agent/src/images/mod.rs).

Recommended boundary: one deliberately owned async execution context, bounded operation admission, and operation lifetimes independent of a signaling connection. Decide whether existing blocking callers bridge into that context or migrate to async; avoid creating a runtime per call or nesting blocking execution inside an async worker. Preserve current time and output bounds as explicit policies, with intentional exceptions for subscriptions.

## Candidate comparison

| Concern | Bollard | Direct HTTP |
|---|---|---|
| Unix sockets | Explicit Unix connection API; `pipe` feature enables socket support | reqwest has native `ClientBuilder::unix_socket`; Hyper requires connector/I/O composition |
| API models | Generated request/response types and endpoint methods | Quasar owns serialization, query escaping, version-sensitive shapes, and response validation |
| Negotiation | Explicit `negotiate_version()` method | Implement version discovery, supported-range intersection, and request prefixing |
| Pulls / logs / events | Typed streams and Docker framing already implemented | Incremental JSON records, log framing, partial reads, and EOF rules become adapter code |
| Cancellation / retries | Quasar still owns operation intent and reconciliation | Same ownership burden, plus transport configuration |
| Portability | Docker-compatible Podman access available; types still follow Docker schema | Easier endpoint-specific escape hatches; every compatibility rule is maintained locally |

Bollard 0.21.1 offers futures/streams, generated models, and a minimal local configuration with default features disabled and `pipe` enabled. Its Podman helper searches environment/rootless/system sockets and can ultimately select Docker. **Use an explicit configured endpoint and verify daemon identity** rather than letting discovery silently change runtime selection. TLS, SSH, and BuildKit features are optional and should be enabled only for selected requirements. [Bollard overview](https://docs.rs/bollard/latest/bollard/), [connection and endpoint methods](https://docs.rs/bollard/latest/bollard/struct.Docker.html).

reqwest 0.13.5 has Unix socket support, JSON response decoding, and byte streaming. These solve transport and buffering primitives, not Docker records or semantics. Its Unix connector ignores TCP/proxy options and bypasses DNS; an HTTPS URI still requests TLS over the socket. Choose an explicit HTTP URI for a plain local socket, disable redirects and configure retry policy deliberately for mutations. [Builder](https://docs.rs/reqwest/latest/reqwest/struct.ClientBuilder.html), [response API](https://docs.rs/reqwest/latest/reqwest/struct.Response.html).

Hyper exposes the lower-level connection handshake, sender, body frames, and connection-driving future. That control is useful for specialized upgraded I/O, but adds lifecycle plumbing without eliminating Docker-specific work. [Hyper client guide](https://hyper.rs/guides/1/client/basic/).

## Negotiation and streaming details that affect the choice

Bollard's checked source initializes `API_DEFAULT_VERSION` to **1.53**, while its overview describes a **1.52** generated schema. Its negotiation fetches version information and lowers the configured client version when the server advertises an older maximum. Do not infer supported capabilities or a minimum-compatible API solely from the overview or a successful negotiation. Record both daemon identity and selected API, explicitly enforce Quasar's chosen floor, and verify requested fields against it. Its request timeout surrounds transport response acquisition; it is not a complete lifetime deadline for subsequent streamed body consumption. [Bollard v0.21.1 source](https://github.com/fussybeaver/bollard/blob/v0.21.1/src/docker.rs).

Pull APIs return progress streams: Bollard turns embedded error details into `DockerStreamError`. A successful HTTP status alone is insufficient; consume the stream and inspect the resulting image before reporting readiness. [Image stream implementation](https://github.com/fussybeaver/bollard/blob/v0.21.1/src/image.rs). The SDK's decoder distinguishes framed stdout/stderr and unframed output, handles partial records, and flushes trailing log bytes at EOF. Direct HTTP must own these cases and bounded record handling. [Decoder implementation](https://github.com/fussybeaver/bollard/blob/v0.21.1/src/read.rs).

For either client, proposed acceptance evidence should include fragmented progress JSON, an error following HTTP 200, TTY and non-TTY logs, a final line without a newline, an idle event stream, disconnect/reconnect, and container disappearance during log drain. Event consumers should reconcile observed state after reconnect; a subscription is not a durable state store. These are implementation requirements proposed here, not tests performed by this research.

## Cancellation, failures, and portability

Tokio timeout cancels its wrapped future by dropping it. This bounds the local wait; it does not prove that a daemon mutation was never accepted or was reversed. Treat an interrupted create/start/pull as potentially uncertain and reconcile using stable object identity before deciding whether to retry. No automatic CLI fallback after an uncertain mutation. [Tokio timeout semantics](https://docs.rs/tokio/latest/tokio/time/fn.timeout.html), [owner-set boundary](https://github.com/accreleus/quasar/issues/224).

Bollard distinguishes HTTP status/message, I/O, timeout, deserialization, stream, and container-wait errors. Map these into Quasar categories while preserving an internal cause; keep operation context because 404 during removal differs from 404 during launch. Maintain the existing sanitized operator messages: SDK error text is not safe public output by default. [Error variants](https://docs.rs/bollard/latest/bollard/errors/enum.Error.html), [existing sanitization](https://github.com/accreleus/quasar/blob/5998fc5002cca6d138d30e170b1cfd313d313336/node-agent/src/images/errors.rs).

Podman's service documents a Docker compatibility layer and a separate native Libpod API; it also says unsupported version prefixes are not rejected. Therefore a successful versioned call is weak evidence of feature support. Rootless socket activation establishes connectivity, not permission to realize Quasar's requested devices, mounts, or namespace behavior. [Podman service documentation](https://docs.podman.io/en/latest/markdown/podman-system-service.1.html). Keep capability checks and engine-specific conversion inside adapters; do not leak Bollard models into the portable interface. A future Libpod adapter can use direct HTTP for native operations without dictating today's Docker implementation.

## Remaining decision and evidence

The registry reports Bollard 0.21.1 released 2026-08-16 without a declared `rust_version`, and reqwest 0.13.5 released 2026-09-08 with MSRV 1.85.0. These are maintenance signals, not proof that a new full dependency resolution builds in Quasar's image. [Bollard registry metadata](https://crates.io/api/v1/crates/bollard), [reqwest registry metadata](https://crates.io/api/v1/crates/reqwest).

Before adoption, require a minimal-feature dependency resolution/build under Rust 1.94.0, preservation of existing pins, the chosen daemon-version matrix, stream/cancellation/error fixtures, and remote Docker lifecycle validation under the repository's implementation gates. Separately decide whether classic image builds remain a later migration unit; enabling BuildKit merely to obtain a client would silently broaden this task. Final client selection and async execution ownership remain human decisions.
