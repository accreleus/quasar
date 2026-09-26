// Developer apply (#360) on Fleet › Releases: the rail card appears only with an
// owned host, the drawer offers only owned hosts, client-side digest checks gate
// Apply, the request carries exactly the filled images split into repository
// and digest, a server refusal shows in the drawer, and a 202 closes it.

import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type {
  PlatformApplyAttempt,
  PlatformHostIdentity,
  PlatformPreflight,
  PlatformReleaseView,
} from "../../../api/types";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { FLEET_TABS } from "../../../components/shell/sectionTabs";
import { ToastProvider } from "../../../components/Toast";
import { ReleasesTab } from "./ReleasesTab";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");

const mocked = vi.mocked(adminApi);

const PF: PlatformPreflight = { state: "unknown", checked_at: null, checks: [] };
const COMMIT = "a".repeat(40);
const NA_DIGEST = "sha256:" + "c05a".repeat(16);
const NA = `registry.example.invalid:5000/quasar-dev/quasar-node-agent@${NA_DIGEST}`;
const RA_DIGEST = "sha256:" + "9b3e".repeat(16);
const RA = `registry.example.invalid:5000/quasar-dev/quasar-recovery@${RA_DIGEST}`;

function host(over: Partial<PlatformHostIdentity>): PlatformHostIdentity {
  return {
    host_id: "h1",
    node_name: "gpu-host-2",
    status: "online",
    agent_version: "0.5.2",
    source_commit: COMMIT,
    built_at: "2026-09-20T10:00:00Z",
    install_mode: "owned",
    updater_present: true,
    identity_known: true,
    ...over,
  } as PlatformHostIdentity;
}

function view(hosts: PlatformHostIdentity[]): PlatformReleaseView {
  return {
    channel: "stable",
    source_repo: "accreleus/quasar",
    edge_branch: "develop",
    checked_at: "2026-09-25T02:00:00Z",
    last_error: null,
    installed: {
      control_plane: {
        version: "0.5.2",
        source_commit: COMMIT,
        built_at: "2026-09-20T10:00:00Z",
        schema_version: 88,
      },
      hosts,
    },
    available: [],
    targets: [
      { kind: "control_plane", host_id: null, node_name: null, eligible: false, reason: "up_to_date", preflight: PF },
    ],
    faults: [],
  } as PlatformReleaseView;
}

const MIXED = [
  host({ host_id: "h1", node_name: "gpu-host-2" }),
  host({ host_id: "h2", node_name: "workbench", install_mode: "source" }),
  host({ host_id: "h3", node_name: "compose-box", install_mode: "registry" }),
  host({ host_id: "h4", node_name: "gpu-host-4" }),
];

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

async function openDrawer() {
  fireEvent.click(await screen.findByRole("button", { name: "Developer apply…" }));
  return screen.getByRole("dialog", { name: "Developer apply" });
}

function applyButton(drawer: HTMLElement) {
  return within(drawer).getByRole("button", { name: "Apply digests" });
}

beforeEach(() => {
  vi.resetAllMocks();
  mocked.listAllSessions.mockResolvedValue({ items: [], next_cursor: null } as never);
  mocked.listPlatformAttempts.mockResolvedValue({ attempts: [] });
  mocked.listPlatformApplyRuns.mockResolvedValue({ runs: [] });
  mocked.listJobs.mockResolvedValue({ items: [], next_cursor: null } as never);
});

