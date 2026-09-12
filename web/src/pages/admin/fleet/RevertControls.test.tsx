// Revert and the failed-attempt presentation (#118), through the page that
// hosts them: the button's gating, the confirmation naming the digest, the
// force flag, the failure panel, and the history's kind.

import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import * as adminApi from "../../../api/admin";
import type {
  PlatformApplyAttempt,
  PlatformRelease,
  PlatformReleaseView,
} from "../../../api/types";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { FLEET_TABS } from "../../../components/shell/sectionTabs";
import { ToastProvider } from "../../../components/Toast";
import { ReleasesTab } from "./ReleasesTab";
import { hostAfterFailureText } from "./releasesCopy";
import { revertStates } from "./RevertControls";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");

const mocked = vi.mocked(adminApi);

const OLD_DIGEST = "sha256:" + "1".repeat(64);
const NEW_DIGEST = "sha256:" + "9".repeat(64);
const AGENT_IMAGE = "ghcr.io/accreleus/quasar/quasar-node-agent";

function release(over: Partial<PlatformRelease> = {}): PlatformRelease {
  return {
    id: "r1",
    channel: "stable",
    version: "0.2.0",
    source_commit: "b".repeat(40),
    built_at: "2026-09-04T12:00:00Z",
    schema_version: 75,
    prerelease: false,
    notes: "",
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
        version: "0.2.0",
        source_commit: "b".repeat(40),
        built_at: "2026-08-19T09:14:02Z",
        schema_version: 75,
      },
      hosts: [
        {
          host_id: "h1",
          node_name: "gpu-host-01",
          status: "online",
          agent_version: "0.2.0",
          source_commit: "b".repeat(40),
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
      // Already on the newest release: the ordinary state in which an operator
      // regrets an update.
      {
        kind: "host",
        host_id: "h1",
        node_name: "gpu-host-01",
        eligible: false,
        reason: "up_to_date",
      },
    ],
    faults: [],
    ...over,
  } as PlatformReleaseView;
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
    requested_digests: [{ name: "node-agent", image: AGENT_IMAGE, digest: NEW_DIGEST }],
    previous_digests: [{ name: "node-agent", digest: OLD_DIGEST }],
    state: "succeeded",
    reason: null,
    sessions_remaining: null,
    force: false,
    output: "",
    requested_by: "u1",
    created_at: "2026-09-05T11:00:00Z",
    started_at: "2026-09-05T11:00:01Z",
    finished_at: "2026-09-05T11:02:00Z",
    ...over,
  } as PlatformApplyAttempt;
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
  // The head's "next check" fragment reads the detection job's schedule.
  mocked.listJobs.mockResolvedValue({ items: [], next_cursor: null } as never);
  mocked.getPlatformReleases.mockResolvedValue(view());
});

describe("revertStates", () => {
  it("offers a revert only when a succeeded attempt recorded a previous digest", () => {
    expect(revertStates([]).size).toBe(0);
    // "Nobody looked": a null digest is nothing to go back to.
    expect(
      revertStates([attempt({ previous_digests: [{ name: "node-agent", digest: null }] })]).size,
    ).toBe(0);
    // A failed attempt is not evidence of what the host is on.
    expect(revertStates([attempt({ state: "failed", reason: "pull_failed" })]).get("h1")?.digest)
      .toBe("");

    const states = revertStates([attempt()]);
    expect(states.get("h1")).toEqual({ digest: OLD_DIGEST, image: AGENT_IMAGE, failed: null });
  });

  it("never derives a revert target from an auto_revert row", () => {
    // Newest first: the updater's own restore sits above the failed apply and
    // the operator's earlier succeeded one. Its previous digests are the
    // release that just failed, so it must not become the Revert target.
    const restored = attempt({
      id: "a3",
      kind: "auto_revert",
      requested_digests: [{ name: "node-agent", image: AGENT_IMAGE, digest: OLD_DIGEST }],
      previous_digests: [{ name: "node-agent", digest: NEW_DIGEST }],
      created_at: "2026-09-05T12:00:00Z",
    });
    const failed = attempt({ id: "a2", state: "failed", reason: "recreate_failed", created_at: "2026-09-05T11:59:00Z" });
    const states = revertStates([restored, failed, attempt()]);
    expect(states.get("h1")?.digest).toBe(OLD_DIGEST);
    expect(states.get("h1")?.failed).toBeNull();
  });

  it("takes the newest succeeded attempt, and reports the newest attempt's failure", () => {
    const states = revertStates([
      attempt({ id: "a3", state: "failed", reason: "unhealthy", created_at: "2026-09-05T13:00:00Z" }),
      attempt({
        id: "a2",
        requested_digests: [{ name: "node-agent", image: AGENT_IMAGE, digest: NEW_DIGEST }],
        previous_digests: [{ name: "node-agent", digest: OLD_DIGEST }],
      }),
      attempt({
        id: "a1",
        previous_digests: [{ name: "node-agent", digest: "sha256:" + "5".repeat(64) }],
      }),
    ]);
    expect(states.get("h1")?.digest).toBe(OLD_DIGEST);
    expect(states.get("h1")?.failed?.id).toBe("a3");
  });

  it("ignores control-plane attempts, which are never revertible", () => {
    expect(
      revertStates([attempt({ target: "control_plane", host_id: null })]).size,
    ).toBe(0);
  });
});

