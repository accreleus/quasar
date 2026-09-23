// Placement tab (RH05 #342). Properties under test, all as an admin sees them:
// the save sends the whole selection at the read's revision; selected,
// prepared and ready stay three separate states; a stale revision saves
// nothing, says so, and shows the current placement without re-sending; and a
// derived tile's inherited placement is read-only.

import { useState } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import * as adminApi from "../../../../api/admin";
import { ApiError } from "../../../../api/client";
import type { AppPlacement, Host } from "../../../../api/types";
import { ToastProvider } from "../../../../components/Toast";
import { PlacementTab, type PlacementDraft } from "./PlacementTab";

vi.mock("../../../../api/admin");
vi.mock("../../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));

const mocked = vi.mocked(adminApi);

const HOST_A = "aaaaaaaa-0000-0000-0000-000000000001";
const HOST_B = "bbbbbbbb-0000-0000-0000-000000000002";

function placement(over: Partial<AppPlacement> = {}): AppPlacement {
  return {
    app_id: "app-1",
    inherited_from: null,
    mode: "all_eligible",
    host_ids: [],
    revision: "3",
    hosts: [
      { host_id: HOST_A, selected: true, prepared: true, ready: false, reason: "agent offline" },
      { host_id: HOST_B, selected: true, prepared: null, ready: true, reason: null },
    ],
    ...over,
  };
}

const hosts = [
  { id: HOST_A, node_name: "gpu-test" },
  { id: HOST_B, node_name: "second-gpu" },
] as Host[];

function renderTab(parent: { id: string; name: string } | null = null) {
  function Fixture() {
    const [draft, setDraft] = useState<PlacementDraft | null>(null);
    return <PlacementTab appId="app-1" parent={parent} draft={draft} setDraft={setDraft} />;
  }
  return render(
    <ToastProvider>
      <MemoryRouter>
        <Fixture />
      </MemoryRouter>
    </ToastProvider>,
  );
}

function row(name: string): HTMLElement {
  return screen.getByText(name).closest(".ae-item") as HTMLElement;
}

beforeEach(() => {
  vi.resetAllMocks();
  mocked.listAllHosts.mockResolvedValue(hosts);
  mocked.getAppPlacement.mockResolvedValue(placement());
});

describe("Placement tab", () => {
  it("shows selected, prepared and ready as separate states per host", async () => {
    renderTab();
    await screen.findByText("gpu-test");
    const a = within(row("gpu-test"));
    expect(a.getByText("Selected")).toBeInTheDocument();
    expect(a.getByText("Prepared")).toBeInTheDocument();
    expect(a.getByText("not ready")).toBeInTheDocument();
    expect(a.getByText("agent offline")).toBeInTheDocument();
    const b = within(row("second-gpu"));
    expect(b.getByText("prepared unknown")).toBeInTheDocument();
    expect(b.getByText("Ready")).toBeInTheDocument();
  });

  it("explains that removal blocks only new sessions and deletes nothing", async () => {
    renderTab();
    await screen.findByText("gpu-test");
    expect(screen.getByText(/nothing is deleted/)).toBeInTheDocument();
    expect(screen.getByText(/only launch it on the host\s+that holds that home/)).toBeInTheDocument();
  });

  it("saves a fixed selection at the read's revision, separately", async () => {
    mocked.updateAppPlacement.mockResolvedValue(
      placement({ mode: "fixed", host_ids: [HOST_B], revision: "4" }),
    );
    renderTab();
    await screen.findByText("gpu-test");
    const save = screen.getByRole("button", { name: "Save placement" });
    expect(save).toBeDisabled();

    fireEvent.click(screen.getByRole("tab", { name: "Only these hosts" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "second-gpu" }));
    fireEvent.click(save);

    await waitFor(() =>
      expect(mocked.updateAppPlacement).toHaveBeenCalledWith("tok", "app-1", {
        expected_revision: "3",
        mode: "fixed",
        host_ids: [HOST_B],
      }),
    );
    await waitFor(() => expect(screen.getByRole("button", { name: "Save placement" })).toBeDisabled());
  });

  it("sends an empty host list when returning to every eligible host", async () => {
    mocked.getAppPlacement.mockResolvedValue(placement({ mode: "fixed", host_ids: [HOST_A] }));
    mocked.updateAppPlacement.mockResolvedValue(placement({ revision: "4" }));
    renderTab();
    await screen.findByRole("checkbox", { name: "gpu-test" });
    expect(screen.getByRole("checkbox", { name: "gpu-test" })).toBeChecked();

    fireEvent.click(screen.getByRole("tab", { name: "Every eligible host" }));
    expect(screen.queryByRole("checkbox")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Save placement" }));

    await waitFor(() =>
      expect(mocked.updateAppPlacement).toHaveBeenCalledWith("tok", "app-1", {
        expected_revision: "3",
        mode: "all_eligible",
        host_ids: [],
      }),
    );
  });

  it("warns that a fixed selection with no host allows no launch", async () => {
    mocked.getAppPlacement.mockResolvedValue(placement({ mode: "fixed", host_ids: [HOST_A] }));
    renderTab();
    fireEvent.click(await screen.findByRole("checkbox", { name: "gpu-test" }));
    expect(screen.getByText("No host is selected.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save placement" })).toBeEnabled();
  });

  it("on a stale revision saves nothing, says so, and reloads without re-sending", async () => {
    mocked.updateAppPlacement.mockRejectedValue(new ApiError(409, "stale_revision", "stale"));
    renderTab();
    await screen.findByText("gpu-test");
    fireEvent.click(screen.getByRole("tab", { name: "Only these hosts" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "gpu-test" }));

    mocked.getAppPlacement.mockResolvedValue(
      placement({ mode: "fixed", host_ids: [HOST_B], revision: "7" }),
    );
    fireEvent.click(screen.getByRole("button", { name: "Save placement" }));

    expect(await screen.findByText(/while you were editing, so nothing was saved/)).toHaveAttribute(
      "role",
      "alert",
    );
    await waitFor(() => expect(mocked.getAppPlacement).toHaveBeenCalledTimes(2));
    expect(mocked.updateAppPlacement).toHaveBeenCalledTimes(1);

    // The edit is kept for a deliberate retry, which goes at the new revision.
    mocked.updateAppPlacement.mockResolvedValue(
      placement({ mode: "fixed", host_ids: [HOST_A], revision: "8" }),
    );
    fireEvent.click(screen.getByRole("button", { name: "Save placement" }));
    await waitFor(() =>
      expect(mocked.updateAppPlacement).toHaveBeenLastCalledWith("tok", "app-1", {
        expected_revision: "7",
        mode: "fixed",
        host_ids: [HOST_A],
      }),
    );
  });

  it("shows a derived tile's inherited placement read-only, linking the parent", async () => {
    mocked.getAppPlacement.mockResolvedValue(
      placement({ app_id: "parent-1", inherited_from: "parent-1", mode: "fixed", host_ids: [HOST_A] }),
    );
    renderTab({ id: "parent-1", name: "Steam" });
    await screen.findByText("gpu-test");

    expect(screen.queryByRole("checkbox")).toBeNull();
    expect(screen.queryByRole("tablist", { name: "Which hosts may run this app" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Save placement" })).toBeNull();
    expect(screen.getByRole("link", { name: "Steam" })).toHaveAttribute(
      "href",
      "/admin/library/apps/parent-1/placement",
    );
    expect(screen.getByText(/allows only the host marked Selected/)).toBeInTheDocument();
  });

  it("drops to read-only when the server answers inherited_placement", async () => {
    mocked.updateAppPlacement.mockRejectedValue(
      new ApiError(409, "inherited_placement", "inherited"),
    );
    renderTab();
    await screen.findByText("gpu-test");
    fireEvent.click(screen.getByRole("tab", { name: "Only these hosts" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "gpu-test" }));

    mocked.getAppPlacement.mockResolvedValue(placement({ inherited_from: "parent-1" }));
    fireEvent.click(screen.getByRole("button", { name: "Save placement" }));

    expect(await screen.findByText(/inherits its placement from its parent/)).toHaveAttribute(
      "role",
      "alert",
    );
    await waitFor(() => expect(screen.queryByRole("checkbox")).toBeNull());
  });
});
