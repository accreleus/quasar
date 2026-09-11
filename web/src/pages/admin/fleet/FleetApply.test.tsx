// Fleet apply (#117): the button's gating, the confirmation naming the release
// and the eligible-host count, force on the wire, the run panel's targets and
// skips, the cancel gate, and the note that covers the control plane's restart.

import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import * as adminApi from "../../../api/admin";
import type {
  PlatformApplyAttempt,
  PlatformApplyRun,
  PlatformRelease,
  PlatformReleaseView,
} from "../../../api/types";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { FLEET_TABS } from "../../../components/shell/sectionTabs";
import { ToastProvider } from "../../../components/Toast";
import { FleetApplyButton, FleetRunPanel } from "./FleetApply";
import { eligibilityText } from "./releasesCopy";
import { ReleasesTab } from "./ReleasesTab";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");

const mocked = vi.mocked(adminApi);

const CP_COMMIT = "a".repeat(40);
const NEW_COMMIT = "b".repeat(40);

function release(over: Partial<PlatformRelease> = {}): PlatformRelease {
  return {
    id: "r1",
    channel: "stable",
    version: "0.3.0",
    source_commit: NEW_COMMIT,
    built_at: "2026-09-04T12:00:00Z",
    schema_version: 75,
    // Served by the control plane, not derived here (#153): schema 75 against
    // the fixture's installed 74 means this release carries a migration.
    migrates: true,
    prerelease: false,
    notes: "",
    compare_url: null,
    manifest: { format_version: 1 },
    discovered_at: "2026-09-04T02:07:11Z",
    ...over,
  } as PlatformRelease;
}

/** Three eligible hosts, so a plural count is what the confirmation must say. */
function view(over: Partial<PlatformReleaseView> = {}): PlatformReleaseView {
  return {
    channel: "stable",
    edge_branch: "develop",
    checked_at: "2026-09-04T02:07:11Z",
    last_error: null,
    installed: {
      control_plane: {
        version: "0.2.0",
        source_commit: CP_COMMIT,
        built_at: "2026-08-19T09:14:02Z",
        schema_version: 74,
      },
      hosts: [],
    },
    available: [release()],
    targets: [
      { kind: "control_plane", host_id: null, node_name: null, eligible: true, reason: null },
      { kind: "host", host_id: "h1", node_name: "gpu-host-01", eligible: true, reason: null },
      { kind: "host", host_id: "h2", node_name: "gpu-host-02", eligible: true, reason: null },
      { kind: "host", host_id: "h3", node_name: "gpu-host-03", eligible: true, reason: null },
      {
        kind: "host",
        host_id: "h4",
        node_name: "gpu-host-04",
        eligible: false,
        reason: "host_offline",
      },
    ],
    faults: [],
    ...over,
  } as PlatformReleaseView;
}

function attempt(over: Partial<PlatformApplyAttempt> = {}): PlatformApplyAttempt {
  return {
    id: "at-" + (over.host_id ?? "cp"),
    run_id: "run-1",
    kind: "apply",
    target: "host",
    host_id: "h1",
    node_name: "gpu-host-01",
    release_id: "r1",
    requested_digests: [],
    previous_digests: [],
    state: "pulling",
    reason: null,
    sessions_remaining: null,
    force: false,
    output: "",
    requested_by: "u1",
    created_at: "2026-09-05T11:00:00Z",
    started_at: null,
    finished_at: null,
    ...over,
  } as PlatformApplyAttempt;
}

function run(over: Partial<PlatformApplyRun> = {}): PlatformApplyRun {
  return {
    id: "run-1",
    release_id: "r1",
    state: "running",
    force: false,
    requested_by: "u1",
    cancel_requested: false,
    cancel_requested_at: null,
    current_target: "host",
    current_host_id: "h1",
    error: null,
    skipped: [],
    attempts: [attempt()],
    created_at: "2026-09-05T11:00:00Z",
    started_at: "2026-09-05T11:00:01Z",
    finished_at: null,
    ...over,
  } as PlatformApplyRun;
}

function renderButton(v: PlatformReleaseView, onStarted = () => {}) {
  return render(
    <ToastProvider>
      <FleetApplyButton view={v} onStarted={onStarted} />
    </ToastProvider>,
  );
}

function renderPanel(r: PlatformApplyRun, targets = view().targets) {
  return render(
    <ToastProvider>
      <FleetRunPanel run={r} targets={targets} onChanged={() => {}} />
    </ToastProvider>,
  );
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
  // The head's "next check" fragment reads the detection job's schedule.
  mocked.listJobs.mockResolvedValue({ items: [], next_cursor: null } as never);
});

