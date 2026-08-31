import { describe, expect, it } from "vitest";
import { deriveAlerts, needsAttention } from "./deriveAlerts";

const online = {
  id: "h1",
  node_name: "quasar-node-1",
  status: "online",
  capacity_detection: "ok",
  readiness: [],
  storage: [
    { label: "data", path: "/var/lib/quasar", total_mb: 240000, available_mb: 12000 },
  ],
  last_heartbeat_at: "2026-08-28T10:00:00Z",
};
const offline = { ...online, id: "h6", node_name: "quasar-node-6", status: "offline", storage: [] };
const degraded = {
  ...online,
  id: "h5",
  node_name: "quasar-node-5",
  capacity_detection: "failed",
  capacity_reason: "GPU enumeration returned 0 devices",
  storage: [],
};
const draining = { ...online, id: "h4", node_name: "quasar-node-4", status: "draining", storage: [] };

const sessions = [
  {
    id: "s1",
    app_name: "Helldivers 2",
    username: "tobi",
    state: "running",
    health_state: "degraded",
    health_reason: "68 ms latency",
  },
];

const NOW = new Date("2026-08-28T10:14:00Z");

describe("deriveAlerts", () => {
  it("orders critical before warning and names a CTA route", () => {
    const a = deriveAlerts([online, offline, degraded], sessions, NOW);
    // The offline host leads its severity because it is the only critical that
    // knows when it went wrong; a capacity report is a level with no onset.
    expect(a.map((x) => [x.severity, x.title])).toEqual([
      ["critical", "quasar-node-6 offline"],
      ["critical", "quasar-node-5 is degraded"],
      ["warning", "1 session degraded"],
      ["warning", "Storage below 10% on quasar-node-1"],
    ]);
    expect(a[0]).toMatchObject({
      body: "No heartbeat for 14 minutes · 0 sessions affected",
      cta: "Open host",
      to: "/admin/fleet/hosts/h6",
    });
    expect(a[3]).toMatchObject({
      body: "11.7 GB free of 234 GB · new homes will fail to provision",
      to: "/admin/fleet/storage",
      cta: "Open storage",
    });
  });

  it("ages an alert from its onset", () => {
    const a = deriveAlerts([offline], [], NOW);
    expect(a[0].since).toBe("2026-08-28T10:00:00.000Z");
    expect(a[0].age).toBe("14m");
  });

  it("shows a dash rather than a made-up age when there is no onset", () => {
    for (const a of deriveAlerts([degraded, online], [], NOW)) {
      expect(a.since).toBeNull();
      expect(a.age).toBe("—");
    }
  });

  it("counts the sessions an offline host took down with it", () => {
    const stranded = [{ id: "s2", state: "running", host_id: "h6", app_name: "Hades II" }];
    expect(deriveAlerts([offline], stranded, NOW)[0].body).toBe(
      "No heartbeat for 14 minutes · 1 session affected",
    );
  });

  it("raises a failed readiness check with the check's own summary", () => {
    const host = {
      ...online,
      storage: [],
      readiness: [
        { id: "vaapi", status: "pass", summary: "VA-API is available" },
        { id: "nvidia_egl", status: "fail", summary: "The NVIDIA EGL vendor file is missing" },
      ],
    };
    expect(deriveAlerts([{ ...host, readiness_reported_at: "2026-08-28T10:11:00Z" }], [], NOW)).toEqual([
      expect.objectContaining({
        severity: "critical",
        age: "3m",
        title: "quasar-node-1 failed a readiness check",
        body: "The NVIDIA EGL vendor file is missing",
        cta: "Open host",
        to: "/admin/fleet/hosts/h1",
      }),
    ]);
  });

  it("raises a recently failed session, and forgets it after fifteen minutes", () => {
    const failed = {
      id: "s3",
      app_name: "Talos",
      username: "kim",
      state: "failed",
      failure_code: "app_exited_early",
      ended_at: "2026-08-28T10:05:00Z",
    };
    expect(deriveAlerts([], [failed], NOW)).toEqual([
      expect.objectContaining({
        severity: "warning",
        title: "Talos failed for kim",
        body: "app_exited_early",
        cta: "Open session",
        to: "/admin/sessions/s3",
      }),
    ]);
    const stale = { ...failed, ended_at: "2026-08-28T09:30:00Z" };
    expect(deriveAlerts([], [stale], NOW)).toEqual([]);
  });

  it("leaves a draining host alone — that is an operator action, not a fault", () => {
    expect(deriveAlerts([draining], [], NOW)).toEqual([]);
  });

  it("ages a multi-session warning from the newest of them", () => {
    const two = [
      { id: "s8", app_name: "Old", username: "kim", state: "running", health_state: "degraded", started_at: "2026-08-28T09:00:00Z" },
      { id: "s9", app_name: "New", username: "ana", state: "running", health_state: "degraded", started_at: "2026-08-28T10:12:00Z" },
    ];
    expect(deriveAlerts([], two, NOW)[0]).toMatchObject({
      since: "2026-08-28T10:12:00.000Z",
      age: "2m",
      body: "New · ana · degraded and 1 more",
    });
  });

  it("sorts newest first inside a severity", () => {
    const older = { ...offline, id: "h7", node_name: "quasar-node-7", last_heartbeat_at: "2026-08-28T09:00:00Z" };
    const titles = deriveAlerts([older, offline], [], NOW).map((a) => a.title);
    expect(titles).toEqual(["quasar-node-6 offline", "quasar-node-7 offline"]);
  });

  it("names several degraded sessions once, pointing at the list", () => {
    const two = [
      ...sessions,
      { id: "s9", app_name: "Hades II", username: "ana", state: "running", health_state: "network_degrading" },
    ];
    expect(deriveAlerts([], two, NOW)[0]).toMatchObject({
      severity: "warning",
      title: "2 sessions degraded",
      cta: "Open sessions",
      to: "/admin/sessions",
    });
  });

  it("says nothing about a healthy fleet", () => {
    const healthy = { ...online, storage: [{ label: "data", path: "/var/lib/quasar", total_mb: 240000, available_mb: 120000 }] };
    expect(deriveAlerts([healthy], [{ id: "s1", state: "running", health_state: "healthy" }], NOW)).toEqual([]);
  });
});

describe("needsAttention", () => {
  it("is true for anything off-line, uncertain about its capacity, or failing a check", () => {
    expect(needsAttention(online)).toBe(false);
    expect(needsAttention(offline)).toBe(true);
    expect(needsAttention(degraded)).toBe(true);
    // Draining is an operator action: no alert row, and no fault marker either.
    expect(needsAttention(draining)).toBe(false);
    expect(
      needsAttention({ ...online, readiness: [{ id: "x", status: "fail", summary: "no" }] }),
    ).toBe(true);
    expect(needsAttention({ ...online, readiness: null })).toBe(false);
  });
});
