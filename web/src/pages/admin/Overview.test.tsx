/**
 * The Overview against one fixed fleet (spec §5.6, mock §A.1).
 *
 * The shared polls are replaced by a fixed `useFleetContext` value: this page
 * must not open its own hosts/sessions poll, and stubbing the context is how
 * that stays true — if the page ever fetches either itself, the fixtures here
 * stop reaching it and these assertions fail.
 */

import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../api/admin");

const reload = vi.fn().mockResolvedValue(undefined);
let fleet: FleetContextValue;
vi.mock("../../lib/fleet/FleetContext", () => ({
  useFleetContext: () => fleet,
}));

import * as adminApi from "../../api/admin";
import type { AdminActivityItem } from "../../api/admin";
import type { AdminSession, AdminUser, Host } from "../../api/types";
import type { FleetContextValue } from "../../lib/fleet/FleetContext";
import { Overview } from "./Overview";

const mocked = vi.mocked(adminApi);
const NOW = Date.parse("2026-08-29T12:00:00Z");

// ── fixtures (shaped like the mock's data.js) ────────────────────────────────

function host(over: Partial<Host> = {}): Host {
  return {
    id: "h1",
    node_name: "quasar-node-1",
    status: "online",
    capacity_detection: "ok",
    capacity_reason: null,
    readiness: [],
    readiness_reported_at: null,
    storage: [],
    last_heartbeat_at: new Date(NOW - 4000).toISOString(),
    capacity: {
      slots_total: 3,
      slots_used: 2,
      vram_mb_total: 32768,
      vram_mb_used: 21504,
      active_sessions: 2,
      gpu_count: 1,
    },
    ...over,
  } as Host;
}

function session(over: Partial<AdminSession> = {}): AdminSession {
  return {
    id: "s1",
    state: "running",
    host_id: "h1",
    app_name: "Cyberpunk 2077",
    username: "mara.k",
    host_name: "quasar-node-2",
    started_at: new Date(NOW - 72 * 60 * 1000).toISOString(),
    created_at: new Date(NOW - 72 * 60 * 1000).toISOString(),
    ended_at: null,
    latest_metrics: {
      agent: { metrics: { bitrate_kbps: 38200 } },
      browser: { metrics: { fps: 118, rtt_ms: 14 } },
    },
    ...over,
  } as AdminSession;
}

function user(over: Partial<AdminUser> = {}): AdminUser {
  return {
    id: "u1",
    username: "mara.k",
    email: "mara@studio.io",
    role: "user",
    disabled: false,
    max_concurrent_sessions: 2,
    created_at: "2026-01-01T00:00:00Z",
    last_seen_at: new Date(NOW - 60_000).toISOString(),
    active_session_count: 1,
    ...over,
  } as AdminUser;
}

function activity(id: number, over: Partial<AdminActivityItem> = {}): AdminActivityItem {
  return {
    id,
    actor_user_id: "u1",
    actor_username: "salty2011",
    action: "host.drain",
    target_type: "host",
    target_id: "a4c7f210-1111-2222-3333-444455556666",
    details: {},
    created_at: "2026-08-29T11:02:11Z",
    severity: "warn",
    ...over,
  };
}

function setFleet(over: Partial<FleetContextValue> = {}) {
  fleet = {
    hosts: [],
    sessions: [],
    loading: false,
    lastFetchedAt: NOW - 3000,
    errors: { hosts: null, sessions: null },
    reload,
    ...over,
  };
}

function renderOverview() {
  return render(
    <MemoryRouter>
      <Overview />
    </MemoryRouter>,
  );
}

/** The card whose panel title is `title`. */
function card(title: string): HTMLElement {
  return screen.getByText(title, { selector: ".panel-title" }).closest(".card") as HTMLElement;
}

/** The KPI card under `eyebrow`. */
function kpi(eyebrow: string): HTMLElement {
  return screen.getByText(eyebrow, { selector: ".eyebrow" }).closest("button") as HTMLElement;
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.setSystemTime(NOW);
  setFleet();
  mocked.listUsers.mockResolvedValue({ items: [], next_cursor: null } as never);
  mocked.listInvites.mockResolvedValue({ invites: [] } as never);
  mocked.listAllSessions.mockResolvedValue({ items: [], next_cursor: null } as never);
  mocked.listAdminActivity.mockResolvedValue({ items: [], next_cursor: null } as never);
});

