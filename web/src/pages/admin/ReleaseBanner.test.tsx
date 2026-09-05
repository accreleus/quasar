import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as adminApi from "../../api/admin";
import type { PlatformReleaseView } from "../../api/types";
import { ReleaseBanner } from "./ReleaseBanner";

vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../api/admin");

const mocked = vi.mocked(adminApi);

function releaseView(over: Partial<PlatformReleaseView> = {}): PlatformReleaseView {
  return {
    channel: "stable",
    edge_branch: "develop",
    checked_at: "2026-09-04T02:07:11Z",
    last_error: null,
    installed: {
      control_plane: {
        version: "0.1.0",
        source_commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        built_at: "2026-08-19T09:14:02Z",
        schema_version: 74,
      },
      hosts: [],
    },
    available: [],
    targets: [{ kind: "control_plane", host_id: null, node_name: null, eligible: true, reason: null }],
    faults: [],
    ...over,
  } as PlatformReleaseView;
}

function release(over: Partial<PlatformReleaseView["available"][number]> = {}) {
  return {
    id: "r1",
    channel: "stable",
    version: "0.2.0",
    source_commit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    built_at: "2026-09-04T12:00:00Z",
    schema_version: 75,
    prerelease: false,
    notes: "### Fixed\n- Boot race in the node agent\n",
    compare_url: null,
    manifest: { format_version: 1 },
    discovered_at: "2026-09-04T02:07:11Z",
    ...over,
  } as PlatformReleaseView["available"][number];
}

function renderBanner() {
  return render(
    <MemoryRouter>
      <ReleaseBanner />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.resetAllMocks();
  sessionStorage.clear();
});
afterEach(() => sessionStorage.clear());

describe("ReleaseBanner", () => {
  it("renders nothing when nothing has been detected", async () => {
    mocked.getPlatformReleases.mockResolvedValue(releaseView());
    renderBanner();

    await waitFor(() => expect(mocked.getPlatformReleases).toHaveBeenCalled());
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("renders nothing when the newest listed release is the one already installed", async () => {
    // A current instance still LISTS its own release, so `available` being
    // non-empty is not "there is an update".
    const installed = release({ source_commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" });
    mocked.getPlatformReleases.mockResolvedValue(releaseView({ available: [installed] }));
    renderBanner();

    await waitFor(() => expect(mocked.getPlatformReleases).toHaveBeenCalled());
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("announces a newer release and links to the Releases page", async () => {
    mocked.getPlatformReleases.mockResolvedValue(releaseView({ available: [release()] }));
    renderBanner();

    expect(await screen.findByText(/Quasar 0\.2\.0 is available/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /see what changed/i })).toHaveAttribute(
      "href",
      "/admin/fleet/releases",
    );
  });

  it("dismissal survives a remount within the session", async () => {
    mocked.getPlatformReleases.mockResolvedValue(releaseView({ available: [release()] }));
    const { unmount } = renderBanner();

    (await screen.findByRole("button", { name: /dismiss for this session/i })).click();
    await waitFor(() => expect(screen.queryByRole("status")).not.toBeInTheDocument());

    unmount();
    renderBanner();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });
});
