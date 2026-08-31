/**
 * One fleet poll and one live-session poll for the whole admin console.
 *
 * Mounted once, by `AdminLayout`, above the router `Outlet`. Every admin
 * surface that wants "what hosts exist" or "what is running right now" reads
 * it from here instead of opening its own resource: five surfaces asking the
 * same two questions on their own timers is five times the request rate and
 * five chances to render different numbers on one screen.
 *
 * It is deliberately mounted in the admin layout, not the shell: the account
 * area uses the same `ConsoleShell` and must never poll an admin endpoint.
 *
 * Pages that need something else (a host's GPUs, a session's metrics, the
 * app catalog) still open their own `useResource` — this is the shared spine,
 * not a store.
 */

import { createContext, useContext, useMemo, useRef, type ReactNode } from "react";
import type { AdminSession, Host } from "../../api/types";
import type { RailBadgeCounts } from "../../components/shell/navTypes";
import { needsAttention } from "./deriveAlerts";
import { useFleet } from "./useFleet";
import { useLiveSessions } from "./useLiveSessions";

export interface FleetContextValue {
  hosts: Host[];
  sessions: AdminSession[];
  /** True until both first loads have settled — the console's first-paint gate. */
  loading: boolean;
  /**
   * Epoch ms of the older of the two applied loads, or null until both have
   * landed. Deliberately the older one: it is the honest answer to "how fresh
   * is everything on this screen", where the newer one would overstate it.
   */
  lastFetchedAt: number | null;
  /** Per-poll load failures. Null means that half is fine; the other half's
   *  data is still good (resource invariant I3 keeps the last good value). */
  errors: { hosts: string | null; sessions: string | null };
  /** Refresh both now. Never rejects — failures land in `errors`. */
  reload: () => Promise<void>;
}

const FleetCtx = createContext<FleetContextValue | null>(null);

export function FleetProvider({ children }: { children: ReactNode }) {
  const fleet = useFleet();
  const live = useLiveSessions();

  const hosts = useStableList(fleet.data, fleet.generation);
  const sessions = useStableList(live.data, live.generation);
  const value = useMemo<FleetContextValue>(
    () => ({
      hosts,
      sessions,
      loading: fleet.loading || live.loading,
      lastFetchedAt:
        fleet.updatedAt != null && live.updatedAt != null
          ? Math.min(fleet.updatedAt, live.updatedAt)
          : null,
      errors: { hosts: fleet.errorMessage, sessions: live.errorMessage },
      reload: async () => {
        await Promise.all([fleet.refresh(), live.refresh()]);
      },
    }),
    [
      hosts,
      sessions,
      fleet.loading,
      live.loading,
      fleet.updatedAt,
      live.updatedAt,
      fleet.errorMessage,
      live.errorMessage,
      fleet.refresh,
      live.refresh,
    ],
  );

  return <FleetCtx.Provider value={value}>{children}</FleetCtx.Provider>;
}

/**
 * Hold the previous array while a poll returns the same data.
 *
 * Every 5 seconds both resources hand back a freshly parsed array. Field-wise
 * it is usually identical, but its identity is not, and an identity change is
 * what re-runs every `useMemo` keyed on it — five seconds of derived alerts,
 * KPIs, sorted tables and chart series recomputed to redraw the same pixels.
 *
 * The comparison is a JSON round-trip, deliberately cheap and deliberately
 * gated on the resource's `generation`: it runs once per applied load, not
 * once per render, so an unrelated parent re-render costs nothing. Key order
 * is stable because both arrays come from the same JSON parse.
 */
function useStableList<T>(list: T[] | undefined, generation: number): T[] {
  const held = useRef<{ generation: number; key: string; value: T[] }>({
    generation: -1,
    key: "",
    value: [],
  });
  if (generation !== held.current.generation) {
    const next = list ?? [];
    const key = JSON.stringify(next);
    if (key !== held.current.key) held.current.value = next;
    held.current.key = key;
    held.current.generation = generation;
  }
  return held.current.value;
}

export function useFleetContext(): FleetContextValue {
  const value = useContext(FleetCtx);
  if (!value) throw new Error("useFleetContext must be used inside <FleetProvider>");
  return value;
}

/**
 * The two rail markers, off the same data every admin page is reading — so
 * the badge can never claim a session the Sessions page does not list.
 * `live` needs no filtering: the poll asked the server for active sessions.
 */
export function useFleetBadges(): RailBadgeCounts {
  const { hosts, sessions } = useFleetContext();
  return useMemo(
    () => ({ live: sessions.length, fault: hosts.filter(needsAttention).length }),
    [hosts, sessions],
  );
}
