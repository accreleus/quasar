// Fleet › Releases. Covers the three page states through <ResourceStates>, the
// per-target eligibility text, notes sanitisation, the channel PATCH, and
// "Check now" being the jobs run-now action rather than this page's read.

import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { PlatformRelease, PlatformReleaseView } from "../../../api/types";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { FLEET_TABS } from "../../../components/shell/sectionTabs";
import { ToastProvider } from "../../../components/Toast";
import { ReleasesTab } from "./ReleasesTab";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");

const mocked = vi.mocked(adminApi);

const CP_COMMIT = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const NEW_COMMIT = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";

function release(over: Partial<PlatformRelease> = {}): PlatformRelease {
  return {
    id: "r1",
    channel: "stable",
    version: "0.2.0",
    source_commit: NEW_COMMIT,
    built_at: "2026-09-04T12:00:00Z",
    schema_version: 75,
    prerelease: false,
    notes: "### Fixed\n- Boot race in the node agent\n",
    compare_url: null,
    manifest: { format_version: 1 },
    discovered_at: "2026-09-04T02:07:11Z",
    ...over,
  } as PlatformRelease;
}

function view(over: Partial<PlatformReleaseView> = {}): PlatformReleaseView {
  return {
    channel: "stable",
    edge_branch: "develop",
    checked_at: "2026-09-04T02:07:11Z",
    last_error: null,
    installed: {
      control_plane: {
        version: "0.1.0",
        source_commit: CP_COMMIT,
        built_at: "2026-08-19T09:14:02Z",
        schema_version: 74,
      },
      hosts: [
        {
          host_id: "h1",
          node_name: "gpu-host-01",
          status: "online",
          agent_version: "0.1.0",
          source_commit: CP_COMMIT,
          built_at: "2026-08-19T09:14:02Z",
          install_mode: "registry",
          updater_present: true,
          identity_known: true,
        },
      ],
    },
    available: [release()],
    targets: [
      { kind: "control_plane", host_id: null, node_name: null, eligible: true, reason: null },
      {
        kind: "host",
        host_id: "h1",
        node_name: "gpu-host-01",
        eligible: false,
        reason: "release_above_control_plane",
      },
    ],
    faults: [],
    ...over,
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

describe("ReleasesTab", () => {
  it("lists an available release with its version, date, commit and rendered notes", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view());
    renderTab();

    expect(await screen.findByText("0.2.0")).toBeInTheDocument();
    expect(screen.getByText(NEW_COMMIT.slice(0, 12))).toBeInTheDocument();
    // The markdown body renders as markup, not as a wall of text.
    expect(screen.getByRole("heading", { name: "Fixed" })).toBeInTheDocument();
    expect(screen.getByText("Boot race in the node agent")).toBeInTheDocument();
  });

  it("renders release notes sanitised", async () => {
    mocked.getPlatformReleases.mockResolvedValue(
      view({ available: [release({ notes: "<script>window.__pwned = true</script>\n\nhi" })] }),
    );
    const { container } = renderTab();

    await screen.findByText("0.2.0");
    expect(container.querySelector("script")).toBeNull();
    expect((window as unknown as { __pwned?: boolean }).__pwned).toBeUndefined();
  });

  it("maps each eligibility reason to text and offers no apply control", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view());
    renderTab();

    await screen.findByText("gpu-host-01");
    expect(screen.getByText(/Waiting on the control plane: this release carries a newer schema/))
      .toBeInTheDocument();
    // Apply is amendment 2 (#116); nothing on this page must offer it yet.
    expect(screen.queryByRole("button", { name: /^apply/i })).not.toBeInTheDocument();
  });

  it("says so when nothing is available, and does not claim an update", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view({ available: [], targets: [] }));
    renderTab();

    expect(
      await screen.findByText(/Nothing at or above this control plane's schema/),
    ).toBeInTheDocument();
  });

  it("reports a load failure instead of spinning", async () => {
    mocked.getPlatformReleases.mockRejectedValue(new ApiError(503, "internal", "database down"));
    renderTab();

    expect(await screen.findByRole("alert")).toHaveTextContent("database down");
  });

  it("surfaces a failing detector alongside a stale checked_at", async () => {
    mocked.getPlatformReleases.mockResolvedValue(
      view({ last_error: "api.github.com answered 503" }),
    );
    renderTab();

    expect(await screen.findByRole("alert")).toHaveTextContent("api.github.com answered 503");
  });

  it("switching the channel PATCHes the settings and refetches", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view());
    mocked.updateSettings.mockResolvedValue({ settings: {} } as never);
    renderTab();

    (await screen.findByRole("tab", { name: "Edge" })).click();

    await waitFor(() =>
      expect(mocked.updateSettings).toHaveBeenCalledWith("tok", { release_channel: "edge" }),
    );
    await waitFor(() => expect(mocked.getPlatformReleases).toHaveBeenCalledTimes(2));
  });

  it("Check now runs the detection job rather than the page's own read", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view());
    mocked.runJobNow.mockResolvedValue({ run: { id: "run-1" } } as never);
    renderTab();

    (await screen.findByRole("button", { name: "Check now" })).click();

    await waitFor(() =>
      expect(mocked.runJobNow).toHaveBeenCalledWith("tok", "platform.release_detect"),
    );
  });

  it("lists a fault so a wrong state is visible", async () => {
    mocked.getPlatformReleases.mockResolvedValue(
      view({
        faults: [
          {
            kind: "agent_ahead_of_control_plane",
            host_id: "h1",
            node_name: "gpu-host-01",
            detail: "the agent is on release 0.3.0",
          },
        ],
      }),
    );
    renderTab();

    expect(await screen.findByText("Agent ahead of the control plane")).toBeInTheDocument();
    expect(screen.getByText(/the agent is on release 0\.3\.0/)).toBeInTheDocument();
  });
});

