/** The `?transport=` URL switch (spec #1024, story 2): the launch request's transport
 *  preference. The profile still decides whether it is honoured (control-api.md §Transport). */
export function transportPreference(search: string = location.search): "webrtc" | "webtransport" | undefined {
  const raw = new URLSearchParams(search).get("transport");
  return raw === "webrtc" || raw === "webtransport" ? raw : undefined;
}

/** The `?codec=pyrowave` URL switch (#1057): the launch request's `pyrowave: true`, as the
 *  Launch options Codec entry will send it (B11). The stream plan still decides. */
export function pyrowavePreference(search: string = location.search): boolean {
  return new URLSearchParams(search).get("codec") === "pyrowave";
}
