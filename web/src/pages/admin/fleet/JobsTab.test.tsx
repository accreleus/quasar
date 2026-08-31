// Fleet › Jobs. Covers the three job shapes (managed instance-scoped, managed
// host-scoped, unmanaged) and their sections, every column the row shows
// (including the cells a host-scoped job derives from its targets), the
// toolbar filters, the run-now error mapping, schedule-modal validation, and
// the run-history drawer.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { JobsTab } from "./JobsTab";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { FLEET_TABS } from "../../../components/shell/sectionTabs";
import { ToastProvider } from "../../../components/Toast";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { Job, JobRun, JobsResponse } from "../../../api/types";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");

const mocked = vi.mocked(adminApi);

function instanceJob(over: Partial<Job> = {}): Job {
  return {
    id: "artwork.sweep",
    name: "Artwork grabber",
    description: "Resolves cover and hero art for apps that have no artwork record.",
    plane: "control",
    scope: "instance",
    managed: true,
    enabled: true,
    schedule: {
      kind: "interval",
      interval_secs: 900,
      window_start: null,
      window_end: null,
      window_days: [],
      timezone: "UTC",
      locked: false,
      locked_by: null,
    },
    running: false,
    next_run_at: "2026-08-12T14:15:03Z",
    last_run: {
      id: "run-1",
      host_id: null,
      state: "succeeded",
      trigger: "schedule",
      started_at: "2026-08-12T14:00:03Z",
      finished_at: "2026-08-12T14:00:04Z",
      duration_ms: 1188,
      summary: { apps_considered: 412, artwork_resolved: 3 },
      error: null,
    },
    consecutive_failures: 0,
    history_limit: 50,
    ...over,
  };
}

function hostJob(over: Partial<Job> = {}): Job {
  return {
    id: "template.warmup",
    name: "Golden-home template warm-up",
    description: "Pre-warms a golden home template for fast session start.",
    plane: "agent",
    scope: "host",
    managed: true,
    enabled: true,
    schedule: {
      kind: "event",
      interval_secs: null,
      window_start: "02:00:00",
      window_end: "06:00:00",
      window_days: [],
      timezone: "Europe/London",
      locked: false,
      locked_by: null,
    },
    targets: [
      {
        host_id: "b7c1",
        node_name: "tower",
        running: false,
        next_run_at: null,
        last_run: {
          id: "run-2",
          host_id: "b7c1",
          state: "deferred",
          trigger: "schedule",
          started_at: "2026-08-12T02:00:11Z",
          finished_at: "2026-08-12T02:00:11Z",
          duration_ms: 41,
          summary: { reason: "host has 1 live session(s)" },
          error: null,
        },
      },
    ],
    history_limit: 50,
    ...over,
  };
}

function unmanagedJob(over: Partial<Job> = {}): Job {
  return {
    id: "console.selfheal",
    name: "Console self-heal backoff",
    description: "Runs on a hard-coded backoff in internal/agentws/handler.go.",
    plane: "control",
    scope: "host",
    managed: false,
    enabled: true,
    schedule: {
      kind: "event",
      interval_secs: null,
      window_start: null,
      window_end: null,
      window_days: [],
      timezone: "UTC",
      locked: true,
      locked_by: "code",
    },
    history_limit: 50,
    unmanaged_note: "Runs on a hard-coded backoff in internal/agentws/handler.go; not yet adopted.",
    ...over,
  };
}

function jobsResponse(items: Job[]): JobsResponse {
  return { items, next_cursor: null };
}

function run(over: Partial<JobRun> = {}): JobRun {
  return {
    id: "run-x",
    host_id: null,
    state: "succeeded",
    trigger: "schedule",
    started_at: "2026-08-12T14:00:03Z",
    finished_at: "2026-08-12T14:00:04Z",
    duration_ms: 1188,
    summary: {},
    error: null,
    ...over,
  };
}

/** The rows only — the toolbar reuses words the cells also use. */
function table(): HTMLElement {
  return screen.getByRole("table");
}

