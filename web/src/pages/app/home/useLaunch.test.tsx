import { act, renderHook, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as libraryApi from "../../../api/library";
import { ApiError } from "../../../api/client";
import type { App } from "../../../api/types";
import { ToastProvider } from "../../../components/Toast";
import { useLaunch } from "./useLaunch";

vi.mock("../../../api/library");

const navigate = vi.fn();
vi.mock("react-router-dom", async (importOriginal) => ({
  ...(await importOriginal<typeof import("react-router-dom")>()),
  useNavigate: () => navigate,
}));

const app = { id: "app-1", name: "Portal" } as App;

function wrapper({ children }: { children: ReactNode }) {
  return (
    <MemoryRouter>
      <ToastProvider>{children}</ToastProvider>
    </MemoryRouter>
  );
}

function renderLaunch() {
  return renderHook(
    () =>
      useLaunch({
        token: "tok",
        apps: [app],
        liveSession: null,
        canDecodeH264: true,
        fetchProfiles: async () => null,
        revealDetail: () => {},
      }),
    { wrapper },
  );
}

describe("useLaunch waiting toast", () => {
  afterEach(() => vi.resetAllMocks());

  it("waits for a GPU that can encode the hand-picked codec (#304)", async () => {
    vi.mocked(libraryApi.launchSession).mockRejectedValue(
      new ApiError(503, "capacity_exhausted", "no free GPU can encode av1; try again shortly", undefined, undefined, 5),
    );
    const { result, unmount } = renderLaunch();

    act(() => {
      void result.current.launchApp(app, "1080p60", "av1");
    });

    expect(await screen.findByText("Waiting for a GPU that can encode AV1…")).toBeTruthy();
    await waitFor(() => expect(result.current.waitingReason).toBe("slot"));
    unmount(); // aborts the pending retry wait
  });

  it("keeps the slot copy when the launch carried no codec", async () => {
    vi.mocked(libraryApi.launchSession).mockRejectedValue(
      new ApiError(503, "capacity_exhausted", "all capacity is in use; try again shortly", undefined, undefined, 5),
    );
    const { result, unmount } = renderLaunch();

    act(() => {
      void result.current.launchApp(app, "1080p60");
    });

    expect(await screen.findByText("Waiting for a slot to free up…")).toBeTruthy();
    unmount();
  });
});

describe("useLaunch while the previous session is stopping (#754)", () => {
  const previous = { id: "prev-1", app_id: "app-0" };
  const homeInUse = () => new ApiError(409, "home_in_use", "still shutting down", undefined, "prev-1");
  const launched = {
    session: { id: "new-1", stream: { width: 1920, height: 1080, fps: 60, codec: "h264" } },
    signaling: { url: "ws://x", token: "t" },
  } as never;
  const prior = (state: string) =>
    ({ session: { ...previous, state } }) as never;

  function renderWithPrevious() {
    return renderHook(
      () =>
        useLaunch({
          token: "tok",
          apps: [app, { id: "app-0", name: "Steam" } as App],
          liveSession: null,
          canDecodeH264: true,
          fetchProfiles: async () => null,
          revealDetail: () => {},
        }),
      { wrapper },
    );
  }

  afterEach(() => {
    vi.useRealTimers();
    vi.resetAllMocks();
    navigate.mockReset();
  });

  it("waits behind a closing step, then launches", async () => {
    vi.useFakeTimers();
    vi.mocked(libraryApi.launchSession).mockRejectedValueOnce(homeInUse()).mockResolvedValue(launched);
    vi.mocked(libraryApi.getSession).mockResolvedValue(prior("stopping"));
    const { result } = renderWithPrevious();

    await act(async () => {
      void result.current.launchApp(app);
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(result.current.waitingReason).toBe("closing");
    expect(screen.getByText("Closing Steam…")).toBeTruthy();
    expect(libraryApi.launchSession).toHaveBeenCalledTimes(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    expect(libraryApi.launchSession).toHaveBeenCalledTimes(2);
    expect(navigate).toHaveBeenCalledWith("/app/session/new-1", expect.anything());
  });

  it("ends in an explained failure when the stop never finishes, at the 45 s budget", async () => {
    vi.useFakeTimers();
    vi.mocked(libraryApi.launchSession).mockRejectedValue(homeInUse());
    vi.mocked(libraryApi.getSession).mockResolvedValue(prior("stopping"));
    const { result } = renderWithPrevious();

    await act(async () => {
      void result.current.launchApp(app);
      await vi.advanceTimersByTimeAsync(44_500);
    });
    expect(result.current.waitingReason).toBe("closing");
    expect(screen.queryByText("Steam is taking too long to close")).toBeNull();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    expect(screen.getByText("Steam is taking too long to close")).toBeTruthy();
    expect(result.current.launching).toBe(false);
    expect(result.current.waitingReason).toBeNull();
    expect(navigate).not.toHaveBeenCalled();
  });

  it("retries when the holder finished between the 409 and the lookup", async () => {
    vi.useFakeTimers();
    vi.mocked(libraryApi.launchSession).mockRejectedValueOnce(homeInUse()).mockResolvedValue(launched);
    vi.mocked(libraryApi.getSession).mockResolvedValue(prior("stopped"));
    const { result } = renderWithPrevious();

    await act(async () => {
      void result.current.launchApp(app);
      await vi.advanceTimersByTimeAsync(1_000);
    });
    expect(libraryApi.launchSession).toHaveBeenCalledTimes(2);
    expect(navigate).toHaveBeenCalledWith("/app/session/new-1", expect.anything());
    expect(screen.queryByText("Portal is already running")).toBeNull();
  });

  it("keeps waiting through a failed lookup once the wait has started", async () => {
    vi.useFakeTimers();
    vi.mocked(libraryApi.launchSession)
      .mockRejectedValueOnce(homeInUse())
      .mockRejectedValueOnce(homeInUse())
      .mockResolvedValue(launched);
    vi.mocked(libraryApi.getSession)
      .mockResolvedValueOnce(prior("stopping"))
      .mockRejectedValueOnce(new ApiError(500, "internal", "boom"));
    const { result } = renderWithPrevious();

    await act(async () => {
      void result.current.launchApp(app);
      await vi.advanceTimersByTimeAsync(2_000);
    });
    expect(libraryApi.launchSession).toHaveBeenCalledTimes(3);
    expect(navigate).toHaveBeenCalledWith("/app/session/new-1", expect.anything());
    expect(screen.queryByText("Portal is already running")).toBeNull();
  });

  it("refuses when the very first lookup fails", async () => {
    vi.mocked(libraryApi.launchSession).mockRejectedValue(homeInUse());
    vi.mocked(libraryApi.getSession).mockRejectedValue(new ApiError(500, "internal", "boom"));
    const { result } = renderWithPrevious();

    await act(async () => {
      await result.current.launchApp(app);
    });
    expect(libraryApi.launchSession).toHaveBeenCalledTimes(1);
    expect(screen.getByText("Portal is already running")).toBeTruthy();
  });

  it("stops quietly when the page goes away mid-wait", async () => {
    vi.useFakeTimers();
    vi.mocked(libraryApi.launchSession).mockRejectedValue(homeInUse());
    vi.mocked(libraryApi.getSession).mockResolvedValue(prior("stopping"));
    const { result, unmount } = renderWithPrevious();

    await act(async () => {
      void result.current.launchApp(app);
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.getByText("Closing Steam…")).toBeTruthy();
    unmount();
    await vi.advanceTimersByTimeAsync(60_000);

    expect(libraryApi.launchSession).toHaveBeenCalledTimes(1);
    expect(screen.queryByText("Steam is taking too long to close")).toBeNull();
    expect(navigate).not.toHaveBeenCalled();
  });

  it("refuses a genuinely running session as before", async () => {
    vi.mocked(libraryApi.launchSession).mockRejectedValue(homeInUse());
    vi.mocked(libraryApi.getSession).mockResolvedValue(prior("running"));
    const { result } = renderWithPrevious();

    await act(async () => {
      await result.current.launchApp(app);
    });
    expect(libraryApi.launchSession).toHaveBeenCalledTimes(1);
    expect(result.current.waitingReason).toBeNull();
    expect(screen.getByText("Portal is already running")).toBeTruthy();
  });

  it("sends one launch for two clicks in the same turn", async () => {
    vi.useFakeTimers();
    vi.mocked(libraryApi.launchSession).mockRejectedValue(homeInUse());
    vi.mocked(libraryApi.getSession).mockResolvedValue(prior("stopping"));
    const { result, unmount } = renderWithPrevious();

    await act(async () => {
      void result.current.launchApp(app);
      void result.current.launchApp(app);
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(libraryApi.launchSession).toHaveBeenCalledTimes(1);
    unmount();
  });
});

describe("useLaunch full screen on Play (#647)", () => {
  const PREFS = "quasar.display.preferences";
  let order: string[];
  let fsEl: Element | null;

  beforeEach(() => {
    order = [];
    fsEl = null;
    localStorage.removeItem(PREFS);
    Object.defineProperty(document, "fullscreenElement", { configurable: true, get: () => fsEl });
    document.documentElement.requestFullscreen = vi.fn(() => {
      order.push("fullscreen");
      return Promise.resolve();
    });
    document.exitFullscreen = vi.fn(() => Promise.resolve());
    Object.defineProperty(navigator, "keyboard", {
      configurable: true,
      value: { lock: vi.fn(() => (order.push("keyboard-lock"), Promise.resolve())), unlock: vi.fn() },
    });
  });
  afterEach(() => {
    vi.resetAllMocks();
    localStorage.removeItem(PREFS);
  });

  it("asks for full screen in the click, before the launch request", () => {
    vi.mocked(libraryApi.launchSession).mockImplementation(() => {
      order.push("launch-request");
      return new Promise(() => {});
    });
    const { result } = renderLaunch();
    act(() => {
      void result.current.launchApp(app);
    });
    expect(order).toEqual(["fullscreen", "launch-request"]);
  });

  it("quick launch spends the click before awaiting the profiles", () => {
    let profilesCalledAfter: string[] = [];
    const { result } = renderHook(
      () =>
        useLaunch({
          token: "tok",
          apps: [app],
          liveSession: null,
          canDecodeH264: true,
          fetchProfiles: () => {
            profilesCalledAfter = [...order];
            return new Promise(() => {});
          },
          revealDetail: () => {},
        }),
      { wrapper },
    );
    act(() => {
      void result.current.quickLaunch(app);
    });
    expect(profilesCalledAfter).toEqual(["fullscreen"]);
  });

  it("leaves full screen when the launch fails", async () => {
    vi.mocked(libraryApi.launchSession).mockImplementation(async () => {
      fsEl = document.documentElement;
      throw new ApiError(500, "internal", "boom");
    });
    const { result } = renderLaunch();
    await act(async () => {
      await result.current.launchApp(app);
    });
    expect(document.exitFullscreen).toHaveBeenCalled();
  });

  it("never takes Keyboard Lock at the click: capture owns it (#647 review)", () => {
    vi.mocked(libraryApi.launchSession).mockImplementation(() => new Promise(() => {}));
    const { result } = renderLaunch();
    act(() => {
      void result.current.launchApp(app);
    });
    expect(order).not.toContain("keyboard-lock");
  });

  it("leaves full screen when the page unmounts mid-launch", async () => {
    vi.mocked(libraryApi.launchSession).mockImplementation(() => new Promise(() => {}));
    document.documentElement.requestFullscreen = vi.fn(() => {
      fsEl = document.documentElement;
      return Promise.resolve();
    });
    const { result, unmount } = renderLaunch();
    act(() => {
      void result.current.launchApp(app);
    });
    unmount();
    await act(async () => {});
    expect(document.exitFullscreen).toHaveBeenCalled();
  });

  it("setting off: no full screen", () => {
    localStorage.setItem(PREFS, JSON.stringify({ scalingMode: "contain", launchImmersive: false }));
    vi.mocked(libraryApi.launchSession).mockImplementation(() => new Promise(() => {}));
    const { result } = renderLaunch();
    act(() => {
      void result.current.launchApp(app);
    });
    expect(document.documentElement.requestFullscreen).not.toHaveBeenCalled();
    expect(order).toEqual([]);
  });
});
