import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { SteamDialogsField } from "./SteamDialogsField";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
const getProfilePreferences = vi.hoisted(() => vi.fn());
const updateSteamDialogs = vi.hoisted(() => vi.fn());
vi.mock("../../../api/library", () => ({ getProfilePreferences, updateSteamDialogs }));

beforeEach(() => {
  getProfilePreferences.mockReset().mockResolvedValue({ steam_dialogs: "show" });
  updateSteamDialogs.mockReset().mockResolvedValue({ steam_dialogs: "dismiss_safe" });
});

describe("SteamDialogsField", () => {
  it("loads the stored value and says what the setting covers", async () => {
    render(<SteamDialogsField />);
    const select = await screen.findByLabelText("Steam dialogs");
    await waitFor(() => expect(select).toBeEnabled());
    expect(select).toHaveValue("show");
    expect(screen.getByText(/Sign-in, Steam Guard, cloud conflicts and installs are always shown/)).toBeInTheDocument();
  });

  it("saves a change with only steam_dialogs in the PATCH", async () => {
    render(<SteamDialogsField />);
    const select = await screen.findByLabelText("Steam dialogs");
    await waitFor(() => expect(select).toBeEnabled());
    fireEvent.change(select, { target: { value: "dismiss_safe" } });
    expect(updateSteamDialogs).toHaveBeenCalledWith("tok", "dismiss_safe");
    await waitFor(() => expect(select).toHaveValue("dismiss_safe"));
  });
});