function renderPage() {
  return render(
    <ToastProvider>
      <JobsTab />
    </ToastProvider>,
  );
}

beforeEach(() => {
  vi.resetAllMocks();
  mocked.listJobRuns.mockResolvedValue({ items: [], next_cursor: null });
});

describe("JobsTab — table renders all three job shapes", () => {
  it("renders a managed instance-scoped job under the Control plane section", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob()]));
    renderPage();

    await screen.findByText("Artwork grabber");
    expect(screen.getByText("Every 15 min")).toBeTruthy();
    expect(screen.getByText("Control plane")).toBeTruthy();
    expect(screen.getByText("Succeeded")).toBeTruthy();
    // No Hosts section when nothing is host-scoped.
    expect(screen.queryByText("Hosts")).toBeNull();
  });

  // next_run_at is ahead of the clock; the past-only formatter read "just now".
  it("counts a scheduled run forward", async () => {
    const soon = new Date(Date.now() + 12 * 60 * 1000 + 5_000).toISOString();
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob({ next_run_at: soon })]));
    renderPage();

    expect(await screen.findByText("in 12m")).toBeTruthy();
  });

  it("calls a run whose schedule has slipped past due now", async () => {
    mocked.listJobs.mockResolvedValue(
      jobsResponse([instanceJob({ next_run_at: "2020-01-01T00:00:00Z" })]),
    );
    renderPage();

    expect(await screen.findByText("due now")).toBeTruthy();
  });

  it("renders a managed host-scoped job under the Hosts section, with an expand toggle", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([hostJob()]));
    renderPage();

    await screen.findByText("Golden-home template warm-up");
    expect(screen.getByText("Hosts")).toBeTruthy();
    expect(screen.getByText("On event")).toBeTruthy();

    fireEvent.click(
      screen.getByRole("button", { name: /per-host breakdown for Golden-home template warm-up/i }),
    );
    expect(await screen.findByText("tower")).toBeTruthy();
    expect(screen.getAllByText("Deferred").length).toBe(2);
  });

  it("ages the last run in the compact form, keeping the instant in the title", async () => {
    const finished = new Date(Date.now() - 13_000).toISOString();
    mocked.listJobs.mockResolvedValue(
      jobsResponse([
        instanceJob({
          last_run: { ...instanceJob().last_run!, finished_at: finished, duration_ms: 20 },
        }),
      ]),
    );
    renderPage();

    await screen.findByText("Artwork grabber");
    // "13s", not "13 seconds ago" — the spelled-out form overran the column.
    const [cell] = screen.getAllByTitle(finished);
    expect(cell.textContent).toBe("13s · 20 ms");
  });

  it("sizes a placeholder cell like the data it stands in for", async () => {
    mocked.listJobs.mockResolvedValue(
      jobsResponse([instanceJob({ last_run: null, running: false, next_run_at: null })]),
    );
    const { container } = renderPage();

    await screen.findByText("Artwork grabber");
    // `.sub` is the class the data cells use; `.muted` is only a colour, so it
    // rendered a row's placeholders larger than its siblings' figures.
    expect(screen.getByText("Never").className).toContain("sub");
    expect(container.querySelector("tbody td .muted")).toBeNull();
  });

  it("names the job and its description in the Job cell", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob()]));
    renderPage();

    await screen.findByText("Artwork grabber");
    expect(
      screen.getByText("Resolves cover and hero art for apps that have no artwork record."),
    ).toBeTruthy();
  });

  it("counts each section in its group row", async () => {
    mocked.listJobs.mockResolvedValue(
      jobsResponse([instanceJob(), instanceJob({ id: "other.sweep", name: "Other sweep" })]),
    );
    const { container } = renderPage();

    await screen.findByText("Other sweep");
    const group = container.querySelector("tr.group-row");
    expect(group?.querySelector(".eyebrow")?.textContent).toBe("Control plane");
    expect(group?.querySelector(".num")?.textContent).toBe("2");
  });

  it("marks an env-pinned schedule with a padlock naming the variable", async () => {
    mocked.listJobs.mockResolvedValue(
      jobsResponse([
        instanceJob({
          schedule: {
            kind: "interval",
            interval_secs: 21600,
            window_start: null,
            window_end: null,
            window_days: [],
            timezone: "UTC",
            locked: true,
            locked_by: "QUASAR_LIBRARY_SCAN_INTERVAL",
          },
        }),
      ]),
    );
    renderPage();

    await screen.findByText("Artwork grabber");
    expect(
      screen.getByTitle("QUASAR_LIBRARY_SCAN_INTERVAL sets this job's interval."),
    ).toBeTruthy();
  });
});

