import { render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ReadinessCard } from "./ReadinessCard";
import type { ReadinessCheck } from "../api/types";

function check(overrides: Partial<ReadinessCheck> = {}): ReadinessCheck {
  return {
    id: "nvidia_egl_vendor_json",
    status: "pass",
    summary: "EGL vendor json present.",
    remediation: "",
    ...overrides,
  } as ReadinessCheck;
}

describe("ReadinessCard", () => {
  it("renders a pass check with no remediation line", () => {
    render(<ReadinessCard checks={[check()]} />);
    const row = screen.getByTestId("readiness-check-nvidia_egl_vendor_json");
    expect(within(row).getByRole("img", { name: "Pass" })).toBeInTheDocument();
    expect(within(row).getByText("EGL vendor json present.")).toBeInTheDocument();
    expect(within(row).queryByRole("button", { name: /copy/i })).not.toBeInTheDocument();
  });

  it("renders a fail check with a copyable remediation line", () => {
    render(
      <ReadinessCard
        checks={[
          check({
            id: "nvidia_lib32",
            status: "fail",
            summary: "No 32-bit GL libs found.",
            remediation: "dnf install nvidia-driver-libs.i686",
          }),
        ]}
      />,
    );
    const row = screen.getByTestId("readiness-check-nvidia_lib32");
    expect(within(row).getByRole("img", { name: "Fail" })).toBeInTheDocument();
    expect(within(row).getByText("dnf install nvidia-driver-libs.i686")).toBeInTheDocument();
    expect(within(row).getByRole("button", { name: /copy/i })).toBeInTheDocument();
    // Any fail surfaces a "Needs attention" summary badge at the card level.
    expect(screen.getByText("Needs attention")).toBeInTheDocument();
  });

  it("renders a skip check neutrally and does not count it as a failure", () => {
    render(<ReadinessCard checks={[check({ id: "amd_only_check", status: "skip", summary: "Not applicable on this host." })]} />);
    expect(screen.getByRole("img", { name: "Skipped" })).toBeInTheDocument();
    expect(screen.queryByText("Needs attention")).not.toBeInTheDocument();
  });

  it("passes an unrecognized status through instead of crashing", () => {
    render(
      <ReadinessCard
        checks={[
          check({
            id: "future_check",
            status: "provisioning" as ReadinessCheck["status"],
            summary: "New check from a newer agent.",
          }),
        ]}
      />,
    );
    const row = screen.getByTestId("readiness-check-future_check");
    // Unknown status renders as its own raw value, neutrally.
    expect(within(row).getByRole("img", { name: "provisioning" })).toBeInTheDocument();
    expect(screen.queryByText("Needs attention")).not.toBeInTheDocument();
  });

  // #483: `warn` (e.g. media_reachability) is advisory but genuinely
  // actionable — unlike `fail` it never flips the card's "Needs attention"
  // badge, but it DOES get the same copyable remediation block as `fail`.
  it("renders a warn check with a copyable remediation line and no Needs attention badge", () => {
    render(
      <ReadinessCard
        checks={[
          check({
            id: "media_reachability",
            status: "warn" as ReadinessCheck["status"],
            summary: "a host firewall with a default-deny posture is active.",
            remediation: "sudo firewall-cmd --permanent --zone=<zone> --add-rich-rule=...",
          }),
        ]}
      />,
    );
    const row = screen.getByTestId("readiness-check-media_reachability");
    expect(within(row).getByRole("img", { name: "Warning" })).toBeInTheDocument();
    expect(
      within(row).getByText("sudo firewall-cmd --permanent --zone=<zone> --add-rich-rule=..."),
    ).toBeInTheDocument();
    expect(within(row).getByRole("button", { name: /copy/i })).toBeInTheDocument();
    // Only `fail` flips the card-level badge — a warn-only host is not
    // reported as needing attention the same way a hard failure is.
    expect(screen.queryByText("Needs attention")).not.toBeInTheDocument();
  });

  it("renders an empty-but-reported state distinctly from never-reported", () => {
    render(<ReadinessCard checks={[]} reportedAt="2026-08-09T00:00:00Z" />);
    expect(screen.getByText("No readiness checks reported.")).toBeInTheDocument();
  });

  it("renders a null (never reported) state", () => {
    render(<ReadinessCard checks={null} />);
    expect(screen.getByText("This host has not reported readiness checks yet.")).toBeInTheDocument();
    expect(screen.getByText("Not reported yet.")).toBeInTheDocument();
  });

  it("copies remediation text to the clipboard", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });

    render(
      <ReadinessCard
        checks={[check({ id: "x", status: "fail", summary: "s", remediation: "echo hello" })]}
      />,
    );
    const btn = screen.getByRole("button", { name: /copy/i });
    btn.click();
    await Promise.resolve();
    expect(writeText).toHaveBeenCalledWith("echo hello");
  });

  // Regression: `navigator.clipboard?.writeText(...)` on an absent Clipboard
  // API awaits `undefined`, which resolves rather than rejects — the old code
  // fell straight into the success branch and claimed "Copied" despite
  // writing nothing. Availability must be checked explicitly.
  it("does not claim Copied when the Clipboard API is unavailable", async () => {
    const original = navigator.clipboard;
    // @ts-expect-error — deliberately simulating a browser/insecure context
    // with no Clipboard API at all, not just a failing write.
    delete navigator.clipboard;

    try {
      render(
        <ReadinessCard
          checks={[check({ id: "x", status: "fail", summary: "s", remediation: "echo hello" })]}
        />,
      );
      const btn = screen.getByRole("button", { name: /copy/i });
      btn.click();
      await Promise.resolve();
      expect(screen.getByRole("button", { name: /^copy$/i })).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: /copied/i })).not.toBeInTheDocument();
    } finally {
      Object.assign(navigator, { clipboard: original });
    }
  });
});
