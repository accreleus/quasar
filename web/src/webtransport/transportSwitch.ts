/** The `?transport=` URL switch (spec #1024, story 2): the launch request's transport
 *  preference. The profile still decides whether it is honoured (control-api.md §Transport). */
export function transportPreference(search: string = location.search): "webrtc" | "webtransport" | undefined {
  const raw = new URLSearchParams(search).get("transport");
  return raw === "webrtc" || raw === "webtransport" ? raw : undefined;
}
