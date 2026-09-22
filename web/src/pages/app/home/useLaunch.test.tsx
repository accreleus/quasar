import { act, renderHook, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as libraryApi from "../../../api/library";
import { ApiError } from "../../../api/client";
import type { App } from "../../../api/types";
import { ToastProvider } from "../../../components/Toast";
import { useLaunch } from "./useLaunch";

vi.mock("../../../api/library");

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
    expect(result.current.waitingReason).toBe("slot");
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
