import { describe, expect, it } from "vitest";
import { launchFailureFromSession, takenOverFailure, unreachableFailure } from "./sessionFailure";

const s = (
  state: string,
  state_detail: string | null = null,
  error_message: string | null = null,
  failure_code: string | null = null,
  app_log_tail: string | null = null,
) =>
  ({ state, state_detail, error_message, failure_code, app_log_tail }) as Parameters<
    typeof launchFailureFromSession
  >[0];

describe("launchFailureFromSession", () => {
  it.each(["pending", "assigned", "starting", "running"])(
    "returns null while %s — the launch is still in flight",
    (state) => {
      expect(launchFailureFromSession(s(state))).toBeNull();
    },
  );

  it("reports a server-side failure and carries the server's own words", () => {
    const v = launchFailureFromSession(s("failed", "encoder_init", "no NVENC session slots free"));
    expect(v?.kind).toBe("failed");
    expect(v?.detail).toBe("no NVENC session slots free");
  });

  it("falls back to state_detail when there is no error_message", () => {
    expect(launchFailureFromSession(s("failed", "encoder_init"))?.detail).toBe("encoder_init");
  });

  // SessionPage renders its own full-screen host-lost card; two competing
  // terminal screens would be worse than one.
  it("defers host_lost to SessionPage's own overlay", () => {
    expect(launchFailureFromSession(s("failed", "host_lost"))).toBeNull();
  });

  // First-run-experience §S5.
  it("gives app_exited_early an operator-language headline and carries the log tail", () => {
    const v = launchFailureFromSession(
      s(
        "failed",
        "app_container_exited",
        "Steam needs to be online to update.",
        "app_exited_early",
        "line1\nline2\nSteam needs to be online to update.",
      ),
    );
    expect(v?.title).toBe("The app exited before producing any video");
    expect(v?.message).toBe("Steam needs to be online to update.");
    expect(v?.logTail).toBe("line1\nline2\nSteam needs to be online to update.");
  });

  it("falls back to a generic log-hint message when app_exited_early has no error_message", () => {
    const v = launchFailureFromSession(s("failed", null, null, "app_exited_early", "exit code 1"));
    expect(v?.title).toBe("The app exited before producing any video");
    expect(v?.message).toMatch(/check the log below/i);
    expect(v?.logTail).toBe("exit code 1");
  });

  it("an unrecognized failure_code falls through to the generic failed verdict", () => {
    const v = launchFailureFromSession(s("failed", "encoder_init", "no slots free", "some_future_code"));
    expect(v?.title).toBe("The launch failed");
    expect(v?.logTail).toBeUndefined();
  });

  // #484 §3.3: the boot watchdog's failure arrives structured — SAME
  // mechanism as app_exited_early (S5): failure_code="app_never_presented"
  // plus a structured app_log_tail field. This is the PRIMARY path (a prior
  // interface note claiming a substring-of-error_message shape was withdrawn
  // after WP-A rebased onto develop, which already ships the S5 fields).
  describe("#484: app_never_presented (boot watchdog) — structured primary path", () => {
    it("gives its own headline via the structured failure_code, carrying the server's message and app_log_tail", () => {
      const v = launchFailureFromSession(
        s(
          "failed",
          "app_never_presented",
          "app produced no frame within 10s of container start",
          "app_never_presented",
          "line1\nline2",
        ),
      );
      expect(v?.title).toBe("The game never started");
      expect(v?.message).toBe("app produced no frame within 10s of container start");
      expect(v?.logTail).toBe("line1\nline2");
    });

    it("falls back to a generic boot-time-limit message when the server sends no error_message", () => {
      const v = launchFailureFromSession(
        s("failed", "app_never_presented", null, "app_never_presented", "line1\nline2"),
      );
      expect(v?.title).toBe("The game never started");
      expect(v?.message).toMatch(/boot time limit/i);
      expect(v?.logTail).toBe("line1\nline2");
    });

    it("carries a null app_log_tail through as-is (no synthesized tail)", () => {
      const v = launchFailureFromSession(
        s("failed", "app_never_presented", "no frame within 10s", "app_never_presented", null),
      );
      expect(v?.logTail).toBeNull();
    });
  });

  // Defensive fallback only — must never be reached while failure_code is
  // set (the structured path above always wins first).
  describe("#484: app_never_presented substring fallback (defensive only)", () => {
    it("still recognizes the marker in error_message when failure_code is absent", () => {
      const v = launchFailureFromSession(
        s(
          "failed",
          "app_never_presented",
          "app produced no frame within 10s of container start (app_never_presented)\n--- app log tail ---\nline1\nline2",
        ),
      );
      expect(v?.title).toBe("The game never started");
      expect(v?.message).toBe("app produced no frame within 10s of container start");
      expect(v?.logTail).toBe("line1\nline2");
    });

    it("is NOT reached when failure_code is set — the structured path takes priority", () => {
      // app_exited_early's headline must win even though error_message
      // happens to also contain the never-presented marker text.
      const v = launchFailureFromSession(
        s("failed", "app_never_presented", "no frame (app_never_presented)", "app_exited_early", null),
      );
      expect(v?.title).toBe("The app exited before producing any video");
    });
  });

  it.each(["stopping", "stopped"])("reports %s before connect as an ended session", (state) => {
    expect(launchFailureFromSession(s(state))?.kind).toBe("ended");
  });

  // #526 — a takeover is its own verdict, not "unreachable". The session is
  // still running; it is this VIEW that lost it, and the copy has to say so or
  // the user relaunches an app that is already up.
  it("distinguishes a takeover from an unreachable stream", () => {
    const takenOver = takenOverFailure();
    expect(takenOver.kind).toBe("taken_over");
    expect(takenOver.kind).not.toBe(unreachableFailure().kind);
    expect(takenOver.message).not.toBe(unreachableFailure().message);
  });

  it("never leaves a verdict without both a title and something to do about it", () => {
    for (const v of [
      launchFailureFromSession(s("failed")),
      launchFailureFromSession(s("stopped")),
      unreachableFailure(),
      unreachableFailure("session is no longer reconnectable"),
      takenOverFailure(),
    ]) {
      expect(v?.title).toBeTruthy();
      expect(v?.message).toBeTruthy();
    }
  });
});
