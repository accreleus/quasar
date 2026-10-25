import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render } from "@testing-library/react";
import { STEAM_WARM_BEAT_MS, useSteamWarmHeartbeat } from "./useSteamWarmHeartbeat";

vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
const putSteamWarm = vi.hoisted(() => vi.fn());
vi.mock("../../api/library", () => ({ putSteamWarm }));

function Probe() {
  useSteamWarmHeartbeat();
  return null;
}

function setHidden(hidden: boolean) {
  Object.defineProperty(document, "hidden", { configurable: true, value: hidden });
  act(() => {
    document.dispatchEvent(new Event("visibilitychange"));
  });
}

beforeEach(() => {
  putSteamWarm.mockReset().mockResolvedValue(undefined);
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
  setHidden(false);
});

describe("useSteamWarmHeartbeat", () => {
  it("beats on open, then every 60 s", async () => {
    render(<Probe />);
    await act(async () => {});
    expect(putSteamWarm).toHaveBeenCalledTimes(1);
    await act(async () => {
      vi.advanceTimersByTime(STEAM_WARM_BEAT_MS);
    });
    expect(putSteamWarm).toHaveBeenCalledTimes(2);
    await act(async () => {
      vi.advanceTimersByTime(STEAM_WARM_BEAT_MS);
    });
    expect(putSteamWarm).toHaveBeenCalledTimes(3);
  });

  it("pauses while the tab is hidden and beats once on return", async () => {
    render(<Probe />);
    await act(async () => {});
    setHidden(true);
    await act(async () => {
      vi.advanceTimersByTime(STEAM_WARM_BEAT_MS * 3);
    });
    expect(putSteamWarm).toHaveBeenCalledTimes(1);
    setHidden(false);
    await act(async () => {});
    expect(putSteamWarm).toHaveBeenCalledTimes(2);
  });

  it("keeps beating after a failed beat, and stops on unmount", async () => {
    putSteamWarm.mockRejectedValueOnce(new Error("offline"));
    const { unmount } = render(<Probe />);
    await act(async () => {});
    await act(async () => {
      vi.advanceTimersByTime(STEAM_WARM_BEAT_MS);
    });
    expect(putSteamWarm).toHaveBeenCalledTimes(2);
    unmount();
    await act(async () => {
      vi.advanceTimersByTime(STEAM_WARM_BEAT_MS * 3);
    });
    expect(putSteamWarm).toHaveBeenCalledTimes(2);
  });
});
