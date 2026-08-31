import { describe, expect, it } from "vitest";
import {
  heartbeatTone,
  hostStateChip,
  hostStateDot,
  hostStateLabel,
  percentOf,
  schedulingLabel,
  storageTotals,
  tone,
  toneColor,
  uptimeSince,
  utilisation,
  type UtilisationGpu,
  type UtilisationHost,
} from "./hostDerived";

function gpu(over: Partial<UtilisationGpu> = {}): UtilisationGpu {
  return { vram_mb_total: 24576, vram_mb_used: 12288, slots_total: 3, slots_reserved: 1, ...over };
}

const CAPACITY: NonNullable<UtilisationHost["capacity"]> = {
  slots_total: 3,
  slots_used: 2,
  vram_mb_total: 32768,
  vram_mb_used: 21504,
};

describe("utilisation", () => {
  it("reads slots and VRAM off the capacity roll-up when the server sent one", () => {
    const u = utilisation({ capacity: CAPACITY, storage: null });
    expect(u.gpu).toEqual({ used: 2, total: 3 });
    expect(u.vram).toEqual({ usedMb: 21504, totalMb: 32768 });
  });

  it("prefers the roll-up over the GPU list, so one row cannot show two answers", () => {
    const u = utilisation({ capacity: CAPACITY }, [gpu(), gpu()]);
    expect(u.gpu).toEqual({ used: 2, total: 3 });
  });

  it("sums the GPU list when there is no roll-up", () => {
    const u = utilisation({ capacity: null }, [
      gpu({ slots_reserved: 2, slots_total: 3, vram_mb_used: 1024, vram_mb_total: 8192 }),
      gpu({ slots_reserved: 1, slots_total: 2, vram_mb_used: 512, vram_mb_total: 8192 }),
    ]);
    expect(u.gpu).toEqual({ used: 3, total: 5 });
    expect(u.vram).toEqual({ usedMb: 1536, totalMb: 16384 });
  });

  it("excludes an unsampled GPU from both sides of the VRAM ratio", () => {
    const u = utilisation({ capacity: null }, [
      gpu({ vram_mb_used: 4096, vram_mb_total: 8192 }),
      gpu({ vram_mb_used: null, vram_mb_total: 24576 }),
    ]);
    expect(u.vram).toEqual({ usedMb: 4096, totalMb: 8192 });
  });

  it("reports VRAM as unknown when no GPU has been sampled", () => {
    const u = utilisation({ capacity: null }, [gpu({ vram_mb_used: null })]);
    expect(u.vram).toBeNull();
  });

  it("reports GPU capacity as unknown when nothing has been reported at all", () => {
    const u = utilisation({ capacity: null }, []);
    expect(u.gpu).toBeNull();
    expect(u.vram).toBeNull();
  });

  it("never reports a RAM percentage: host memory use is not on the wire", () => {
    expect(utilisation({ capacity: CAPACITY }).ramPct).toBeNull();
  });

  it("sums disk use over every reported volume", () => {
    const u = utilisation({
      capacity: null,
      storage: [
        { total_mb: 1000, available_mb: 250 },
        { total_mb: 1000, available_mb: 750 },
      ],
    });
    expect(u.diskPct).toBe(50);
  });

  it("reports disk as unknown when the agent has reported no volumes", () => {
    expect(utilisation({ capacity: null, storage: [] }).diskPct).toBeNull();
    expect(utilisation({ capacity: null, storage: null }).diskPct).toBeNull();
  });
});

describe("storageTotals", () => {
  it("returns used and total in MiB", () => {
    expect(storageTotals([{ total_mb: 4096, available_mb: 1024 }])).toEqual({
      usedMb: 3072,
      totalMb: 4096,
    });
  });

  it("returns null for a zero-sized volume rather than dividing by it", () => {
    expect(storageTotals([{ total_mb: 0, available_mb: 0 }])).toBeNull();
  });
});

