/**
 * Facts baked into the bundle at build time (vite.config.ts `define`).
 *
 * SOURCE_REF is the git ref this SPA was built from: the exact release tag when
 * the build sits on one, otherwise the commit. It is what lets the admin
 * console point a second host at the installer script from the SAME tree the
 * control plane runs (#100) without the control plane having to know its own
 * version. Empty when the build could not determine it — the UI then offers the
 * bare enrollment string only.
 */
export const SOURCE_REF: string = typeof __QUASAR_SOURCE_REF__ === "string" ? __QUASAR_SOURCE_REF__ : "";
