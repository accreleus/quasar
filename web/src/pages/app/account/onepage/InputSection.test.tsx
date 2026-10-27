import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ApiError } from "../../../../api/client";
import { InputSection } from "./InputSection";

vi.mock("../../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
const getRegionInput = vi.hoisted(() => vi.fn());
const patchRegionInput = vi.hoisted(() => vi.fn());
vi.mock("../../../../api/me", () => ({ getRegionInput, patchRegionInput }));

const body = (input: object = {}, defaults: object = {}) => ({
  region: {},
  input,
  defaults: { region: {}, input: { match_cursor: "auto", mouse_sensitivity: 1, ...defaults } },
});

beforeEach(() => {
  getRegionInput.mockReset().mockResolvedValue(body());
  patchRegionInput.mockReset().mockImplementation(async (_t: string, p: { input: object }) => body(p.input));
});

const slider = async () => {
  const el = (await screen.findByLabelText("Sensitivity")) as HTMLInputElement;
  await waitFor(() => expect(el).toBeEnabled());
  return el;
};
const selected = () => screen.getAllByRole("tab").find((t) => t.getAttribute("aria-selected") === "true")?.textContent;

describe("Input section: Match cursor to my window", () => {
  it("shows the install default until the user picks, and explains it", async () => {
    getRegionInput.mockResolvedValue(body({}, { match_cursor: "on" }));
    render(<InputSection />);
    await waitFor(() => expect(selected()).toBe("Always on"));
    expect(screen.getByText(/On for every launch, games included/)).toBeInTheDocument();
  });

  it.each([
    ["Auto", "auto", /Auto turns this on for desktops/],
    ["Always on", "on", /games included/],
    ["Off", "off", /Off for every launch/],
  ])("%s saves %s", async (label, value, hint) => {
    getRegionInput.mockResolvedValue(body({ match_cursor: value === "auto" ? "off" : "auto" }));
    render(<InputSection />);
    const tab = await screen.findByRole("tab", { name: label });
    await waitFor(() => expect(tab).toBeEnabled());
    fireEvent.click(tab);
    await waitFor(() => expect(patchRegionInput).toHaveBeenCalledWith("tok", { input: { match_cursor: value } }));
    expect(await screen.findByText("Saved. Applies from your next launch.")).toBeInTheDocument();
    expect(selected()).toBe(label);
    expect(screen.getByText(hint)).toBeInTheDocument();
  });

  it("says why when the save fails, and goes back to the stored value", async () => {
    patchRegionInput.mockRejectedValue(new ApiError(400, "validation_failed", "bad value"));
    render(<InputSection />);
    const tab = await screen.findByRole("tab", { name: "Off" });
    await waitFor(() => expect(tab).toBeEnabled());
    fireEvent.click(tab);
    expect(await screen.findByText("bad value")).toBeInTheDocument();
    expect(selected()).toBe("Auto");
  });
});

describe("Input section: sensitivity", () => {
  it("rests at 1× in the middle of 0.25× to 4×", async () => {
    render(<InputSection />);
    const el = await slider();
    expect(el).toHaveValue("0");
    expect(el).toHaveAttribute("min", "-2");
    expect(el).toHaveAttribute("max", "2");
    expect(el).toHaveAttribute("aria-valuetext", "1×");
  });

  it.each([
    [-2, 0.25, "0.25×"],
    [0, 1, "1×"],
    [2, 4, "4×"],
    [1.5, 2.828, "2.83×"],
  ])("position %s saves %s×", async (position, saved, text) => {
    getRegionInput.mockResolvedValue(body({ mouse_sensitivity: 3 }));
    render(<InputSection />);
    const el = await slider();
    fireEvent.change(el, { target: { value: String(position) } });
    expect(el).toHaveAttribute("aria-valuetext", text);
    await waitFor(() => expect(patchRegionInput).toHaveBeenCalledWith("tok", { input: { mouse_sensitivity: saved } }), { timeout: 2000 });
    expect(await screen.findByText(`Saved. Your next launch uses ${text} sensitivity.`)).toBeInTheDocument();
  });

  it("saves once, after the slider has rested", async () => {
    render(<InputSection />);
    const el = await slider();
    for (const v of ["0.25", "0.5", "1"]) fireEvent.change(el, { target: { value: v } });
    await waitFor(() => expect(patchRegionInput).toHaveBeenCalled(), { timeout: 2000 });
    expect(patchRegionInput).toHaveBeenCalledTimes(1);
    expect(patchRegionInput).toHaveBeenCalledWith("tok", { input: { mouse_sensitivity: 2 ** 1 } });
  });

  it("starts from the user's saved value", async () => {
    getRegionInput.mockResolvedValue(body({ mouse_sensitivity: 2 }));
    render(<InputSection />);
    await waitFor(() => expect(screen.getByLabelText("Sensitivity")).toHaveAttribute("aria-valuetext", "2×"));
  });

  it("goes back to the stored value and says why when the save fails", async () => {
    patchRegionInput.mockRejectedValue(new ApiError(400, "validation_failed", "bad value"));
    render(<InputSection />);
    const el = await slider();
    fireEvent.change(el, { target: { value: "2" } });
    expect(await screen.findByText("bad value", undefined, { timeout: 2000 })).toBeInTheDocument();
    await waitFor(() => expect(el).toHaveAttribute("aria-valuetext", "1×"));
  });

  it("is disabled until the settings have loaded, and says so when they cannot", async () => {
    getRegionInput.mockRejectedValue(new ApiError(500, "internal", "boom"));
    render(<InputSection />);
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.getByLabelText("Sensitivity")).toBeDisabled();
  });
});