describe("FleetApplyButton", () => {
  it("is offered when a newer release is listed and nothing is running", () => {
    renderButton(view());
    expect(screen.getByRole("button", { name: "Update Quasar" })).toBeInTheDocument();
  });

  it("is absent while a fleet run is active — the run panel is the only control then", () => {
    renderButton(view({ active_apply: { run: run(), attempts: [] } } as Partial<PlatformReleaseView>));
    expect(screen.queryByRole("button", { name: "Update Quasar" })).not.toBeInTheDocument();
  });

  it("is absent when this instance is already on the newest release", () => {
    renderButton(view({ available: [release({ source_commit: CP_COMMIT })] }));
    expect(screen.queryByRole("button", { name: "Update Quasar" })).not.toBeInTheDocument();
  });

  it("names the release, the eligible hosts and the restart in its confirmation", async () => {
    renderButton(view());
    screen.getByRole("button", { name: "Update Quasar" }).click();

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/then 3 eligible hosts, to/)).toBeInTheDocument();
    expect(within(dialog).getByText("0.3.0")).toBeInTheDocument();
    expect(within(dialog).getByText(/ends every live session on 3 hosts/)).toBeInTheDocument();
    expect(within(dialog).getByText(/lose contact for about 20 seconds/)).toBeInTheDocument();
  });

  // #153. The fixture release is served with migrates: true (schema 75 against
  // an installed 74), so the control-plane step still empties the instance.
  it("says a migrating release waits for every session on the instance", async () => {
    renderButton(view());
    screen.getByRole("button", { name: "Update Quasar" }).click();

    const dialog = await screen.findByRole("dialog");
    expect(
      within(dialog).getByText(/changes the database, so the update waits for every session/),
    ).toBeInTheDocument();
  });

  // The same release at the installed schema runs no migration, so its restart
  // carries the sessions rather than ending them (#128 made that true, #153
  // stopped draining for it). Promising an outage that does not happen is as
  // wrong as hiding one that does.
  it("says a non-migrating release keeps live sessions streaming", async () => {
    renderButton(view({ available: [release({ schema_version: 74, migrates: false })] }));
    screen.getByRole("button", { name: "Update Quasar" }).click();

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/Live sessions keep streaming through it/)).toBeInTheDocument();
    expect(within(dialog).queryByText(/waits for every session on the instance/)).toBeNull();
  });

  // Nothing moves before the control plane, so a run it cannot take is refused
  // by the server; the button must not offer it.
  it("is disabled, with the control plane's own reason, when the control plane cannot take it", () => {
    const v = view({
      targets: [
        {
          kind: "control_plane",
          host_id: null,
          node_name: null,
          eligible: false,
          reason: "install_mode_source",
        },
        { kind: "host", host_id: "h1", node_name: "gpu-host-01", eligible: true, reason: null },
      ],
    } as Partial<PlatformReleaseView>);
    renderButton(v);

    const button = screen.getByRole("button", { name: "Update Quasar" });
    expect(button).toBeDisabled();
    expect(button).toHaveAttribute("title", eligibilityText("install_mode_source"));
  });

  it("stays enabled when the control plane is only already on the release", () => {
    const v = view({
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
    } as Partial<PlatformReleaseView>);
    renderButton(v);

    expect(screen.getByRole("button", { name: "Update Quasar" })).toBeEnabled();
  });

  it("says one eligible host in the singular", async () => {
    const v = view({
      targets: [
        { kind: "control_plane", host_id: null, node_name: null, eligible: true, reason: null },
        { kind: "host", host_id: "h1", node_name: "gpu-host-01", eligible: true, reason: null },
      ],
    } as Partial<PlatformReleaseView>);
    renderButton(v);
    screen.getByRole("button", { name: "Update Quasar" }).click();

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/then 1 eligible host, to/)).toBeInTheDocument();
    expect(within(dialog).getByText(/ends every live session on 1 host/)).toBeInTheDocument();
  });

  it("sends force false by default", async () => {
    mocked.applyPlatformReleaseToFleet.mockResolvedValue({ run: run() });
    renderButton(view());
    screen.getByRole("button", { name: "Update Quasar" }).click();
    (await screen.findByRole("button", { name: "Update" })).click();

    await waitFor(() =>
      expect(mocked.applyPlatformReleaseToFleet).toHaveBeenCalledWith("tok", {
        release_id: "r1",
        force: false,
      }),
    );
  });

  it("ticking force sends it, and the run is reported started", async () => {
    mocked.applyPlatformReleaseToFleet.mockResolvedValue({ run: run() });
    const onStarted = vi.fn();
    renderButton(view(), onStarted);
    screen.getByRole("button", { name: "Update Quasar" }).click();

    const dialog = await screen.findByRole("dialog");
    within(dialog).getByRole("checkbox").click();
    within(dialog).getByRole("button", { name: "Update" }).click();

    await waitFor(() =>
      expect(mocked.applyPlatformReleaseToFleet).toHaveBeenCalledWith("tok", {
        release_id: "r1",
        force: true,
      }),
    );
    expect(onStarted).toHaveBeenCalled();
  });
});

