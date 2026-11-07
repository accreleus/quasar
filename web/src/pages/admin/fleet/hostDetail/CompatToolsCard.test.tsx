/** Host detail "Compatibility tools" card (#1077, mock #1067): one row per state the contract defines. */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AuthContext } from "../../../../auth/context";
import type { AuthContextValue } from "../../../../auth/context";
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
afterEach(cleanup);

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
    expect(screen.getByRole("link", { name: "See the job" }).getAttribute("href")).toBe("/admin/fleet/jobs");
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

  it("reads every tool as unsupported when the agent offers none", async () => {
    show([entry({ state: "unsupported", bytes: null })], { supported: false, free_bytes: null });
    expect(await screen.findByText("Unsupported")).toBeTruthy();
    expect(screen.getByText(/does not offer compatibility tools/)).toBeTruthy();
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
