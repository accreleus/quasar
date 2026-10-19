import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render } from "@testing-library/react";
import { useState } from "react";
import { LaunchActivityContext, SteamWarmHeartbeat, useReportLaunching } from "./SteamWarmHeartbeat";
import { STEAM_WARM_BEAT_MS } from "./useSteamWarmHeartbeat";

vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
const putSteamWarm = vi.hoisted(() => vi.fn());
vi.mock("../../api/library", () => ({ putSteamWarm }));

let setLaunching: (v: boolean) => void = () => {};
function Page() {
  const [launching, set] = useState(false);
  setLaunching = set;
  useReportLaunching(launching);
  return null;
}
function Layout() {
  const [launching, set] = useState(false);
  return (
    <LaunchActivityContext.Provider value={set}>
      <SteamWarmHeartbeat paused={launching} />
      <Page />
    </LaunchActivityContext.Provider>
  );
}

beforeEach(() => {
  putSteamWarm.mockReset().mockResolvedValue(undefined);
  vi.useFakeTimers();
});
afterEach(() => vi.useRealTimers());

describe("SteamWarmHeartbeat", () => {
  it("beats once on mount, and a null child renders nothing", async () => {
    const { container } = render(<SteamWarmHeartbeat paused={false} />);
    await act(async () => {});
    expect(putSteamWarm).toHaveBeenCalledTimes(1);
    expect(container).toBeEmptyDOMElement();
  });

  it("holds the beat while a launch is in flight, and beats again when it ends", async () => {
    render(<Layout />);
    await act(async () => {});
    expect(putSteamWarm).toHaveBeenCalledTimes(1);

    act(() => setLaunching(true));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(STEAM_WARM_BEAT_MS * 3);
    });
    expect(putSteamWarm).toHaveBeenCalledTimes(1);

    act(() => setLaunching(false));
    await act(async () => {});
    expect(putSteamWarm).toHaveBeenCalledTimes(2);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(STEAM_WARM_BEAT_MS);
    });
    expect(putSteamWarm).toHaveBeenCalledTimes(3);
  });
});