describe("heartbeatTone", () => {
  const healthy = { status: "online", capacity_detection: "ok", readiness: [] };

  it("is success while the host is online and healthy", () => {
    expect(heartbeatTone(healthy)).toBe("success");
  });

  it("is warning while draining, because a drain is deliberate", () => {
    expect(heartbeatTone({ ...healthy, status: "draining" })).toBe("warning");
  });

  it("is warning for an online host that needs attention, not success", () => {
    expect(heartbeatTone({ ...healthy, capacity_detection: "failed" })).toBe("warning");
    expect(
      heartbeatTone({ ...healthy, readiness: [{ status: "fail", summary: "no EGL" }] }),
    ).toBe("warning");
  });

  it("is danger once the host is offline", () => {
    expect(heartbeatTone({ ...healthy, status: "offline" })).toBe("danger");
  });
});

describe("schedulingLabel", () => {
  it("says accepting sessions only while online", () => {
    expect(schedulingLabel({ status: "online" })).toBe("accepting sessions");
    expect(schedulingLabel({ status: "draining" })).toBe("paused");
    expect(schedulingLabel({ status: "offline" })).toBe("paused");
  });
});

describe("host state", () => {
  const online = { status: "online", capacity_detection: "ok", readiness: [] };

  it("calls an online host with a failed readiness check degraded", () => {
    const host = { ...online, readiness: [{ status: "fail", summary: "no EGL vendor json" }] };
    expect(hostStateLabel(host)).toBe("degraded");
    expect(hostStateDot(host)).toBe("bad");
    expect(hostStateChip(host)).toBe("danger");
  });

  it("calls an online host with a failed capacity report degraded", () => {
    expect(hostStateLabel({ ...online, capacity_detection: "failed" })).toBe("degraded");
  });

  it("leaves a draining host draining: a drain is not a fault", () => {
    const host = { ...online, status: "draining" };
    expect(hostStateLabel(host)).toBe("draining");
    expect(hostStateDot(host)).toBe("warn");
    expect(hostStateChip(host)).toBe("warning");
  });

  it("passes an unrecognised status through rather than rejecting it", () => {
    const host = { ...online, status: "quarantined" };
    expect(hostStateLabel(host)).toBe("quarantined");
    expect(hostStateDot(host)).toBe("off");
    expect(hostStateChip(host)).toBe("neutral");
  });
});

describe("tone", () => {
  it("steps at 75 and 90 percent", () => {
    expect(tone(74)).toBe("success");
    expect(tone(75)).toBe("warning");
    expect(tone(90)).toBe("danger");
    expect(toneColor(95)).toBe("var(--danger)");
    expect(toneColor(10)).toBe("var(--accent)");
  });
});

describe("uptimeSince", () => {
  const now = Date.parse("2026-08-29T12:00:00Z");
  const ago = (ms: number) => new Date(now - ms).toISOString();

  it("reads days and hours for a long-lived agent", () => {
    expect(uptimeSince(ago(18 * 86400_000 + 4 * 3600_000), now)).toBe("18d 4h");
  });

  it("reads hours and minutes below a day", () => {
    expect(uptimeSince(ago(90 * 60_000), now)).toBe("1h 30m");
  });

  it("reads minutes, then seconds, as it gets shorter", () => {
    expect(uptimeSince(ago(47 * 60_000), now)).toBe("47m");
    expect(uptimeSince(ago(8_000), now)).toBe("8s");
  });

  it("says n/a when the control plane has never seen the agent connect", () => {
    expect(uptimeSince(null, now)).toBe("n/a");
    expect(uptimeSince("not a date", now)).toBe("n/a");
  });
});

describe("percentOf", () => {
  it("rounds, and reads a zero total as zero rather than NaN", () => {
    expect(percentOf(1, 3)).toBe(33);
    expect(percentOf(0, 0)).toBe(0);
  });
});
