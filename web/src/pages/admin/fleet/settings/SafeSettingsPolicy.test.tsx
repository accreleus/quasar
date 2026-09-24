import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "../../../../api/client";
import type { ConfigKnob } from "../../../../api/types";

vi.mock("../../../../auth/context", () => ({ useAuth: () => ({ token: "admin-token" }) }));
vi.mock("../../../../api/admin", () => ({ getHostPolicy: vi.fn(), updateHostPolicy: vi.fn(), retryHostPolicyGroup: vi.fn() }));

import * as api from "../../../../api/admin";
import { SafeSettingsPolicy } from "./SafeSettingsPolicy";

const knobs: ConfigKnob[] = [
  { key: "gop", type: "int", default: 60, min: 1, nullable: false, class: "live", env_var: "QUASAR_GOP" },
  { key: "abr_mode", type: "enum", default: "smooth", enum: ["off", "protective", "smooth"], nullable: false, class: "live", env_var: "QUASAR_ABR_MODE" },
  { key: "zerocopy", type: "bool", default: false, nullable: false, class: "live", env_var: "QUASAR_ZEROCOPY" },
  { key: "slices", type: "int", default: 8, min: 1, nullable: false, class: "live", env_var: "QUASAR_SLICES" },
  { key: "encoder", type: "enum", default: "openh264", enum: ["openh264", "va", "nvenc", "vulkan"], nullable: false, class: "restart", env_var: "QUASAR_ENCODER" },
] as ConfigKnob[];

type Status = "pending" | "applied" | "failed" | "upgrade_required" | "uncertain";
const group = (status: Status, extra: Record<string, unknown> = {}) => ({
  desired_revision: "4", applied_revision: status === "applied" ? "4" : null, desired_digest: "d", scope: "next_session",
  status, fresh: status === "applied", observed_at: status === "applied" ? "2026-09-23T10:00:00Z" : null, remedy: null, approval_preview: null, ...extra,
});
const view = (groups: Record<string, ReturnType<typeof group>> = {}) => ({
  revision: "4",
  choices: { gop: { source: "explicit", value: 90 }, abr_mode: { source: "deployment" }, zerocopy: { source: "deployment" }, slices: { source: "deployment" }, encoder: { source: "deployment" } },
  resolved: {
    gop: { value: 90, source: "explicit", observed_at: null, evidence_id: null },
    abr_mode: { value: "smooth", source: "deployment", observed_at: null, evidence_id: "conn-1" },
    zerocopy: { value: false, source: "deployment", observed_at: null, evidence_id: "conn-1" },
    slices: { value: null, source: "deployment", observed_at: null, evidence_id: null },
    encoder: { value: null, source: "deployment", observed_at: null, evidence_id: null },
  },
  groups: { gop: group("applied"), abr_mode: group("pending"), zerocopy: group("applied"), slices: group("upgrade_required"), ...groups },
  image_preparation: { status: "unknown", observed_at: null, remedy: null }, readiness: { status: "unknown", observed_at: null, remedy: null },
});

function renderPolicy(onOwnedKeys = vi.fn()) {
  render(<SafeSettingsPolicy hostId="host-1" knobs={knobs} renderNodeOptions={[]} onOwnedKeys={onOwnedKeys} />);
  return onOwnedKeys;
}