describe("Overview KPIs", () => {
  it("shows the four numbers and their meta lines", async () => {
    setFleet({
      hosts: [
        host(),
        host({ id: "h2", node_name: "quasar-node-2" }),
        host({
          id: "h3",
          node_name: "quasar-node-3",
          status: "offline",
          last_heartbeat_at: new Date(NOW - 14 * 60 * 1000).toISOString(),
        }),
      ],
      sessions: [
        session(),
        session({ id: "s2", health_state: "network_degrading", health_reason: "68 ms latency" }),
      ],
    });
    mocked.listUsers.mockResolvedValue({
      items: [user(), user({ id: "u2", username: "devon", active_session_count: 0 })],
      next_cursor: null,
    } as never);
    mocked.listInvites.mockResolvedValue({
      invites: [{ id: "i1" }, { id: "i2" }],
    } as never);

    renderOverview();

    // Live sessions: 2 live, 1 of them degraded, 76.4 Mb/s of agent bitrate.
    const live = kpi("Live sessions");
    expect(within(live).getByText("2")).toBeTruthy();
    expect(within(live).getByText("1 degraded · 76.4 Mb/s out")).toBeTruthy();

    // GPU slots: the offline host's slots are not schedulable, so 2 hosts.
    const slots = kpi("GPU slots");
    expect(within(slots).getByText("/ 6")).toBeTruthy();
    expect(within(slots).getByText("2 free across 2 hosts")).toBeTruthy();

    const hosts = kpi("Hosts");
    expect(within(hosts).getByText("/ 3")).toBeTruthy();
    expect(within(hosts).getByText("1 need attention")).toBeTruthy();

    await waitFor(() =>
      expect(screen.getByText("1 streaming now · 2 invites pending")).toBeTruthy(),
    );
    const users = kpi("Users");
    expect(within(users).getByText("active")).toBeTruthy();
  });

  it("says all healthy when no host needs attention", () => {
    setFleet({ hosts: [host()] });
    renderOverview();
    expect(screen.getByText("all healthy")).toBeTruthy();
  });
});

describe("Overview live sessions", () => {
  it("renders the browser fps, the browser rtt and the agent bitrate", () => {
    setFleet({ sessions: [session()] });
    renderOverview();

    const row = card("Live sessions").querySelector("tbody tr") as HTMLElement;
    expect(within(row).getByText("Cyberpunk 2077")).toBeTruthy();
    expect(within(row).getByText("mara.k · 1h 12m")).toBeTruthy();
    // The host column strips the shared `quasar-` prefix.
    expect(within(row).getByText("node-2")).toBeTruthy();
    expect(within(row).getByText("118")).toBeTruthy();
    expect(within(row).getByText("14 ms")).toBeTruthy();
    expect(within(row).getByText("38.2 Mb/s")).toBeTruthy();
  });

  it("colours a latency over 50 ms as a fault, and leaves a healthy one alone", () => {
    setFleet({
      sessions: [
        session(),
        session({
          id: "s2",
          latest_metrics: { browser: { metrics: { fps: 52, rtt_ms: 68 } } },
        } as Partial<AdminSession>),
      ],
    });
    renderOverview();

    expect(screen.getByText("68 ms").getAttribute("style")).toContain("--danger-text");
    expect(screen.getByText("14 ms").getAttribute("style") ?? "").not.toContain("--danger-text");
  });

  it("shows a dash for a session that has reported no metrics yet", () => {
    setFleet({ sessions: [session({ id: "s9", latest_metrics: undefined })] });
    renderOverview();
    const row = card("Live sessions").querySelector("tbody tr") as HTMLElement;
    expect(within(row).getAllByText("—")).toHaveLength(3);
  });

  it("has an empty state when nothing is streaming", () => {
    renderOverview();
    expect(screen.getByText("No live sessions")).toBeTruthy();
    expect(screen.getByText("Sessions users start appear here while they run.")).toBeTruthy();
  });
});

describe("Overview needs attention", () => {
  it("lists criticals before warnings, each with its own call to action", async () => {
    setFleet({
      hosts: [
        host(),
        host({
          id: "h2",
          node_name: "quasar-node-6",
          status: "offline",
          last_heartbeat_at: new Date(NOW - 14 * 60 * 1000).toISOString(),
        }),
      ],
      sessions: [session({ health_state: "network_degrading", health_reason: "68 ms latency" })],
    });
    mocked.listAllSessions.mockResolvedValue({
      items: [
        session({
          id: "sf",
          state: "failed",
          app_name: "Helldivers 2",
          username: "tobi",
          failure_code: "encoder_init_failed",
          ended_at: new Date(NOW - 4 * 60 * 1000).toISOString(),
        }),
      ],
      next_cursor: null,
    } as never);

    renderOverview();

    const attention = card("Needs attention");
    await waitFor(() =>
      expect(within(attention).getByText(/Helldivers 2 failed for tobi/)).toBeTruthy(),
    );

    const titles = Array.from(attention.querySelectorAll(".alert-title")).map((n) => n.textContent);
    expect(titles).toEqual([
      "quasar-node-6 offline",
      "Helldivers 2 failed for tobi",
      "1 session degraded",
    ]);
    const ctas = Array.from(attention.querySelectorAll(".alert-act .btn")).map((n) => n.textContent);
    expect(ctas).toEqual(["Open host", "Open session", "Open session"]);
    expect(within(attention).getByText("1 critical")).toBeTruthy();
    expect(within(attention).getByText("2 warning")).toBeTruthy();
  });

  it("does not claim the fleet is healthy while a source is still loading", () => {
    setFleet({ loading: true, lastFetchedAt: null });
    renderOverview();
    expect(screen.queryByText("Nothing needs attention")).toBeNull();
    expect(within(card("Needs attention")).getByRole("status")).toBeTruthy();
  });

  it("does not claim the fleet is healthy when a source failed", async () => {
    setFleet({ errors: { hosts: "could not load fleet", sessions: null } });
    renderOverview();
    await waitFor(() =>
      expect(within(card("Needs attention")).getByRole("alert").textContent).toBe(
        "could not load fleet",
      ),
    );
    expect(screen.queryByText("Nothing needs attention")).toBeNull();
  });

  it("has an empty state and still offers the audit log, once both sources are in", async () => {
    setFleet({ hosts: [host()] });
    renderOverview();
    const attention = card("Needs attention");
    await waitFor(() => expect(within(attention).getByText("Nothing needs attention")).toBeTruthy());
    expect(within(attention).getByText("Hosts, sessions and storage are healthy.")).toBeTruthy();
    expect(within(attention).getByRole("button", { name: /Open audit log/ })).toBeTruthy();
  });
});

