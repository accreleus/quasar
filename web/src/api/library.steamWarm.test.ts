import { afterEach, describe, expect, it, vi } from "vitest";
import { putSteamWarm, updateSteamDialogs } from "./library";

afterEach(() => vi.unstubAllGlobals());

describe("warm Steam calls", () => {
  it("PATCHes profile-preferences with only steam_dialogs", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ steam_dialogs: "dismiss_safe" })));
    vi.stubGlobal("fetch", fetchMock);
    await updateSteamDialogs("tok", "dismiss_safe");
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toMatch(/\/me\/profile-preferences$/);
    expect(init.method).toBe("PATCH");
    expect(JSON.parse(init.body)).toEqual({ steam_dialogs: "dismiss_safe" });
  });

  it("PUTs the heartbeat with no body and takes the 204", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    await expect(putSteamWarm("tok")).resolves.toBeUndefined();
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toMatch(/\/me\/steam-warm$/);
    expect(init.method).toBe("PUT");
    expect(init.body).toBeUndefined();
  });
});
