// Fleet › Releases on the PREVIEW channel: a preview offer lists the commits newer
// than the installed build (amendment 48, #843): the first `new_commit_count`
// entries of its manifest's list, as plain text.

import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import * as adminApi from "../../../api/admin";
import type { PlatformPreflight, PlatformRelease, PlatformReleaseView } from "../../../api/types";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { FLEET_TABS } from "../../../components/shell/sectionTabs";
import { ToastProvider } from "../../../components/Toast";
import { ReleasesTab } from "./ReleasesTab";

const PF: PlatformPreflight = { state: "unknown", checked_at: null, checks: [] };

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");

const mocked = vi.mocked(adminApi);

const hex = (c: string) => c.repeat(40);
const COMMITS = [
  { commit: hex("e"), subject: "#901 Newest landing" },
  { commit: hex("d"), subject: "#900 <b>Not markup</b> & [not](a link)" },
  { commit: hex("c"), subject: "docs: the installed build" },
  { commit: hex("b"), subject: "#898 Older than installed" },
];

function previewView(newCommitCount: number | null, commits: typeof COMMITS | undefined = COMMITS): PlatformReleaseView {
  const release = {
    id: "p1",
    channel: "preview",
    version: null,
    source_commit: hex("e"),
    built_at: "2026-10-27T12:00:00Z",
    schema_version: 107,
    prerelease: false,
    notes: "",
    compare_url: null,
    manifest: {
      format_version: 2,
      version: null,
      prerelease: false,
      source_commit: hex("e"),
      built_at: "2026-10-27T12:00:00Z",
      schema_version: 107,
      components: [],
      floor: [],
      ...(commits ? { commits } : {}),
    },
    migrates: false,
    new_commit_count: newCommitCount,
    discovered_at: "2026-10-27T12:05:00Z",
  } as unknown as PlatformRelease;
  return {
    channel: "preview",
    source_repo: "accreleus/quasar",
    edge_branch: "develop",
    checked_at: "2026-10-27T12:05:00Z",
    last_error: null,
    installed: {
      control_plane: { version: "dev", source_commit: hex("c"), built_at: "2026-10-26T09:14:02Z", schema_version: 107 },
      hosts: [],
    },
    available: [release],
    targets: [{ kind: "control_plane", host_id: null, node_name: null, eligible: true, reason: null, preflight: PF }],
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

beforeEach(() => {
  vi.resetAllMocks();
  mocked.listAllSessions.mockResolvedValue({ items: [], next_cursor: null } as never);
  mocked.listPlatformAttempts.mockResolvedValue({ attempts: [] });
  mocked.listPlatformApplyRuns.mockResolvedValue({ runs: [] });
  mocked.listJobs.mockResolvedValue({ items: [], next_cursor: null } as never);
  mocked.getSettings.mockResolvedValue({ settings: { release_check: "daily" } } as never);
});

describe("ReleasesTab commit list on preview", () => {
  it("lists exactly the first new_commit_count commits, short commit and subject, as text", async () => {
    mocked.getPlatformReleases.mockResolvedValue(previewView(2));
    const { container } = renderTab();

    expect(await screen.findByText("2 commits newer than the installed build")).toBeInTheDocument();
    const rows = within(container.querySelector(".rel-commits") as HTMLElement).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent(`${hex("e").slice(0, 12)}#901 Newest landing`);
    // Upstream text stays text: no element, no link, the characters as written.
    expect(rows[1]).toHaveTextContent("#900 <b>Not markup</b> & [not](a link)");
    expect(rows[1].querySelector("b, a")).toBeNull();
    // Nothing at or below the installed commit.
    expect(screen.queryByText("docs: the installed build")).not.toBeInTheDocument();
    expect(screen.queryByText("#898 Older than installed")).not.toBeInTheDocument();
  });

  it("says 1 commit for one", async () => {
    mocked.getPlatformReleases.mockResolvedValue(previewView(1));
    renderTab();
    expect(await screen.findByText("1 commit newer than the installed build")).toBeInTheDocument();
  });

  it.each([
    ["the installed build is the newest", previewView(0)],
    ["the manifest carries no list (count is null)", previewView(null, undefined)],
    ["the server sent no count", previewView(undefined as unknown as null)],
  ])("shows no list when %s", async (_name, view) => {
    mocked.getPlatformReleases.mockResolvedValue(view);
    const { container } = renderTab();
    expect(await screen.findAllByText(hex("e").slice(0, 12))).not.toHaveLength(0);
    expect(container.querySelector(".rel-commits")).toBeNull();
    expect(screen.getByText(/A branch build publishes no notes/)).toBeInTheDocument();
  });
});