describe("hostAfterFailureText", () => {
  it("says what each failure left running, and nothing at all when it cannot know", () => {
    // Rejected before anything was pulled: the host never moved.
    for (const reason of ["updater_absent", "busy", "invalid", "namespace_rejected",
      "digest_malformed", "unsupported", "signature_missing", "signature_invalid"]) {
      expect(hostAfterFailureText(reason)).toMatch(/still running the build it had/);
    }
    // The pull failed, so the old container was never replaced.
    expect(hostAfterFailureText("pull_failed")).toMatch(/nothing was recreated/);
    // Past the health wait the updater restores the previous build itself
    // (ADR 0004), so none of these may claim no rollback was tried.
    for (const reason of ["recreate_failed", "never_started", "unhealthy"]) {
      expect(hostAfterFailureText(reason)).toMatch(/puts the previous build back itself/);
      expect(hostAfterFailureText(reason)).not.toMatch(/still running the build it had/);
    }
    // The updater may have accepted the apply and then stopped answering, past
    // the point the old container is gone, so neither build may be claimed.
    expect(hostAfterFailureText("updater_unreachable")).toMatch(/how far this apply got/);
    expect(hostAfterFailureText("updater_unreachable")).not.toMatch(/still running the build it had/);
    // A timeout knows neither: both builds are unaccounted for (#201).
    expect(hostAfterFailureText("timeout")).toMatch(/has not reported back/);
    expect(hostAfterFailureText("timeout")).not.toMatch(/still running the build it had/);
    // An unknown or absent reason claims nothing.
    expect(hostAfterFailureText("a_reason_from_the_future")).toBe("");
    expect(hostAfterFailureText(null)).toBe("");
    expect(hostAfterFailureText(undefined)).toBe("");
  });
});

