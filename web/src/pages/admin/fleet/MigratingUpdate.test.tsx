// A migrating update of an owned control plane (#364), through rendered output: the
// Update dialog's database note per database mode and backup_space status, the
// external-backup checkbox gating Update and riding the request, the refusal banner after
// backup_failed, the restore card (shown only while the failed build serves the page,
// its command the output's last line), the history's "not restored" line, and the
// developer-apply Database section.

import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import * as adminApi from "../../../api/admin";
import type {
  PlatformApplyAttempt,
  PlatformApplyRun,
  PlatformPreflightCheck,
  PlatformRelease,
  PlatformReleaseView,
} from "../../../api/types";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { FLEET_TABS } from "../../../components/shell/sectionTabs";
import { ToastProvider } from "../../../components/Toast";
import { FleetApplyButton } from "./FleetApply";
import { ReleasesTab } from "./ReleasesTab";
import { dumpTakenAt } from "./migratingUpdate";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");

const mocked = vi.mocked(adminApi);

const OLD_COMMIT = "a".repeat(40);
const NEW_COMMIT = "b".repeat(40);
const OLD_CP_DIGEST = "sha256:" + "1".repeat(64);
const NEW_CP_DIGEST = "sha256:" + "2".repeat(64);
const RA_DIGEST = "sha256:" + "3".repeat(64);

const PASS = "Quasar's database is about 1.4 GB; 212 GB is free for the pre-update dump.";
const UNKNOWN = "Free space on this machine has not been reported, so it is checked just before the dump.";
const FAIL =
  "The pre-update dump needs about 1.4 GB and 0.6 GB is free on this machine. Free some space there, then check again.";

const OWN_CMD =
  "docker exec quasar-recovery quasar-recovery restore --dump 20260925T140200Z-schema-88 --to 0.5.2";
const EXT_CMD = "docker exec quasar-recovery quasar-recovery restore --to 0.5.2";

function release(over: Partial<PlatformRelease> = {}): PlatformRelease {
  return {
    id: "r-new",
    channel: "stable",
    version: "0.6.0",
    source_commit: NEW_COMMIT,
    built_at: "2026-09-24T16:40:00Z",
    schema_version: 91,
    migrates: true,
    prerelease: false,
    notes: "",
    compare_url: null,
    manifest: {
      format_version: 2,
      components: [{ name: "control-plane", image: "img", digest: NEW_CP_DIGEST }],
    },
    discovered_at: "2026-09-24T17:00:00Z",
    ...over,
  } as PlatformRelease;
}

const OLD_RELEASE = release({
  id: "r-old",
  version: "0.5.2",
  source_commit: OLD_COMMIT,
  schema_version: 88,
  migrates: false,
  manifest: { format_version: 2, components: [{ name: "control-plane", image: "img", digest: OLD_CP_DIGEST }] } as never,
});

type Mode = "owned" | "external" | null;

/** living-room-pc: a combined host whose control plane a recovery actor created. */
function view(opts: { mode?: Mode; space?: PlatformPreflightCheck | null; commit?: string; migrates?: boolean } = {}) {
  const { mode = "owned", space = null, commit = OLD_COMMIT, migrates = true } = opts;
  const cpBlocked = space?.status === "fail";
  return {
    channel: "stable",
    source_repo: "accreleus/quasar",
    edge_branch: "develop",
    checked_at: "2026-09-25T02:00:00Z",
    last_error: null,
    installed: {
      control_plane: {
        version: commit === OLD_COMMIT ? "0.5.2" : "0.6.0",
        source_commit: commit,
        built_at: "2026-09-20T10:00:00Z",
        schema_version: commit === OLD_COMMIT ? 88 : 91,
        install_mode: "owned",
        database_mode: mode,
        machine_role: "combined",
        machine_node_name: "living-room-pc",
      },
      hosts: [],
    },
    available: [release({ migrates: migrates && commit === OLD_COMMIT }), OLD_RELEASE],
    targets: [
      {
        kind: "control_plane",
        host_id: null,
        node_name: null,
        eligible: !cpBlocked,
        reason: cpBlocked ? "preflight_blocked" : null,
        preflight: {
          state: cpBlocked ? "blocked" : "ok",
          checked_at: "2026-09-25T02:00:00Z",
          checks: space ? [space] : [],
        },
      },
      { kind: "host", host_id: "h1", node_name: "gpu-host-2", eligible: true, reason: null },
      { kind: "host", host_id: "h5", node_name: "gpu-host-5", eligible: false, reason: "host_offline" },
    ],
    faults: [],
  } as unknown as PlatformReleaseView;
}