describe("Overview fleet capacity", () => {
  it("renders each host's slots and vram against the fleet chip", () => {
    setFleet({ hosts: [host(), host({ id: "h2", node_name: "quasar-node-2", capacity: null })] });
    renderOverview();

    const capacity = card("Fleet capacity");
    expect(within(capacity).getByText("2/3 slots")).toBeTruthy();
    expect(within(capacity).getByText("2/3")).toBeTruthy();
    expect(within(capacity).getByText("21 GB/32 GB")).toBeTruthy();
    expect(within(capacity).getByText("1 GPU")).toBeTruthy();
    // Compact age, for a narrow right-aligned column (§A.4).
    expect(within(capacity).getAllByText("4s")).toHaveLength(2);
    // A host with no capacity report draws no bar rather than an empty one.
    expect(within(capacity).getByText("No GPUs reported")).toBeTruthy();
    expect(capacity.querySelectorAll(".bar-row.unknown")).toHaveLength(2);
  });
});

describe("Overview host state chip", () => {
  it("calls an online host with a failed capacity report degraded, not online", () => {
    setFleet({
      hosts: [
        host(),
        host({
          id: "h2",
          node_name: "quasar-node-5",
          capacity_detection: "failed",
          capacity_reason: "GPU enumeration returned 0 devices",
          capacity: null,
        }),
        host({ id: "h3", node_name: "quasar-node-4", status: "draining" }),
        host({ id: "h4", node_name: "quasar-node-6", status: "offline", capacity: null }),
      ],
    });
    renderOverview();

    const rows = card("Fleet capacity").querySelectorAll("tbody tr");
    const chip = (i: number) => rows[i].querySelector(".chip");
    expect(chip(0)?.textContent).toBe("online");
    expect(chip(0)?.className).toContain("chip-success");
    expect(chip(1)?.textContent).toBe("degraded");
    expect(chip(1)?.className).toContain("chip-danger");
    // Draining is an operator's own doing, not a fault.
    expect(chip(2)?.textContent).toBe("draining");
    expect(chip(2)?.className).toContain("chip-warning");
    expect(chip(3)?.textContent).toBe("offline");
    expect(chip(3)?.className).toContain("chip-danger");
  });
});

describe("Overview recent activity", () => {
  it("shows the six entries the audit tail returned", async () => {
    mocked.listAdminActivity.mockResolvedValue({
      items: [
        activity(1),
        activity(2, { action: "session.failed", severity: "err", actor_username: null }),
        activity(3),
        activity(4),
        activity(5),
        activity(6),
      ],
      next_cursor: null,
    } as never);

    renderOverview();

    const recent = card("Recent activity");
    await waitFor(() => expect(recent.querySelectorAll(".act-row")).toHaveLength(6));
    expect(mocked.listAdminActivity).toHaveBeenCalledWith("tok", { limit: 6 });
    // A system action has no actor row to join.
    expect(within(recent).getByText("system")).toBeTruthy();
    expect(within(recent).getByText("session.failed").getAttribute("style")).toContain(
      "--danger-text",
    );
    expect(within(recent).getAllByText("host a4c7f210")).toHaveLength(6);
  });
});

describe("Overview head", () => {
  it("dates the page from the shared poll and refreshes both sources", async () => {
    renderOverview();
    await waitFor(() => expect(mocked.listUsers).toHaveBeenCalledTimes(1));
    expect(screen.getByText("Live fleet state · updated 3 seconds ago")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));

    expect(reload).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(mocked.listUsers).toHaveBeenCalledTimes(2));
  });
});
