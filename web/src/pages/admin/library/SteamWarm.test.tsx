import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { SteamWarm, parseIdleMinutes } from "./SteamWarm";
import type { UseSettingsResult } from "../settings/useSettings";
import type { InstanceSettings } from "../../../api/types";

const patch = vi.fn();
const settingsWith = (over: Partial<InstanceSettings> = {}): UseSettingsResult =>
  ({
    settings: { steam_warm_enabled: false, steam_warm_rewarm: "library_open", steam_warm_idle_minutes: 10, ...over },
    loading: false,
    error: null,
    patch,
    patchOrThrow: vi.fn(),
    pending: null,
  }) as unknown as UseSettingsResult;

beforeEach(() => patch.mockReset().mockResolvedValue(true));

const typeMinutes = (v: string) =>
  fireEvent.change(screen.getByLabelText("Keep a warm Steam for (minutes)"), { target: { value: v } });

describe("parseIdleMinutes", () => {
  it.each([["0", null], ["61", null], ["1.5", null], ["", null], ["abc", null], ["1", 1], ["60", 60], ["10", 10]])(
    "%j -> %j",
    (text, want) => expect(parseIdleMinutes(text)).toBe(want),
  );
});

describe("SteamWarm", () => {
  it("renders nothing against a control plane that predates the settings", () => {
    const { container } = render(<SteamWarm settings={settingsWith({ steam_warm_enabled: undefined })} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("patches the switch and the re-warm choice, one key each", () => {
    render(<SteamWarm settings={settingsWith()} />);
    fireEvent.click(screen.getByLabelText("Keep Steam warm while a user browses"));
    expect(patch).toHaveBeenCalledWith("steam_warm_enabled", true);
    fireEvent.change(screen.getByLabelText("Warm Steam again"), { target: { value: "next_steam_launch" } });
    expect(patch).toHaveBeenCalledWith("steam_warm_rewarm", "next_steam_launch");
  });

  it.each(["0", "61", "1.5", ""])("rejects %j client-side and sends nothing", (v) => {
    render(<SteamWarm settings={settingsWith()} />);
    typeMinutes(v);
    fireEvent.click(screen.getByRole("button", { name: "Save warm Steam minutes" }));
    expect(screen.getByRole("alert")).toHaveTextContent("between 1 and 60");
    expect(patch).not.toHaveBeenCalled();
  });

  it("saves a valid number as an integer", () => {
    render(<SteamWarm settings={settingsWith()} />);
    typeMinutes("25");
    fireEvent.click(screen.getByRole("button", { name: "Save warm Steam minutes" }));
    expect(patch).toHaveBeenCalledWith("steam_warm_idle_minutes", 25);
  });
});