describe("Developer apply", () => {
  it("offers the rail card only when an owned host exists", async () => {
    mocked.getPlatformReleases.mockResolvedValue(
      view([host({ install_mode: "registry" }), host({ host_id: "h2", install_mode: "source" })]),
    );
    renderTab();
    await screen.findByText("Installed");
    expect(screen.queryByRole("button", { name: "Developer apply…" })).not.toBeInTheDocument();
  });

  it("lists only owned hosts as machines, and names what an agent update ends", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view(MIXED));
    renderTab();
    const drawer = await openDrawer();

    const options = within(within(drawer).getByLabelText("Machine")).getAllByRole("option");
    expect(options.map((o) => o.textContent)).toEqual(["gpu-host-2 · GPU host", "gpu-host-4 · GPU host"]);
    expect(drawer).toHaveTextContent("Applying a node agent ends that host’s sessions.");
    // A GPU host runs no control plane.
    const cp = within(drawer).getByLabelText("Control plane");
    expect(cp).toBeDisabled();
    expect(cp).toHaveAttribute("placeholder", "not on this machine");
  });

  it("keeps Apply disabled until an image is entered", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view(MIXED));
    renderTab();
    const drawer = await openDrawer();

    expect(applyButton(drawer)).toBeDisabled();
    expect(drawer).toHaveTextContent("Enter at least one image by digest.");
  });

  it("refuses a tag in place of a digest, and names how many images to fix", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view(MIXED));
    renderTab();
    const drawer = await openDrawer();

    fireEvent.change(within(drawer).getByLabelText("Node agent"), {
      target: { value: "ghcr.io/accreleus/quasar-node-agent:my-branch" },
    });
    expect(within(drawer).getByText("Use a digest (@sha256:…), not a tag.")).toBeInTheDocument();
    expect(within(drawer).getByLabelText("Node agent")).toHaveAttribute("aria-invalid", "true");
    expect(drawer).toHaveTextContent("Fix the image above to continue.");
    expect(applyButton(drawer)).toBeDisabled();
  });

  it("offers the recovery actor beside the node agent", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view(MIXED));
    mocked.developerApply.mockResolvedValue({ attempt: {} as PlatformApplyAttempt });
    renderTab();
    const drawer = await openDrawer();

    const ra = within(drawer).getByLabelText("Recovery actor");
    expect(ra).toBeEnabled();
    expect(ra).toHaveAttribute("placeholder", "namespace/quasar-recovery@sha256:…");
    fireEvent.change(within(drawer).getByLabelText("Node agent"), { target: { value: NA } });
    fireEvent.change(ra, { target: { value: RA } });
    fireEvent.click(applyButton(drawer));

    await waitFor(() => expect(mocked.developerApply).toHaveBeenCalledTimes(1));
    expect(mocked.developerApply.mock.calls[0][1].components).toEqual([
      {
        name: "recovery-actor",
        image: "registry.example.invalid:5000/quasar-dev/quasar-recovery",
        digest: RA_DIGEST,
      },
      {
        name: "node-agent",
        image: "registry.example.invalid:5000/quasar-dev/quasar-node-agent",
        digest: NA_DIGEST,
      },
    ]);
  });

  it("posts only the filled images, split into repository and digest", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view(MIXED));
    mocked.developerApply.mockResolvedValue({ attempt: {} as PlatformApplyAttempt });
    renderTab();
    const drawer = await openDrawer();

    fireEvent.change(within(drawer).getByLabelText("Machine"), { target: { value: "h4" } });
    fireEvent.change(within(drawer).getByLabelText("Node agent"), { target: { value: ` ${NA} ` } });
    expect(drawer).toHaveTextContent("Checks each digest at the registry before anything stops.");
    fireEvent.click(applyButton(drawer));

    await waitFor(() => expect(mocked.developerApply).toHaveBeenCalledTimes(1));
    expect(mocked.developerApply).toHaveBeenCalledWith("tok", {
      target: "host",
      host_id: "h4",
      components: [
        {
          name: "node-agent",
          image: "registry.example.invalid:5000/quasar-dev/quasar-node-agent",
          digest: NA_DIGEST,
        },
      ],
      force: false,
    });
  });

  it("shows a namespace refusal from the server in the drawer", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view(MIXED));
    mocked.developerApply.mockRejectedValue(
      new ApiError(409, "namespace_rejected", "registry.example.invalid/team is not an allowed namespace."),
    );
    renderTab();
    const drawer = await openDrawer();

    fireEvent.change(within(drawer).getByLabelText("Node agent"), { target: { value: NA } });
    fireEvent.click(applyButton(drawer));

    const alert = await within(drawer).findByRole("alert");
    expect(alert).toHaveTextContent("An image is not under a namespace this machine allows.");
    expect(alert).toHaveTextContent("registry.example.invalid/team is not an allowed namespace.");
    expect(screen.getByRole("dialog", { name: "Developer apply" })).toBeInTheDocument();
  });

  it("explains a host that is not eligible from the reason beside the error", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view(MIXED));
    mocked.developerApply.mockRejectedValue(
      new ApiError(409, "host_not_eligible", "no", undefined, undefined, undefined, "host_offline"),
    );
    renderTab();
    const drawer = await openDrawer();

    fireEvent.change(within(drawer).getByLabelText("Node agent"), { target: { value: NA } });
    fireEvent.click(applyButton(drawer));

    const alert = await within(drawer).findByRole("alert");
    expect(alert).toHaveTextContent("The host's agent is not connected.");
    expect(within(alert).getByText(/reason: host_offline/)).toBeInTheDocument();
  });

  it("closes the drawer and reloads the page on 202", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view(MIXED));
    mocked.developerApply.mockResolvedValue({ attempt: {} as PlatformApplyAttempt });
    renderTab();
    const drawer = await openDrawer();
    const reads = mocked.getPlatformReleases.mock.calls.length;

    fireEvent.change(within(drawer).getByLabelText("Node agent"), { target: { value: NA } });
    fireEvent.click(applyButton(drawer));

    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Developer apply" })).not.toBeInTheDocument(),
    );
    await waitFor(() => expect(mocked.getPlatformReleases.mock.calls.length).toBeGreaterThan(reads));
  });

  it("labels a developer-apply attempt in the history", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view(MIXED));
    mocked.listPlatformAttempts.mockResolvedValue({
      attempts: [
        {
          id: "a1",
          run_id: null,
          kind: "developer_apply",
          target: "host",
          host_id: "h1",
          node_name: "gpu-host-2",
          release_id: null,
          requested_digests: [{ name: "node-agent", image: "img", digest: NA_DIGEST }],
          previous_digests: [],
          state: "succeeded",
          reason: null,
          sessions_remaining: null,
          force: false,
          output: "",
          requested_by: "u1",
          created_at: "2026-09-25T11:00:00Z",
          started_at: null,
          finished_at: null,
        } as PlatformApplyAttempt,
      ],
    });
    renderTab();

    const row = await screen.findByText((_, el) => el?.textContent === "Developer apply · Updated");
    expect(row).toBeInTheDocument();
  });

  it("names each component of an attempt that moved the recovery actor first", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view(MIXED));
    const attempt = (id: string, requested: { name: string; digest: string }[]) =>
      ({
        id,
        run_id: null,
        kind: "apply",
        target: "host",
        host_id: "h1",
        node_name: "gpu-host-2",
        release_id: null,
        requested_digests: requested.map((c) => ({ ...c, image: "img" })),
        previous_digests: requested.map((c) => ({
          name: c.name,
          digest: "sha256:" + "1111".repeat(16),
        })),
        state: "succeeded",
        reason: null,
        sessions_remaining: null,
        force: false,
        output: "",
        requested_by: "u1",
        created_at: "2026-09-25T11:00:00Z",
        started_at: null,
        finished_at: null,
      }) as PlatformApplyAttempt;
    mocked.listPlatformAttempts.mockResolvedValue({
      attempts: [
        attempt("a1", [
          { name: "recovery-actor", digest: RA_DIGEST },
          { name: "node-agent", digest: NA_DIGEST },
        ]),
        attempt("a2", [{ name: "recovery-actor", digest: RA_DIGEST }]),
      ],
    });
    renderTab();

    expect(await screen.findByText(/^Recovery actor 111111111111/)).toBeInTheDocument();
    expect(screen.getByText(/^Node agent 111111111111/)).toBeInTheDocument();
    expect(screen.getByText("Recovery actor · gpu-host-2")).toBeInTheDocument();
  });
});