function check(status: string, detail: string): PlatformPreflightCheck {
  return { id: "backup_space", status, detail } as PlatformPreflightCheck;
}

function cpAttempt(over: Partial<PlatformApplyAttempt> = {}): PlatformApplyAttempt {
  return {
    id: "4b90aa00-0000-0000-0000-0000000000e1",
    run_id: "run-1",
    kind: "apply",
    target: "control_plane",
    host_id: null,
    node_name: null,
    release_id: "r-new",
    requested_digests: [
      { name: "recovery-actor", image: "ra", digest: RA_DIGEST },
      { name: "control-plane", image: "cp", digest: NEW_CP_DIGEST },
    ],
    previous_digests: [{ name: "control-plane", digest: OLD_CP_DIGEST }],
    state: "failed",
    reason: "unhealthy",
    sessions_remaining: null,
    force: false,
    output: `pulled\nhealth check failed at 14:07\n${OWN_CMD}\n`,
    pre_update_dump: "20260925T140200Z-schema-88",
    requested_by: "u1",
    created_at: "2026-09-25T14:01:00Z",
    started_at: "2026-09-25T14:01:00Z",
    finished_at: "2026-09-25T14:07:00Z",
    ...over,
  } as PlatformApplyAttempt;
}

function renderButton(v: PlatformReleaseView, onRecheck = vi.fn()) {
  render(
    <ToastProvider>
      <FleetApplyButton view={v} onStarted={() => {}} onRecheck={onRecheck} />
    </ToastProvider>,
  );
  return onRecheck;
}

async function openDialog() {
  fireEvent.click(screen.getByRole("button", { name: "Update Quasar" }));
  return screen.findByRole("dialog");
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
});