describe("safe next-session settings", () => {
  beforeEach(() => { vi.clearAllMocks(); vi.mocked(api.getHostPolicy).mockResolvedValue(view() as never); });

  it("shows provenance, freshness and desired/applied scope per typed group, excluding restart and unowned keys", async () => {
    const onOwnedKeys = renderPolicy();
    const gop = await screen.findByRole("group", { name: "GOP length" });
    expect(within(gop).getByText("applied")).toBeTruthy();
    expect(within(gop).getByText(/Explicit value 90/)).toBeTruthy();
    expect(within(gop).getByText(/Verified on the current connection/)).toBeTruthy();
    expect(within(gop).getByText(/Desired revision 4 · applied revision 4 · next session/)).toBeTruthy();
    const mode = screen.getByRole("group", { name: "Adaptation mode" });
    expect(within(mode).getByText("pending")).toBeTruthy();
    expect(within(mode).getByText(/Deployment setting smooth/)).toBeTruthy();
    expect(within(mode).getByText(/Not yet verified/)).toBeTruthy();
    expect(screen.queryByRole("group", { name: "Encoder" })).toBeNull();
    expect(screen.queryByRole("group", { name: "Encoder slices" })).toBeNull();
    expect(screen.queryByRole("option", { name: /Automatic/ })).toBeNull();
    await waitFor(() => expect(onOwnedKeys).toHaveBeenLastCalledWith(new Set(["gop", "abr_mode", "zerocopy"])));
  });

  it("lets an operator save Automatic hardware through the revisioned writer", async () => {
    const hardware = group("pending", { scope: "restart", approval_preview: {
      available: true, resolved: { encoder: "vulkan", render_node: "/dev/dri/renderD129" },
    } });
    vi.mocked(api.getHostPolicy).mockResolvedValue(view({ hardware }) as never);
    vi.mocked(api.updateHostPolicy).mockResolvedValue(view({ hardware }) as never);
    const onOwnedKeys = renderPolicy();
    const encoder = await screen.findByRole("group", { name: "Encoder" });
    expect(within(encoder).getByText(/Current hardware evidence supports encoder: vulkan/)).toBeTruthy();
    fireEvent.change(within(encoder).getByLabelText("Encoder source"), { target: { value: "automatic" } });
    fireEvent.click(screen.getByRole("button", { name: "Save policy settings" }));
    await waitFor(() => expect(api.updateHostPolicy).toHaveBeenCalledWith("admin-token", "host-1", "4", {
      encoder: { source: "automatic" },
    }));
    await waitFor(() => expect(onOwnedKeys).toHaveBeenLastCalledWith(new Set(["gop", "abr_mode", "zerocopy", "encoder"])));
  });

  it("saves several groups as one revisioned edit and leaves application to each group", async () => {
    vi.mocked(api.updateHostPolicy).mockResolvedValue(view({ gop: group("pending"), abr_mode: group("pending") }) as never);
    renderPolicy();
    const gop = await screen.findByRole("group", { name: "GOP length" });
    fireEvent.change(within(gop).getByLabelText("GOP length value"), { target: { value: "120" } });
    const mode = screen.getByRole("group", { name: "Adaptation mode" });
    fireEvent.change(within(mode).getByLabelText("Adaptation mode source"), { target: { value: "explicit" } });
    fireEvent.change(within(mode).getByLabelText("Adaptation mode value"), { target: { value: "protective" } });
    fireEvent.click(screen.getByRole("button", { name: "Save next-session settings" }));
    await waitFor(() => expect(api.updateHostPolicy).toHaveBeenCalledWith("admin-token", "host-1", "4", {
      gop: { source: "explicit", value: 120 }, abr_mode: { source: "explicit", value: "protective" },
    }));
    expect(await screen.findByRole("status")).toHaveTextContent(/Saved.*each setting reports/i);
  });

  it("returns a deployment choice without a value", async () => {
    vi.mocked(api.updateHostPolicy).mockResolvedValue(view() as never);
    renderPolicy();
    const gop = await screen.findByRole("group", { name: "GOP length" });
    fireEvent.change(within(gop).getByLabelText("GOP length source"), { target: { value: "deployment" } });
    fireEvent.click(screen.getByRole("button", { name: "Save next-session settings" }));
    await waitFor(() => expect(api.updateHostPolicy).toHaveBeenCalledWith("admin-token", "host-1", "4", { gop: { source: "deployment" } }));
  });

  it("rejects the whole edit with the server's reason and keeps the draft", async () => {
    vi.mocked(api.updateHostPolicy).mockRejectedValue(new ApiError(400, "validation_failed", '"gop" must be >= 1'));
    renderPolicy();
    const gop = await screen.findByRole("group", { name: "GOP length" });
    fireEvent.change(within(gop).getByLabelText("GOP length value"), { target: { value: "0" } });
    fireEvent.click(screen.getByRole("button", { name: "Save next-session settings" }));
    expect(await screen.findByRole("alert")).toHaveTextContent('"gop" must be >= 1');
    expect((within(gop).getByLabelText("GOP length value") as HTMLInputElement).value).toBe("0");
  });

  it("reloads the current view after a concurrent edit", async () => {
    vi.mocked(api.updateHostPolicy).mockRejectedValue(new ApiError(409, "stale_revision", "stale"));
    vi.mocked(api.getHostPolicy).mockResolvedValueOnce(view() as never).mockResolvedValue({ ...view(), revision: "5" } as never);
    renderPolicy();
    const gop = await screen.findByRole("group", { name: "GOP length" });
    fireEvent.change(within(gop).getByLabelText("GOP length value"), { target: { value: "75" } });
    fireEvent.click(screen.getByRole("button", { name: "Save next-session settings" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/changed elsewhere/);
    await waitFor(() => expect(api.getHostPolicy).toHaveBeenCalledTimes(2));
  });

  it("offers Retry only after the transient budget is exhausted", async () => {
    vi.mocked(api.getHostPolicy).mockResolvedValue(view({
      gop: group("failed", { remedy: "retry_exhausted: The host did not confirm this setting after 5 attempts. Use Retry, or reconnect the host." }),
      zerocopy: group("failed", { remedy: "validation_failed: The host rejected this value. Change the setting; it is not retried automatically." }),
      abr_mode: group("pending", { remedy: "Waiting for the host to verify the next-session setting (attempt 2 of 5).", next_retry_at: "2026-09-23T10:00:10Z" }),
    }) as never);
    vi.mocked(api.retryHostPolicyGroup).mockResolvedValue(view() as never);
    renderPolicy();
    const gop = await screen.findByRole("group", { name: "GOP length" });
    expect(within(gop).getByText(/did not confirm this setting after 5 attempts/)).toBeTruthy();
    const zero = screen.getByRole("group", { name: "Zero-copy path" });
    expect(within(zero).getByText(/rejected this value/)).toBeTruthy();
    expect(within(zero).queryByRole("button", { name: "Retry" })).toBeNull();
    expect(within(screen.getByRole("group", { name: "Adaptation mode" })).getByText(/Next retry/)).toBeTruthy();
    fireEvent.click(within(gop).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(api.retryHostPolicyGroup).toHaveBeenCalledWith("admin-token", "host-1", "gop"));
  });

  it("explains a retry route this control plane does not serve yet", async () => {
    vi.mocked(api.getHostPolicy).mockResolvedValue(view({ gop: group("failed", { remedy: "retry_exhausted: after 5 attempts." }) }) as never);
    vi.mocked(api.retryHostPolicyGroup).mockRejectedValue(new ApiError(404, "not_found", "not found"));
    renderPolicy();
    const gop = await screen.findByRole("group", { name: "GOP length" });
    fireEvent.click(within(gop).getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/Reconnect the host or save the setting again/);
  });
});
