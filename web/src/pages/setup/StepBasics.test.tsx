/**
 * StepBasics — registration-mode load/save behavior.
 *
 * The load-failure tests encode a review finding (silent data corruption):
 * when GET /v1/admin/settings fails, the step must NOT fall back to a
 * writable "closed" default — on an existing instance configured `open` or
 * `invite_only`, a transient GET failure + Continue would silently overwrite
 * the real registration mode. A value never read from the server must never
 * be submitted back to it: the control stays unselected, Continue stays
 * disabled, and the operator either retries the read or deliberately picks
 * a mode knowing the current value is unknown.
 */

import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { StepBasics } from "./StepBasics";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { ApiError } from "../../api/client";

vi.mock("../../api/admin", () => ({
  getSettings: vi.fn(),
  accessCheck: vi.fn(),
  updateSettings: vi.fn(),
}));

import * as adminApi from "../../api/admin";

const settingsResponse = (registration_mode: string) =>
  ({ settings: { registration_mode } }) as never;

function renderStep(onNext = vi.fn()) {
  const authValue: AuthContextValue = {
    status: "authenticated",
    user: { id: "u1", email: "admin@example.com", username: "admin", role: "admin" },
    token: "tok",
    isAdmin: true,
    sessionExpired: false,
    login: vi.fn(),
    claim: vi.fn(),
    logout: vi.fn(),
  };
  render(
    <AuthContext.Provider value={authValue}>
      <StepBasics onNext={onNext} />
    </AuthContext.Provider>,
  );
  return onNext;
}

