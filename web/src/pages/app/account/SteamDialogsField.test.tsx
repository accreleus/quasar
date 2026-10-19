import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ApiError } from "../../../api/client";
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
    expect(await screen.findByText(/Saved/)).toBeInTheDocument();
    expect(select).toBeEnabled();
    expect(select).toHaveValue("dismiss_safe");
  });

  it("keeps the select enabled while a save runs, so keyboard focus is not dropped", async () => {
    let finish: (v: unknown) => void = () => {};
    updateSteamDialogs.mockReturnValue(new Promise((r) => (finish = r)));
    render(<SteamDialogsField />);
    const select = await screen.findByLabelText("Steam dialogs");
    await waitFor(() => expect(select).toBeEnabled());
    select.focus();
    fireEvent.change(select, { target: { value: "dismiss_safe" } });
    expect(select).toBeEnabled();
    expect(document.activeElement).toBe(select);
    finish({ steam_dialogs: "dismiss_safe" });
    expect(await screen.findByText(/Saved/)).toBeInTheDocument();
  });

  it("reverts the select and shows the server's message when the PATCH fails", async () => {
    updateSteamDialogs.mockRejectedValue(new ApiError(400, "validation_failed", "bad value"));
    render(<SteamDialogsField />);
    const select = await screen.findByLabelText("Steam dialogs");
    await waitFor(() => expect(select).toBeEnabled());
    fireEvent.change(select, { target: { value: "dismiss_safe" } });
    expect(await screen.findByRole("alert")).toHaveTextContent("bad value");
    expect(select).toHaveValue("show");
    expect(screen.queryByText(/Saved/)).toBeNull();
  });
});