describe("Developer apply to the control plane's own machine (#363)", () => {
  const CP_DIGEST = "sha256:" + "e71f".repeat(16);
  const CP = `registry.example.invalid:5000/quasar-dev/quasar-control-plane@${CP_DIGEST}`;

  function ownedView(role: "combined" | "control_only"): PlatformReleaseView {
    const v = view([
      host({ host_id: "h0", node_name: "living-room-pc" }),
      host({ host_id: "h1", node_name: "gpu-host-2" }),
    ]);
    Object.assign(v.installed.control_plane, {
      install_mode: "owned",
      machine_role: role,
      machine_node_name: role === "combined" ? "living-room-pc" : "attic-server",
    });
    return v;
  }

  it("offers a combined host once, as the control plane's machine", async () => {
    mocked.getPlatformReleases.mockResolvedValue(ownedView("combined"));
    renderTab();
    const drawer = await openDrawer();
    const options = within(within(drawer).getByLabelText("Machine")).getAllByRole("option");
    expect(options.map((o) => o.textContent)).toEqual([
      "living-room-pc · Combined host",
      "gpu-host-2 · GPU host",
    ]);
    expect(within(drawer).getByLabelText("Control plane")).toBeEnabled();
    expect(within(drawer).getByLabelText("Node agent")).toBeEnabled();
  });

  it("sends a control-plane image as the control-plane target, with its actor", async () => {
    mocked.getPlatformReleases.mockResolvedValue(ownedView("control_only"));
    mocked.developerApply.mockResolvedValue({ attempt: {} as PlatformApplyAttempt });
    renderTab();
    const drawer = await openDrawer();
    expect(within(drawer).getByLabelText("Node agent")).toBeDisabled();
    fireEvent.change(within(drawer).getByLabelText("Recovery actor"), { target: { value: RA } });
    fireEvent.change(within(drawer).getByLabelText("Control plane"), { target: { value: CP } });
    fireEvent.click(applyButton(drawer));

    await waitFor(() => expect(mocked.developerApply).toHaveBeenCalledTimes(1));
    expect(mocked.developerApply).toHaveBeenCalledWith("tok", {
      target: "control_plane",
      components: [
        { name: "recovery-actor", image: "registry.example.invalid:5000/quasar-dev/quasar-recovery", digest: RA_DIGEST },
        { name: "control-plane", image: "registry.example.invalid:5000/quasar-dev/quasar-control-plane", digest: CP_DIGEST },
      ],
      force: false,
    });
  });

  it("sends the combined host's node agent alone as that host's request", async () => {
    mocked.getPlatformReleases.mockResolvedValue(ownedView("combined"));
    mocked.developerApply.mockResolvedValue({ attempt: {} as PlatformApplyAttempt });
    renderTab();
    const drawer = await openDrawer();
    fireEvent.change(within(drawer).getByLabelText("Node agent"), { target: { value: NA } });
    fireEvent.click(applyButton(drawer));
    await waitFor(() => expect(mocked.developerApply).toHaveBeenCalledTimes(1));
    expect(mocked.developerApply.mock.calls[0][1]).toMatchObject({ target: "host", host_id: "h0" });
  });

  it("refuses combinations the control plane's machine cannot take in one request", async () => {
    mocked.getPlatformReleases.mockResolvedValue(ownedView("combined"));
    renderTab();
    const drawer = await openDrawer();
    fireEvent.change(within(drawer).getByLabelText("Recovery actor"), { target: { value: RA } });
    expect(applyButton(drawer)).toBeDisabled();
    expect(drawer).toHaveTextContent("the recovery actor moves only with the control plane");

    fireEvent.change(within(drawer).getByLabelText("Control plane"), { target: { value: CP } });
    fireEvent.change(within(drawer).getByLabelText("Node agent"), { target: { value: NA } });
    expect(applyButton(drawer)).toBeDisabled();
    expect(drawer).toHaveTextContent("Apply the control plane first, then its node agent on its own.");
  });
});
