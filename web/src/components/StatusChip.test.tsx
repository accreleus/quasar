import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { StatusChip, type StatusChipConfig } from "./StatusChip";

const READINESS_CONFIG: StatusChipConfig = {
  pass: { label: "Pass", variant: "success" },
  fail: { label: "Fail", variant: "danger" },
  skip: { label: "Skipped", variant: "neutral" },
};

const HOST_CONFIG: StatusChipConfig = {
  online: { label: "Online", variant: "success", dot: true },
  draining: { label: "Draining", variant: "warning", dot: true },
  offline: { label: "Offline", variant: "danger", dot: true },
};

const PROVIDER_CONFIG: StatusChipConfig = {
  queued: { label: "Queued…", variant: "neutral" },
  pulling: { label: "Downloading…", variant: "info", dot: true },
  building: { label: "Building…", variant: "info", dot: true },
  ready: { label: "Ready", variant: "success", dot: true },
  failed: { label: "Failed", variant: "danger" },
  settling: { label: "Installing…", variant: "neutral" },
};

describe("StatusChip", () => {
  it("renders the configured label and tone for a known status", () => {
    render(<StatusChip status="pass" config={READINESS_CONFIG} />);
    const el = screen.getByText("Pass");
    expect(el.className).toContain("chip-success");
  });

  it("falls back to a neutral chip with the raw status text for an unrecognized status", () => {
    render(<StatusChip status="warn" config={READINESS_CONFIG} />);
    const el = screen.getByText("warn");
    expect(el.className).toContain("chip-neutral");
  });

  it("never crashes on a status absent from config, even with an empty config", () => {
    expect(() => render(<StatusChip status="anything" config={{}} />)).not.toThrow();
    expect(screen.getByText("anything")).toBeInTheDocument();
  });

  it("falls back for a prototype-property status name instead of resolving Object.prototype.constructor", () => {
    render(<StatusChip status="constructor" config={READINESS_CONFIG} />);
    const el = screen.getByText("constructor");
    expect(el.className).toContain("chip-neutral");
  });

  it("uses a caller-supplied fallback instead of the default when given one", () => {
    render(
      <StatusChip
        status="mystery"
        config={READINESS_CONFIG}
        fallback={{ label: "Unknown", variant: "warning" }}
      />,
    );
    expect(screen.getByText("Unknown")).toBeInTheDocument();
    expect(screen.queryByText("mystery")).not.toBeInTheDocument();
  });

  it("renders a dot when the matched entry requests one, and omits it otherwise", () => {
    const { container: withDot } = render(<StatusChip status="online" config={HOST_CONFIG} />);
    expect(withDot.querySelector(".dot")).not.toBeNull();

    const { container: noDot } = render(<StatusChip status="failed" config={PROVIDER_CONFIG} />);
    expect(noDot.querySelector(".dot")).toBeNull();
  });

  it("forwards className to the underlying Chip (ReadinessCard's chip-sm usage)", () => {
    render(<StatusChip status="skip" config={READINESS_CONFIG} className="chip-sm" />);
    expect(screen.getByText("Skipped").className).toContain("chip-sm");
  });

  // --- host status vocabulary (StepHosts.tsx) ---
  it.each([
    ["online", "Online", "chip-success"],
    ["draining", "Draining", "chip-warning"],
    ["offline", "Offline", "chip-danger"],
  ] as const)("host status %s renders %s with tone %s", (status, label, toneClass) => {
    render(<StatusChip status={status} config={HOST_CONFIG} />);
    const el = screen.getByText(label);
    expect(el.className).toContain(toneClass);
    expect(el.querySelector(".dot")).not.toBeNull();
  });

  // --- provider status vocabulary (StepLibraries.tsx) ---
  it.each([
    ["queued", "Queued…", "chip-neutral", false],
    ["pulling", "Downloading…", "chip-info", true],
    ["building", "Building…", "chip-info", true],
    ["ready", "Ready", "chip-success", true],
    ["failed", "Failed", "chip-danger", false],
    ["settling", "Installing…", "chip-neutral", false],
  ] as const)("provider status %s renders %s with tone %s (dot=%s)", (status, label, toneClass, hasDot) => {
    render(<StatusChip status={status} config={PROVIDER_CONFIG} />);
    const el = screen.getByText(label);
    expect(el.className).toContain(toneClass);
    expect(el.querySelector(".dot") !== null).toBe(hasDot);
  });
});
