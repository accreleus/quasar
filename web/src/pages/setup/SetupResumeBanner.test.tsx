/**
 * SetupResumeBanner — the /admin resume/skip offer. It reads the SHARED
 * setup-status cache, so Skip must update that context (retiring the banner
 * in the same render pass) and a failed Skip must degrade without crashing.
 */

import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { SetupResumeBanner } from "./SetupResumeBanner";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { SetupStatusProvider } from "../../setup/useSetupStatus";

vi.mock("../../api/setup", () => ({
  getSetupStatus: vi.fn(),
  completeSetup: vi.fn(),
}));

import * as setupApi from "../../api/setup";

function renderBanner() {
  const authValue: AuthContextValue = {
    status: "authenticated",
    user: { id: "u1", email: "admin@example.com", username: "admin", role: "admin" },
    token: "tok",
    isAdmin: true,
    login: vi.fn(),
    claim: vi.fn(),
    logout: vi.fn(),
  };
  return render(
    <MemoryRouter>
      <AuthContext.Provider value={authValue}>
        <SetupStatusProvider>
          <SetupResumeBanner />
        </SetupStatusProvider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

describe("SetupResumeBanner", () => {
  beforeEach(() => {
    vi.mocked(setupApi.getSetupStatus).mockReset();
    vi.mocked(setupApi.completeSetup).mockReset();
  });

  afterEach(() => {
    // `restoreAllMocks` only restores `vi.spyOn` spies since vitest 3; clear call
    // history too, or a plain `vi.fn()`'s counters survive into the next test.
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it("renders nothing when setup is complete", async () => {
    vi.mocked(setupApi.getSetupStatus).mockResolvedValue({ admin_exists: true, setup_completed: true });
    renderBanner();

    await waitFor(() => expect(setupApi.getSetupStatus).toHaveBeenCalled());
    expect(screen.queryByText(/first-run setup/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("renders the resume/skip offer while setup is incomplete", async () => {
    vi.mocked(setupApi.getSetupStatus).mockResolvedValue({ admin_exists: true, setup_completed: false });
    renderBanner();

    expect(await screen.findByText(/first-run setup isn.t finished/i)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /resume setup/i })).toHaveAttribute("href", "/setup");
    expect(screen.getByRole("button", { name: /^skip$/i })).toBeEnabled();
  });

  it("Skip calls POST /v1/setup/complete and retires the banner via the shared context", async () => {
    vi.mocked(setupApi.getSetupStatus).mockResolvedValue({ admin_exists: true, setup_completed: false });
    vi.mocked(setupApi.completeSetup).mockResolvedValue({ admin_exists: true, setup_completed: true });
    renderBanner();

    fireEvent.click(await screen.findByRole("button", { name: /^skip$/i }));

    await waitFor(() => {
      expect(setupApi.completeSetup).toHaveBeenCalledWith("tok");
    });
    // The shared context got the server's response — the banner is gone
    // without any re-fetch of /v1/setup/status.
    await waitFor(() => {
      expect(screen.queryByText(/first-run setup/i)).not.toBeInTheDocument();
    });
    expect(setupApi.getSetupStatus).toHaveBeenCalledTimes(1);
  });

  it("a failed Skip does not crash the banner — it stays offered and re-clickable", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    vi.mocked(setupApi.getSetupStatus).mockResolvedValue({ admin_exists: true, setup_completed: false });
    vi.mocked(setupApi.completeSetup).mockRejectedValue(new Error("network down"));
    renderBanner();

    fireEvent.click(await screen.findByRole("button", { name: /^skip$/i }));

    await waitFor(() => {
      expect(setupApi.completeSetup).toHaveBeenCalled();
    });
    // Banner still present, button recovered from "Skipping…".
    expect(await screen.findByRole("button", { name: /^skip$/i })).toBeEnabled();
    expect(screen.getByText(/first-run setup isn.t finished/i)).toBeInTheDocument();
    expect(warn).toHaveBeenCalled();
  });
});