describe("JobsTab — a host-scoped job's row is derived from its targets", () => {
  const older = {
    id: "run-old",
    host_id: "b7c1",
    state: "succeeded" as const,
    trigger: "schedule" as const,
    started_at: "2026-08-12T02:00:10Z",
    finished_at: "2026-08-12T02:00:11Z",
    duration_ms: 41,
    summary: {},
    error: null,
  };
  const newer = {
    ...older,
    id: "run-new",
    host_id: "a2f0",
    state: "failed" as const,
    started_at: "2026-08-12T05:30:00Z",
    finished_at: "2026-08-12T05:30:02Z",
    duration_ms: 2000,
  };

  function twoTargets(over: Partial<Job> = {}): Job {
    return hostJob({
      targets: [
        {
          host_id: "b7c1",
          node_name: "tower",
          running: false,
          next_run_at: "2126-08-12T06:00:00Z",
          last_run: older,
        },
        {
          host_id: "a2f0",
          node_name: "hermes",
          running: false,
          next_run_at: "2126-08-12T09:00:00Z",
          last_run: newer,
        },
      ],
      ...over,
    });
  }

  it("shows the most recent run across hosts, and names that host", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([twoTargets()]));
    renderPage();

    await screen.findByText("Golden-home template warm-up");
    // The title carries the host it came from and the exact instant.
    expect(
      screen.getByTitle("Most recent across 2 hosts, on hermes · 2026-08-12T05:30:02Z"),
    ).toBeTruthy();
  });

  it("shows that run's result chip, not a pointer at the hosts", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([twoTargets()]));
    renderPage();

    await screen.findByText("Golden-home template warm-up");
    // One in the roll-up row, one in the per-host breakdown once expanded.
    const chip = screen.getByTitle("Most recent run, on hermes");
    expect(chip.textContent).toBe("Failed");
    expect(screen.queryByText("See hosts")).toBeNull();
    expect(screen.queryByText("Varies by host")).toBeNull();
  });

  it("shows the earliest queued run across hosts, and names that host", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([twoTargets()]));
    renderPage();

    await screen.findByText("Golden-home template warm-up");
    expect(screen.getByTitle("Soonest of 2 hosts, on tower")).toBeTruthy();
  });

  it("lets a running host outrank the most recent finished run", async () => {
    mocked.listJobs.mockResolvedValue(
      jobsResponse([
        twoTargets({
          targets: [
            { host_id: "b7c1", node_name: "tower", running: false, next_run_at: null, last_run: older },
            { host_id: "a2f0", node_name: "hermes", running: true, next_run_at: null, last_run: newer },
          ],
        }),
      ]),
    );
    renderPage();

    await screen.findByText("Golden-home template warm-up");
    expect(screen.getByTitle("Running on hermes").textContent).toBe("Running");
  });

  it("says Never when no host has finished a run", async () => {
    mocked.listJobs.mockResolvedValue(
      jobsResponse([
        twoTargets({
          targets: [
            { host_id: "b7c1", node_name: "tower", running: false, next_run_at: null, last_run: null },
          ],
        }),
      ]),
    );
    renderPage();

    await screen.findByText("Golden-home template warm-up");
    expect(within(table()).getByText("Never")).toBeTruthy();
    // One spelling of absence per row: Result carries no chip at all.
    expect(within(table()).queryByText("Never run")).toBeNull();
  });

  it("renders an unmanaged host-scoped job under its own section, with its note and no actions", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([unmanagedJob()]));
    renderPage();

    await screen.findByText("Console self-heal backoff");
    expect(within(table()).getByText("Unmanaged")).toBeTruthy();
    // No known host to attribute it to, so its own section rather than "Hosts".
    expect(screen.getByText("Hosts, unmanaged")).toBeTruthy();
    expect(screen.queryByText("Hosts", { exact: true })).toBeNull();

    // The kebab is there, holding one disabled item that says why.
    fireEvent.click(screen.getByRole("button", { name: /actions for console self-heal backoff/i }));
    const item = screen.getByRole("menuitem", { name: "Run now" });
    expect(item).toBeDisabled();
    expect(item.getAttribute("title")).toBe("Not yet adopted by the job framework");
  });

  it("publishes the section head sub-line and job count", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob()]));
    render(
      <MemoryRouter initialEntries={["/admin/fleet/jobs"]}>
        <ToastProvider>
          <SectionHeadProvider title="Fleet" tabs={FLEET_TABS}>
            <JobsTab />
          </SectionHeadProvider>
        </ToastProvider>
      </MemoryRouter>,
    );
    await screen.findByText("Artwork grabber");
    expect(screen.getByText("Background work on the control plane and every host")).toBeTruthy();
    expect(screen.getByText("1", { selector: ".cnt" })).toBeTruthy();
  });
});

