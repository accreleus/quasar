/**
 * The shared per-GPU codec chip row (#296/#302): fixed display order and the
 * null ("neither this GPU nor its host has reported") case. HostExpansion.test.tsx
 * and CapacityCard.test.tsx each add one integration assertion that this
 * component is actually wired in; the exhaustive cases live here.
 */
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { GpuCodecChips } from "./GpuCodecChips";

describe("GpuCodecChips", () => {
  it("renders chips in fixed order (h264, h265, av1) regardless of wire order", () => {
    render(<GpuCodecChips codecs={["av1", "h264", "h265"]} />);
    const labels = screen.getAllByText(/^(H\.264|HEVC|AV1)$/).map((el) => el.textContent);
    expect(labels).toEqual(["H.264", "HEVC", "AV1"]);
  });

  it("renders a subset in order", () => {
    render(<GpuCodecChips codecs={["h265", "h264"]} />);
    expect(screen.getAllByText(/^(H\.264|HEVC)$/).map((el) => el.textContent)).toEqual([
      "H.264",
      "HEVC",
    ]);
  });

  it("renders a muted 'Not reported' chip when this GPU inherits an unreported host set", () => {
    render(<GpuCodecChips codecs={null} />);
    const chip = screen.getByText("Not reported");
    expect(chip.className).toContain("chip-neutral");
    expect(chip.title).toMatch(/has reported/);
  });

  it("treats an empty list the same as null — never zero chips with no explanation", () => {
    render(<GpuCodecChips codecs={[]} />);
    expect(screen.getByText("Not reported")).toBeTruthy();
  });
});
