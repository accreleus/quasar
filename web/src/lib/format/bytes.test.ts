import { describe, expect, it } from "vitest";
import { bytes, bytesFromMb } from "./bytes";

const KB = 1024;
const MB = KB * 1024;
const GB = MB * 1024;

describe("bytes", () => {
  it("keeps plain byte counts", () => {
    expect(bytes(0)).toBe("0 B");
    expect(bytes(1)).toBe("1 B");
    expect(bytes(1023)).toBe("1023 B");
  });

  it("scales by 1024 and labels plainly, as df and nvidia-smi do", () => {
    expect(bytes(KB)).toBe("1 KB");
    expect(bytes(512 * MB)).toBe("512 MB");
    expect(bytes(12.4e9)).toBe("11.5 GB");
    expect(bytes(1024 * GB)).toBe("1 TB");
  });

  it("shows one decimal below 100 and none at or above", () => {
    expect(bytes(1.5 * KB)).toBe("1.5 KB");
    expect(bytes(11.7 * GB)).toBe("11.7 GB");
    expect(bytes(234.375 * GB)).toBe("234 GB");
  });

  it("drops a trailing zero decimal", () => {
    expect(bytes(12 * GB)).toBe("12 GB");
    expect(bytes(2 * MB)).toBe("2 MB");
  });

  it("promotes a value that would round up into the next unit", () => {
    // Rounds to 1024 MB, which is a unit that does not exist.
    expect(bytes(1023.96 * MB)).toBe("1 GB");
    expect(bytes(1023.6 * KB)).toBe("1 MB");
  });

  it("signs negatives and refuses to invent a number it does not have", () => {
    expect(bytes(-512 * MB)).toBe("-512 MB");
    expect(bytes(Number.NaN)).toBe("unknown");
    expect(bytes(Number.POSITIVE_INFINITY)).toBe("unknown");
  });
});

describe("bytesFromMb", () => {
  it("reads the API's _mb figures as MiB", () => {
    expect(bytesFromMb(12000)).toBe("11.7 GB");
    expect(bytesFromMb(240000)).toBe("234 GB");
    expect(bytesFromMb(512)).toBe("512 MB");
  });
});
