import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "../../../../api/client";

vi.mock("../../../../auth/context", () => ({ useAuth: () => ({ token: "admin-token" }) }));
vi.mock("../../../../api/admin", () => ({ getHostPolicy: vi.fn(), updateHostPolicy: vi.fn() }));

import * as api from "../../../../api/admin";
import { IdleTimeoutPolicy } from "./IdleTimeoutPolicy";

const view = (status: "pending" | "failed" | "applied" | "upgrade_required" = "pending", remedy?: string) => ({
  revision: "1", choices: { idle_timeout_secs: { source: "deployment" } }, resolved: {},
  groups: { idle_timeout_secs: { desired_revision: "1", applied_revision: null, desired_digest: "d", scope: "next_session", status, fresh: false, observed_at: null, remedy: remedy ?? (status === "failed" ? "Retry after reconnect." : null), approval_preview: null } },
  image_preparation: { status: "unknown", observed_at: null, remedy: null }, readiness: { status: "unknown", observed_at: null, remedy: null },
});

describe("idle timeout policy", () => {
  beforeEach(() => { vi.clearAllMocks(); vi.mocked(api.getHostPolicy).mockResolvedValue(view() as never); });

  it("saves explicit intent and keeps application pending until readback", async () => {
    vi.mocked(api.updateHostPolicy).mockResolvedValue({ ...view(), revision: "2", choices: { idle_timeout_secs: { source: "explicit", value: 900 } } } as never);
    render(<IdleTimeoutPolicy hostId="host-1" onAvailable={vi.fn()} />);
    await screen.findByText(/Application: pending/);
    expect(screen.queryByRole("option", { name: /Automatic/ })).toBeNull();
    fireEvent.change(screen.getByLabelText("Source"), { target: { value: "explicit" } });
    fireEvent.change(screen.getByLabelText("Seconds"), { target: { value: "900" } });
    fireEvent.click(screen.getByRole("button", { name: "Save idle timeout" }));
    await waitFor(() => expect(api.updateHostPolicy).toHaveBeenCalledWith("admin-token", "host-1", "1", { idle_timeout_secs: { source: "explicit", value: 900 } }));
    expect(await screen.findByRole("status")).toHaveTextContent(/Saved intent.*confirm application/);
  });

  it("shows a stale-edit remedy with the refreshed current view", async () => {
    vi.mocked(api.updateHostPolicy).mockRejectedValue(new ApiError(409, "stale_revision", "stale"));
    vi.mocked(api.getHostPolicy).mockResolvedValueOnce(view() as never).mockResolvedValueOnce({ ...view(), revision: "2" } as never);
    render(<IdleTimeoutPolicy hostId="host-1" onAvailable={vi.fn()} />);
    await screen.findByText(/Application: pending/);
    fireEvent.change(screen.getByLabelText("Source"), { target: { value: "explicit" } });
    fireEvent.click(screen.getByRole("button", { name: "Save idle timeout" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/changed elsewhere/);
  });

  it("shows failure and remedy separately from saved source", async () => {
    vi.mocked(api.getHostPolicy).mockResolvedValue(view("failed") as never);
    render(<IdleTimeoutPolicy hostId="host-1" onAvailable={vi.fn()} />);
    expect(await screen.findByText(/Application: failed/)).toBeTruthy();
    expect(screen.getByText("Retry after reconnect.")).toBeTruthy();
    expect(screen.getByText(/Source: deployment/)).toBeTruthy();
  });

  it.each([
    "The legacy writer remains active for this group. Upgrade the agent to enable RH05 verification.",
    "Typed ownership remains protected after agent downgrade. Re-upgrade the agent or repair ownership; the legacy value is not sent.",
  ])("keeps upgrade-required policy read-only and exposes its remedy: %s", async (remedy) => {
    vi.mocked(api.getHostPolicy).mockResolvedValue(view("upgrade_required", remedy) as never);
    const onAvailable = vi.fn();
    render(<IdleTimeoutPolicy hostId="host-1" onAvailable={onAvailable} />);
    expect(await screen.findByText(remedy)).toBeTruthy();
    expect(onAvailable).toHaveBeenCalledWith(false);
    expect(screen.queryByRole("button", { name: "Save idle timeout" })).toBeNull();
    expect(screen.getByText(/Application: upgrade required/)).toBeTruthy();
  });
});
