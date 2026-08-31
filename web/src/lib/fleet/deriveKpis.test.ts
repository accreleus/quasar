import { describe, expect, it } from "vitest";
import { deriveKpis } from "./deriveKpis";

const NOW = new Date("2026-08-28T10:00:00Z");

describe("deriveKpis", () => {
  it("sums capacity and counts states", () => {
    const hosts = [
      {
        status: "online",
        capacity: {
          slots_total: 4,
          slots_used: 3,
          vram_mb_total: 1,
          vram_mb_used: 0,
          active_sessions: 3,
          gpu_count: 1,
        },
        capacity_detection: "ok",
        readiness: [],
      },
      { status: "offline", capacity: null, capacity_detection: "ok", readiness: [] },
    ];
    const sessions = [
      {
        state: "running",
        health_state: "degraded",
        latest_metrics: { agent: { metrics: { bitrate_kbps: 38200 } } },
      },
      { state: "running", latest_metrics: { agent: { metrics: { bitrate_kbps: 22000 } } } },
    ];
    const users = [
      { disabled: false, last_seen_at: "2026-08-28T09:00:00Z", active_session_count: 1 },
      { disabled: false, last_seen_at: null, active_session_count: 0 },
    ];
    const k = deriveKpis({ hosts, sessions, users, pendingInvites: 2, now: NOW });
    expect(k.sessions).toEqual({ live: 2, degraded: 1, mbpsOut: 60.2 });
    expect(k.slots).toEqual({ used: 3, total: 4, free: 1, onlineHosts: 1, capacityHosts: 1 });
    expect(k.hosts).toEqual({ online: 1, total: 2, attention: 1 });
    expect(k.users).toEqual({ active: 1, streaming: 1, pendingInvites: 2 });
  });

  it("reads an empty fleet as zero, not as unknown", () => {
    expect(deriveKpis({ hosts: [], sessions: [], users: [], pendingInvites: 0, now: NOW })).toEqual({
      sessions: { live: 0, degraded: 0, mbpsOut: 0 },
      slots: { used: 0, total: 0, free: 0, onlineHosts: 0, capacityHosts: 0 },
      hosts: { online: 0, total: 0, attention: 0 },
      users: { active: 0, streaming: 0, pendingInvites: 0 },
    });
  });

  it("counts only non-terminal sessions as live", () => {
    const sessions = [
      { state: "running" },
      { state: "starting" },
      { state: "ended" },
      { state: "failed" },
    ];
    expect(deriveKpis({ hosts: [], sessions, users: [], pendingInvites: 0, now: NOW }).sessions.live)
      .toBe(2);
  });

  it("keeps an offline host's slots out of the fleet total", () => {
    const capacity = {
      slots_total: 4,
      slots_used: 1,
      vram_mb_total: 0,
      vram_mb_used: 0,
      active_sessions: 1,
      gpu_count: 1,
    };
    const hosts = [
      { status: "draining", capacity, capacity_detection: "ok", readiness: [] },
      { status: "offline", capacity, capacity_detection: "ok", readiness: [] },
    ];
    expect(deriveKpis({ hosts, sessions: [], users: [], pendingInvites: 0, now: NOW }).slots).toEqual(
      { used: 1, total: 4, free: 3, onlineHosts: 0, capacityHosts: 1 },
    );
  });

  it("ignores a disabled account in both user counts", () => {
    const users = [
      { disabled: false, last_seen_at: "2026-08-27T10:30:00Z", active_session_count: 0 },
      { disabled: false, last_seen_at: "2026-08-27T09:30:00Z", active_session_count: 0 },
      { disabled: true, last_seen_at: "2026-08-28T09:59:00Z", active_session_count: 2 },
    ];
    const k = deriveKpis({ hosts: [], sessions: [], users, pendingInvites: 0, now: NOW });
    expect(k.users).toEqual({ active: 1, streaming: 0, pendingInvites: 0 });
  });

  it("does not count a drained host as needing attention", () => {
    const hosts = [
      { status: "draining", capacity: null, capacity_detection: "ok", readiness: [] },
      { status: "offline", capacity: null, capacity_detection: "ok", readiness: [] },
    ];
    expect(deriveKpis({ hosts, sessions: [], users: [], pendingInvites: 0, now: NOW }).hosts).toEqual(
      { online: 0, total: 2, attention: 1 },
    );
  });

  it("rounds the outbound bitrate to a tenth of a megabit", () => {
    const sessions = [
      { state: "running", latest_metrics: { agent: { metrics: { bitrate_kbps: 1234 } } } },
      { state: "running", latest_metrics: { browser: { metrics: { bitrate_kbps: 9999 } } } },
      { state: "running" },
    ];
    // Browser-reported bitrate is inbound to that one client; only the agent
    // figure is what the fleet is sending.
    expect(deriveKpis({ hosts: [], sessions, users: [], pendingInvites: 0, now: NOW }).sessions.mbpsOut)
      .toBe(1.2);
  });
});