describe("FleetRunPanel", () => {
  it("names the current target and renders a state per attempt", () => {
    renderPanel(
      run({
        attempts: [
          attempt({ id: "cp", target: "control_plane", host_id: null, node_name: null, state: "succeeded" }),
          attempt({ state: "pulling" }),
        ],
      }),
    );

    expect(screen.getByText(/Now: gpu-host-01/)).toBeInTheDocument();
    expect(screen.getByText("Updated")).toBeInTheDocument();
    expect(screen.getByText("Pulling the image")).toBeInTheDocument();
  });

  // When the control-plane step waits at all — since #153, only for a release
  // carrying a migration — the wait is fleet-wide, not one host's, and the
  // panel must say so.
  it("says the control-plane step is waiting on the whole fleet", () => {
    renderPanel(
      run({
        current_target: "control_plane",
        current_host_id: null,
        attempts: [
          attempt({
            id: "cp",
            target: "control_plane",
            host_id: null,
            node_name: null,
            state: "waiting_sessions",
            sessions_remaining: 3,
          }),
        ],
      }),
    );

    expect(screen.getByText(/waiting on 3 sessions across the fleet/)).toBeInTheDocument();
  });

  it("lists what it passed over, with the reason as a sentence", () => {
    renderPanel(
      run({ skipped: [{ host_id: "h4", node_name: "gpu-host-04", reason: "host_offline" }] }),
    );

    expect(screen.getByText("Not updated")).toBeInTheDocument();
    expect(screen.getByText("gpu-host-04")).toBeInTheDocument();
    expect(screen.getByText(/The host's agent is not connected/)).toBeInTheDocument();
  });

  it("reports a failed run's state and its run-level error", () => {
    renderPanel(
      run({
        state: "failed",
        current_target: null,
        current_host_id: null,
        error: "the updater could not be reached",
        attempts: [attempt({ state: "failed", reason: "recreate_failed" })],
      }),
    );

    expect(screen.getByText("Stopped at the first target that failed.")).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent("the updater could not be reached");
    expect(screen.queryByRole("button", { name: "Cancel" })).not.toBeInTheDocument();
  });

  it("cancel is offered while targets are still queued, and stops the run", async () => {
    mocked.cancelPlatformApplyRun.mockResolvedValue({ run: run({ cancel_requested: true }) });
    renderPanel(run());

    const cancel = screen.getByRole("button", { name: "Cancel" });
    expect(cancel).toBeEnabled();
    cancel.click();

    await waitFor(() =>
      expect(mocked.cancelPlatformApplyRun).toHaveBeenCalledWith("tok", "run-1"),
    );
  });

  it("cancel is disabled once it has been requested", () => {
    renderPanel(run({ cancel_requested: true }));
    expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled();
  });

  it("cancel is disabled once the last target has started, and says why", () => {
    renderPanel(
      run({
        current_host_id: "h3",
        attempts: [
          attempt({ host_id: "h1", node_name: "gpu-host-01", state: "succeeded" }),
          attempt({ id: "at-h2", host_id: "h2", node_name: "gpu-host-02", state: "succeeded" }),
          attempt({ id: "at-h3", host_id: "h3", node_name: "gpu-host-03", state: "pulling" }),
        ],
      }),
    );

    expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled();
    expect(screen.getByText(/a cancel cannot interrupt it/)).toBeInTheDocument();
  });
});

describe("ReleasesTab › fleet run", () => {
  it("shows the restarting note while the control-plane attempt is recreating", async () => {
    const cp = attempt({
      id: "cp",
      target: "control_plane",
      host_id: null,
      node_name: null,
      state: "recreating",
    });
    mocked.getPlatformReleases.mockResolvedValue(
      view({
        active_apply: {
          run: run({ current_target: "control_plane", current_host_id: null, attempts: [cp] }),
          attempts: [cp],
        },
      } as Partial<PlatformReleaseView>),
    );
    renderTab();

    expect(
      await screen.findByText(/The control plane is restarting on the new release/),
    ).toBeInTheDocument();
    expect(screen.getByText("Fleet update")).toBeInTheDocument();
  });
});

// Amendment 9: a run that passed a host over is partial, says so in one
// sentence, and offers to retry just those hosts; a failed run is unchanged.
describe("FleetRunPanel partial outcome (#190)", () => {
  const partial = () =>
    run({
      state: "succeeded_partial",
      current_target: null,
      current_host_id: null,
      finished_at: "2026-09-05T11:20:00Z",
      attempts: [
        attempt({ id: "at-cp", target: "control_plane", host_id: null, node_name: null, state: "succeeded" }),
        attempt({ id: "at-h1", host_id: "h1", node_name: "gpu-host-01", state: "succeeded" }),
        attempt({ id: "at-h2", host_id: "h2", node_name: "gpu-host-02", state: "succeeded" }),
      ],
      skipped: [
        { host_id: "h3", node_name: "gpu-host-03", reason: "up_to_date" },
        { host_id: "h4", node_name: "gpu-host-04", reason: "host_offline" },
      ],
    });

  it("reads as partial, in a sentence that counts the hosts and names the skipped one", () => {
    renderPanel(partial());
    expect(screen.getByText("succeeded_partial")).toBeInTheDocument();
    expect(screen.getByTestId("fleet-partial")).toHaveTextContent(
      "Applied to the control plane and 2 of 3 hosts — 1 skipped: gpu-host-04 (offline)",
    );
    expect(screen.getByRole("button", { name: "Retry skipped hosts" })).toBeInTheDocument();
  });

  it("retry is a plain fleet apply of the same release carrying retry_of", async () => {
    mocked.applyPlatformReleaseToFleet.mockResolvedValue({ run: run({ id: "run-2", retry_of: "run-1" }) } as never);
    const onChanged = vi.fn();
    render(
      <ToastProvider>
        <FleetRunPanel run={partial()} targets={view().targets} onChanged={onChanged} />
      </ToastProvider>,
    );
    screen.getByRole("button", { name: "Retry skipped hosts" }).click();
    await waitFor(() =>
      expect(mocked.applyPlatformReleaseToFleet).toHaveBeenCalledWith("tok", {
        release_id: "r1",
        force: false,
        retry_of: "run-1",
      }),
    );
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  it("marks a retry run, and never offers Retry on a failed run", () => {
    renderPanel(run({ retry_of: "run-0" } as Partial<PlatformApplyRun>));
    expect(screen.getByText("retry")).toBeInTheDocument();
    render(
      <ToastProvider>
        <FleetRunPanel
          run={run({ state: "failed", current_target: null, attempts: [attempt({ state: "failed", reason: "unhealthy" })] })}
          targets={view().targets}
          onChanged={() => {}}
        />
      </ToastProvider>,
    );
    expect(screen.queryByRole("button", { name: "Retry skipped hosts" })).not.toBeInTheDocument();
    expect(screen.queryByTestId("fleet-partial")).not.toBeInTheDocument();
  });
});

describe("FleetApplyButton preflight (#187)", () => {
  const blockedCheck = {
    id: "updater_socket",
    status: "fail",
    detail: "the updater's socket volume is not mounted in this container; recreate the control plane",
  };

  it("names the failing check and its fix when the control plane is blocked", () => {
    const v = view();
    v.targets[0] = {
      ...v.targets[0],
      eligible: false,
      reason: "preflight_blocked",
      preflight: { state: "blocked", checked_at: "2026-09-05T11:00:00Z", checks: [blockedCheck] },
    } as PlatformReleaseView["targets"][number];
    renderButton(v);
    const button = screen.getByRole("button", { name: "Update Quasar" });
    expect(button).toBeDisabled();
    expect(button).toHaveAttribute("title", expect.stringContaining("recreate the control plane"));
  });

  it("lists the hosts a run will skip, with the fix for a blocked one, before asking for consent", async () => {
    const v = view();
    v.targets[2] = {
      ...v.targets[2],
      eligible: false,
      reason: "preflight_blocked",
      preflight: {
        state: "blocked",
        checked_at: null,
        checks: [{ id: "health_addr_bindable", status: "fail", detail: "127.0.0.1:9091 is answered by pid 4121, not this agent" }],
      },
    } as PlatformReleaseView["targets"][number];
    renderButton(v);
    screen.getByRole("button", { name: "Update Quasar" }).click();
    const note = await screen.findByTestId("fleet-will-skip");
    expect(note).toHaveTextContent("Will be skipped and stay on the old release (2)");
    expect(within(note).getByText("gpu-host-02")).toBeInTheDocument();
    expect(note).toHaveTextContent("pid 4121");
    expect(within(note).getByText("gpu-host-04")).toBeInTheDocument();
    expect(note).toHaveTextContent("The host's agent is not connected");
  });
});
