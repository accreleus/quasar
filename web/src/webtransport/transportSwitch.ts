/** The `?transport=` URL switch (spec #1024, story 2): the launch request's transport
 *  preference. The profile still decides whether it is honoured (control-api.md §Transport). */
export function transportPreference(search: string = location.search): "webrtc" | "webtransport" | undefined {
  const raw = switchValue(search, "transport", TRANSPORT_KEY);
  return raw === "webrtc" || raw === "webtransport" ? raw : undefined;
}

/** The `?codec=pyrowave` dev switch (#1057): the launch request's `pyrowave: true`, as the
 *  Launch options Codec entry sends it. The stream plan still decides. */
export function pyrowavePreference(search: string = location.search): boolean {
  return switchValue(search, "codec", CODEC_KEY) === "pyrowave";
}

/** Remembers the switches in this URL for the tab (#1216), so a game page opened from the
 *  library still launches with them. Called by useLaunch on every page that launches. */
export function rememberUrlSwitches(search: string = location.search): void {
  transportPreference(search);
  pyrowavePreference(search);
}

// #1216: a switch seen in the URL is kept in sessionStorage; a later URL without it reads that.
// An explicit value in the URL replaces the remembered one. Storage blocked: the URL alone, as before.
const TRANSPORT_KEY = "quasar.switch.transport";
const CODEC_KEY = "quasar.switch.codec";

function switchValue(search: string, param: string, key: string): string | null {
  const fromUrl = new URLSearchParams(search).get(param);
  try {
    if (fromUrl !== null) sessionStorage.setItem(key, fromUrl);
    return fromUrl ?? sessionStorage.getItem(key);
  } catch {
    return fromUrl;
  }
}