describe("Update Quasar on an owned control plane whose release migrates", () => {
  it("says Quasar dumps its own database first, with the server's numbers verbatim", async () => {
    renderButton(view({ space: check("pass", PASS) }));
    const dialog = await openDialog();
    const note = within(dialog).getByTestId("fleet-database");
    expect(note).toHaveTextContent("Quasar dumps its database first.");
    expect(note).toHaveTextContent("Before the control plane on living-room-pc is replaced");
    expect(note).toHaveTextContent(PASS);
    expect(note).toHaveTextContent("If the dump cannot be taken, the update stops there");
    expect(note).toHaveTextContent("The last three dumps are kept on that machine.");
    expect(dialog).toHaveTextContent("this page loses contact for about a minute");
    // The skipped-host note and force stay.
    expect(within(dialog).getByTestId("fleet-will-skip")).toHaveTextContent("gpu-host-5");
    expect(within(dialog).getByRole("button", { name: "Update" })).toBeEnabled();
  });

  it("sends no backup confirmation for Quasar's own database", async () => {
    mocked.applyPlatformReleaseToFleet.mockResolvedValue({ run: {} as PlatformApplyRun });
    renderButton(view({ space: check("pass", PASS) }));
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("button", { name: "Update" }));
    await waitFor(() =>
      expect(mocked.applyPlatformReleaseToFleet).toHaveBeenCalledWith("tok", {
        release_id: "r-new",
        force: false,
      }),
    );
  });

  it("says free space is checked just before the dump when it has not been reported", async () => {
    renderButton(view({ space: check("unknown", UNKNOWN) }));
    const note = within(await openDialog()).getByTestId("fleet-database");
    expect(note).toHaveTextContent(UNKNOWN);
    expect(note).toHaveTextContent("If there is not enough, or the dump fails, the update stops there");
  });

  it("opens despite a failing backup_space, explains it, keeps Update disabled and checks again", async () => {
    const recheck = renderButton(view({ space: check("fail", FAIL) }));
    expect(screen.getByRole("button", { name: "Update Quasar" })).toBeEnabled();
    const dialog = await openDialog();
    const note = within(dialog).getByTestId("fleet-database");
    expect(note).toHaveClass("warn");
    expect(note).toHaveTextContent("Not enough free space for the database dump.");
    expect(note).toHaveTextContent(FAIL);
    expect(within(dialog).getByRole("button", { name: "Update" })).toBeDisabled();
    fireEvent.click(within(dialog).getByRole("button", { name: /Check again/ }));
    expect(recheck).toHaveBeenCalledTimes(1);
  });

  it("keeps another failing check a hard stop on the head button", () => {
    const v = view({ space: check("fail", FAIL) });
    v.targets[0].preflight!.checks.unshift({
      id: "image_resolvable",
      status: "fail",
      detail: "the images did not resolve",
    } as PlatformPreflightCheck);
    renderButton(v);
    expect(screen.getByRole("button", { name: "Update Quasar" })).toBeDisabled();
  });

  it("asks for the operator's backup on their own database, gating Update on the checkbox", async () => {
    mocked.applyPlatformReleaseToFleet.mockResolvedValue({ run: {} as PlatformApplyRun });
    renderButton(view({ mode: "external" }));
    const dialog = await openDialog();
    const note = within(dialog).getByTestId("fleet-database");
    expect(note).toHaveClass("warn");
    expect(note).toHaveTextContent("Quasar does not back up your database.");
    const update = within(dialog).getByRole("button", { name: "Update" });
    expect(update).toBeDisabled();
    expect(dialog).toHaveTextContent("Update stays unavailable until you confirm.");

    fireEvent.click(
      within(dialog).getByRole("checkbox", { name: /I have a current backup of this database/ }),
    );
    expect(update).toBeEnabled();
    expect(dialog).not.toHaveTextContent("Update stays unavailable until you confirm.");
    fireEvent.click(update);
    await waitFor(() =>
      expect(mocked.applyPlatformReleaseToFleet).toHaveBeenCalledWith("tok", {
        release_id: "r-new",
        force: false,
        external_backup_confirmed: true,
      }),
    );
  });

  it("says nothing about the database for a release that does not migrate", async () => {
    renderButton(view({ migrates: false, space: check("pass", "This release does not change the database, so no dump is taken.") }));
    const dialog = await openDialog();
    expect(within(dialog).queryByTestId("fleet-database")).not.toBeInTheDocument();
    expect(dialog).toHaveTextContent("Live sessions keep streaming through it");
  });
});