describe("StepBasics", () => {
  beforeEach(() => {
    vi.mocked(adminApi.getSettings).mockReset();
    vi.mocked(adminApi.accessCheck).mockResolvedValue({
      request: { host: new URL(window.location.origin).host },
      origins: { source: "database", allowed: [window.location.origin] },
    } as never);
    vi.mocked(adminApi.updateSettings).mockReset();
  });

  it("adds the detected browser origin while preserving existing origins on Continue", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settingsResponse("closed"));
    vi.mocked(adminApi.accessCheck).mockResolvedValueOnce({
      origins: { source: "database", allowed: ["https://existing.example"] },
    } as never);
    vi.mocked(adminApi.updateSettings).mockResolvedValue({} as never);
    const next = renderStep();
    await waitFor(() => expect(screen.getByRole("button", { name: /continue/i })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await waitFor(() => expect(next).toHaveBeenCalled());
    expect(adminApi.updateSettings).toHaveBeenCalledWith("tok", {
      registration_mode: "closed", allowed_origins: ["https://existing.example", window.location.origin],
    });
  });

  it("does not overwrite a rejecting environment pin or advance", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settingsResponse("closed"));
    vi.mocked(adminApi.accessCheck).mockResolvedValue({
      request: { host: "internal.example:8443" },
      origins: { source: "environment", allowed: [] },
    } as never);
    const next = renderStep();
    await waitFor(() => expect(screen.getByRole("button", { name: /continue/i })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("QUASAR_ALLOWED_ORIGINS"));
    expect(adminApi.updateSettings).not.toHaveBeenCalled();
    expect(next).not.toHaveBeenCalled();
  });

  it("keeps a matching explicit environment policy unchanged", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settingsResponse("closed"));
    vi.mocked(adminApi.accessCheck).mockResolvedValue({
      request: { host: "internal.example:8443" },
      origins: { source: "environment", allowed: [window.location.origin] },
    } as never);
    vi.mocked(adminApi.updateSettings).mockResolvedValue({} as never);
    const next = renderStep();
    await waitFor(() => expect(screen.getByRole("button", { name: /continue/i })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await waitFor(() => expect(next).toHaveBeenCalled());
    expect(adminApi.updateSettings).toHaveBeenCalledWith("tok", { registration_mode: "closed" });
  });

  it("does not save settings or advance when the access policy cannot be read", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settingsResponse("closed"));
    vi.mocked(adminApi.accessCheck).mockRejectedValue(new Error("Access check unavailable"));
    const next = renderStep();
    await waitFor(() => expect(screen.getByRole("button", { name: /continue/i })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Access check unavailable"));
    expect(adminApi.updateSettings).not.toHaveBeenCalled();
    expect(next).not.toHaveBeenCalled();
  });

  it("verifies the saved browser address is active before advancing", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settingsResponse("closed"));
    vi.mocked(adminApi.accessCheck).mockResolvedValue({
      origins: { source: "database", allowed: [] },
    } as never);
    vi.mocked(adminApi.updateSettings).mockResolvedValue({} as never);
    const next = renderStep();
    await waitFor(() => expect(screen.getByRole("button", { name: /continue/i })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("not active for streaming"));
    expect(next).not.toHaveBeenCalled();
  });

  it("shows a loading state (and a disabled Continue) while settings are in flight", () => {
    vi.mocked(adminApi.getSettings).mockReturnValue(new Promise(() => {}) as never);
    renderStep();

    expect(screen.getByText(/loading current settings/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /continue/i })).toBeDisabled();
    expect(screen.queryByRole("tablist")).not.toBeInTheDocument();
  });

  it("populates the control with the server's registration mode on a successful load", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settingsResponse("open"));
    renderStep();

    await waitFor(() => {
      expect(screen.getByRole("tab", { name: "Open", selected: true })).toBeInTheDocument();
    });
    expect(screen.getByRole("button", { name: /continue/i })).toBeEnabled();
  });

  it("shows the save error and does not advance when PATCH fails", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settingsResponse("closed"));
    vi.mocked(adminApi.updateSettings).mockRejectedValue(
      new ApiError(500, "internal", "could not persist settings"),
    );
    const onNext = renderStep();

    await waitFor(() => expect(screen.getByRole("tab", { name: "Closed", selected: true })).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));

    await waitFor(() => {
      expect(screen.getByRole("alert")).toHaveTextContent("could not persist settings");
    });
    expect(onNext).not.toHaveBeenCalled();
    // The form recovers — the operator can retry the save.
    expect(screen.getByRole("button", { name: /continue/i })).toBeEnabled();
  });

  it("a failed load does NOT fall back to a writable 'closed' — Continue stays disabled (review finding 1)", async () => {
    vi.mocked(adminApi.getSettings).mockRejectedValue(new ApiError(502, "bad_gateway", "settings fetch failed"));
    const onNext = renderStep();

    await waitFor(() => {
      expect(screen.getByRole("alert")).toHaveTextContent("settings fetch failed");
    });

    // No mode was ever read from the server, so nothing may be selected and
    // nothing may be submittable.
    for (const tab of screen.getAllByRole("tab")) {
      expect(tab).toHaveAttribute("aria-selected", "false");
    }
    expect(screen.getByRole("button", { name: /continue/i })).toBeDisabled();

    // Even a form submit event must not write anything back.
    fireEvent.submit(screen.getByRole("button", { name: /continue/i }).closest("form")!);
    expect(adminApi.updateSettings).not.toHaveBeenCalled();
    expect(onNext).not.toHaveBeenCalled();

    // An explicit retry path is offered.
    expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
  });

  it("after a failed load, retry re-fetches and populates the real server value", async () => {
    vi.mocked(adminApi.getSettings)
      .mockRejectedValueOnce(new ApiError(502, "bad_gateway", "settings fetch failed"))
      .mockResolvedValueOnce(settingsResponse("invite_only"));
    renderStep();

    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("settings fetch failed"));
    fireEvent.click(screen.getByRole("button", { name: /retry/i }));

    await waitFor(() => {
      expect(screen.getByRole("tab", { name: "Invite only", selected: true })).toBeInTheDocument();
    });
    expect(screen.getByRole("button", { name: /continue/i })).toBeEnabled();
  });

  it("after a failed load, a deliberate pick (told the value is unknown) enables Continue and submits that pick", async () => {
    vi.mocked(adminApi.getSettings).mockRejectedValue(new ApiError(502, "bad_gateway", "settings fetch failed"));
    vi.mocked(adminApi.updateSettings).mockResolvedValue({} as never);
    const onNext = renderStep();

    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("settings fetch failed"));
    fireEvent.click(screen.getByRole("tab", { name: "Open" }));

    const cont = screen.getByRole("button", { name: /continue/i });
    expect(cont).toBeEnabled();
    fireEvent.click(cont);

    await waitFor(() => expect(onNext).toHaveBeenCalled());
    expect(adminApi.updateSettings).toHaveBeenCalledWith("tok", { registration_mode: "open", allowed_origins: [window.location.origin] });
  });

  it("disables Continue while the save is in flight", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settingsResponse("closed"));
    vi.mocked(adminApi.updateSettings).mockReturnValue(new Promise(() => {}) as never);
    renderStep();

    await waitFor(() => expect(screen.getByRole("tab", { name: "Closed", selected: true })).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));

    await waitFor(() => {
      expect(screen.getByRole("button", { name: /saving/i })).toBeDisabled();
    });
  });
});
