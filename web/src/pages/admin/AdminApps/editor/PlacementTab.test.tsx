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
import type { AppPlacement, CatalogImage, Host } from "../../../../api/types";
import { ToastProvider } from "../../../../components/Toast";
import { PlacementTab, type PlacementDraft, type PlacementImageRef } from "./PlacementTab";

vi.mock("../../../../api/admin");
vi.mock("../../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));

const mocked = vi.mocked(adminApi);

const HOST_A = "aaaaaaaa-0000-0000-0000-000000000001";
const HOST_B = "bbbbbbbb-0000-0000-0000-000000000002";

function placement(over: Partial<AppPlacement> = {}): AppPlacement {
  return {
    app_id: "app-1",
    inherited_from: null,
	managed_image_id: null,
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

function renderTab(
  parent: { id: string; name: string } | null = null,
  image: PlacementImageRef | null = null,
) {
  function Fixture() {
    const [draft, setDraft] = useState<PlacementDraft | null>(null);
    return (
      <PlacementTab appId="app-1" parent={parent} image={image} draft={draft} setDraft={setDraft} />
    );
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

const REF = "ghcr.io/example/runner:2";

function catalogImage(over: Partial<CatalogImage> = {}): CatalogImage {
  return {
    id: "runner",
    display_name: "Runner",
    kind: "prebuilt",
    version: "2",
    registry_ref: REF,
    installed: true,
    installed_version: "2",
    hosts: [],
    ...over,
  };
}

function imagesResolve(images: CatalogImage[]) {
  mocked.listImages.mockResolvedValue({ images });
}

beforeEach(() => {
  vi.resetAllMocks();
  mocked.listAllHosts.mockResolvedValue(hosts);
  mocked.getAppPlacement.mockResolvedValue(placement());
  imagesResolve([]);
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

describe("Placement tab: image preparation", () => {
  const managed = { ref: REF, runtimePresetId: "" };

  beforeEach(() => {
    mocked.getAppPlacement.mockResolvedValue(placement({ managed_image_id: "runner" }));
  });

  it("shows each selected host's managed image progress, with the failure and where to fix it", async () => {
    mocked.getAppPlacement.mockResolvedValue(
      placement({
        managed_image_id: "runner",
        hosts: [
          { host_id: HOST_A, selected: true, prepared: false, ready: true, reason: "awaiting_preparation" },
          { host_id: HOST_B, selected: true, prepared: false, ready: true, reason: "preparation_failed" },
        ],
      }),
    );
    imagesResolve([
      catalogImage({
        hosts: [
          { host_id: HOST_A, state: "pulling" },
          { host_id: HOST_B, state: "failed", error: "manifest unknown" },
        ],
      }),
    ]);
    renderTab(null, managed);

    const a = within(await waitForRow("gpu-test"));
    expect(await a.findByText(/Downloading…/)).toBeInTheDocument();
    // The raw reason code is translated, never shown to the operator.
    expect(screen.queryByText("awaiting_preparation")).toBeNull();

    const b = within(row("second-gpu"));
    expect(b.getByText(/Image failed/)).toBeInTheDocument();
    expect(b.getByText(/manifest unknown/)).toBeInTheDocument();
    expect(b.getByRole("link", { name: /Open Runner/ })).toHaveAttribute(
      "href",
      "/admin/library/images/runner",
    );
    expect(screen.getByRole("link", { name: "Runner" })).toHaveAttribute(
      "href",
      "/admin/library/images/runner",
    );
    mocked.retryHostImage.mockResolvedValue(undefined);
    fireEvent.click(b.getByRole("button", { name: "Retry preparation" }));
    await waitFor(() => expect(mocked.retryHostImage).toHaveBeenCalledWith("tok", HOST_B, "runner"));
  });

  it("says a ready host holds the image, and names an out-of-date version", async () => {
    mocked.getAppPlacement.mockResolvedValue(
      placement({
        managed_image_id: "runner",
        hosts: [
          { host_id: HOST_A, selected: true, prepared: true, ready: true, reason: null },
          { host_id: HOST_B, selected: true, prepared: false, ready: true, reason: "awaiting_preparation" },
        ],
      }),
    );
    imagesResolve([
      catalogImage({
        hosts: [
          { host_id: HOST_A, state: "ready", version: "2" },
          { host_id: HOST_B, state: "ready", version: "1" },
        ],
      }),
    ]);
    renderTab(null, managed);

    expect(await within(await waitForRow("gpu-test")).findByText(/Image ready/)).toBeInTheDocument();
    expect(within(row("second-gpu")).getByText(/holds version 1, but version 2 is installed/)).toBeInTheDocument();
  });

  it("does not label an old-version failure as a failure of the adopted image", async () => {
    mocked.getAppPlacement.mockResolvedValue(placement({
      managed_image_id: "runner",
      hosts: [{ host_id: HOST_A, selected: true, prepared: false, ready: true, reason: "awaiting_preparation" }],
    }));
    imagesResolve([catalogImage({ hosts: [{ host_id: HOST_A, state: "failed", version: "1", error: "old failure" }] })]);
    renderTab(null, managed);
    const a = within(await waitForRow("gpu-test"));
    expect(await a.findByText(/holds version 1, but version 2 is installed/)).toBeInTheDocument();
    expect(a.queryByText(/Image failed/)).toBeNull();
    expect(a.queryByRole("button", { name: "Retry preparation" })).toBeNull();
  });

  it("says a lazily installed image arrives with the first session on a host", async () => {
    imagesResolve([catalogImage({ lazy: true, hosts: [] })]);
    renderTab(null, managed);
    const a = within(await waitForRow("gpu-test"));
    expect(await a.findByText(/downloads when a session is first placed here/)).toBeInTheDocument();
  });

  it("uses the server's adopted image ID when the catalog digest moves", async () => {
    const digest = `ghcr.io/example/runner@sha256:${"a".repeat(64)}`;
    imagesResolve([
      catalogImage({ registry_digest: digest, hosts: [{ host_id: HOST_A, state: "building" }] }),
    ]);
    renderTab(null, { ref: "ghcr.io/example/runner@sha256:old", runtimePresetId: "" });
    expect(await within(await waitForRow("gpu-test")).findByText(/Building…/)).toBeInTheDocument();
  });

  it("explains an unmanaged image instead of reporting preparation for it", async () => {
    mocked.getAppPlacement.mockResolvedValue(
      placement({
		managed_image_id: null,
        hosts: [
          { host_id: HOST_A, selected: true, prepared: null, ready: true, reason: null },
          { host_id: HOST_B, selected: true, prepared: null, ready: true, reason: null },
        ],
      }),
    );
    imagesResolve([catalogImage({ registry_ref: "ghcr.io/example/other:1" })]);
    renderTab(null, { ref: "registry.example/custom:7", runtimePresetId: "" });

    expect(await screen.findByText(/not installed from the image catalog/)).toBeInTheDocument();
    expect(screen.getByText("registry.example/custom:7")).toBeInTheDocument();
    const a = within(row("gpu-test"));
    expect(a.getByText("not managed")).toBeInTheDocument();
    expect(a.queryByText("prepared unknown")).toBeNull();
    expect(screen.queryByText(/Downloading…|Image ready|Image failed/)).toBeNull();
  });

  it("does not call an image-free preset unmanaged", async () => {
    mocked.getAppPlacement.mockResolvedValue(placement({
      managed_image_id: null,
      hosts: [{ host_id: HOST_A, selected: true, prepared: null, ready: true, reason: "no_image" }],
    }));
    imagesResolve([catalogImage()]);
    renderTab(null, { ref: "", runtimePresetId: "preset-without-image" });
    expect(await screen.findByText("This app has no image to prepare.")).toBeInTheDocument();
    expect(screen.queryByText(/not installed from the image catalog/)).toBeNull();
    expect(within(row("gpu-test")).queryByText("not managed")).toBeNull();
  });

  it("makes no managed or unmanaged claim when the image catalog cannot be read", async () => {
    mocked.listImages.mockRejectedValue(new ApiError(500, "internal", "catalog down"));
    renderTab(null, { ref: "registry.example/custom:7", runtimePresetId: "" });

    expect(await screen.findByText("catalog down")).toBeInTheDocument();
    expect(screen.queryByText(/not installed from the image catalog/)).toBeNull();
    expect(within(row("second-gpu")).getByText("prepared unknown")).toBeInTheDocument();
  });

  it("never calls an image unmanaged while a host reports preparation for it", async () => {
    // A locally built template's tag is not on the catalog wire, so the web
    // cannot match it; the placement read's evidence still says managed.
    imagesResolve([]);
    renderTab(null, { ref: "quasar-local/runner:2", runtimePresetId: "" });
    await waitForRow("gpu-test");
    await waitFor(() => expect(mocked.listImages).toHaveBeenCalled());
    expect(screen.queryByText(/not installed from the image catalog/)).toBeNull();
    expect(within(row("gpu-test")).getByText("Prepared")).toBeInTheDocument();
  });
});

async function waitForRow(name: string): Promise<HTMLElement> {
  await screen.findByText(name);
  return row(name);
}