describe("Releases after a migrating control-plane update went wrong", () => {
  it("reports a refused dump in the banner, the actor already moved, identifiers only under Details", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view({ space: check("pass", PASS) }));
    mocked.listPlatformAttempts.mockResolvedValue({
      attempts: [
        cpAttempt({ reason: "backup_failed", pre_update_dump: null, output: "pg_dump: no space left on device" }),
      ],
    });
    renderTab();
    const banner = await screen.findByTestId("update-refused");
    expect(banner).toHaveTextContent("Update refused");
    expect(banner).toHaveTextContent("v0.5.2 → v0.6.0 stopped before the control plane moved");
    expect(banner).toHaveTextContent("Quasar could not dump its database on living-room-pc.");
    expect(banner).toHaveTextContent("The recovery actor on living-room-pc is already on v0.6.0");
    const details = within(banner).getByText("Details").closest("details")!;
    expect(details.open).toBe(false);
    expect(details).toHaveTextContent("reason: backup_failed");
    // The update banner is replaced, the head's Update Quasar still offered.
    expect(screen.queryByText("Update available")).not.toBeInTheDocument();
  });

  it("does not name the actor when the attempt did not move it", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view({ space: check("pass", PASS) }));
    mocked.listPlatformAttempts.mockResolvedValue({
      attempts: [
        cpAttempt({
          reason: "backup_failed",
          pre_update_dump: null,
          requested_digests: [{ name: "control-plane", image: "cp", digest: NEW_CP_DIGEST }],
        }),
      ],
    });
    renderTab();
    const banner = await screen.findByTestId("update-refused");
    expect(banner).not.toHaveTextContent("already on");
  });

  it("shows the restore card while the failed build serves the page, its command the output's last line", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view({ commit: NEW_COMMIT }));
    mocked.listPlatformAttempts.mockResolvedValue({ attempts: [cpAttempt()] });
    renderTab();
    const card = await screen.findByTestId("restore-own");
    expect(card).toHaveTextContent("Update failed");
    expect(card).toHaveTextContent("v0.6.0 failed after changing the database");
    expect(card).toHaveTextContent("To go back to v0.5.2, run this on living-room-pc.");
    // The dump's time, read from its name, as the mock says it.
    expect(card).toHaveTextContent("loads the dump taken at 25 Sep 2026, 14:02 — before the migration, under v0.5.2 —");
    expect(card).toHaveTextContent("Anything written after 25 Sep 2026, 14:02 is lost.");
    expect(within(card).getByTestId("restore-command")).toHaveTextContent(OWN_CMD);
    expect(card).toHaveTextContent("Run on living-room-pc as root");
    expect(within(card).getByRole("button", { name: "Copy restore command" })).toBeInTheDocument();
    const details = within(card).getByText("Attempt details").closest("details")!;
    expect(details.open).toBe(false);
    expect(details).toHaveTextContent("dump: 20260925T140200Z-schema-88");
    expect(details).toHaveTextContent("schema: 88 → 91");
    // It replaces the update banner, which would otherwise say "Up to date".
    expect(screen.queryByText("Up to date.")).not.toBeInTheDocument();
    expect(await screen.findByText("Failed · not restored · dump kept")).toBeInTheDocument();
  });

  it("hides the restore card once the previous build serves the page again, keeping the history", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view({ commit: OLD_COMMIT }));
    mocked.listPlatformAttempts.mockResolvedValue({ attempts: [cpAttempt()] });
    renderTab();
    expect(await screen.findByText("Failed · not restored · dump kept")).toBeInTheDocument();
    expect(screen.queryByTestId("restore-own")).not.toBeInTheDocument();
    expect(screen.getByText("Update available")).toBeInTheDocument();
  });

  it("says the dump has not been reported yet, with no command", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view({ commit: NEW_COMMIT }));
    mocked.listPlatformAttempts.mockResolvedValue({
      attempts: [cpAttempt({ pre_update_dump: null, output: "health check failed" })],
    });
    renderTab();
    const card = await screen.findByTestId("restore-unknown");
    expect(card).toHaveTextContent("has not reported which dump it took");
    expect(card).toHaveTextContent("This card fills in when the recovery actor answers.");
    expect(within(card).queryByTestId("restore-command")).not.toBeInTheDocument();
    expect(await screen.findByText("Failed · not restored · dump not reported yet")).toBeInTheDocument();
  });

  it("tells the operator to restore their own backup first on an external database", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view({ mode: "external", commit: NEW_COMMIT }));
    mocked.listPlatformAttempts.mockResolvedValue({
      attempts: [cpAttempt({ pre_update_dump: null, output: `health check failed\n${EXT_CMD}` })],
    });
    renderTab();
    const card = await screen.findByTestId("restore-external");
    expect(card).toHaveTextContent("Quasar holds no dump of your database");
    expect(card).toHaveTextContent("restore the backup you confirmed into your database with your own tools");
    // The control plane must be stopped first, or its next boot migrates the backup again.
    expect(card).toHaveTextContent("docker stop quasar-control-plane");
    expect(card).toHaveTextContent(
      "Run on living-room-pc as root, after stopping the control plane and restoring your backup",
    );
    expect(within(card).getByTestId("restore-command")).toHaveTextContent(EXT_CMD);
    expect(await screen.findByText("Failed · not restored · your own database")).toBeInTheDocument();
  });

  it("shows no restore card for a control-plane failure that replaced nothing", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view({ commit: NEW_COMMIT }));
    mocked.listPlatformAttempts.mockResolvedValue({
      attempts: [cpAttempt({ reason: "pull_failed", pre_update_dump: null, output: "pull failed" })],
    });
    renderTab();
    await screen.findByText(/The image could not be pulled/);
    expect(screen.queryByTestId(/restore-/)).not.toBeInTheDocument();
  });
});

