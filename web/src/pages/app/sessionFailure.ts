// Launch-failure verdicts for the connection loader (UX assessment §2.2/§3.2).
//
// Pure mapping from the authoritative session record (SessionPage polls
// `GET /v1/sessions/{id}` once/sec during launch) to loader copy — replaces a
// loader that had no terminal state, deliberately not a second client-side
// notion of "failed".
//
// host_lost is deliberately NOT a verdict here — SessionPage renders its own
// "Host went offline" card; two competing terminal screens would be worse.

import type { Session } from "../../api/types";
import { failurePresentation } from "../../components/SessionFailureDetail";

export type LaunchFailureKind =
  /** The control plane says the session failed. */
  | "failed"
  /** The session reached a terminal stopped state before the stream opened. */
  | "ended"
  /** The browser could not (re)establish transport and has stopped trying. */
  | "unreachable"
  /** #526 — another tab/window/device attached to this session and won. */
  | "taken_over";

export interface LaunchFailure {
  kind: LaunchFailureKind;
  /** Short headline — what happened. */
  title: string;
  /** One sentence the user can act on. */
  message: string;
  /** Server-authored detail (state_detail / error_message), shown as small
   * print. Never a raw exception string reaching this field — that's the bug
   * this replaces. */
  detail?: string;
  /** First-run-experience §S5: the app container's captured log tail
   * (`Session.app_log_tail`), rendered PREFORMATTED, separate from `detail`.
   * Only ever server text. */
  logTail?: string | null;
}

type SessionVerdictInput = Pick<
  Session,
  "state" | "state_detail" | "error_message" | "failure_code" | "app_log_tail"
>;

// #484 §3.3: boot-watchdog failure (QUASAR_APP_BOOT_TIMEOUT_SECS expiry)
// arrives the same way app_exited_early (§S5) does — structured
// `failure_code: "app_never_presented"` + `app_log_tail`, never a substring of
// `error_message`. Generic fallback copy, used only when the server sends no
// `error_message`, keyed like `failurePresentation()`'s headline map.
const FAILURE_CODE_GENERIC_MESSAGE: Partial<Record<string, string>> = {
  app_never_presented:
    "The app produced no video within the boot time limit. Nothing is running, so it is safe to launch again.",
};

// Defensive fallback ONLY, for a session whose reason arrives embedded in
// error_message text instead of the structured fields above (e.g. a
// mismatched agent/control-plane pair mid-rollout). Never the primary path.
const APP_NEVER_PRESENTED_MARKER = "(app_never_presented)";
const APP_LOG_TAIL_DELIMITER = "--- app log tail ---\n";

function splitAppNeverPresentedMessage(errorMessage: string): {
  message: string;
  logTail: string | null;
} {
  const idx = errorMessage.indexOf(APP_LOG_TAIL_DELIMITER);
  const head = idx === -1 ? errorMessage : errorMessage.slice(0, idx);
  const logTail = idx === -1 ? null : errorMessage.slice(idx + APP_LOG_TAIL_DELIMITER.length) || null;
  const message = head.replace(APP_NEVER_PRESENTED_MARKER, "").trim();
  return { message, logTail };
}

/**
 * Terminal verdict for a session that has not opened its input channel yet, or
 * `null` while the launch is still legitimately in flight
 * (pending/assigned/starting/running).
 */
export function launchFailureFromSession(s: SessionVerdictInput): LaunchFailure | null {
  if (s.state === "failed") {
    // SessionPage's own host-lost card owns this one.
    if (s.state_detail === "host_lost") return null;

    // §S5: a known failure_code gets an operator-language headline
    // (`failurePresentation`, components/SessionFailureDetail.tsx — the ONE
    // map, shared with admin/SessionDetail). Covers app_exited_early and
    // (#484 §3.3) app_never_presented. Unknown code falls to the substring
    // check, then the generic verdict.
    const headline = failurePresentation(s.failure_code);
    if (headline) {
      return {
        kind: "failed",
        title: headline,
        message:
          s.error_message ??
          FAILURE_CODE_GENERIC_MESSAGE[s.failure_code ?? ""] ??
          "The app container exited before the stream could start. Check the log below for what happened.",
        detail: s.state_detail ?? undefined,
        logTail: s.app_log_tail,
      };
    }

    // Defensive fallback (see marker comment above) — not #484's primary path.
    if (s.error_message?.includes(APP_NEVER_PRESENTED_MARKER)) {
      const { message, logTail } = splitAppNeverPresentedMessage(s.error_message);
      return {
        kind: "failed",
        title: "The game never started",
        message: message || FAILURE_CODE_GENERIC_MESSAGE.app_never_presented!,
        detail: s.state_detail ?? undefined,
        logTail: logTail ?? s.app_log_tail,
      };
    }

    return {
      kind: "failed",
      title: "The launch failed",
      message:
        "The host could not start this session. Nothing is running, so it is safe to launch again.",
      detail: s.error_message ?? s.state_detail ?? undefined,
    };
  }
  if (s.state === "stopping" || s.state === "stopped") {
    return {
      kind: "ended",
      title: "The session ended before it started",
      message:
        "This session was stopped before the stream opened. Launch again from your library.",
      detail: s.state_detail ?? undefined,
    };
  }
  return null;
}

/** Browser gave up on transport (bounded ICE recovery exhausted, no
 * replacement signalling token). `detail` MUST be human copy or
 * server-authored text. */
export function unreachableFailure(detail?: string): LaunchFailure {
  return {
    kind: "unreachable",
    title: "Could not reach the stream",
    message:
      "The connection to the host could not be established from this device. Check your network, then launch again.",
    detail,
  };
}

/** #526 — a later attach took this session's signaling (WS close 4410). The
 * session is alive elsewhere, so the copy doesn't invite a relaunch (that's
 * how two tabs used to evict each other in a loop). Reuses the loader's
 * ordinary failure card. */
export function takenOverFailure(): LaunchFailure {
  return {
    kind: "taken_over",
    title: "This session moved to another tab",
    message:
      "You opened this session somewhere else, and that window has the stream now. Close this one, or resume the session from your library to bring it back here.",
  };
}