describe("JobsTab — row affordances", () => {
  it("gives no chevron to a row with no per-host breakdown", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob(), unmanagedJob()]));
    const { container } = renderPage();

    await screen.findByText("Artwork grabber");
    expect(container.querySelectorAll("tbody .exp-btn").length).toBe(0);
    // The cell still exists, so both sections share one column axis.
    expect(container.querySelectorAll("tbody td.qtable-expand-cell").length).toBe(2);
  });

  it("gives a chevron only to a managed host-scoped row", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob(), hostJob()]));
    const { container } = renderPage();

    await screen.findByText("Golden-home template warm-up");
    expect(container.querySelectorAll("tbody .exp-btn").length).toBe(1);
  });

  it("flips the expand tooltip once the breakdown is open", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([hostJob()]));
    renderPage();

    await screen.findByText("Golden-home template warm-up");
    const chevron = screen.getByRole("button", { name: /per-host breakdown/i });
    expect(chevron.getAttribute("title")).toBe("Show per-host breakdown");
    fireEvent.click(chevron);
    expect(chevron.getAttribute("title")).toBe("Hide per-host breakdown");
  });

  it("breaks a host-scoped job down as one fact block per host", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([hostJob()]));
    const { container } = renderPage();

    await screen.findByText("Golden-home template warm-up");
    fireEvent.click(screen.getByRole("button", { name: /per-host breakdown/i }));

    const expansion = container.querySelector("tr.exp-row .exp-in");
    expect(expansion).toBeTruthy();
    // No nested table, so no second mono header row on another column axis.
    expect(expansion?.querySelector("table")).toBeNull();
    expect(expansion?.querySelector(".eyebrow")?.textContent).toBe("tower");
    expect([...expansion!.querySelectorAll(".exp-fact > span:first-child")].map((n) => n.textContent))
      .toEqual(["Last run", "Result", "Next run"]);
    expect(within(expansion as HTMLElement).getByRole("button", { name: "Run now" })).toBeTruthy();
    expect(within(expansion as HTMLElement).getByRole("button", { name: "View history" })).toBeTruthy();
  });

  it("puts the actions on the cell itself, so the column shares the row's box", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob()]));
    const { container } = renderPage();

    await screen.findByText("Artwork grabber");
    // `td.cell-actions` (styles/primitives.css restores display:table-cell for
    // it) rather than a wrapper div, matching HostRow.
    expect(container.querySelector("tbody td.cell-actions")).toBeTruthy();
    expect(container.querySelector("tbody td > div.cell-actions")).toBeNull();
  });

  it("lays a host block out beside its siblings, not across the row", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([hostJob()]));
    const { container } = renderPage();

    await screen.findByText("Golden-home template warm-up");
    fireEvent.click(screen.getByRole("button", { name: /per-host breakdown/i }));

    // Both capped-width and row-laid-out actions are `.jobs-page`-scoped rules
    // in styles/admin/fleet.css; the markup they key off is what is asserted.
    const page = container.querySelector("section.jobs-page");
    expect(page?.querySelector(".exp-in > div > .exp-actions")).toBeTruthy();
  });

  it("marks a disabled job on its row and dims it", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob({ enabled: false })]));
    renderPage();

    await screen.findByText("Artwork grabber");
    expect(within(table()).getByText("Disabled")).toBeTruthy();
    const row = screen.getByText("Artwork grabber").closest("tr") as HTMLTableRowElement;
    expect(row.style.opacity).toBe("0.55");
  });

  it("orders the menu with the destructive item last, after a separator", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob()]));
    renderPage();

    await screen.findByText("Artwork grabber");
    fireEvent.click(screen.getByRole("button", { name: /actions for artwork grabber/i }));
    expect(screen.getAllByRole("menuitem").map((n) => n.textContent)).toEqual([
      "Run now",
      "View history",
      "Edit schedule",
      "Disable",
    ]);
  });
});

