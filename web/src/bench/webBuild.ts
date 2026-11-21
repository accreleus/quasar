// The web build's own identity: the commit it was built from and when. Vite bakes both in
// (`define` in vite.config.ts); bench mode reports them (window.__qBench.build()) so an
// end-to-end suite run can write the exact web client into its builds.json (#1281).
// vitest does not use vite.config.ts, so the constants are absent there and read "unknown".
declare const __QUASAR_WEB_COMMIT__: string;
declare const __QUASAR_WEB_BUILT_AT__: string;

export interface WebBuild {
  commit: string;
  built_at: string;
}

export const WEB_BUILD: WebBuild = {
  commit: typeof __QUASAR_WEB_COMMIT__ === "string" ? __QUASAR_WEB_COMMIT__ : "unknown",
  built_at: typeof __QUASAR_WEB_BUILT_AT__ === "string" ? __QUASAR_WEB_BUILT_AT__ : "unknown",
};
