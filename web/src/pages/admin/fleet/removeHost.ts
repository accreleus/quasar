/**
 * The console's "remove host" for an owned GPU host (design_handoff_v3
 * fleet-rh06-v3.html `rhRemove`; screenshots rh06/remove-*), over control-api.md
 * amendment 14 "Removing an owned GPU host".
 *
 * The route refuses a host with live sessions unless forced, and the approved flow
 * waits for them rather than ending them. So the console drains the host first
 * (`POST /v1/hosts/{id}/drain`), waits here for its sessions to end, then asks for the
 * removal; the removal is done once the host goes offline (its agent is the first
 * thing removed). The wait lives in this page's memory, shared by the Hosts tab and
 * the host page: an admin who leaves it finds the host drained, and Remove again
 * completes it.
 */

import { useEffect, useSyncExternalStore } from "react";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { Host, PlatformIdentity } from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { useAdminAction } from "../../../lib/resource/action";
import { isControlPlaneMachine, isOwned } from "./hostServices";

export type RemovalPhase = "waiting" | "sent" | "failed" | "removed";

export interface Removal {
  phase: RemovalPhase;
  startedAt: number;
  /** Who started it, as the page knows its admin. */
  by: string | null;
  /** When the recovery actor accepted it. */
  sentAt?: number;
  /** A failure: what happened as a sentence, and its own words for Details. */
  summary?: string;
  detail?: string;
  /** Whether this removal took the drain, so cancelling it lifts the drain. */
  drained: boolean;
}

/** How long an accepted removal may leave the host connected before it reads as stuck. */
export const REMOVAL_STALL_MS = 90_000;

const removals = new Map<string, Removal>();
/** Hosts whose removal request is on the wire: it is sent once. */
const sending = new Set<string>();
const listeners = new Set<() => void>();

export function getRemoval(hostId: string): Removal | null {
  return removals.get(hostId) ?? null;
}

export function setRemoval(hostId: string, r: Removal | null) {
  if (r) removals.set(hostId, r);
  else removals.delete(hostId);
  for (const l of listeners) l();
}

function subscribe(l: () => void) {
  listeners.add(l);
  return () => {
    listeners.delete(l);
  };
}

export function useRemoval(hostId: string): Removal | null {
  return useSyncExternalStore(subscribe, () => getRemoval(hostId));
}

/** Whether this host is removed from here: an owned GPU host, never the control plane's own. */
export function removable(host: Host, machine: PlatformIdentity | null | undefined): boolean {
  return isOwned(host) && !isControlPlaneMachine(host, machine);
}

export type NextStep = "send" | "removed" | "stalled" | "disconnected" | null;

/** What a removal in flight does next, given the host as it now stands. */
export function nextStep(
  r: Removal,
  host: Pick<Host, "status">,
  liveSessions: number,
  now: number,
): NextStep {
  if (r.phase === "waiting") {
    if (host.status === "offline") return "disconnected";
    return liveSessions === 0 ? "send" : null;
  }
  if (r.phase === "sent") {
    if (host.status === "offline") return "removed";
    if (r.sentAt != null && now - r.sentAt > REMOVAL_STALL_MS) return "stalled";
  }
  return null;
}

function failureDetail(e: unknown): string {
  if (e instanceof ApiError) return `${e.status} ${e.code}: ${e.message}`;
  return e instanceof Error ? e.message : String(e);
}

function failed(r: Removal, summary: string, detail: string): Removal {
  return { ...r, phase: "failed", summary, detail };
}

/**
 * The removal of one host: `start` from the confirmation, `cancel` while it waits for
 * sessions, and `start` again to retry one that did not finish. Drives its own next
 * step as the host's polled state changes.
 */
export function useHostRemoval(host: Host | undefined, liveSessions: number, now: number) {
  const { token, user } = useAuth();
  const removal = useRemoval(host?.id ?? "");

  const send = useAdminAction(
    async (hostId: string) => {
      if (!token) return;
      sending.add(hostId);
      try {
        await adminApi.removePlatformHost(token, hostId, {});
      } finally {
        sending.delete(hostId);
      }
    },
    {
      failure: "Could not remove the host",
      onSuccess: (_r, hostId) => {
        const r = getRemoval(hostId);
        if (r) setRemoval(hostId, { ...r, phase: "sent", sentAt: Date.now() });
      },
      onFailure: (e, hostId) => {
        const r = getRemoval(hostId);
        if (r) {
          setRemoval(
            hostId,
            failed(r, "Its recovery actor did not take the removal; nothing was removed.", failureDetail(e)),
          );
        }
      },
    },
  );

  const start = useAdminAction(
    async (target: Host, sessions: number) => {
      if (!token) return;
      const drained = sessions > 0 && target.status === "online";
      if (drained) await adminApi.drainHost(token, target.id);
      // The effect below asks for the removal once no session is left.
      setRemoval(target.id, {
        phase: "waiting",
        startedAt: Date.now(),
        by: user?.username ?? null,
        drained,
      });
    },
    {
      failure: "Could not start removing the host",
      onFailure: (e, target) =>
        setRemoval(
          target.id,
          failed(
            { phase: "failed", startedAt: Date.now(), by: user?.username ?? null, drained: false },
            "It could not be drained; nothing was removed.",
            failureDetail(e),
          ),
        ),
    },
  );

  const cancel = useAdminAction(
    async (target: Host) => {
      const r = getRemoval(target.id);
      if (token && r?.drained) await adminApi.uncordonHost(token, target.id);
      setRemoval(target.id, null);
    },
    { success: "Removal cancelled", failure: "Could not cancel the removal" },
  );

  const step = removal && host ? nextStep(removal, host, liveSessions, now) : null;
  const runSend = send.run;
  useEffect(() => {
    if (!removal || !host) return;
    if (step === "send" && !sending.has(host.id)) {
      void runSend(host.id);
    } else if (step === "removed") {
      setRemoval(host.id, { ...removal, phase: "removed" });
    } else if (step === "stalled") {
      setRemoval(
        host.id,
        failed(
          removal,
          "Its recovery actor accepted the removal, but the node agent is still connected.",
          `accepted: ${new Date(removal.sentAt ?? 0).toISOString()}\nstill connected after ${REMOVAL_STALL_MS / 1000} s`,
        ),
      );
    } else if (step === "disconnected") {
      setRemoval(
        host.id,
        failed(
          removal,
          "It disconnected before its sessions ended, so its recovery actor could not be asked; nothing was removed.",
          "host disconnected while the removal waited for its sessions",
        ),
      );
    }
  }, [step, removal, host, runSend]);

  return {
    removal,
    start: (target: Host, sessions: number) => void start.run(target, sessions),
    cancel: (target: Host) => void cancel.run(target),
    pending: start.pending != null || send.pending != null || cancel.pending != null,
  };
}
