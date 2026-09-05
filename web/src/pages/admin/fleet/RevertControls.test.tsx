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

describe("Revert on the Releases page", () => {
  it("offers no Revert when the host has nothing to go back to", async () => {
    renderTab();
    await screen.findByText("gpu-host-01");
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
    expect(within(panel).getByTestId("failed-output-h1")).toHaveTextContent("exit 1");
    expect(within(panel).getByText(new RegExp(`node-agent ${OLD_DIGEST.slice(7, 19)}`))).toBeInTheDocument();
    // The manual path is the same registry recipe, pinned to the previous digest.
    expect(
      within(panel).getByText(`QUASAR_AGENT_IMAGE=${AGENT_IMAGE}@${OLD_DIGEST}`),
    ).toBeInTheDocument();
    // And the panel offers the action that does it for the operator.
    expect(within(panel).getByRole("button", { name: /^Revert$/ })).toBeInTheDocument();
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