describe("ReleasesTab › manual update paths", () => {
  const hostIdentity = (over: Record<string, unknown> = {}) => ({
    host_id: "h1",
    node_name: "gpu-host-01",
    status: "online",
    agent_version: "0.1.0",
    source_commit: CP_COMMIT,
    built_at: "2026-08-19T09:14:02Z",
    install_mode: "registry",
    updater_present: true,
    identity_known: true,
    ...over,
  });

  /** A view whose one host is ineligible for `reason`, with `identity` as the
   *  facts the commands are keyed on. */
  function ineligible(reason: string, identity: Record<string, unknown> = {}): PlatformReleaseView {
    return view({
      installed: {
        control_plane: {
          version: "0.1.0",
          source_commit: NEW_COMMIT,
          built_at: "2026-08-19T09:14:02Z",
          schema_version: 75,
        },
        hosts: [hostIdentity(identity)],
      },
      targets: [
        { kind: "control_plane", host_id: null, node_name: null, eligible: true, reason: null },
        { kind: "host", host_id: "h1", node_name: "gpu-host-01", eligible: false, reason },
      ],
    } as Partial<PlatformReleaseView>);
  }

  it("a source-built host shows its commit, the release and the redeploy command, with no apply", async () => {
    mocked.getPlatformReleases.mockResolvedValue(
      ineligible("install_mode_source", { install_mode: "source" }),
    );
    renderTab();

    const block = await screen.findByTestId("manual-h1");
    expect(block).toHaveTextContent(CP_COMMIT.slice(0, 12));
    expect(block).toHaveTextContent("release 0.2.0");
    expect(block).toHaveTextContent("deploy/redeploy.sh <va|nvidia> v0.2.0");
    expect(screen.queryByRole("button", { name: /^apply/i })).not.toBeInTheDocument();
  });

  it("a host with no updater shows the one-time updater addition and the registry recipe", async () => {
    mocked.getPlatformReleases.mockResolvedValue(
      ineligible("updater_absent", { updater_present: false }),
    );
    renderTab();

    const block = await screen.findByTestId("manual-h1");
    expect(block).toHaveTextContent("up -d --no-deps quasar-updater");
    expect(block).toHaveTextContent("pull quasar-control-plane quasar-node-agent");
    expect(block).toHaveTextContent("QUASAR_CONTROL_IMAGE=");
  });

  it("a host with no identity is told to upgrade the agent first", async () => {
    mocked.getPlatformReleases.mockResolvedValue(
      ineligible("identity_unknown", {
        source_commit: null,
        install_mode: null,
        updater_present: null,
        identity_known: false,
      }),
    );
    renderTab();

    const block = await screen.findByTestId("manual-h1");
    expect(block).toHaveTextContent(/predates identity reporting/i);
    expect(block).toHaveTextContent("build unknown");
  });

  it("an offline host gets the state and no command to run at it", async () => {
    mocked.getPlatformReleases.mockResolvedValue(ineligible("host_offline", { status: "offline" }));
    renderTab();

    const block = await screen.findByTestId("manual-h1");
    expect(block).toHaveTextContent(/online first/i);
    expect(within(block).queryByRole("button", { name: /copy/i })).not.toBeInTheDocument();
  });

  it("an eligible target and a waiting one show no commands at all", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view()); // host: release_above_control_plane
    renderTab();

    await screen.findByText("gpu-host-01");
    expect(screen.queryByTestId("manual-h1")).not.toBeInTheDocument();
    expect(screen.queryByTestId("manual-control-plane")).not.toBeInTheDocument();
  });
});
