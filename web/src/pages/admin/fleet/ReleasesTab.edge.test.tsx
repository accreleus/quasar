// Fleet › Releases on the EDGE channel: a build with no version and no notes,
// rendered as a commit plus the compare link that stands in for them.

import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import * as adminApi from "../../../api/admin";
import type { PlatformRelease, PlatformReleaseView } from "../../../api/types";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { FLEET_TABS } from "../../../components/shell/sectionTabs";
import { ToastProvider } from "../../../components/Toast";
import { ReleasesTab } from "./ReleasesTab";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");

const mocked = vi.mocked(adminApi);

const CP_COMMIT = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const EDGE_COMMIT = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
const COMPARE = `https://github.com/accreleus/quasar/compare/${CP_COMMIT}...${EDGE_COMMIT}`;

function edgeView(over: Partial<PlatformRelease> = {}): PlatformReleaseView {
  const release: PlatformRelease = {
    id: "e1",
    channel: "edge",
    version: null,
    source_commit: EDGE_COMMIT,
    built_at: "2026-09-04T12:00:00Z",
    schema_version: 76,
    prerelease: false,
    notes: "",
    compare_url: COMPARE,
    manifest: null,
    discovered_at: "2026-09-04T12:05:00Z",
    ...over,
  } as PlatformRelease;
  return {
    channel: "edge",
    edge_branch: "develop",
    checked_at: "2026-09-04T12:05:00Z",
    last_error: null,
    installed: {
      control_plane: {
        version: "dev",
        source_commit: CP_COMMIT,
        built_at: "2026-08-19T09:14:02Z",
        schema_version: 76,
      },
      hosts: [],
    },
    available: [release],
    targets: [
      { kind: "control_plane", host_id: null, node_name: null, eligible: true, reason: null },
    ],
    faults: [],
  } as PlatformReleaseView;
}

function renderTab() {
  return render(
    <MemoryRouter>
      <ToastProvider>
        <SectionHeadProvider title="Fleet" tabs={FLEET_TABS}>
          <ReleasesTab />
        </SectionHeadProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => vi.resetAllMocks());

describe("ReleasesTab on edge", () => {
  it("shows the commit and the compare link in place of a version and notes", async () => {
    mocked.getPlatformReleases.mockResolvedValue(edgeView());
    const { container } = renderTab();

    expect(await screen.findByText(EDGE_COMMIT.slice(0, 12))).toBeInTheDocument();
    const link = screen.getByRole("link", { name: /compare with the installed build/i });
    expect(link).toHaveAttribute("href", COMPARE);
    // No notes exist on a branch build, so nothing renders a markdown body.
    expect(container.querySelector(".release-entry h1, .release-entry h2, .release-entry h3"))
      .toBeNull();
    // The branch is readable beside the channel control.
    expect(screen.getByText("develop", { selector: ".mono" })).toBeInTheDocument();
  });

  it("says a branch build has no notes when there is nothing to compare from", async () => {
    mocked.getPlatformReleases.mockResolvedValue(edgeView({ compare_url: null }));
    renderTab();

    expect(await screen.findByText(/A branch build publishes no notes/)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /compare/i })).not.toBeInTheDocument();
  });
});
