/**
 * The shared console polls: one fleet request and one live-session request per
 * tick, no matter how many surfaces are reading them.
 *
 * It also covers the rail's two counts, which read from these same polls
 * rather than one of their own.
 */

import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, act } from "@testing-library/react";
import * as adminApi from "../../api/admin";
import type { AdminSession, Host } from "../../api/types";
import { FLEET_POLL_MS } from "./useFleet";
import { FleetProvider, useFleetBadges, useFleetContext } from "./FleetContext";

vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../api/admin");

const mocked = vi.mocked(adminApi);

const host = (over: Partial<Host> = {}) =>
  ({
    id: "h1",
    node_name: "quasar-node-1",
    status: "online",
    capacity_detection: "ok",
    readiness: [],
    storage: [],
    ...over,
  }) as Host;

/** Drive document.visibilityState + fire the event the resource listens on. */
function setHidden(hidden: boolean) {
  Object.defineProperty(document, "hidden", { configurable: true, value: hidden });
  Object.defineProperty(document, "visibilityState", {
    configurable: true,
    value: hidden ? "hidden" : "visible",
  });
  act(() => {
    document.dispatchEvent(new Event("visibilitychange"));
  });
}

let seen: { badges: ReturnType<typeof useFleetBadges>; fleet: ReturnType<typeof useFleetContext> };

function Probe() {
  seen = { badges: useFleetBadges(), fleet: useFleetContext() };
  return null;
}

/** Two consumers, to prove the poll count is a property of the provider. */
function renderConsole() {
  return render(
    <FleetProvider>
      <Probe />
      <Probe />
    </FleetProvider>,
  );
}

describe("FleetProvider", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.clearAllMocks();
    mocked.listHosts.mockResolvedValue({ items: [], next_cursor: null });
    mocked.listAllSessions.mockResolvedValue({ items: [], next_cursor: null });
    setHidden(false);
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("makes one request of each kind per tick, however many consumers read it", async () => {
    renderConsole();
    await act(async () => {});
    expect(mocked.listHosts).toHaveBeenCalledTimes(1);
    expect(mocked.listAllSessions).toHaveBeenCalledTimes(1);
    // The state filter is the point of the amendment — client-side filtering
    // silently under-counts past the first page.
    expect(mocked.listAllSessions).toHaveBeenCalledWith("tok", undefined, { state: "active" });

    await act(async () => {
      vi.advanceTimersByTime(FLEET_POLL_MS);
    });
    expect(mocked.listHosts).toHaveBeenCalledTimes(2);
    expect(mocked.listAllSessions).toHaveBeenCalledTimes(2);
  });

  it("stops polling while the tab is hidden and refetches on return", async () => {
    renderConsole();
    await act(async () => {});
    mocked.listHosts.mockClear();
    mocked.listAllSessions.mockClear();

    setHidden(true);
    await act(async () => {
      vi.advanceTimersByTime(FLEET_POLL_MS * 3);
    });
    expect(mocked.listHosts).not.toHaveBeenCalled();
    expect(mocked.listAllSessions).not.toHaveBeenCalled();

    setHidden(false);
    await act(async () => {});
    expect(mocked.listHosts).toHaveBeenCalledTimes(1);
    expect(mocked.listAllSessions).toHaveBeenCalledTimes(1);
  });

  it("clears both timers on unmount", async () => {
    const { unmount } = renderConsole();
    await act(async () => {});
    unmount();
    mocked.listHosts.mockClear();
    mocked.listAllSessions.mockClear();

    await act(async () => {
      vi.advanceTimersByTime(FLEET_POLL_MS * 3);
    });
    expect(mocked.listHosts).not.toHaveBeenCalled();
    expect(mocked.listAllSessions).not.toHaveBeenCalled();
  });

  it("counts live sessions and hosts needing attention for the rail", async () => {
    mocked.listHosts.mockResolvedValue({
      items: [
        host(),
        host({ id: "h2", node_name: "quasar-node-2", status: "offline" }),
        host({ id: "h3", node_name: "quasar-node-3", capacity_detection: "failed" }),
      ],
      next_cursor: null,
    });
    mocked.listAllSessions.mockResolvedValue({
      items: [{ id: "s1" }, { id: "s2" }] as AdminSession[],
      next_cursor: null,
    });

    renderConsole();
    await act(async () => {});
    expect(seen.badges).toEqual({ live: 2, fault: 2 });
    expect(seen.fleet.hosts).toHaveLength(3);
    expect(seen.fleet.sessions).toHaveLength(2);
  });

  it("keeps array identity across a poll that changed nothing", async () => {
    mocked.listHosts.mockResolvedValue({ items: [host()], next_cursor: null });
    renderConsole();
    await act(async () => {});
    const first = seen.fleet.hosts;

    await act(async () => {
      vi.advanceTimersByTime(FLEET_POLL_MS);
    });
    // Same data, same array — nothing downstream has to recompute.
    expect(seen.fleet.hosts).toBe(first);

    mocked.listHosts.mockResolvedValue({
      items: [host(), host({ id: "h2", node_name: "quasar-node-2" })],
      next_cursor: null,
    });
    await act(async () => {
      vi.advanceTimersByTime(FLEET_POLL_MS);
    });
    expect(seen.fleet.hosts).not.toBe(first);
    expect(seen.fleet.hosts).toHaveLength(2);
  });

  it("dates the screen by the older of the two loads, and reports each half's failure", async () => {
    mocked.listAllSessions.mockRejectedValue(new Error("boom"));
    renderConsole();
    await act(async () => {});

    expect(seen.fleet.errors.hosts).toBeNull();
    expect(seen.fleet.errors.sessions).toBe("could not load live sessions");
    // Sessions never landed, so there is no instant at which the whole screen
    // was true — better no timestamp than a reassuring one.
    expect(seen.fleet.lastFetchedAt).toBeNull();

    mocked.listAllSessions.mockResolvedValue({ items: [], next_cursor: null });
    await act(async () => {
      await seen.fleet.reload();
    });
    expect(seen.fleet.errors.sessions).toBeNull();
    expect(seen.fleet.lastFetchedAt).not.toBeNull();
  });

  it("refuses to be read outside the provider", () => {
    const quiet = vi.spyOn(console, "error").mockImplementation(() => {});
    expect(() => render(<Probe />)).toThrow(/FleetProvider/);
    quiet.mockRestore();
  });
});