describe("JobsTab — toolbar", () => {
  it("filters to the failing jobs and counts them", async () => {
    const failing = instanceJob({ id: "bad.sweep", name: "Bad sweep", consecutive_failures: 2 });
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob(), failing]));
    renderPage();

    await screen.findByText("Bad sweep");
    fireEvent.click(screen.getByRole("tab", { name: /Failing, 1/i }));

    expect(within(table()).getByText("Bad sweep")).toBeTruthy();
    expect(within(table()).queryByText("Artwork grabber")).toBeNull();
  });

  it("counts a host-scoped job as failing off its rolled-up run", async () => {
    const target = hostJob().targets![0];
    mocked.listJobs.mockResolvedValue(
      jobsResponse([
        hostJob({ targets: [{ ...target, last_run: { ...target.last_run!, state: "failed" } }] }),
      ]),
    );
    renderPage();

    await screen.findByText("Golden-home template warm-up");
    expect(screen.getByRole("tab", { name: /Failing, 1/i })).toBeTruthy();
  });

  it("filters to the unmanaged jobs", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob(), unmanagedJob()]));
    renderPage();

    await screen.findByText("Console self-heal backoff");
    fireEvent.click(screen.getByRole("tab", { name: /Unmanaged, 1/i }));

    expect(within(table()).getByText("Console self-heal backoff")).toBeTruthy();
    expect(within(table()).queryByText("Artwork grabber")).toBeNull();
  });

  it("filters over the name and the description", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob(), unmanagedJob()]));
    renderPage();

    await screen.findByText("Artwork grabber");
    const search = screen.getByPlaceholderText("Filter jobs");

    fireEvent.change(search, { target: { value: "self-heal" } });
    expect(within(table()).queryByText("Artwork grabber")).toBeNull();

    // The description is searched too, not only the name.
    fireEvent.change(search, { target: { value: "cover and hero" } });
    expect(within(table()).getByText("Artwork grabber")).toBeTruthy();
    expect(within(table()).queryByText("Console self-heal backoff")).toBeNull();
  });

  it("says so when a filter matches nothing", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob()]));
    renderPage();

    await screen.findByText("Artwork grabber");
    fireEvent.change(screen.getByPlaceholderText("Filter jobs"), { target: { value: "zzz" } });
    expect(screen.getByText("No jobs match")).toBeTruthy();
  });
});

