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
  it("shows AV1 compatibility as an NVIDIA warning, with the installed driver and guidance", () => {
    render(<ReadinessCard checks={[check({
      id: "nvidia_vulkan_av1_compatibility",
      status: "warn",
      summary: "NVIDIA 595.99.02 on RTX 5090 produces corrupted Vulkan AV1 video. AV1 is disabled.",
      remediation: "610.57.04 is validated on this GPU. Restart the agent after upgrading.",
    })]} />);
    const group = screen.getByTestId("readiness-group");
    expect(group).toHaveAttribute("data-group", "nvidia");
    expect(within(group).getByRole("heading", { name: "Vulkan AV1 compatibility" })).toBeInTheDocument();
    expect(within(group).getByRole("img", { name: "Warning" })).toBeInTheDocument();
    expect(within(group).getByText(/NVIDIA 595.99.02/)).toBeInTheDocument();
    expect(within(group).getByText(/610.57.04 is validated/)).toBeInTheDocument();
    expect(screen.queryByText("Needs attention")).not.toBeInTheDocument();
  });

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

  // #102: skipped = not applicable to this host (contract), so those rows leave
  // the first screen and sit behind one closed disclosure.
  it("hides not-applicable checks behind a closed disclosure and groups the rest by area", () => {
    render(
      <ReadinessCard
        checks={[
          check({ id: "nvidia_egl_vendor_json", status: "skip", summary: "no NVIDIA GPU detected on this host" }),
          check({ id: "nvidia_eglcore_library", status: "skip", summary: "no NVIDIA GPU detected on this host" }),
          check({ id: "nvidia_lib32_gl", status: "skip", summary: "no NVIDIA GPU detected on this host" }),
          check({ id: "render_node", status: "pass", summary: "render node present" }),
          check({ id: "uinput", status: "pass", summary: "/dev/uinput present" }),
          check({ id: "media_reachability", status: "warn", summary: "firewall filters UDP", remediation: "sudo ufw allow 40000:40100/udp" }),
        ]}
      />,
    );
    const main = screen.getByTestId("readiness-checks");
    expect(within(main).queryByTestId("readiness-check-nvidia_egl_vendor_json")).not.toBeInTheDocument();
    expect(within(main).getAllByTestId("readiness-group").map((g) => g.getAttribute("data-group"))).toEqual(["gpu", "input", "network"]);
    expect(within(main).getByText("GPU & display")).toBeInTheDocument();
    expect(within(main).getByText("Network")).toBeInTheDocument();
    expect(within(main).queryByText("NVIDIA driver")).not.toBeInTheDocument();

    const more = screen.getByTestId("readiness-not-applicable");
    expect(more).not.toHaveAttribute("open");
    expect(within(more).getByText("3 checks not applicable to this host")).toBeInTheDocument();
    expect(within(more).getByTestId("readiness-check-nvidia_lib32_gl")).toBeInTheDocument();
  });

  it("shows the NVIDIA driver group with all four checks together on an NVIDIA host, and no disclosure", () => {
    render(
      <ReadinessCard
        checks={[
          check({ id: "render_node" }),
          check({ id: "nvidia_egl_vendor_json" }),
          check({ id: "driver_volume_version", status: "provisioning", summary: "provisioning the driver userspace" }),
          check({ id: "nvidia_eglcore_library" }),
          check({ id: "nvidia_lib32_gl" }),
        ]}
      />,
    );
    const nvidia = screen.getByTestId("readiness-checks").querySelector('[data-group="nvidia"]')!;
    expect(within(nvidia as HTMLElement).getByText("NVIDIA driver")).toBeInTheDocument();
    expect([...nvidia.querySelectorAll('[data-testid^="readiness-check-"]')].map((e) => e.getAttribute("data-testid"))).toEqual([
      "readiness-check-driver_volume_version",
      "readiness-check-nvidia_egl_vendor_json",
      "readiness-check-nvidia_eglcore_library",
      "readiness-check-nvidia_lib32_gl",
    ]);
    expect(screen.queryByTestId("readiness-not-applicable")).not.toBeInTheDocument();
    expect(screen.getByRole("img", { name: "Provisioning" })).toBeInTheDocument();
  });

  // #254: readiness is what the host can establish about itself; the card says so on
  // every render and never claims a browser can reach the host.
  it("carries the host-local note whether or not checks were reported", () => {
    const { unmount } = render(<ReadinessCard checks={null} />);
    expect(screen.getByTestId("readiness-host-local-note")).toHaveTextContent(/what this host can establish about itself/i);
    expect(screen.getByTestId("readiness-host-local-note")).toHaveTextContent(/does not show whether a browser can reach/i);
    unmount();
    render(<ReadinessCard checks={[check({ id: "render_node" })]} />);
    expect(screen.getByTestId("readiness-host-local-note")).toBeInTheDocument();
  });

  it("shows the runtime checks first under Container runtime, an unreachable engine with its fix", () => {
    render(
      <ReadinessCard
        checks={[
          check({ id: "render_node" }),
          check({ id: "runtime_cdi", status: "skip", summary: "the engine did not report CDI" }),
          check({
            id: "runtime_endpoint",
            status: "fail",
            summary: "the container runtime at unix:///var/run/docker.sock is unreachable: connection refused",
            remediation: "Check that Docker is running on the host and that /var/run/docker.sock is mounted into the agent container.",
          }),
          check({ id: "runtime_api_version", status: "skip", summary: "no engine answered" }),
        ]}
      />,
    );
    const main = screen.getByTestId("readiness-checks");
    expect(within(main).getAllByTestId("readiness-group").map((g) => g.getAttribute("data-group"))).toEqual(["runtime", "gpu"]);
    const runtime = main.querySelector('[data-group="runtime"]') as HTMLElement;
    expect(within(runtime).getByText("Container runtime")).toBeInTheDocument();
    expect(within(runtime).getByRole("heading", { name: "runtime endpoint" })).toBeInTheDocument();
    expect(within(runtime).getByText(/is unreachable/)).toBeInTheDocument();
    expect(within(runtime).getByRole("button", { name: /copy/i })).toBeInTheDocument();
    expect(screen.getByText("Needs attention")).toBeInTheDocument();
    expect(screen.getByText("2 checks not applicable to this host")).toBeInTheDocument();
  });

  // #253: storage has its own group; a warn there carries the fix like any other warn.
  it("shows the storage checks under a Storage group with their fix", () => {
    render(
      <ReadinessCard
        checks={[
          check({ id: "render_node" }),
          check({ id: "homes_root_writable", status: "pass", summary: "the app identity (uid 1000) wrote a test home under /var/lib/quasar/homes" }),
          check({
            id: "homes_free_space",
            status: "warn",
            summary: "1.2 GiB free under /var/lib/quasar/homes, below the 5 GiB floor",
            remediation: "Free space on the filesystem holding /var/lib/quasar/homes, or set QUASAR_HOMES_FREE_SPACE_FLOOR_GIB.",
          }),
        ]}
      />,
    );
    const main = screen.getByTestId("readiness-checks");
    expect(within(main).getAllByTestId("readiness-group").map((g) => g.getAttribute("data-group"))).toEqual(["gpu", "storage"]);
    const storage = main.querySelector('[data-group="storage"]') as HTMLElement;
    expect(within(storage).getByText("Storage")).toBeInTheDocument();
    expect([...storage.querySelectorAll('[data-testid^="readiness-check-"]')].map((e) => e.getAttribute("data-testid"))).toEqual([
      "readiness-check-homes_free_space",
      "readiness-check-homes_root_writable",
    ]);
    expect(within(storage).getByRole("heading", { name: "homes free space" })).toBeInTheDocument();
    expect(within(storage).getByText(/QUASAR_HOMES_FREE_SPACE_FLOOR_GIB/)).toBeInTheDocument();
    expect(screen.queryByText("Needs attention")).not.toBeInTheDocument();
  });

  it("says 'check' in the singular when one is not applicable", () => {
    render(<ReadinessCard checks={[check({ id: "render_node" }), check({ id: "nvidia_lib32_gl", status: "skip" })]} />);
    expect(screen.getByText("1 check not applicable to this host")).toBeInTheDocument();
  });

  it("passes an unrecognized status through instead of crashing", () => {
    render(
      <ReadinessCard
        checks={[
          check({
            id: "future_check",
            status: "recalibrating" as ReadinessCheck["status"],
            summary: "New check from a newer agent.",
          }),
        ]}
      />,
    );
    const row = screen.getByTestId("readiness-check-future_check");
    // Unknown status renders as its own raw value, neutrally.
    expect(within(row).getByRole("img", { name: "recalibrating" })).toBeInTheDocument();
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

  describe("provenance (#261)", () => {
    it.each([
      ["host_probe", "host probe"],
      ["local", "local check"],
      ["runtime", "container runtime"],
      ["operator", "operator configuration"],
      ["telemetry_v2", "telemetry_v2"],
    ])("shows provenance line with observed_at and source label for %s", (source, label) => {
      const observedAt = "2026-09-19T10:00:00Z";
      const expectedTimeString = new Date(observedAt).toLocaleString();
      render(
        <ReadinessCard
          checks={[
            check({
              observed_at: observedAt,
              source: source,
            }),
          ]}
        />,
      );
      const row = screen.getByTestId("readiness-check-nvidia_egl_vendor_json");
      const provenance = within(row).getByTestId("readiness-provenance-nvidia_egl_vendor_json");
      expect(provenance).toHaveTextContent("Observed");
      expect(provenance).toHaveTextContent(expectedTimeString);
      expect(provenance).toHaveTextContent(label);
    });

    it("shows source label when only source is present, without Observed time", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              source: "host_probe",
            }),
          ]}
        />,
      );
      const row = screen.getByTestId("readiness-check-nvidia_egl_vendor_json");
      const provenance = within(row).getByTestId("readiness-provenance-nvidia_egl_vendor_json");
      expect(provenance).toHaveTextContent("host probe");
      expect(provenance).not.toHaveTextContent("Observed");
    });

    it("shows Observed time when only observed_at is present, without source label", () => {
      const observedAt = "2026-09-19T10:00:00Z";
      const expectedTimeString = new Date(observedAt).toLocaleString();
      render(
        <ReadinessCard
          checks={[
            check({
              observed_at: observedAt,
            }),
          ]}
        />,
      );
      const row = screen.getByTestId("readiness-check-nvidia_egl_vendor_json");
      const provenance = within(row).getByTestId("readiness-provenance-nvidia_egl_vendor_json");
      expect(provenance).toHaveTextContent("Observed");
      expect(provenance).toHaveTextContent(expectedTimeString);
    });

    it("renders no provenance element for older agent without observed_at, source, or blocks", () => {
      render(
        <ReadinessCard
          checks={[check()]}
        />,
      );
      const row = screen.getByTestId("readiness-check-nvidia_egl_vendor_json");
      expect(within(row).queryByTestId("readiness-provenance-nvidia_egl_vendor_json")).not.toBeInTheDocument();
      expect(within(row).queryByTestId("readiness-blocks-nvidia_egl_vendor_json")).not.toBeInTheDocument();
    });

    it("does not render Invalid Date text for malformed observed_at", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              observed_at: "not-a-date",
              source: "local",
            }),
          ]}
        />,
      );
      const row = screen.getByTestId("readiness-check-nvidia_egl_vendor_json");
      expect(row).not.toHaveTextContent("Invalid Date");
      const provenance = within(row).getByTestId("readiness-provenance-nvidia_egl_vendor_json");
      expect(provenance).toHaveTextContent("local check");
    });

    it("shows Blocks launches marker for fail status with blocks", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              status: "fail",
              blocks: { scope: "gpu", gpu_index: 1, enforced_by: "control_plane" },
            }),
          ]}
        />,
      );
      const row = screen.getByTestId("readiness-check-nvidia_egl_vendor_json");
      const marker = within(row).getByTestId("readiness-blocks-nvidia_egl_vendor_json");
      expect(marker).toHaveTextContent(/^Blocks launches$/);
      expect(marker).toHaveAttribute("title", "Blocks launches placed on GPU 1");
    });

    it("shows Can block launches marker for warn status with blocks", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              status: "warn",
              blocks: { scope: "host", enforced_by: "control_plane" },
            }),
          ]}
        />,
      );
      const row = screen.getByTestId("readiness-check-nvidia_egl_vendor_json");
      const marker = within(row).getByTestId("readiness-blocks-nvidia_egl_vendor_json");
      expect(marker).toHaveTextContent(/^Can block launches$/);
      expect(marker).toHaveAttribute("title", "Blocks every launch on this host");
    });

    it("shows Can block launches marker for unknown status with blocks", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              status: "unknown",
              blocks: { scope: "homes", enforced_by: "control_plane" },
            }),
          ]}
        />,
      );
      const row = screen.getByTestId("readiness-check-nvidia_egl_vendor_json");
      const marker = within(row).getByTestId("readiness-blocks-nvidia_egl_vendor_json");
      expect(marker).toHaveTextContent(/^Can block launches$/);
      expect(marker).toHaveAttribute("title", "Blocks launches that use a managed home");
    });

    it("shows correct title for pass status with blocks", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              status: "pass",
              blocks: { scope: "gpu", gpu_index: 0, enforced_by: "control_plane" },
            }),
          ]}
        />,
      );
      const row = screen.getByTestId("readiness-check-nvidia_egl_vendor_json");
      const marker = within(row).getByTestId("readiness-blocks-nvidia_egl_vendor_json");
      expect(marker).toHaveAttribute("title", "Blocks launches placed on GPU 0");
    });

    it("shows agent-enforced title for host scope", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              status: "fail",
              blocks: { scope: "host", enforced_by: "agent" },
            }),
          ]}
        />,
      );
      const row = screen.getByTestId("readiness-check-nvidia_egl_vendor_json");
      const marker = within(row).getByTestId("readiness-blocks-nvidia_egl_vendor_json");
      expect(marker).toHaveAttribute("title", "Blocks every launch on this host. Enforced by the host agent; cannot be overridden.");
    });

    it("renders no blocks marker for unrecognised scope", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              status: "fail",
              blocks: { scope: "rack", enforced_by: "control_plane" },
            }),
          ]}
        />,
      );
      const row = screen.getByTestId("readiness-check-nvidia_egl_vendor_json");
      expect(within(row).queryByTestId("readiness-blocks-nvidia_egl_vendor_json")).not.toBeInTheDocument();
    });

    it("renders unknown status with Indeterminate image, not Fail or Skipped", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              status: "unknown",
              summary: "Host probe inconclusive",
            }),
          ]}
        />,
      );
      const row = screen.getByTestId("readiness-check-nvidia_egl_vendor_json");
      expect(within(row).getByRole("img", { name: "Indeterminate" })).toBeInTheDocument();
      expect(within(row).queryByRole("img", { name: "Fail" })).not.toBeInTheDocument();
      expect(within(row).queryByRole("img", { name: "Skipped" })).not.toBeInTheDocument();
      expect(within(row).getByText("Host probe inconclusive")).toBeInTheDocument();
    });

    it("places unknown status check in a readiness group, not in not-applicable", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              id: "render_node",
              status: "unknown",
              summary: "Indeterminate result",
            }),
          ]}
        />,
      );
      const checks = screen.getByTestId("readiness-checks");
      const group = within(checks).getByTestId("readiness-group");
      expect(group).toBeInTheDocument();
      expect(within(group).getByTestId("readiness-check-render_node")).toBeInTheDocument();
      expect(screen.queryByTestId("readiness-not-applicable")).not.toBeInTheDocument();
    });

    it("does not show Needs attention when only unknown status checks are present", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              status: "unknown",
            }),
          ]}
        />,
      );
      expect(screen.queryByText("Needs attention")).not.toBeInTheDocument();
    });
  });

  describe("readiness override (#263)", () => {
    it("with no gate/overrides/handlers props, a failing check with blocks renders no override elements", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              id: "audio_probe",
              status: "fail",
              blocks: { scope: "host", enforced_by: "control_plane" },
            }),
          ]}
        />,
      );
      expect(screen.queryByTestId("readiness-override-set-audio_probe")).not.toBeInTheDocument();
      expect(screen.queryByTestId("readiness-override-clear-audio_probe")).not.toBeInTheDocument();
      expect(screen.queryByTestId("readiness-overridden-audio_probe")).not.toBeInTheDocument();
      expect(screen.queryByTestId("readiness-inert-overrides")).not.toBeInTheDocument();
    });

    it("with gate.blocking entry and onSetOverride, renders a Launch anyway button", () => {
      const onSetOverride = vi.fn();
      render(
        <ReadinessCard
          checks={[
            check({
              id: "audio_probe",
              status: "fail",
              blocks: { scope: "host", enforced_by: "control_plane" },
            }),
          ]}
          gate={{
            state: "active",
            blocking: [
              {
                check_id: "audio_probe",
                scope: "host",
                gpu_index: null,
                enforced_by: "control_plane",
                overridden: false,
              },
            ],
          }}
          onSetOverride={onSetOverride}
        />,
      );
      const btn = screen.getByTestId("readiness-override-set-audio_probe");
      expect(btn).toHaveAccessibleName("Launch anyway");
      btn.click();
      expect(onSetOverride).toHaveBeenCalledOnce();
      expect(onSetOverride).toHaveBeenCalledWith("audio_probe");
      expect(screen.getByTestId("readiness-blocks-audio_probe")).toHaveTextContent("Blocks launches");
    });

    it("without onSetOverride, no set button is rendered", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              id: "audio_probe",
              status: "fail",
              blocks: { scope: "host", enforced_by: "control_plane" },
            }),
          ]}
          gate={{
            state: "active",
            blocking: [
              {
                check_id: "audio_probe",
                scope: "host",
                gpu_index: null,
                enforced_by: "control_plane",
                overridden: false,
              },
            ],
          }}
        />,
      );
      expect(screen.queryByTestId("readiness-override-set-audio_probe")).not.toBeInTheDocument();
    });

    it("with enforced_by: agent, no set button is rendered even with onSetOverride", () => {
      const onSetOverride = vi.fn();
      render(
        <ReadinessCard
          checks={[
            check({
              id: "runtime_endpoint",
              status: "fail",
              blocks: { scope: "host", enforced_by: "agent" },
            }),
          ]}
          gate={{
            state: "active",
            blocking: [
              {
                check_id: "runtime_endpoint",
                scope: "host",
                gpu_index: null,
                enforced_by: "agent",
                overridden: false,
              },
            ],
          }}
          onSetOverride={onSetOverride}
        />,
      );
      expect(screen.queryByTestId("readiness-override-set-runtime_endpoint")).not.toBeInTheDocument();
    });

    it("when check id is not in gate.blocking, no set button is rendered", () => {
      const onSetOverride = vi.fn();
      render(
        <ReadinessCard
          checks={[
            check({
              id: "audio_probe",
              status: "fail",
              blocks: { scope: "host", enforced_by: "control_plane" },
            }),
          ]}
          gate={{ state: "active", blocking: [] }}
          onSetOverride={onSetOverride}
        />,
      );
      expect(screen.queryByTestId("readiness-override-set-audio_probe")).not.toBeInTheDocument();
    });

    it("when overridden, shows Overridden by admin marker with creator and date, no set button, and Withdraw override button", () => {
      const onClearOverride = vi.fn();
      const createdAt = "2026-09-19T10:00:00Z";
      const expectedDateString = new Date(createdAt).toLocaleString();
      render(
        <ReadinessCard
          checks={[
            check({
              id: "audio_probe",
              status: "fail",
              blocks: { scope: "host", enforced_by: "control_plane" },
            }),
          ]}
          gate={{
            state: "active",
            blocking: [
              {
                check_id: "audio_probe",
                scope: "host",
                gpu_index: null,
                enforced_by: "control_plane",
                overridden: true,
              },
            ],
          }}
          overrides={[
            {
              check_id: "audio_probe",
              created_by: "u1",
              created_by_username: "alice",
              created_at: createdAt,
              inert: false,
            },
          ]}
          onSetOverride={vi.fn()}
          onClearOverride={onClearOverride}
        />,
      );
      const row = screen.getByTestId("readiness-check-audio_probe");
      expect(within(row).getByRole("img", { name: "Fail" })).toBeInTheDocument();
      const marker = screen.getByTestId("readiness-overridden-audio_probe");
      expect(marker).toHaveTextContent("Overridden by admin");
      expect(marker).toHaveAttribute("title");
      expect(marker.getAttribute("title")).toContain("alice");
      expect(marker.getAttribute("title")).toContain(expectedDateString);
      expect(screen.queryByTestId("readiness-blocks-audio_probe")).not.toBeInTheDocument();
      expect(screen.queryByTestId("readiness-override-set-audio_probe")).not.toBeInTheDocument();
      const clearBtn = screen.getByTestId("readiness-override-clear-audio_probe");
      expect(clearBtn).toHaveAccessibleName("Withdraw override");
      clearBtn.click();
      expect(onClearOverride).toHaveBeenCalledOnce();
      expect(onClearOverride).toHaveBeenCalledWith("audio_probe");
    });

    it("with overridden and created_by_username: null, title does not contain null string", () => {
      const createdAt = "2026-09-19T10:00:00Z";
      render(
        <ReadinessCard
          checks={[
            check({
              id: "audio_probe",
              status: "fail",
              blocks: { scope: "host", enforced_by: "control_plane" },
            }),
          ]}
          gate={{
            state: "active",
            blocking: [
              {
                check_id: "audio_probe",
                scope: "host",
                gpu_index: null,
                enforced_by: "control_plane",
                overridden: true,
              },
            ],
          }}
          overrides={[
            {
              check_id: "audio_probe",
              created_by: "u1",
              created_by_username: null,
              created_at: createdAt,
              inert: false,
            },
          ]}
          onClearOverride={vi.fn()}
        />,
      );
      const marker = screen.getByTestId("readiness-overridden-audio_probe");
      const title = marker.getAttribute("title") || "";
      expect(title).not.toContain("null");
      expect(title).toContain(new Date(createdAt).toLocaleString());
    });

    it("when overridden but without onClearOverride, the Overridden marker is shown but no clear button", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              id: "audio_probe",
              status: "fail",
              blocks: { scope: "host", enforced_by: "control_plane" },
            }),
          ]}
          gate={{
            state: "active",
            blocking: [
              {
                check_id: "audio_probe",
                scope: "host",
                gpu_index: null,
                enforced_by: "control_plane",
                overridden: true,
              },
            ],
          }}
          overrides={[
            {
              check_id: "audio_probe",
              created_by: "u1",
              created_by_username: "alice",
              created_at: "2026-09-19T10:00:00Z",
              inert: false,
            },
          ]}
        />,
      );
      expect(screen.getByTestId("readiness-overridden-audio_probe")).toBeInTheDocument();
      expect(screen.queryByTestId("readiness-override-clear-audio_probe")).not.toBeInTheDocument();
    });

    it("with overridePending matching the set button's check id, the set button is disabled", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              id: "audio_probe",
              status: "fail",
              blocks: { scope: "host", enforced_by: "control_plane" },
            }),
          ]}
          gate={{
            state: "active",
            blocking: [
              {
                check_id: "audio_probe",
                scope: "host",
                gpu_index: null,
                enforced_by: "control_plane",
                overridden: false,
              },
            ],
          }}
          onSetOverride={vi.fn()}
          overridePending="audio_probe"
        />,
      );
      const btn = screen.getByTestId("readiness-override-set-audio_probe");
      expect(btn).toBeDisabled();
    });

    it("with overridePending matching the clear button's check id, the clear button is disabled", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              id: "audio_probe",
              status: "fail",
              blocks: { scope: "host", enforced_by: "control_plane" },
            }),
          ]}
          gate={{
            state: "active",
            blocking: [
              {
                check_id: "audio_probe",
                scope: "host",
                gpu_index: null,
                enforced_by: "control_plane",
                overridden: true,
              },
            ],
          }}
          overrides={[
            {
              check_id: "audio_probe",
              created_by: "u1",
              created_by_username: "alice",
              created_at: "2026-09-19T10:00:00Z",
              inert: false,
            },
          ]}
          onClearOverride={vi.fn()}
          overridePending="audio_probe"
        />,
      );
      const btn = screen.getByTestId("readiness-override-clear-audio_probe");
      expect(btn).toBeDisabled();
    });

    it("with a different overridePending id, buttons remain enabled", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              id: "audio_probe",
              status: "fail",
              blocks: { scope: "host", enforced_by: "control_plane" },
            }),
          ]}
          gate={{
            state: "active",
            blocking: [
              {
                check_id: "audio_probe",
                scope: "host",
                gpu_index: null,
                enforced_by: "control_plane",
                overridden: false,
              },
            ],
          }}
          onSetOverride={vi.fn()}
          overridePending="other_probe"
        />,
      );
      const btn = screen.getByTestId("readiness-override-set-audio_probe");
      expect(btn).not.toBeDisabled();
    });

    it("with inert overrides, shows a section with the check id, explanatory text, and a clear button", () => {
      const onClearOverride = vi.fn();
      render(
        <ReadinessCard
          checks={[
            check({
              id: "render_node",
              status: "pass",
            }),
          ]}
          overrides={[
            {
              check_id: "old_probe",
              created_by: "u1",
              created_by_username: "alice",
              created_at: "2026-09-19T10:00:00Z",
              inert: true,
            },
          ]}
          onClearOverride={onClearOverride}
        />,
      );
      const section = screen.getByTestId("readiness-inert-overrides");
      expect(section).toBeInTheDocument();
      expect(section).toHaveTextContent("old_probe");
      expect(section).toHaveTextContent("This host no longer reports this check, so the override does nothing.");
      const clearBtn = screen.getByTestId("readiness-override-clear-old_probe");
      expect(clearBtn).toHaveAccessibleName("Withdraw override");
      clearBtn.click();
      expect(onClearOverride).toHaveBeenCalledWith("old_probe");
    });

    it("with no inert overrides, the readiness-inert-overrides element is absent", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              id: "render_node",
              status: "pass",
            }),
          ]}
          overrides={[]}
        />,
      );
      expect(screen.queryByTestId("readiness-inert-overrides")).not.toBeInTheDocument();
    });

    it("with inert overrides but without onClearOverride, lists the check but has no button", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              id: "render_node",
              status: "pass",
            }),
          ]}
          overrides={[
            {
              check_id: "old_probe",
              created_by: "u1",
              created_by_username: "alice",
              created_at: "2026-09-19T10:00:00Z",
              inert: true,
            },
          ]}
        />,
      );
      const section = screen.getByTestId("readiness-inert-overrides");
      expect(section).toHaveTextContent("old_probe");
      expect(screen.queryByTestId("readiness-override-clear-old_probe")).not.toBeInTheDocument();
    });

    it("with gate.state: abstaining, renders an abstaining marker", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              id: "audio_probe",
              status: "fail",
              blocks: { scope: "host", enforced_by: "control_plane" },
            }),
          ]}
          gate={{
            state: "abstaining",
            blocking: [
              {
                check_id: "audio_probe",
                scope: "host",
                gpu_index: null,
                enforced_by: "control_plane",
                overridden: false,
              },
            ],
          }}
        />,
      );
      const marker = screen.getByTestId("readiness-gate-abstaining");
      expect(marker).toHaveTextContent("This report is stale, so nothing is blocked until the host reports again.");
    });

    it("with gate.state: active (or no gate), the abstaining marker is absent", () => {
      const { unmount } = render(
        <ReadinessCard
          checks={[
            check({
              id: "audio_probe",
              status: "fail",
              blocks: { scope: "host", enforced_by: "control_plane" },
            }),
          ]}
          gate={{
            state: "active",
            blocking: [
              {
                check_id: "audio_probe",
                scope: "host",
                gpu_index: null,
                enforced_by: "control_plane",
                overridden: false,
              },
            ],
          }}
        />,
      );
      expect(screen.queryByTestId("readiness-gate-abstaining")).not.toBeInTheDocument();
      unmount();
      render(
        <ReadinessCard
          checks={[
            check({
              id: "audio_probe",
              status: "fail",
              blocks: { scope: "host", enforced_by: "control_plane" },
            }),
          ]}
        />,
      );
      expect(screen.queryByTestId("readiness-gate-abstaining")).not.toBeInTheDocument();
    });

    // A stored override is held through warn, unknown and skip (only a pass lapses
    // it), so it is neither in `blocking` nor inert. It must still be visible and
    // withdrawable, or it silently lifts the next failure.
    it.each(["warn", "unknown", "skip"])(
      "shows and can withdraw a held override on a check that is now %s",
      (status) => {
        const onClearOverride = vi.fn();
        render(
          <ReadinessCard
            checks={[
              check({
                id: "homes_free_space",
                status,
                blocks: { scope: "homes", enforced_by: "control_plane" },
              }),
            ]}
            gate={{ state: "active", blocking: [] }}
            overrides={[
              {
                check_id: "homes_free_space",
                created_by: "u1",
                created_by_username: "alice",
                created_at: "2026-09-19T10:00:00Z",
                inert: false,
              },
            ]}
            onSetOverride={vi.fn()}
            onClearOverride={onClearOverride}
          />,
        );
        const row = screen.getByTestId("readiness-check-homes_free_space");
        const marker = within(row).getByTestId("readiness-overridden-homes_free_space");
        expect(marker).toHaveTextContent(/^Overridden by admin$/);
        expect(marker.getAttribute("title")).toContain("alice");
        // It is not failing, so it still reads as a check that can block.
        expect(within(row).getByTestId("readiness-blocks-homes_free_space")).toHaveTextContent(/^Can block launches$/);
        expect(within(row).queryByTestId("readiness-override-set-homes_free_space")).not.toBeInTheDocument();
        within(row).getByTestId("readiness-override-clear-homes_free_space").click();
        expect(onClearOverride).toHaveBeenCalledWith("homes_free_space");
        expect(screen.queryByTestId("readiness-inert-overrides")).not.toBeInTheDocument();
      },
    );

    it("shows the Needs attention chip for an overridden failing check", () => {
      render(
        <ReadinessCard
          checks={[
            check({
              id: "audio_probe",
              status: "fail",
              blocks: { scope: "host", enforced_by: "control_plane" },
            }),
          ]}
          gate={{
            state: "active",
            blocking: [
              {
                check_id: "audio_probe",
                scope: "host",
                gpu_index: null,
                enforced_by: "control_plane",
                overridden: true,
              },
            ],
          }}
          overrides={[
            {
              check_id: "audio_probe",
              created_by: "u1",
              created_by_username: "alice",
              created_at: "2026-09-19T10:00:00Z",
              inert: false,
            },
          ]}
        />,
      );
      expect(screen.getByText("Needs attention")).toBeInTheDocument();
    });
  });
});
