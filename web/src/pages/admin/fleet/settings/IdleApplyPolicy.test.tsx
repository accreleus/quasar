import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../../../../auth/context", () => ({ useAuth: () => ({ token: "admin-token" }) }));
vi.mock("../../../../api/admin", () => ({
  getHostPolicy: vi.fn(), getHostIdleApply: vi.fn(),
  approveHostIdleApply: vi.fn(), cancelHostIdleApply: vi.fn(),
}));

import * as api from "../../../../api/admin";
import { IdleApplyPolicy } from "./IdleApplyPolicy";

const preview = {
  available: true, revision: "1", content_sha256: "a".repeat(64), resolved: { encoder: "openh264" },
  prerequisites_sha256: "b".repeat(64), prerequisites: [],
  approval_boot_incarnation: "00000000-0000-4000-8000-000000000001",
  approval_review_id: "00000000-0000-4000-8000-000000000002", remedy: null,
};
const view = (attemptId: string | null = null) => ({
  revision: "1", choices: { encoder: { source: "explicit", value: "openh264" } }, resolved: {},
  groups: { hardware: { desired_revision: "1", applied_revision: null, desired_digest: "a".repeat(64), scope: "restart", status: "pending", fresh: false, remedy: null, attempt_id: attemptId, approval_preview: preview } },
  image_preparation: { status: "unknown", observed_at: null, remedy: null },
  readiness: { status: "unknown", observed_at: null, remedy: null },
});
const waiting = { attempt_id: "00000000-0000-4000-8000-000000000003", group: "hardware", revision: "1", content_sha256: preview.content_sha256,
  prerequisites_sha256: preview.prerequisites_sha256, phase: "waiting", started: false, admission_restricted: true, remedy: "Waiting for sessions.", next_retry_at: null };

describe("idle apply policy", () => {
  beforeEach(() => { vi.clearAllMocks(); vi.mocked(api.getHostPolicy).mockResolvedValue(view() as never); });

  it("shows an unavailable candidate through the group remedy without inventing review fields", async () => {
    const missing = view();
    missing.groups.hardware.approval_preview = null as never;
    missing.groups.hardware.remedy = "Current authenticated inventory is unavailable." as never;
    vi.mocked(api.getHostPolicy).mockResolvedValue(missing as never);
    render(<IdleApplyPolicy hostId="host-1" />);
    expect(await screen.findByText("Current authenticated inventory is unavailable.")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Approve idle wait" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("approves the reviewed tuple and shows waiting without claiming application", async () => {
    vi.mocked(api.approveHostIdleApply).mockResolvedValue(waiting as never);
    render(<IdleApplyPolicy hostId="host-1" />);
    expect(await screen.findByText(/Execution is unavailable/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Approve idle wait" }));
    await waitFor(() => expect(api.approveHostIdleApply).toHaveBeenCalledWith(
      "admin-token", "host-1", "hardware", preview, expect.any(String),
    ));
    expect(await screen.findByText(/Approval: waiting/)).toBeTruthy();
    expect(screen.getByText(/Saved configuration: pending/)).toBeTruthy();
    expect(screen.queryByText(/applied successfully/i)).toBeNull();
  });

  it("reloads an existing attempt and cancels without removing saved policy", async () => {
    vi.mocked(api.getHostPolicy).mockResolvedValue(view(waiting.attempt_id) as never);
    vi.mocked(api.getHostIdleApply).mockResolvedValue(waiting as never);
    vi.mocked(api.cancelHostIdleApply).mockResolvedValue({ ...waiting, phase: "revoked_unstarted", admission_restricted: false } as never);
    render(<IdleApplyPolicy hostId="host-1" />);
    expect(await screen.findByText(/Approval: waiting/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Cancel approval" }));
    await waitFor(() => expect(api.cancelHostIdleApply).toHaveBeenCalledWith("admin-token", "host-1", waiting.attempt_id));
    expect(await screen.findByText(/Approval: revoked unstarted/)).toBeTruthy();
    expect(screen.getByText(/Saved configuration: pending/)).toBeTruthy();
  });
});
