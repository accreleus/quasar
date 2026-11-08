/** Host detail "Compatibility tools" card (#1077, mock #1067): one row per state the contract defines. */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AuthContext } from "../../../../auth/context";
import type { AuthContextValue } from "../../../../auth/context";
import { ApiError } from "../../../../api/client";
import type { HostCompatTools } from "../../../../api/types";
import { CompatToolsCard } from "./CompatToolsCard";

vi.mock("../../../../api/admin", () => ({ getHostCompatTools: vi.fn() }));
import * as adminApi from "../../../../api/admin";

const GB = 1024 ** 3;
const auth = { token: "tok" } as unknown as AuthContextValue;

type Entry = HostCompatTools["tools"][number];
const entry = (over: Partial<Entry>): Entry => ({
  tool_id: "t1",
  steam_name: "GE-Proton10-4",
  tag: "GE-Proton10-4",
  state: "installed",
  bytes: 1.6 * GB,
  reason: null,
  run_id: null,
  ...over,
});

function show(tools: Entry[], over: Partial<HostCompatTools> = {}) {
  vi.mocked(adminApi.getHostCompatTools).mockResolvedValue({
    supported: true,
    free_bytes: 1.1 * GB,
    reported_at: "2026-11-07T00:00:00Z",
    tools,
    ...over,
  });
  render(
    <AuthContext.Provider value={auth}>
      <MemoryRouter>
        <CompatToolsCard hostId="h1" />
      </MemoryRouter>
    </AuthContext.Provider>,
  );
}

