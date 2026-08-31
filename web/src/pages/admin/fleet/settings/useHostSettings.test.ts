// Pins the saved-vs-draft override split (review fix): `dirty` must derive
// from draft-vs-last-persisted, never from "has any override" — a host that
// loads with overrides already set must not read as dirty, and discard must
// restore the persisted overrides, not wipe them.

import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../../../../auth/context", () => ({ useAuth: () => ({ token: "token" }) }));
vi.mock("../../../../api/admin", () => ({
  getHost: vi.fn(),
  getConfigCatalog: vi.fn(),
  getHostSettings: vi.fn(),
  getHostGPUs: vi.fn(),
  updateHostSettings: vi.fn(),
  restartHost: vi.fn(),
}));
vi.mock("../../../../lib/fleet/FleetContext", () => ({
  useFleetContext: () => ({
    hosts: [], sessions: [], loading: false, lastFetchedAt: 0,
    errors: { hosts: null, sessions: null }, reload: vi.fn(),
  }),
}));

import * as adminApi from "../../../../api/admin";
import type { Host } from "../../../../api/types";
import { useHostSettings } from "./useHostSettings";

const HOST = { id: "host-1", node_name: "Tower" } as unknown as Host;

function settingsResponse(overrides: Record<string, boolean | number | string> = {}) {
  return {
    resolved: { idle_timeout_secs: 1800, gop: 120 },
    overrides,
    pending_restart: false,
    effective: null,
  };
}

describe("useHostSettings: saved vs. draft overrides", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(adminApi.getHost).mockResolvedValue({ host: HOST } as never);
    vi.mocked(adminApi.getConfigCatalog).mockResolvedValue({ knobs: [] } as never);
    vi.mocked(adminApi.getHostGPUs).mockResolvedValue({ items: [] } as never);
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("is not dirty on load even though the host already carries persisted overrides", async () => {
    vi.mocked(adminApi.getHostSettings).mockResolvedValue(
      settingsResponse({ idle_timeout_secs: 900 }) as never,
    );

    const { result } = renderHook(() => useHostSettings("host-1"));

    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.overrides).toEqual({ idle_timeout_secs: 900 });
    expect(result.current.changedCount).toBe(1); // "has an override" is still true...
    expect(result.current.dirty).toBe(false); // ...but nothing was edited, so not dirty.
  });

  it("becomes dirty only once the draft diverges from the persisted overrides", async () => {
    vi.mocked(adminApi.getHostSettings).mockResolvedValue(
      settingsResponse({ idle_timeout_secs: 900 }) as never,
    );

    const { result } = renderHook(() => useHostSettings("host-1"));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.dirty).toBe(false);

    act(() => result.current.setValue("gop", 240));
    expect(result.current.dirty).toBe(true);
  });

  it("discard restores the last-persisted overrides, not an empty draft", async () => {
    vi.mocked(adminApi.getHostSettings).mockResolvedValue(
      settingsResponse({ idle_timeout_secs: 900 }) as never,
    );

    const { result } = renderHook(() => useHostSettings("host-1"));
    await waitFor(() => expect(result.current.loading).toBe(false));

    act(() => result.current.setValue("gop", 240));
    expect(result.current.overrides).toEqual({ idle_timeout_secs: 900, gop: 240 });

    act(() => result.current.discard());
    expect(result.current.overrides).toEqual({ idle_timeout_secs: 900 });
    expect(result.current.dirty).toBe(false);
  });

  it("a successful save adopts the response overrides as the new saved snapshot", async () => {
    vi.mocked(adminApi.getHostSettings).mockResolvedValue(settingsResponse() as never);
    vi.mocked(adminApi.updateHostSettings).mockResolvedValue({
      resolved: { idle_timeout_secs: 900, gop: 120 },
      overrides: { idle_timeout_secs: 900 },
      restart_triggered: false,
      effective: null,
    } as never);

    const { result } = renderHook(() => useHostSettings("host-1"));
    await waitFor(() => expect(result.current.loading).toBe(false));

    act(() => result.current.setValue("idle_timeout_secs", 900));
    expect(result.current.dirty).toBe(true);

    await act(async () => { await result.current.save(); });

    // The just-saved override is now the baseline: dirty again, not still-dirty.
    expect(result.current.dirty).toBe(false);
    expect(result.current.overrides).toEqual({ idle_timeout_secs: 900 });
  });
});
