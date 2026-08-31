// Pure classifier for a poll observed while a quick-switch swap is in flight
// (useSwapTransition.ts owns the polling loop). Mirrors control-plane's
// swapDetail* constants (control-plane/internal/session/swapper.go
// handleSwapCallback, coordinator.go:44-47):
//   "swapping"      in progress
//   "swap complete" agent's new app presented a frame; app_id now committed
//   "swap rejected" agent ack'd false / unreachable — no-op, previous app kept
//   "rolled back"   SUBSTRING match on the agent's own detail string
//                   "swap failed; rolled back: <reason>" (node-agent/src/agent.rs)
//
// Any other detail while state stays "running" is pending (matches the control
// plane's "unrecognised detail = ordinary progress" default). state leaving
// "running" is always terminal failure regardless of detail text.
import type { Session } from "../../api/types";

export type SwapOutcome =
  | { kind: "pending" }
  | { kind: "success" }
  | { kind: "error"; message: string };

const ROLLED_BACK_MARKER = "rolled back:";

export function swapOutcome(
  session: Pick<Session, "state" | "state_detail" | "error_message">,
): SwapOutcome {
  if (session.state !== "running") {
    return {
      kind: "error",
      message: session.error_message ?? "The session ended unexpectedly during the switch.",
    };
  }

  const detail = session.state_detail ?? "";

  if (detail === "swap complete") return { kind: "success" };

  if (detail.includes("rolled back")) {
    // Surface just the reason; fall back to raw detail if the marker is absent.
    const idx = detail.indexOf(ROLLED_BACK_MARKER);
    const reason = idx >= 0 ? detail.slice(idx + ROLLED_BACK_MARKER.length).trim() : detail;
    return {
      kind: "error",
      message: reason || "The switch failed and the previous app was restored.",
    };
  }

  if (detail === "swap rejected") {
    return { kind: "error", message: "The switch could not be started." };
  }

  return { kind: "pending" };
}