beforeEach(() => vi.clearAllMocks());
afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("CompatToolsCard", () => {
  it("shows an installed tool with its disk and the total used", async () => {
    show([entry({}), entry({ tool_id: "t2", steam_name: "GE-Proton9-27", tag: "GE-Proton9-27", bytes: 0.9 * GB })]);
    expect(await screen.findByText("GE-Proton10-4")).toBeTruthy();
    expect(screen.getByText("1.6 GB")).toBeTruthy();
    expect(screen.getByText("2.5 GB used")).toBeTruthy();
  });

  it("shows an install in flight without inventing a percent", async () => {
    show([entry({ state: "installing", bytes: null })]);
    expect(await screen.findByText("Installing")).toBeTruthy();
    expect(screen.queryByText(/%/)).toBeNull();
    expect(screen.queryByText(/used$/)).toBeNull();
  });

  it("names a failed install's reason and links to the job", async () => {
    show([entry({ state: "failed", bytes: null, reason: "checksum_mismatch", run_id: "r1" })]);
    expect(await screen.findByText("Failed")).toBeTruthy();
    expect(screen.getByText(/did not match its checksum/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "See the Compatibility tools job for GE-Proton10-4" }).getAttribute("href")).toBe("/admin/fleet/jobs");
  });

  it("reads an install deferred for disk as not started, with the free space", async () => {
    show([entry({ state: "waiting", bytes: null, reason: "insufficient_disk" })]);
    expect(await screen.findByText("Not started")).toBeTruthy();
    expect(screen.getByText(/1.1 GB free/)).toBeTruthy();
  });

  it("shows a removal and a wait for the host, naming the tag first and the Steam name beneath", async () => {
    show([
      entry({ state: "removing", bytes: null }),
      entry({ tool_id: "t2", tag: "GE-Proton11-12", steam_name: "GE-Proton11-12-x86_64", state: "waiting", bytes: null }),
    ]);
    expect(await screen.findByText("Removing")).toBeTruthy();
    expect(screen.getByText("GE-Proton11-12")).toBeTruthy();
    expect(screen.getByText("GE-Proton11-12-x86_64")).toBeTruthy();
    expect(screen.getByText("Waiting")).toBeTruthy();
    expect(screen.getByText("Installs when this host is ready.")).toBeTruthy();
  });

  it("says once, for the card, that the agent offers no tools, and reads every tool as unsupported", async () => {
    show(
      [entry({ state: "unsupported", bytes: null }), entry({ tool_id: "t2", tag: "GE-Proton9-27", steam_name: "GE-Proton9-27", state: "unsupported", bytes: null })],
      { supported: false, free_bytes: null },
    );
    expect(await screen.findAllByText("Unsupported")).toHaveLength(2);
    expect(screen.getAllByText(/does not offer compatibility tools/)).toHaveLength(1);
  });

  it("gives each failed row's job link its own name", async () => {
    show([
      entry({ state: "failed", bytes: null, reason: "io_error", run_id: "r1" }),
      entry({ tool_id: "t2", tag: "GE-Proton9-27", steam_name: "GE-Proton9-27", state: "failed", bytes: null, reason: "io_error", run_id: "r2" }),
    ]);
    expect(await screen.findByRole("link", { name: "See the Compatibility tools job for GE-Proton10-4" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "See the Compatibility tools job for GE-Proton9-27" })).toBeTruthy();
  });

  it("shows a state it does not know as its own word instead of crashing the page", async () => {
    show([entry({ state: "pausing" as Entry["state"], bytes: null })]);
    expect(await screen.findByText("pausing")).toBeTruthy();
  });

  describe("a poll that fails after the first load", () => {
    async function firstLoadThenFail(err: Error) {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      const get = vi.mocked(adminApi.getHostCompatTools);
      get.mockResolvedValueOnce({ supported: true, free_bytes: null, reported_at: null, tools: [entry({})] });
      get.mockRejectedValue(err);
      render(
        <AuthContext.Provider value={auth}>
          <MemoryRouter>
            <CompatToolsCard hostId="h1" />
          </MemoryRouter>
        </AuthContext.Provider>,
      );
      await screen.findByText("GE-Proton10-4");
      await act(() => vi.advanceTimersByTimeAsync(5_000));
      return get;
    }

    it("shows the error above the rows it keeps, and stops polling when access is gone", async () => {
      const get = await firstLoadThenFail(new ApiError(404, "not_found", "host not found"));
      expect(screen.getByRole("alert").textContent).toBe("host not found");
      expect(screen.getByText("GE-Proton10-4")).toBeTruthy();
      expect(get).toHaveBeenCalledTimes(2);
      await act(() => vi.advanceTimersByTimeAsync(30_000));
      expect(get).toHaveBeenCalledTimes(2);
    });

    it("retries a first load that fails transiently, and shows the rows once it succeeds", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      const get = vi.mocked(adminApi.getHostCompatTools);
      get.mockRejectedValueOnce(new ApiError(500, "internal", "boom"));
      get.mockResolvedValue({ supported: true, free_bytes: null, reported_at: null, tools: [entry({})] });
      render(
        <AuthContext.Provider value={auth}>
          <MemoryRouter>
            <CompatToolsCard hostId="h1" />
          </MemoryRouter>
        </AuthContext.Provider>,
      );
      expect(await screen.findByText("boom")).toBeTruthy();
      await act(() => vi.advanceTimersByTimeAsync(5_000));
      expect(await screen.findByText("GE-Proton10-4")).toBeTruthy();
      expect(screen.queryByRole("alert")).toBeNull();
    });

    it("keeps polling through a transient failure", async () => {
      const get = await firstLoadThenFail(new ApiError(500, "internal", "boom"));
      expect(screen.getByRole("alert").textContent).toBe("boom");
      await act(() => vi.advanceTimersByTimeAsync(5_000));
      expect(get).toHaveBeenCalledTimes(3);
    });
  });

  it("says so when the agent offers no compatibility tools", async () => {
    show([], { supported: false, free_bytes: null });
    expect(await screen.findByText(/does not offer compatibility tools/)).toBeTruthy();
  });

  it("says so when nothing is installed", async () => {
    show([]);
    expect(await screen.findByText("No tools installed on this host.")).toBeTruthy();
  });
});