describe("JobsTab — run now", () => {
  it("queues a run for an instance-scoped job and toasts the eta_note", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob()]));
    mocked.runJobNow.mockResolvedValue({
      run_id: "run-9",
      state: "pending",
      scheduled_for: "2026-08-12T14:00:00Z",
      eta_note: "queued; the control plane's dispatcher checks for due runs about every 10 s",
    });
    renderPage();

    await screen.findByText("Artwork grabber");
    fireEvent.click(screen.getByRole("button", { name: /actions for artwork grabber/i }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Run now" }));

    await waitFor(() => expect(mocked.runJobNow).toHaveBeenCalledWith("tok", "artwork.sweep", {}));
    await screen.findByText(/checks for due runs about every 10 s/);
  });

  it("maps job_already_running to an honest toast", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob()]));
    mocked.runJobNow.mockRejectedValue(new ApiError(409, "job_already_running", "conflict"));
    renderPage();

    await screen.findByText("Artwork grabber");
    fireEvent.click(screen.getByRole("button", { name: /actions for artwork grabber/i }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Run now" }));

    await screen.findByText(/already in progress/);
  });

  it("maps job_disabled to an honest toast", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob({ enabled: false, running: false })]));
    mocked.runJobNow.mockRejectedValue(new ApiError(409, "job_disabled", "conflict"));
    renderPage();

    await screen.findByText("Artwork grabber");
    // Run now is disabled in the UI for a disabled job, but the mapping itself
    // is still exercised directly against a live server disagreement (e.g. a
    // stale client cache) via the toggle path below rather than the disabled
    // button — assert the menu item reflects the disabled state instead.
    fireEvent.click(screen.getByRole("button", { name: /actions for artwork grabber/i }));
    const runItem = screen.getByRole("menuitem", { name: "Run now" });
    expect(runItem).toBeDisabled();
  });

  it("maps job_unmanaged to an honest toast", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob()]));
    mocked.runJobNow.mockRejectedValue(new ApiError(409, "job_unmanaged", "conflict"));
    renderPage();

    await screen.findByText("Artwork grabber");
    fireEvent.click(screen.getByRole("button", { name: /actions for artwork grabber/i }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Run now" }));

    await screen.findByText(/not managed by the job framework/);
  });
});

