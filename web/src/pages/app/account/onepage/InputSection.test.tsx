import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ApiError } from "../../../../api/client";
import { InputSection } from "./InputSection";

vi.mock("../../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
const getRegionInput = vi.hoisted(() => vi.fn());
const patchRegionInput = vi.hoisted(() => vi.fn());
vi.mock("../../../../api/me", () => ({ getRegionInput, patchRegionInput }));
const rawMouseSupported = vi.hoisted(() => vi.fn(() => true));
vi.mock("../../../../input/capture", () => ({ rawMouseSupported }));

const body = (input: object = {}, defaults: object = {}) => ({
  region: {},
  input,
  defaults: { region: {}, input: { keyboard: { layout: "us", variant: "" }, match_cursor: "auto", mouse_sensitivity: 1, ...defaults } },
});

beforeEach(() => {
  getRegionInput.mockReset().mockResolvedValue(body());
  rawMouseSupported.mockReset().mockReturnValue(true);
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

describe("Input section: saves", () => {
  it("one PATCH at a time, so a late response cannot revert the other field", async () => {
    // A server that merges each PATCH when it arrives and answers with the resource as it was then.
    let server: Record<string, unknown> = {};
    const replies: (() => void)[] = [];
    patchRegionInput.mockImplementation((_t: string, p: { input: object }) => {
      server = { ...server, ...p.input };
      const snapshot = { ...server };
      return new Promise((resolve) => replies.push(() => resolve(body(snapshot))));
    });
    render(<InputSection />);
    const tab = await screen.findByRole("tab", { name: "Always on" });
    await waitFor(() => expect(tab).toBeEnabled());
    fireEvent.click(tab);
    fireEvent.change(await slider(), { target: { value: "1" } });
    await new Promise((r) => setTimeout(r, 700));
    // Answer newest first: the first PATCH, if it is still out, then resolves last.
    for (let i = 0; i < 5; i++) {
      while (replies.length) replies.pop()?.();
      await new Promise((r) => setTimeout(r, 50));
    }
    await waitFor(() => expect(patchRegionInput).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.getByLabelText("Sensitivity")).toHaveAttribute("aria-valuetext", "2×"));
    expect(selected()).toBe("Always on");
  });
});

describe("Input section: Raw mouse input", () => {
  const rawSwitch = async () => {
    const el = (await screen.findByLabelText("Raw mouse input (no acceleration)")) as HTMLInputElement;
    return el;
  };

  it("is off until the user or the install default says otherwise", async () => {
    render(<InputSection />);
    const el = await rawSwitch();
    await waitFor(() => expect(el).toBeEnabled());
    expect(el).not.toBeChecked();
  });

  it("shows the install default until the user picks", async () => {
    getRegionInput.mockResolvedValue(body({}, { raw_mouse: true }));
    render(<InputSection />);
    await waitFor(() => expect(screen.getByLabelText("Raw mouse input (no acceleration)")).toBeChecked());
  });

  it("on saves true, off saves false", async () => {
    render(<InputSection />);
    const el = await rawSwitch();
    await waitFor(() => expect(el).toBeEnabled());
    fireEvent.click(el);
    await waitFor(() => expect(patchRegionInput).toHaveBeenLastCalledWith("tok", { input: { raw_mouse: true } }));
    expect(await screen.findByText("Saved. Applies from your next launch.")).toBeInTheDocument();
    await waitFor(() => expect(el).toBeChecked());
    fireEvent.click(el);
    await waitFor(() => expect(patchRegionInput).toHaveBeenLastCalledWith("tok", { input: { raw_mouse: false } }));
    await waitFor(() => expect(el).not.toBeChecked());
  });

  it("says why when the save fails, and goes back to the stored value", async () => {
    patchRegionInput.mockRejectedValue(new ApiError(400, "validation_failed", "bad value"));
    render(<InputSection />);
    const el = await rawSwitch();
    await waitFor(() => expect(el).toBeEnabled());
    fireEvent.click(el);
    expect(await screen.findByText("bad value")).toBeInTheDocument();
    expect(el).not.toBeChecked();
  });

  it("is disabled, off and explained in a browser without it, whatever is stored", async () => {
    rawMouseSupported.mockReturnValue(false);
    getRegionInput.mockResolvedValue(body({ raw_mouse: true }));
    render(<InputSection />);
    const el = await rawSwitch();
    await screen.findByText("Not available in this browser. Use Chrome or Edge for raw input.");
    expect(el).toBeDisabled();
    expect(el).not.toBeChecked();
    expect(el).toHaveAccessibleDescription(/Not available in this browser/);
    expect(patchRegionInput).not.toHaveBeenCalled();
  });
});

describe("Input section: Flip scroll direction", () => {
  const flipSwitch = async () => {
    const el = (await screen.findByLabelText("Flip scroll direction")) as HTMLInputElement;
    await waitFor(() => expect(el).toBeEnabled());
    return el;
  };

  it("is off until the user or the install default says otherwise", async () => {
    render(<InputSection />);
    expect(await flipSwitch()).not.toBeChecked();
  });

  it("shows the install default until the user picks", async () => {
    getRegionInput.mockResolvedValue(body({}, { flip_scroll: true }));
    render(<InputSection />);
    await waitFor(() => expect(screen.getByLabelText("Flip scroll direction")).toBeChecked());
  });

  it("on saves true, off saves false, leaving the other fields alone", async () => {
    render(<InputSection />);
    const el = await flipSwitch();
    fireEvent.click(el);
    await waitFor(() => expect(patchRegionInput).toHaveBeenLastCalledWith("tok", { input: { flip_scroll: true } }));
    expect(await screen.findByText("Saved. Applies from your next launch.")).toBeInTheDocument();
    await waitFor(() => expect(el).toBeChecked());
    fireEvent.click(el);
    await waitFor(() => expect(patchRegionInput).toHaveBeenLastCalledWith("tok", { input: { flip_scroll: false } }));
    await waitFor(() => expect(el).not.toBeChecked());
  });

  it("says why when the save fails, and goes back to the stored value", async () => {
    patchRegionInput.mockRejectedValue(new ApiError(400, "validation_failed", "bad value"));
    render(<InputSection />);
    const el = await flipSwitch();
    fireEvent.click(el);
    expect(await screen.findByText("bad value")).toBeInTheDocument();
    expect(el).not.toBeChecked();
  });
});

describe("Input section: Gamepad Nintendo layout", () => {
  const nin = async () => {
    const el = (await screen.findByLabelText("Nintendo layout (swap A/B and X/Y)")) as HTMLInputElement;
    await waitFor(() => expect(el).toBeEnabled());
    return el;
  };

  it("is off until the user or the install default says otherwise, and explains itself", async () => {
    render(<InputSection />);
    expect(await nin()).not.toBeChecked();
    expect(screen.getByText(/the button printed A acts as A in games/)).toBeInTheDocument();
  });

  it("shows the install default until the user picks", async () => {
    getRegionInput.mockResolvedValue(body({}, { nintendo_layout: true }));
    render(<InputSection />);
    await waitFor(() => expect(screen.getByLabelText("Nintendo layout (swap A/B and X/Y)")).toBeChecked());
  });

  it("on saves true, off saves false, leaving the other fields alone", async () => {
    render(<InputSection />);
    const el = await nin();
    fireEvent.click(el);
    await waitFor(() => expect(patchRegionInput).toHaveBeenLastCalledWith("tok", { input: { nintendo_layout: true } }));
    expect(await screen.findByText("Saved. Applies from your next launch.")).toBeInTheDocument();
    await waitFor(() => expect(el).toBeChecked());
    fireEvent.click(el);
    await waitFor(() => expect(patchRegionInput).toHaveBeenLastCalledWith("tok", { input: { nintendo_layout: false } }));
    await waitFor(() => expect(el).not.toBeChecked());
  });

  it("says why when the save fails, and goes back to the stored value", async () => {
    patchRegionInput.mockRejectedValue(new ApiError(400, "validation_failed", "bad value"));
    render(<InputSection />);
    const el = await nin();
    fireEvent.click(el);
    expect(await screen.findByText("bad value")).toBeInTheDocument();
    expect(el).not.toBeChecked();
  });
});

describe("Input section: Keyboard", () => {
  const layout = async () => {
    const el = (await screen.findByLabelText("Layout")) as HTMLSelectElement;
    await waitFor(() => expect(el).toBeEnabled());
    return el;
  };
  const variant = () => screen.getByLabelText("Variant") as HTMLSelectElement;

  it("shows the install keyboard until the user picks, with Variant locked to it", async () => {
    getRegionInput.mockResolvedValue(body({}, { keyboard: { layout: "de", variant: "nodeadkeys" } }));
    render(<InputSection />);
    const el = await layout();
    expect(el).toHaveValue("");
    expect(el.options[0]).toHaveTextContent("Same as the install (German)");
    expect(variant()).toHaveValue("nodeadkeys");
    expect(variant()).toBeDisabled();
  });

  it("a layout pick saves the layout with the basic variant", async () => {
    render(<InputSection />);
    fireEvent.change(await layout(), { target: { value: "gb" } });
    await waitFor(() => expect(patchRegionInput).toHaveBeenCalledWith("tok", { input: { keyboard: { layout: "gb", variant: "" } } }));
    expect(await screen.findByText("Saved. Your next launch uses English (UK), Default.")).toBeInTheDocument();
    expect(variant()).toBeEnabled();
  });

  it("a layout change resets the variant, and a variant pick keeps the layout", async () => {
    getRegionInput.mockResolvedValue(body({ keyboard: { layout: "us", variant: "dvorak" } }));
    render(<InputSection />);
    await waitFor(() => expect(variant()).toHaveValue("dvorak"));
    fireEvent.change(await layout(), { target: { value: "fr" } });
    await waitFor(() => expect(patchRegionInput).toHaveBeenLastCalledWith("tok", { input: { keyboard: { layout: "fr", variant: "" } } }));
    await waitFor(() => expect(variant()).toHaveValue(""));
    fireEvent.change(variant(), { target: { value: "bepo" } });
    await waitFor(() => expect(patchRegionInput).toHaveBeenLastCalledWith("tok", { input: { keyboard: { layout: "fr", variant: "bepo" } } }));
    expect(await screen.findByText("Saved. Your next launch uses French, BÉPO.")).toBeInTheDocument();
  });

  it("'Same as the install' sends null", async () => {
    getRegionInput.mockResolvedValue(body({ keyboard: { layout: "gb", variant: "" } }, { keyboard: { layout: "us", variant: "" } }));
    render(<InputSection />);
    fireEvent.change(await layout(), { target: { value: "" } });
    await waitFor(() => expect(patchRegionInput).toHaveBeenCalledWith("tok", { input: { keyboard: null } }));
    expect(await screen.findByText("Saved. Your next launch uses the install’s keyboard.")).toBeInTheDocument();
    await waitFor(() => expect(screen.getByLabelText("Layout")).toHaveValue(""));
  });

  it("shows a stored value outside the list as itself", async () => {
    getRegionInput.mockResolvedValue(body({ keyboard: { layout: "cz", variant: "qwerty" } }));
    render(<InputSection />);
    const el = await layout();
    await waitFor(() => expect(el).toHaveValue("cz"));
    expect(el).toHaveDisplayValue("cz");
    expect(variant()).toHaveDisplayValue("qwerty");
  });

  it("says an idle Steam was closed when the response says so", async () => {
    patchRegionInput.mockImplementation(async (_t: string, p: { input: object }) => ({ ...body(p.input), parked_session_stopped: true }));
    render(<InputSection />);
    fireEvent.change(await layout(), { target: { value: "us" } });
    expect(await screen.findByText(/Your idle Steam was closed so it picks this up\./)).toBeInTheDocument();
  });

  it("says why when the save fails, and goes back to the stored value", async () => {
    patchRegionInput.mockRejectedValue(new ApiError(400, "validation_failed", "bad layout"));
    render(<InputSection />);
    const el = await layout();
    fireEvent.change(el, { target: { value: "gb" } });
    expect(await screen.findByText("bad layout")).toBeInTheDocument();
    expect(el).toHaveValue("");
  });

  // The Layout pick is held open (its PATCH has not answered) while the user reaches for Variant.
  const holdPatch = () => {
    const sent: { body: { input: { keyboard: unknown } }; answer: () => void }[] = [];
    patchRegionInput.mockImplementation((_t: string, p: { input: { keyboard: unknown } }) => new Promise((resolve) => sent.push({ body: p, answer: () => resolve(body(p.input)) })));
    return sent;
  };

  it("while a layout save is out, both selects show the pick and Variant cannot send the stored layout", async () => {
    getRegionInput.mockResolvedValue(body({ keyboard: { layout: "us", variant: "" } }));
    const sent = holdPatch();
    render(<InputSection />);
    fireEvent.change(await layout(), { target: { value: "fr" } });
    await waitFor(() => expect(screen.getByLabelText("Layout")).toHaveValue("fr"));
    expect(screen.getByLabelText("Layout")).toBeDisabled();
    expect(variant()).toBeDisabled();
    expect([...variant().options].map((o) => o.textContent)).toEqual(["Default (AZERTY)", "BÉPO", "AFNOR"]);
    fireEvent.change(variant(), { target: { value: "bepo" } });
    expect(sent).toHaveLength(1);
    sent[0].answer();
    expect(await screen.findByText("Saved. Your next launch uses French, Default (AZERTY).")).toBeInTheDocument();
    expect(sent).toHaveLength(1);
    await waitFor(() => expect(screen.getByLabelText("Layout")).toHaveValue("fr"));
  });

  it("while 'Same as the install' is out, a Variant pick cannot undo it", async () => {
    getRegionInput.mockResolvedValue(body({ keyboard: { layout: "gb", variant: "" } }));
    const sent = holdPatch();
    render(<InputSection />);
    fireEvent.change(await layout(), { target: { value: "" } });
    await waitFor(() => expect(screen.getByLabelText("Layout")).toHaveValue(""));
    fireEvent.change(variant(), { target: { value: "dvorak" } });
    expect(sent).toHaveLength(1);
    expect(sent[0].body).toEqual({ input: { keyboard: null } });
    sent[0].answer();
    await screen.findByText("Saved. Your next launch uses the install’s keyboard.");
    expect(sent).toHaveLength(1);
  });

  it("a layout outside the list can go back to its basic variant", async () => {
    getRegionInput.mockResolvedValue(body({ keyboard: { layout: "ru", variant: "winkeys" } }));
    render(<InputSection />);
    await waitFor(() => expect(variant()).toHaveValue("winkeys"));
    expect([...variant().options].map((o) => o.textContent)).toEqual(["Default", "winkeys"]);
    await waitFor(() => expect(variant()).toBeEnabled());
    fireEvent.change(variant(), { target: { value: "" } });
    await waitFor(() => expect(patchRegionInput).toHaveBeenCalledWith("tok", { input: { keyboard: { layout: "ru", variant: "" } } }));
  });

  it("links the hint to both selects", async () => {
    render(<InputSection />);
    await layout();
    expect(screen.getByLabelText("Layout")).toHaveAccessibleDescription(/Desktops are set to this layout/);
    expect(variant()).toHaveAccessibleDescription(/Desktops are set to this layout/);
  });
});
