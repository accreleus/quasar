// Fleet › Releases, laid out to design_handoff_v3/screens/releases-v3.html.
// Covers the three page states through <ResourceStates>, the update banner, the
// release cards and their changelog rows, the targets rollup and the per-host
// detail behind it, the eligibility text, notes sanitisation, the channel
// PATCH, "Check now" being the jobs run-now action rather than this page's
// read, and the per-host apply: the button's gating, the force confirmation
// naming N, live attempt state, a refused apply, and the history.

import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type {
  PlatformApplyAttempt,
  PlatformRelease,
  PlatformReleaseView,
} from "../../../api/types";
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
    source_repo: "accreleus/quasar",
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

/** The targets card folds the per-host table away; a test that reaches for a
 *  row's control opens it the way an operator would. */
function openPerHostDetail() {
  const detail = document.querySelector("details.rel-detail");
  if (detail) (detail as HTMLDetailsElement).open = true;
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

function attempt(over: Partial<PlatformApplyAttempt> = {}): PlatformApplyAttempt {
  return {
    id: "a1",
    run_id: null,
    kind: "apply",
    target: "host",
    host_id: "h1",
    node_name: "gpu-host-01",
    release_id: "r1",
    requested_digests: [{ name: "node-agent", image: "img", digest: "sha256:" + "9".repeat(64) }],
    previous_digests: [{ name: "node-agent", digest: "sha256:" + "1".repeat(64) }],
    state: "waiting_sessions",
    reason: null,
    sessions_remaining: 2,
    force: false,
    output: "",
    requested_by: "u1",
    created_at: "2026-09-05T11:00:00Z",
    started_at: null,
    finished_at: null,
    ...over,
  } as PlatformApplyAttempt;
}

/** An eligible host target is what the Apply button is gated on. */
function eligibleHostView(over: Partial<PlatformReleaseView> = {}): PlatformReleaseView {
  return view({
    targets: [
      { kind: "control_plane", host_id: null, node_name: null, eligible: true, reason: null },
      { kind: "host", host_id: "h1", node_name: "gpu-host-01", eligible: true, reason: null },
    ],
    ...over,
  });
}

function sessionsOnH1(n: number) {
  return {
    items: Array.from({ length: n }, (_, i) => ({
      id: "s" + i,
      host_id: "h1",
      state: "running",
    })),
    next_cursor: null,
  } as never;
}

beforeEach(() => {
  vi.resetAllMocks();
  mocked.listAllSessions.mockResolvedValue(sessionsOnH1(0));
  mocked.listPlatformAttempts.mockResolvedValue({ attempts: [] });
  // The head's "next check" fragment reads the detection job's schedule.
  mocked.listJobs.mockResolvedValue({ items: [], next_cursor: null } as never);
});

describe("ReleasesTab", () => {
  it("lists an available release with its version, date, commit and note rows", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view());
    renderTab();

    expect(await screen.findByText("v0.2.0")).toBeInTheDocument();
    expect(screen.getByText(NEW_COMMIT.slice(0, 12))).toBeInTheDocument();
    // The body is parsed into rows: a category tag and a title, not markdown.
    expect(screen.getByText("FIX")).toBeInTheDocument();
    expect(screen.getByText("Boot race in the node agent")).toBeInTheDocument();
    // Once on the update banner, once on the release card.
    expect(screen.getAllByText(/1 fixed/).length).toBe(2);
  });

  it("chips the newest release as latest and the installed one as installed", async () => {
    mocked.getPlatformReleases.mockResolvedValue(
      view({ available: [release(), release({ id: "r0", version: "0.1.0", source_commit: CP_COMMIT })] }),
    );
    renderTab();

    await screen.findByText("v0.2.0");
    expect(screen.getByText("Latest", { selector: ".chip" })).toBeInTheDocument();
    expect(screen.getByText("Installed", { selector: ".chip" })).toBeInTheDocument();
  });

  it("links each release card at its GitHub release page, composed from source_repo", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view());
    renderTab();

    expect(await screen.findByRole("link", { name: "View on GitHub" })).toHaveAttribute(
      "href",
      "https://github.com/accreleus/quasar/releases/tag/v0.2.0",
    );
  });

  it("links a note row's issues at that repo, and expanding shows the detail", async () => {
    mocked.getPlatformReleases.mockResolvedValue(
      view({
        available: [
          release({ notes: "### Fixed\n- **Boot race in the node agent (#98).** It raced.\n" }),
        ],
      }),
    );
    const { container } = renderTab();

    await screen.findByText("Boot race in the node agent");
    expect(screen.getByRole("link", { name: "#98" })).toHaveAttribute(
      "href",
      "https://github.com/accreleus/quasar/issues/98",
    );
    // The detail is in the row's disclosure, not in the summary line.
    expect(container.querySelector(".rel-b")).toHaveTextContent("It raced.");
  });

  it("offers the update as a version step, and does not when there is none", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view());
    const { container, unmount } = renderTab();
    expect(await screen.findByText(/Update available/i)).toBeInTheDocument();
    // The step reads "v<installed> → v<available>" in one line.
    expect(container.querySelector(".rel-version")).toHaveTextContent("v0.1.0 → v0.2.0");
    unmount();

    mocked.getPlatformReleases.mockResolvedValue(
      view({ available: [release({ source_commit: CP_COMMIT })] }),
    );
    renderTab();
    expect(await screen.findByText(/Up to date/)).toBeInTheDocument();
    expect(screen.queryByText(/Update available/i)).not.toBeInTheDocument();
  });

  it("rolls the targets up, and the per-host detail still carries the table", async () => {
    mocked.getPlatformReleases.mockResolvedValue(eligibleHostView());
    renderTab();

    // The rollup: the control plane's chip and k/N for the agents.
    expect(await screen.findByText("1/1")).toBeInTheDocument();
    expect(screen.getByText("Per-host detail")).toBeInTheDocument();
    openPerHostDetail();
    expect(screen.getByRole("columnheader", { name: "Target" })).toBeInTheDocument();
  });

  it("renders release notes sanitised", async () => {
    mocked.getPlatformReleases.mockResolvedValue(
      view({ available: [release({ notes: "<script>window.__pwned = true</script>\n\nhi" })] }),
    );
    const { container } = renderTab();

    await screen.findByText("v0.2.0");
    expect(container.querySelector("script")).toBeNull();
    expect((window as unknown as { __pwned?: boolean }).__pwned).toBeUndefined();
  });

  it("maps each eligibility reason to text and offers no apply control for an ineligible host", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view());
    renderTab();

    // The rollup names the not-ready host, and the table behind it repeats the
    // reason on its own row.
    expect((await screen.findAllByText("gpu-host-01")).length).toBeGreaterThan(0);
    expect(
      screen.getAllByText(/Waiting on the control plane: this release carries a newer schema/)
        .length,
    ).toBeGreaterThan(0);
    // An ineligible target never gets the button; the gate is the server's too.
    openPerHostDetail();
    expect(screen.queryByRole("button", { name: /^apply$/i })).not.toBeInTheDocument();
  });

  it("calls a current control plane up to date, not not-ready", async () => {
    mocked.getPlatformReleases.mockResolvedValue(
      view({
        available: [release({ source_commit: CP_COMMIT })],
        targets: [
          {
            kind: "control_plane",
            host_id: null,
            node_name: null,
            eligible: false,
            reason: "up_to_date",
          },
          { kind: "host", host_id: "h1", node_name: "gpu-host-01", eligible: true, reason: null },
        ],
      }),
    );
    renderTab();

    // Once in the rollup, once on the row behind the per-host disclosure.
    expect((await screen.findAllByText("Up to date")).length).toBe(2);
    expect(screen.queryByText("Not ready")).not.toBeInTheDocument();
  });

  it("keeps Not ready for every other ineligibility", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view()); // host: release_above_control_plane
    renderTab();

    expect(await screen.findByText("Not ready")).toBeInTheDocument();
    expect(screen.queryByText("Up to date")).not.toBeInTheDocument();
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

  it("puts the channel, the last check and the job's next run in the head", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view());
    mocked.listJobs.mockResolvedValue({
      items: [
        {
          id: "platform.release_detect",
          enabled: true,
          next_run_at: "2026-09-07T02:00:00Z",
        },
      ],
      next_cursor: null,
    } as never);
    renderTab();

    expect(await screen.findByText(/stable channel/)).toHaveTextContent(
      /next check Mon 02:00 UTC/,
    );
  });

  it("omits the next check rather than inventing one when the job says nothing", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view());
    renderTab();

    expect(await screen.findByText(/stable channel/)).not.toHaveTextContent(/next check/);
  });

  it("offers Apply on an eligible host, and its confirmation names the live sessions force ends", async () => {
    mocked.getPlatformReleases.mockResolvedValue(eligibleHostView());
    mocked.listAllSessions.mockResolvedValue(sessionsOnH1(2));
    mocked.applyPlatformReleaseToHost.mockResolvedValue({ attempt: attempt() } as never);
    renderTab();

    await screen.findByText("Per-host detail");
    openPerHostDetail();
    screen.getByRole("button", { name: /^Apply$/ }).click();

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/ends 2 live sessions/)).toBeInTheDocument();

    within(dialog).getByRole("checkbox").click();
    within(dialog).getByRole("button", { name: "Update" }).click();

    await waitFor(() =>
      expect(mocked.applyPlatformReleaseToHost).toHaveBeenCalledWith("tok", "h1", {
        release_id: "r1",
        force: true,
      }),
    );
  });

  it("sends force false by default", async () => {
    mocked.getPlatformReleases.mockResolvedValue(eligibleHostView());
    mocked.applyPlatformReleaseToHost.mockResolvedValue({ attempt: attempt() } as never);
    renderTab();

    await screen.findByText("Per-host detail");
    openPerHostDetail();
    screen.getByRole("button", { name: /^Apply$/ }).click();
    (await screen.findByRole("button", { name: "Update" })).click();

    await waitFor(() =>
      expect(mocked.applyPlatformReleaseToHost).toHaveBeenCalledWith("tok", "h1", {
        release_id: "r1",
        force: false,
      }),
    );
  });

  it("says the count is unknown rather than inventing one", async () => {
    mocked.getPlatformReleases.mockResolvedValue(eligibleHostView());
    mocked.listAllSessions.mockRejectedValue(new ApiError(503, "internal", "no"));
    renderTab();

    await screen.findByText("Per-host detail");
    openPerHostDetail();
    screen.getByRole("button", { name: /^Apply$/ }).click();
    expect(
      within(await screen.findByRole("dialog")).getByText(/ends every live session on this host/),
    ).toBeInTheDocument();
  });

  it("renders each attempt state on the target row, and no Apply while one is open", async () => {
    for (const [state, text] of [
      ["waiting_sessions", "Waiting for sessions to end"],
      ["pulling", "Pulling the image"],
      ["recreating", "Recreating the agent"],
      ["verifying", "Verifying"],
    ] as const) {
      mocked.getPlatformReleases.mockResolvedValue(
        eligibleHostView({ active_apply: { run: null, attempts: [attempt({ state })] } }),
      );
      const { unmount } = renderTab();
      expect(await screen.findByText(text)).toBeInTheDocument();
      openPerHostDetail();
      expect(screen.queryByRole("button", { name: /^Apply$/ })).not.toBeInTheDocument();
      unmount();
    }
  });

  it("shows how many sessions an open attempt is waiting on", async () => {
    mocked.getPlatformReleases.mockResolvedValue(
      eligibleHostView({ active_apply: { run: null, attempts: [attempt()] } }),
    );
    renderTab();

    expect(await screen.findByText(/waiting on 2 sessions/)).toBeInTheDocument();
  });

  it("renders a failed attempt's reason as text", async () => {
    mocked.getPlatformReleases.mockResolvedValue(
      eligibleHostView({
        active_apply: {
          run: null,
          attempts: [attempt({ state: "failed", reason: "recreate_failed" })],
        },
      }),
    );
    renderTab();

    expect(await screen.findByText(/this host's agent is stopped/)).toBeInTheDocument();
  });

  it("surfaces a refused apply through the action hook's toast", async () => {
    mocked.getPlatformReleases.mockResolvedValue(eligibleHostView());
    mocked.applyPlatformReleaseToHost.mockRejectedValue(
      new ApiError(409, "attempt_in_flight", "an update is already in flight on this host"),
    );
    renderTab();

    await screen.findByText("Per-host detail");
    openPerHostDetail();
    screen.getByRole("button", { name: /^Apply$/ }).click();
    (await screen.findByRole("button", { name: "Update" })).click();

    expect(
      await screen.findByText(/an update is already in flight on this host/),
    ).toBeInTheDocument();
  });

  it("lists the apply history with its digests", async () => {
    mocked.getPlatformReleases.mockResolvedValue(eligibleHostView());
    mocked.listPlatformAttempts.mockResolvedValue({
      attempts: [attempt({ state: "succeeded", sessions_remaining: null })],
    });
    renderTab();

    // "Apply · Updated", the row's own state line, over its digest step.
    expect(await screen.findByText("Apply")).toBeInTheDocument();
    expect(screen.getByText(/Updated/)).toBeInTheDocument();
    expect(screen.getByText(/111111111111/)).toBeInTheDocument();
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

    expect((await screen.findAllByText("gpu-host-01")).length).toBeGreaterThan(0);
    expect(screen.queryByTestId("manual-h1")).not.toBeInTheDocument();
    expect(screen.queryByTestId("manual-control-plane")).not.toBeInTheDocument();
  });
});
