# Release-trust golden vectors

The behaviour of the release trust gates, written down as data, so the Go updater
(`control-plane/internal/updater`) and its Rust port in the recovery actor
(`node-agent/crates/quasar-recovery`, module `trust`) can be held to the same answers
(#356, RH06 decision A3). Both runners read every file here:

- Go: `TestTrustVectorsPassAgainstGo` in `control-plane/internal/updater/trustvectors_test.go`
  (`make test-go`).
- Rust: `every_trust_vector_passes_against_the_rust_port` in
  `node-agent/crates/quasar-recovery/src/trust/vector_tests.rs` (`make test-rust`).

Each runner fails on a file or a `kind` it does not know, or a kind that is missing, runs every vector in every
file, and checks that the count it ran equals the count on disk. A vector one side does
not run is therefore a failing test, not a silent gap.

## Where the vectors come from

Do not edit these files by hand. They are generated from the case table in
`control-plane/internal/updater/trustvectors_cases_test.go`, and every expectation is
the Go updater's own output for the inputs:

```
cd control-plane
QUASAR_WRITE_TRUST_VECTORS=1 go test ./internal/updater -run TestTrustVectorsAreCurrent
```

`TestTrustVectorsAreCurrent` regenerates the files in memory on every `make test-go`
and fails if a committed file differs, so the files cannot drift from Go's behaviour.
Where a case came from one of the Go updater's tests, its `source` names the file and
test; `added:` cases pin a rule those tests exercise only implicitly (check order, a
boundary, a Go `encoding/json` / `net/url` / `encoding/base64` quirk the port must
reproduce). Where the Go test asserted a reason, the generator re-asserts it.

Messages are pinned exactly, except where Go embeds text from a library (a JSON
syntax error, a transport error, a `net/netip` error); there the part the updater
authors is pinned as `message_prefix` / `error_prefix`.

## Files

| file | kind | what it pins |
|---|---|---|
| `admit.json` | `admit` | `Plan`'s request gates (uuid, single flight, closed component table, image, digest, namespace allowlist, in that order) and the ADR 0003 signature gate. `fetch` vectors gather evidence the way the updater's server does. `caller: agent` vectors pin the agent-socket guard (architecture §5.2), which the caller-less Go updater does not have: Go is held to `expect_without_caller_guard`, the port to both. |
| `verify_signature.json` | `verify_signature` | `VerifyManifestSignature`: the detached document decoded exactly as Go decodes it, any entry under any trusted key, key rotation. |
| `evidence.json` | `evidence` | How HTTP outcomes become `signed` / `absent` / `fetch_error`. Only a 404 on the signature asset is an absence. |
| `evidence_gate.json` | `evidence_gate` | Whether a request's assets are fetched at all. |
| `config.json` | `config` | The operator knobs: signature mode, trusted keys, allowed namespaces, manifest base URL. |
| `redirect.json` | `redirect` | The fetch's redirect policy. |

Byte strings are `{"text": ...}` (exact UTF-8), `{"base64": ...}`, or
`{"segments": [{"text": ..., "count": n}]}` for large or deeply nested inputs.
URLs live under `https://releases.example.invalid`.

## Keys

No key material is committed, in line with the Go updater's own signature tests. A
vector names a key by label (`"public_key_of": "release-2026"`, or
`{public_key_of:release-2026}` inside a trusted-key list), and both runners derive it:
the ed25519 seed is SHA-256 of the fixed prefix in `vectorKeySeedPrefix` followed by
the label. These keys are **public by construction** — anyone can recompute the private
half — so they are test keys only. Never put one in `QUASAR_UPDATER_TRUSTED_KEYS`.

## Where the Rust fetch differs from Go's (resolve before #367)

The vectors pin the decisions; the HTTPS client under them (`trust::https`) is held to
the Go updater's `signature_source.go` on `http.DefaultTransport` by its own tests. Where
it cannot match Go it refuses, so each difference can only turn a Go success into a
`signature_invalid`, never into an unverified apply. Two must be resolved before the Go
updater is deleted (RH06-15, #367):

- **Proxies.** Go routes the fetch through `HTTPS_PROXY` (honouring `NO_PROXY`). The Rust
  client ports that decision exactly but has no proxy client, so a request Go would
  proxy is refused. Either add CONNECT-over-https (and the proxy schemes Go accepts), or
  document that a host behind a proxy cannot run `verify` or `require`.
- **Trust roots.** Go uses the updater image's `ca-certificates` and honours
  `SSL_CERT_FILE` / `SSL_CERT_DIR`. The Rust client uses webpki-roots (Mozilla's set)
  and ignores both variables, so a mirror behind a private CA fails closed.

Also different, and refusing the same way: a non-ASCII mirror host (no IDNA), HTTP/2
(not offered), and connection reuse (none). Refusal and error message text follows Go's
where it can; only the prefix Go authors is pinned.