describe("Revert on the Releases page", () => {
  it("offers no Revert when the host has nothing to go back to", async () => {
    renderTab();
    // The host is named by the targets rollup and by the table behind it.
    expect((await screen.findAllByText("gpu-host-01")).length).toBeGreaterThan(0);
    expect(screen.queryByRole("button", { name: /^Revert$/ })).not.toBeInTheDocument();
  });

  it("names the digest being restored, and its release when it is still known", async () => {
    mocked.listPlatformAttempts.mockResolvedValue({ attempts: [attempt()] });
    mocked.getPlatformReleases.mockResolvedValue(
      view({
        available: [
          release({
            manifest: {
              format_version: 1,
              components: [{ name: "node-agent", image: AGENT_IMAGE, digest: OLD_DIGEST }],
            },
          } as Partial<PlatformRelease>),
        ],
      }),
    );
    renderTab();

    (await screen.findByRole("button", { name: /^Revert$/ })).click();
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(OLD_DIGEST.slice(7, 19))).toBeInTheDocument();
    expect(within(dialog).getByText("0.2.0")).toBeInTheDocument();
  });

  it("sends force false by default and true when the operator agrees to end the sessions", async () => {
    mocked.listPlatformAttempts.mockResolvedValue({ attempts: [attempt()] });
    mocked.listAllSessions.mockResolvedValue({
      items: [{ id: "s1", host_id: "h1", state: "running" }],
      next_cursor: null,
    } as never);
    mocked.revertPlatformHost.mockResolvedValue({ attempt: attempt({ kind: "revert" }) } as never);
    renderTab();

    (await screen.findByRole("button", { name: /^Revert$/ })).click();
    let dialog = await screen.findByRole("dialog");
    within(dialog).getByRole("button", { name: "Revert" }).click();
    await waitFor(() =>
      expect(mocked.revertPlatformHost).toHaveBeenCalledWith("tok", "h1", { force: false }),
    );

    (await screen.findByRole("button", { name: /^Revert$/ })).click();
    dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/ends 1 live session/)).toBeInTheDocument();
    within(dialog).getByRole("checkbox").click();
    within(dialog).getByRole("button", { name: "Revert" }).click();
    await waitFor(() =>
      expect(mocked.revertPlatformHost).toHaveBeenCalledWith("tok", "h1", { force: true }),
    );
  });

  it("presents a failed attempt with its reason, output, previous digests and the manual recipe", async () => {
    mocked.listPlatformAttempts.mockResolvedValue({
      attempts: [
        attempt({
          id: "a9",
          state: "failed",
          reason: "unhealthy",
          output: "container quasar-node-agent is unhealthy\nexit 1",
          created_at: "2026-09-05T13:00:00Z",
        }),
        attempt(),
      ],
    });
    renderTab();

    const panel = await screen.findByTestId("failed-h1");
    expect(within(panel).getByText(/never became healthy/)).toBeInTheDocument();
    // The updater restores the previous build itself past the health wait, so
    // the panel must not tell the operator nothing was rolled back (#201).
    expect(within(panel).queryByText(/nothing was rolled back/i)).toBeNull();
    expect(within(panel).getByText(/puts the previous build back itself/)).toBeInTheDocument();
    expect(within(panel).getByTestId("failed-output-h1")).toHaveTextContent("exit 1");
    expect(within(panel).getByText(new RegExp(`node-agent ${OLD_DIGEST.slice(7, 19)}`))).toBeInTheDocument();
    // The manual path is the same registry recipe, pinned to the previous digest.
    expect(
      within(panel).getByText(`QUASAR_AGENT_IMAGE=${AGENT_IMAGE}@${OLD_DIGEST}`),
    ).toBeInTheDocument();
    // And the panel offers the action that does it for the operator.
    expect(within(panel).getByRole("button", { name: /^Revert$/ })).toBeInTheDocument();
  });

  // #201: one fixed sentence could not be true for every reason. A timeout is
  // the shape where BOTH builds are unaccounted for, so the panel must not
  // claim the host is still running what it had.
  it("tells a timed-out host's operator to look at the host, not at this page", async () => {
    mocked.listPlatformAttempts.mockResolvedValue({
      attempts: [
        attempt({
          id: "a10",
          state: "failed",
          reason: "timeout",
          output: "the updater's own result could not be relayed",
          created_at: "2026-09-05T13:00:00Z",
        }),
        attempt(),
      ],
    });
    renderTab();

    const panel = await screen.findByTestId("failed-h1");
    expect(within(panel).getByText(/has not reported back/)).toBeInTheDocument();
    expect(within(panel).queryByText(/still running the build it had/)).toBeNull();
    expect(within(panel).getByTestId("failed-output-h1")).toHaveTextContent("could not be relayed");
  });

  // A failure the updater rejected outright never reached the host's containers.
  it("says the host is untouched when nothing was applied", async () => {
    mocked.listPlatformAttempts.mockResolvedValue({
      attempts: [
        attempt({
          id: "a11",
          state: "failed",
          reason: "namespace_rejected",
          created_at: "2026-09-05T13:00:00Z",
        }),
        attempt(),
      ],
    });
    renderTab();

    const panel = await screen.findByTestId("failed-h1");
    expect(within(panel).getByText(/Nothing was applied: this host is still running the build it had\./))
      .toBeInTheDocument();
  });

  // An identifier this build does not know says nothing at all: a guess about
  // what a host is running is worse than no sentence. The cast is the point —
  // the generated enum is closed, the wire is not, and the server stores an
  // unrecognised reason verbatim (agent-api.md).
  it("offers no aftermath sentence for a reason it does not know", async () => {
    mocked.listPlatformAttempts.mockResolvedValue({
      attempts: [
        attempt({
          id: "a12",
          state: "failed",
          reason: "a_reason_from_the_future" as PlatformApplyAttempt["reason"],
          created_at: "2026-09-05T13:00:00Z",
        }),
        attempt(),
      ],
    });
    renderTab();

    const panel = await screen.findByTestId("failed-h1");
    expect(within(panel).getByText(/a_reason_from_the_future/)).toBeInTheDocument();
    expect(within(panel).queryByText(/still running the build it had/)).toBeNull();
    expect(within(panel).queryByText(/nothing was rolled back/i)).toBeNull();
  });

  it("labels each history row with the button that was pressed", async () => {
    mocked.listPlatformAttempts.mockResolvedValue({
      attempts: [attempt({ id: "a2", kind: "revert" }), attempt()],
    });
    renderTab();

    const history = await screen.findAllByText("Revert");
    expect(history.length).toBeGreaterThan(0);
    expect(await screen.findByText("Apply")).toBeInTheDocument();
  });
});
