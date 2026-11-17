// Chrome asks per site before a page reaches a local-network address, and a fresh browser context keeps no permission.
// Granting it up front keeps a session page from stalling on the prompt (a WebTransport launch would fall back to WebRTC).
// Best-effort, as stream/run.mjs openHost does it (#1455): a Chrome without the permission never asks, and the warning makes a
// grant that stops working visible.
export async function grantLocalNetwork(context, warn = console.warn) {
  await context.grantPermissions?.(["local-network-access"]).catch((e) => warn(`validate: local network access not granted (${String(e?.message ?? e).split("\n")[0]}); Chrome may ask on each stream`));
}
