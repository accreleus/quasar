import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { AccountAppearance } from "./AccountAppearance";
import { ToastProvider } from "../../../components/Toast";
import { OverlayPreferencesProvider } from "../../../settings/OverlayPreferencesContext";

const getUIPreferences = vi.fn();
const patchUIPreferences = vi.fn();
vi.mock("../../../api/me", () => ({
  getUIPreferences: (...a: unknown[]) => getUIPreferences(...a),
  patchUIPreferences: (...a: unknown[]) => patchUIPreferences(...a),
}));
vi.mock("../../../auth/context", () => ({
  useAuth: () => ({ token: "test-token", user: { id: "u1" } }),
}));

function renderPage() {
  return render(
    <ToastProvider>
      <OverlayPreferencesProvider>
        <AccountAppearance />
      </OverlayPreferencesProvider>
    </ToastProvider>,
  );
}

beforeEach(() => {
  localStorage.clear();
  getUIPreferences.mockReset().mockResolvedValue({ session_overlay: {} });
  patchUIPreferences.mockReset().mockResolvedValue({});
});

describe("AccountAppearance", () => {
  it("is off by default and PATCHes only classic_layout when toggled", async () => {
    renderPage();
    const sw = screen.getByLabelText("Classic layout") as HTMLInputElement;
    await waitFor(() => expect(sw.disabled).toBe(false));
    expect(sw.checked).toBe(false);

    fireEvent.click(sw);
    expect(sw.checked).toBe(true);
    await waitFor(() =>
      expect(patchUIPreferences).toHaveBeenCalledWith("test-token", { classic_layout: true }),
    );
  });

  it("shows the stored choice", async () => {
    getUIPreferences.mockResolvedValue({ session_overlay: {}, classic_layout: true });
    renderPage();
    await waitFor(() =>
      expect((screen.getByLabelText("Classic layout") as HTMLInputElement).checked).toBe(true),
    );
  });

  it("reverts when the PATCH fails", async () => {
    patchUIPreferences.mockRejectedValue(new Error("network down"));
    renderPage();
    const sw = screen.getByLabelText("Classic layout") as HTMLInputElement;
    await waitFor(() => expect(sw.disabled).toBe(false));

    fireEvent.click(sw);
    await waitFor(() => expect(sw.checked).toBe(false));
    expect(await screen.findByText("Could not save layout")).toBeTruthy();
    expect(localStorage.getItem("quasar.ui.classic.u1")).toBe("0");
  });
});
