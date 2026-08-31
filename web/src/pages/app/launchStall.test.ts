// #482 — the launch screen must not blame the scheduler for a transport failure.
//
// The live case, in one line: session `running`, `state_detail = "pipeline live;
// offer ready"`, host_id set, GPU idle, agent online — and the UI said "No host
// has picked this session up yet". The decision table below is the fix.

import { describe, expect, it } from "vitest";
import {
  PHASE_STALL_MS,
  TRANSPORT_STALL_MS,
  resolveStall,
  type StallInputs,
} from "./launchStall";

const base: StallInputs = {
  phase: "host",
  hostAssigned: false,
  sessionRunning: false,
  iceState: null,
  phaseElapsedMs: 0,
  transportElapsedMs: 0,
};

const at = (over: Partial<StallInputs>): StallInputs => ({ ...base, ...over });

describe("resolveStall — silence while inside budget", () => {
  it("says nothing before the phase budget expires", () => {
    expect(resolveStall(at({ phaseElapsedMs: PHASE_STALL_MS.host - 1 }))).toBeNull();
  });

  it("says nothing about the transport before the transport budget expires", () => {
    expect(
      resolveStall(
        at({
          sessionRunning: true,
          hostAssigned: true,
          iceState: "checking",
          transportElapsedMs: TRANSPORT_STALL_MS - 1,
        }),
      ),
    ).toBeNull();
  });

  it("says nothing once ICE is up and the phase is inside budget", () => {
    expect(
      resolveStall(
        at({
          phase: "stream",
          sessionRunning: true,
          hostAssigned: true,
          iceState: "connected",
          transportElapsedMs: 10 * TRANSPORT_STALL_MS,
        }),
      ),
    ).toBeNull();
  });
});

describe("resolveStall — scheduling copy is only for a genuinely unplaced session", () => {
  it("still blames scheduling while no host has accepted", () => {
    const v = resolveStall(at({ phaseElapsedMs: PHASE_STALL_MS.host }));
    expect(v).toEqual({ kind: "phase", title: "Still looking for a host", message: expect.any(String) });
    expect(v?.message).toMatch(/No host has picked this session up/i);
  });

  it("never says 'no host has picked this up' once a host is assigned", () => {
    const v = resolveStall(
      at({ hostAssigned: true, phaseElapsedMs: PHASE_STALL_MS.host }),
    );
    expect(v?.message).not.toMatch(/no host has picked/i);
    // Host owns it but it is not running yet: still bring-up, not transport.
    expect(v).toEqual({ kind: "phase", title: "Still starting the game", message: expect.any(String) });
  });

  // The #482 report, exactly: host_id set, state running, unrecognised status
  // string so `deriveStage` fell back to the "host" phase.
  it("blames the transport, not the scheduler, on the reported live case", () => {
    const v = resolveStall(
      at({
        phase: "host",
        hostAssigned: true,
        sessionRunning: true,
        iceState: "checking",
        phaseElapsedMs: PHASE_STALL_MS.host,
        transportElapsedMs: PHASE_STALL_MS.host,
      }),
    );
    expect(v?.kind).toBe("transport");
    expect(v?.message).not.toMatch(/GPU may be busy|agent may be offline/i);
    expect(v?.message).toMatch(/different networks/i);
    expect(v?.message).toMatch(/UDP/);
    expect(v?.message).toMatch(/STUN or TURN/i);
    // #509 reworded this: an ICE-server list is configurable now, so the copy
    // no longer claims LAN/VPN is the only possibility — it says LAN or VPN is
    // what you get WITHOUT a STUN/TURN server, which is still the default.
    expect(v?.message).toMatch(/LAN or VPN/i);
  });
});

describe("resolveStall — ICE state drives the transport verdict", () => {
  it("reports a failed ICE path immediately, without waiting out any budget", () => {
    const v = resolveStall(at({ iceState: "failed", sessionRunning: true, hostAssigned: true }));
    expect(v?.kind).toBe("transport");
  });

  // `disconnected` is the transient the RecoveryController restarts through
  // (degrade -> restart_ice -> checking -> connected). Reporting it on sight
  // would flash the transport message on every WiFi blip and then retract it.
  it("waits out the transport budget on 'disconnected' rather than reporting on sight", () => {
    const dip = {
      iceState: "disconnected" as const,
      sessionRunning: true,
      hostAssigned: true,
      phase: "stream" as const,
    };
    expect(resolveStall(at({ ...dip, transportElapsedMs: TRANSPORT_STALL_MS - 1 }))).toBeNull();
    expect(resolveStall(at({ ...dip, transportElapsedMs: TRANSPORT_STALL_MS }))?.kind).toBe("transport");
  });

  it("clears the moment a dip recovers to connected", () => {
    const recovered = at({
      phase: "stream",
      hostAssigned: true,
      sessionRunning: true,
      iceState: "connected",
      transportElapsedMs: 10 * TRANSPORT_STALL_MS,
    });
    expect(resolveStall(recovered)).toBeNull();
  });

  it("reports a running session stuck in 'checking' past the transport budget", () => {
    const v = resolveStall(
      at({
        phase: "stream",
        hostAssigned: true,
        sessionRunning: true,
        iceState: "checking",
        transportElapsedMs: TRANSPORT_STALL_MS,
      }),
    );
    expect(v?.kind).toBe("transport");
  });

  it("reports a running session with no PC state at all past the transport budget", () => {
    const v = resolveStall(
      at({ phase: "stream", hostAssigned: true, sessionRunning: true, transportElapsedMs: TRANSPORT_STALL_MS }),
    );
    expect(v?.kind).toBe("transport");
  });

  it("does not invent a transport problem for a session that is not running yet", () => {
    expect(
      resolveStall(at({ hostAssigned: true, iceState: "checking", transportElapsedMs: 10 * TRANSPORT_STALL_MS })),
    ).toBeNull();
  });
});

describe("resolveStall — the other phases keep their own copy", () => {
  it.each([
    ["game", /downloads and unpacks/i],
    ["stream", /video connection hasn't completed/i],
    ["app", /game is starting/i],
  ] as const)("keeps the %s phase copy", (phase, matcher) => {
    const v = resolveStall(at({ phase, phaseElapsedMs: PHASE_STALL_MS[phase] }));
    expect(v?.kind).toBe("phase");
    expect(v?.message).toMatch(matcher);
  });

  it("honours the stallMs seam for the phase budget", () => {
    expect(resolveStall(at({ phaseElapsedMs: 4_000, stallMs: 5_000 }))).toBeNull();
    expect(resolveStall(at({ phaseElapsedMs: 5_000, stallMs: 5_000 }))?.kind).toBe("phase");
  });

  it("keeps the transport budget well under every phase budget", () => {
    for (const ms of Object.values(PHASE_STALL_MS)) {
      expect(TRANSPORT_STALL_MS).toBeLessThan(ms);
    }
  });
});
