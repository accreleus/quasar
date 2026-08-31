// Package runtimeconfig is the shared vocabulary for app-container runtime
// values. The container network arrives from two write paths that never call
// each other — admin-authored (internal/crud/presets.go) and catalog-authored
// (internal/images/preset.go) — both writing runtime_presets.network; two
// copies of the accepted-value list would drift silently toward the laxer one.
// Dependency-neutral (stdlib only) so crud and images both import it without a
// cycle. It does not replace the other boundaries: the SQL CHECK (migration
// 0061) and the node agent's Rust allowlist stay independent enforcement
// points — three layers that agree is defence in depth, one layer three call
// is a single point of failure.
package runtimeconfig

// NetworkInherit is the "no requirement stated" value of the app-facing network
// vocabulary — the schema default of runtime_presets.network. It means: inherit
// whatever the node agent's host default is (QUASAR_CONTAINER_NETWORK, else the
// hardened `none`). It is NOT the same as `none`: an app that states `none`
// pins itself to isolated even on a host whose operator set a bridged default.
const NetworkInherit = ""

// appNetworks is the set an app may declare (runtime_spec.network, a preset's
// column, the admin API, a catalog manifest's `runtime` block).
//
// `host` is deliberately absent — the security boundary of the feature:
// `--network host` removes the namespace altogether, giving a tenant workload
// the host's loopback (control plane, Postgres, docker-proxy) and the ability
// to bind host ports. Everything in this set is portable — a manifest is
// authored elsewhere and installed by an admin approving an app, not an
// infrastructure change — so `host` here would let a manifest dissolve the
// isolation boundary on every installing host. An operator can still choose
// host networking, but only per-host via QUASAR_CONTAINER_NETWORK — a path
// that is not expressible in any object that travels between machines.
var appNetworks = map[string]bool{
	NetworkInherit: true,
	"none":         true,
	"bridge":       true,
}

// ValidNetwork reports whether s is a network value an APP may declare.
// Callers own their own error shape: internal/crud answers a 400, and
// internal/images returns an error that rolls back the install/update.
func ValidNetwork(s string) bool { return appNetworks[s] }

// NetworkValues lists the app-facing values in the order operator-facing copy
// should present them, for building error messages and API enum documentation
// from ONE source rather than restating the set in every message.
func NetworkValues() []string { return []string{NetworkInherit, "none", "bridge"} }

// NetworkError names `host` explicitly rather than only listing what is
// allowed: an operator who typed it needs to know it is refused on purpose and
// where the supported door is.
const NetworkError = `network must be one of "" (inherit the host default), "none" or "bridge" — ` +
	`"host" is not available to an app, preset, or image manifest because it removes the container's ` +
	`network isolation; an operator can set QUASAR_CONTAINER_NETWORK=host on a specific host instead`