describe("Developer apply of a control-plane digest (#364 Database section)", () => {
  const CP = `registry.example.invalid:5000/quasar-dev/quasar-control-plane@${NEW_CP_DIGEST}`;

  async function openDrawer(v: PlatformReleaseView) {
    mocked.getPlatformReleases.mockResolvedValue(v);
    renderTab();
    fireEvent.click(await screen.findByRole("button", { name: "Developer apply…" }));
    return screen.getByRole("dialog", { name: "Developer apply" });
  }

  it("draws the Database section only once a control-plane image is named", async () => {
    const drawer = await openDrawer(view({ space: check("pass", PASS) }));
    expect(drawer).not.toHaveTextContent("Quasar dumps its database first.");
    fireEvent.change(within(drawer).getByLabelText("Control plane"), { target: { value: CP } });
    expect(drawer).toHaveTextContent("Quasar dumps its database first.");
    expect(drawer).toHaveTextContent("If this build changes the database");
    expect(drawer).not.toHaveTextContent("#364");
  });

  it("sends the external-backup checkbox as external_backup_confirmed without blocking Apply", async () => {
    mocked.developerApply.mockResolvedValue({ attempt: {} as PlatformApplyAttempt });
    const drawer = await openDrawer(view({ mode: "external" }));
    fireEvent.change(within(drawer).getByLabelText("Control plane"), { target: { value: CP } });
    expect(drawer).toHaveTextContent("Quasar does not back up your database.");
    const apply = within(drawer).getByRole("button", { name: "Apply digests" });
    expect(apply).toBeEnabled();
    fireEvent.click(apply);
    await waitFor(() => expect(mocked.developerApply).toHaveBeenCalledTimes(1));
    expect(mocked.developerApply.mock.calls[0][1]).toMatchObject({
      target: "control_plane",
      external_backup_confirmed: false,
    });
  });

  it("sends true once the backup is confirmed", async () => {
    mocked.developerApply.mockResolvedValue({ attempt: {} as PlatformApplyAttempt });
    const drawer = await openDrawer(view({ mode: "external" }));
    fireEvent.change(within(drawer).getByLabelText("Control plane"), { target: { value: CP } });
    fireEvent.click(within(drawer).getByRole("checkbox", { name: /I have a current backup/ }));
    fireEvent.click(within(drawer).getByRole("button", { name: "Apply digests" }));
    await waitFor(() => expect(mocked.developerApply).toHaveBeenCalledTimes(1));
    expect(mocked.developerApply.mock.calls[0][1]).toMatchObject({ external_backup_confirmed: true });
  });
});

describe("dumpTakenAt", () => {
  it("reads the time from the recovery actor's dump names only", () => {
    expect(dumpTakenAt("20260925T140200Z-schema-88")).toBe("2026-09-25T14:02:00Z");
    expect(dumpTakenAt("20260925T140200Z-schema-88-7a1f6f1e")).toBe("2026-09-25T14:02:00Z");
    expect(dumpTakenAt("2026-09-25T1402Z-schema-88")).toBeNull();
    expect(dumpTakenAt(null)).toBeNull();
  });
});