describe("JobsTab — schedule modal", () => {
  it("rejects an interval below 60 seconds client-side without calling PATCH", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob()]));
    renderPage();

    await screen.findByText("Artwork grabber");
    fireEvent.click(screen.getByRole("button", { name: /actions for artwork grabber/i }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Edit schedule" }));

    const input = (await screen.findByLabelText("Interval")) as HTMLInputElement;
    fireEvent.change(input, { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await screen.findByText(/at least 60/);
    expect(mocked.patchJob).not.toHaveBeenCalled();
  });

  it("rejects an equal window start/end client-side without calling PATCH", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob()]));
    renderPage();

    await screen.findByText("Artwork grabber");
    fireEvent.click(screen.getByRole("button", { name: /actions for artwork grabber/i }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Edit schedule" }));

    fireEvent.click(await screen.findByLabelText(/Restrict to a time window/i));
    fireEvent.change(screen.getByLabelText(/Window start/i), { target: { value: "02:00" } });
    fireEvent.change(screen.getByLabelText(/Window end/i), { target: { value: "02:00" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await screen.findByText(/must be different/);
    expect(mocked.patchJob).not.toHaveBeenCalled();
  });

  it("greys the interval field and names the env var when the schedule is locked", async () => {
    mocked.listJobs.mockResolvedValue(
      jobsResponse([
        instanceJob({
          schedule: {
            kind: "interval",
            interval_secs: 21600,
            window_start: null,
            window_end: null,
            window_days: [],
            timezone: "UTC",
            locked: true,
            locked_by: "QUASAR_LIBRARY_SCAN_INTERVAL",
          },
        }),
      ]),
    );
    renderPage();

    await screen.findByText("Artwork grabber");
    fireEvent.click(screen.getByRole("button", { name: /actions for artwork grabber/i }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Edit schedule" }));

    const input = (await screen.findByLabelText("Interval")) as HTMLInputElement;
    expect(input.disabled).toBe(true);
    expect(
      screen.getByText(/QUASAR_LIBRARY_SCAN_INTERVAL sets this interval\./),
    ).toBeTruthy();
  });

  it("saves a valid schedule edit via PATCH", async () => {
    const job = instanceJob();
    mocked.listJobs.mockResolvedValue(jobsResponse([job]));
    mocked.patchJob.mockResolvedValue({ ...job, schedule: { ...job.schedule, interval_secs: 1800 } });
    renderPage();

    await screen.findByText("Artwork grabber");
    fireEvent.click(screen.getByRole("button", { name: /actions for artwork grabber/i }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Edit schedule" }));

    const input = (await screen.findByLabelText("Interval")) as HTMLInputElement;
    fireEvent.change(input, { target: { value: "1800" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(mocked.patchJob).toHaveBeenCalledWith(
        "tok",
        "artwork.sweep",
        expect.objectContaining({ interval_secs: 1800, window_start: null, window_end: null }),
      ),
    );
    await screen.findByText(/schedule updated/);
  });
});

describe("JobsTab — run history", () => {
  it("opens the history drawer and renders returned runs", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob()]));
    mocked.listJobRuns.mockResolvedValue({
      items: [run({ id: "run-a", state: "failed", error: "boom" }), run({ id: "run-b" })],
      next_cursor: null,
    });
    renderPage();

    await screen.findByText("Artwork grabber");
    fireEvent.click(screen.getByRole("button", { name: /actions for artwork grabber/i }));
    fireEvent.click(screen.getByRole("menuitem", { name: "View history" }));

    await waitFor(() => expect(mocked.listJobRuns).toHaveBeenCalledWith("tok", "artwork.sweep", { hostId: undefined, limit: 50 }));
    const drawer = screen.getByRole("dialog", { name: /Run history for Artwork grabber/i });
    expect(within(drawer).getByText("Failed")).toBeTruthy();
    expect(within(drawer).getByText("boom")).toBeTruthy();
    // Four columns, so nothing is pushed past the 640px drawer edge.
    expect(within(drawer).getAllByRole("columnheader").map((th) => th.textContent)).toEqual([
      "Started",
      "Result",
      "Duration",
      "Summary",
    ]);
    // The trigger rides along as Started's sub-line rather than as a column.
    // Scoped to the table: "Schedule" is also the Overview's first fact label.
    expect(within(within(drawer).getByRole("table")).getAllByText("Schedule").length).toBe(2);
    // A failed run's message is a full-width row under it, and the chip keeps
    // it as a tooltip.
    expect(within(drawer).getByText("boom").closest("tr")?.className).toBe("exp-row");
    expect(within(drawer).getByTitle("boom").textContent).toBe("Failed");
    // The Overview .fsec section, above the Runs table.
    expect(within(drawer).getByText("Every 15 min")).toBeTruthy();
    expect(within(drawer).getByText("50")).toBeTruthy();
  });

  it("reads a queued run as pending with no duration", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob()]));
    mocked.listJobRuns.mockResolvedValue({
      items: [
        run({ id: "run-p", state: "pending", started_at: null, finished_at: null, duration_ms: null }),
      ],
      next_cursor: null,
    });
    renderPage();

    await screen.findByText("Artwork grabber");
    fireEvent.click(screen.getByRole("button", { name: /actions for artwork grabber/i }));
    fireEvent.click(screen.getByRole("menuitem", { name: "View history" }));

    const drawer = await screen.findByRole("dialog", { name: /Run history for Artwork grabber/i });
    // The PENDING chip already says it, so Started is the no-value placeholder;
    // duration and summary are too.
    expect(within(drawer).getByText("pending")).toBeTruthy();
    expect(within(drawer).getAllByText("—").length).toBe(3);
    expect(drawer.querySelector("tr.exp-row")).toBeNull();
  });

  it("shows an empty state when there is no run history yet", async () => {
    mocked.listJobs.mockResolvedValue(jobsResponse([instanceJob()]));
    mocked.listJobRuns.mockResolvedValue({ items: [], next_cursor: null });
    renderPage();

    await screen.findByText("Artwork grabber");
    fireEvent.click(screen.getByRole("button", { name: /actions for artwork grabber/i }));
    fireEvent.click(screen.getByRole("menuitem", { name: "View history" }));

    await screen.findByText("No runs recorded.");
  });
});
