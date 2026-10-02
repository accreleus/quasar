import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AuthContext, type AuthContextValue } from "../../../auth/context";
import { ApiError } from "../../../api/client";
import * as adminApi from "../../../api/admin";
import { ImageCleanupModal } from "./ImageCleanupModal";

vi.mock("../../../api/admin");

const auth: AuthContextValue = {
  status: "authenticated", user: { id: "u1", email: "admin@example.com", username: "admin", role: "admin" },
  token: "tok", isAdmin: true, sessionExpired: false,
  login: vi.fn(), claim: vi.fn(), logout: vi.fn(),
};
const candidate = {
  image_id: "retired-image", version: "old", image_ref: "example/retired@sha256:old",
  runtime_image_id: "sha256:old", generation: "3", eligible: true, reasons: [], remedy: null,
};
const attempt = {
  attempt_id: "a1", image_id: candidate.image_id, version: candidate.version,
  image_ref: candidate.image_ref, runtime_image_id: candidate.runtime_image_id,
  generation: "4", state: "removing" as const, reason: null,
};

function renderModal() {
  return render(<AuthContext.Provider value={auth}>
    <ImageCleanupModal token="tok" hostID="h1" hostName="node-1" onClose={vi.fn()} />
  </AuthContext.Provider>);
}

async function requestRemoval() {
  fireEvent.click(await screen.findByRole("button", { name: "Review removal" }));
  fireEvent.click(screen.getByRole("button", { name: "Remove this cached version" }));
  await waitFor(() => expect(adminApi.requestHostImageCleanup).toHaveBeenCalledTimes(1));
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(adminApi.getHostImageCleanup).mockResolvedValue({
    host_id: "h1", inventory_status: "current", observed_at: "2026-09-24T00:00:00Z",
    remedy: null, images: [candidate],
  } as never);
  vi.mocked(adminApi.requestHostImageCleanup).mockResolvedValue(attempt as never);
});
afterEach(cleanup);

describe("ImageCleanupModal", () => {
  it("recovers automatically after a transient status read failure", async () => {
    vi.mocked(adminApi.getHostImageCleanupAttempt)
      .mockRejectedValueOnce(new ApiError(503, "unavailable", "temporarily unavailable"))
      .mockResolvedValue({ ...attempt, state: "removed" } as never);
    renderModal();
    await requestRemoval();

    expect(await screen.findByText(/Could not check the cleanup outcome/)).toBeTruthy();
    expect(screen.queryByText(/Removal confirmed/i)).toBeNull();
    expect(await screen.findByText(/Removal confirmed/i, {}, { timeout: 5000 })).toBeTruthy();
    expect(adminApi.getHostImageCleanupAttempt).toHaveBeenCalledTimes(2);
  });

  it("keeps an uncertain attempt pending until a durable status read confirms removal", async () => {
    vi.mocked(adminApi.getHostImageCleanupAttempt)
      .mockResolvedValueOnce({ ...attempt, state: "unknown" } as never)
      .mockResolvedValue({ ...attempt, state: "removed" } as never);
    renderModal();
    await requestRemoval();

    expect(await screen.findByText(/outcome is uncertain/i)).toBeTruthy();
    expect(screen.queryByText(/Removal confirmed/i)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Check removal status" }));
    expect(await screen.findByText(/Removal confirmed/i)).toBeTruthy();
    expect(adminApi.getHostImageCleanupAttempt).toHaveBeenCalledWith("tok", "h1", "a1", expect.any(AbortSignal));
  });

  it("shows the safe failure reason and prompts a fresh preview", async () => {
    vi.mocked(adminApi.getHostImageCleanupAttempt).mockResolvedValue({
      ...attempt, state: "failed", reason: "reference_in_use",
    } as never);
    renderModal();
    await requestRemoval();
    expect(await screen.findByText(/Removal failed/i)).toBeTruthy();
    expect(screen.getByText(/container reference/i)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Refresh inventory" })).toBeTruthy();
    expect(screen.queryByText(/Removal confirmed/i)).toBeNull();
  });

  it("refreshes candidate protection when a removal finishes", async () => {
    vi.mocked(adminApi.getHostImageCleanup)
      .mockResolvedValueOnce({ host_id: "h1", inventory_status: "current", observed_at: "2026-09-24T00:00:00Z", remedy: null, images: [candidate] } as never)
      .mockResolvedValueOnce({ host_id: "h1", inventory_status: "current", observed_at: "2026-09-24T00:00:01Z", remedy: null, images: [{ ...candidate, eligible: false, reasons: ["removing"] }] } as never)
      .mockResolvedValue({ host_id: "h1", inventory_status: "current", observed_at: "2026-09-24T00:00:02Z", remedy: null, images: [{ ...candidate, eligible: false, reasons: ["container_reference"] }] } as never);
    vi.mocked(adminApi.getHostImageCleanupAttempt).mockResolvedValue({ ...attempt, state: "failed", reason: "reference_in_use" } as never);

    renderModal();
    await requestRemoval();
    expect(await screen.findByText(/Removal failed/i)).toBeTruthy();
    expect(await screen.findByText(/Used by a container, including a stopped container/i)).toBeTruthy();
    expect(screen.queryByText(/Removal is already in progress/i)).toBeNull();
  });

  it("does not infer success when the attempt read has been pruned", async () => {
    vi.mocked(adminApi.getHostImageCleanupAttempt).mockRejectedValue(
      new ApiError(404, "not_found", "not found"),
    );
    renderModal();
    await requestRemoval();
    expect(await screen.findByText(/prior cleanup outcome is unavailable/i)).toBeTruthy();
    expect(screen.queryByText(/Removal confirmed/i)).toBeNull();
  });
});
