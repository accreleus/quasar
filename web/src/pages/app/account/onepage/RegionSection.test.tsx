// The Region section: the timezone list saves through PATCH /v1/me/region-input and says what happened.

import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RegionSection } from "./RegionSection";
import { ApiError } from "../../../../api/client";
import { AuthContext, type AuthContextValue } from "../../../../auth/context";

const api = vi.hoisted(() => ({ getRegionInput: vi.fn(), patchRegionInput: vi.fn() }));
vi.mock("../../../../api/me", () => api);

const auth = {
  status: "authenticated",
  user: { id: "u1", email: "sam@example.io", username: "sam", role: "user" },
  token: "t0k3n",
  isAdmin: false,
  sessionExpired: false,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
} as unknown as AuthContextValue;

const body = (timezone: string | null, extra: Record<string, unknown> = {}) => ({
  region: { timezone, language: null, region_format: null },
  input: {},
  defaults: { region: { timezone: "UTC", language: "en-US", region_format: null }, input: {} },
  ...extra,
});

const renderSection = () =>
  render(
    <AuthContext.Provider value={auth}>
      <RegionSection />
    </AuthContext.Provider>,
  );

const list = () => screen.findByRole("listbox");
const option = (name: RegExp) => screen.getByRole("option", { name });

beforeEach(() => {
  vi.clearAllMocks();
  vi.spyOn(Intl.DateTimeFormat.prototype, "resolvedOptions").mockReturnValue({ timeZone: "Europe/London" } as Intl.ResolvedDateTimeFormatOptions);
  api.getRegionInput.mockResolvedValue(body(null));
  api.patchRegionInput.mockImplementation(async (_t: string, patch: { region: { timezone: string | null } }) =>
    body(patch.region.timezone, { parked_session_stopped: false }),
  );
});
afterEach(() => vi.restoreAllMocks());

describe("Region section", () => {
  it("starts on Follow my browser and says which zone that is", async () => {
    renderSection();
    await list();
    expect(option(/Follow my browser/).getAttribute("aria-selected")).toBe("true");
    expect(screen.getByText(/Now: London \(Europe\/London\), following your browser/)).toBeTruthy();
    expect(screen.getByRole("status").textContent).toBe("Saved as you pick.");
  });

  it("falls back to the install default when the browser reports no usable zone", async () => {
    vi.spyOn(Intl.DateTimeFormat.prototype, "resolvedOptions").mockReturnValue({ timeZone: "" } as Intl.ResolvedDateTimeFormatOptions);
    api.getRegionInput.mockResolvedValue({ ...body(null), defaults: { region: { timezone: "Europe/Berlin" }, input: {} } });
    renderSection();
    await list();
    expect(screen.getByText(/Now: Berlin \(Europe\/Berlin\), the install default/)).toBeTruthy();
  });

  it("opens on a saved zone, and reads one that is not in the curated list", async () => {
    api.getRegionInput.mockResolvedValue(body("Asia/Calcutta"));
    renderSection();
    await list();
    expect(option(/Follow my browser/).getAttribute("aria-selected")).toBe("false");
    expect(screen.getByText(/Now: Calcutta \(Asia\/Calcutta\) ·/)).toBeTruthy();
  });

  it("saves a pick at once and says the next launch uses it", async () => {
    renderSection();
    await list();
    fireEvent.click(option(/Tokyo/));
    await waitFor(() => expect(api.patchRegionInput).toHaveBeenCalledWith("t0k3n", { region: { timezone: "Asia/Tokyo" } }));
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("Saved. Your next launch starts at Tokyo time."));
    expect(option(/Tokyo/).getAttribute("aria-selected")).toBe("true");
  });

  it("says the idle Steam was closed when the server closed it", async () => {
    api.patchRegionInput.mockResolvedValue(body("Asia/Tokyo", { parked_session_stopped: true }));
    renderSection();
    await list();
    fireEvent.click(option(/Tokyo/));
    await waitFor(() => expect(screen.getByRole("status").textContent).toMatch(/Your idle Steam was closed so it picks this up\.$/));
  });

  it("Follow my browser clears the saved zone", async () => {
    api.getRegionInput.mockResolvedValue(body("Asia/Tokyo"));
    renderSection();
    await list();
    fireEvent.click(option(/Follow my browser/));
    await waitFor(() => expect(api.patchRegionInput).toHaveBeenCalledWith("t0k3n", { region: { timezone: null } }));
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("Saved. Your next launch follows your browser's timezone."));
  });

  it("does not save the zone that is already saved", async () => {
    api.getRegionInput.mockResolvedValue(body("Asia/Tokyo"));
    renderSection();
    await list();
    fireEvent.click(option(/Tokyo/));
    expect(api.patchRegionInput).not.toHaveBeenCalled();
  });

  it("holds the list shut while a save runs, so one save runs at a time", async () => {
    let release: (v: unknown) => void = () => {};
    api.patchRegionInput.mockReturnValue(new Promise((r) => (release = r)));
    renderSection();
    await list();
    fireEvent.click(option(/Tokyo/));
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("Saving…"));
    fireEvent.click(option(/Paris/));
    expect(api.patchRegionInput).toHaveBeenCalledTimes(1);
    expect((screen.getByLabelText("Search timezones") as HTMLInputElement).disabled).toBe(false);
    release(body("Asia/Tokyo"));
    await waitFor(() => expect(screen.getByRole("status").textContent).toMatch(/^Saved/));
  });

  it("shows the server's message and keeps the saved zone when a save fails", async () => {
    api.getRegionInput.mockResolvedValue(body("Europe/Paris"));
    api.patchRegionInput.mockRejectedValue(new ApiError(400, "validation_failed", "region.timezone: unknown zone"));
    renderSection();
    await list();
    fireEvent.click(option(/Tokyo/));
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("region.timezone: unknown zone"));
    expect(option(/Paris/).getAttribute("aria-selected")).toBe("true");
    expect(option(/Tokyo/).getAttribute("aria-selected")).toBe("false");
  });

  it("searches by city, country and zone name", async () => {
    renderSection();
    await list();
    const box = screen.getByLabelText("Search timezones");
    fireEvent.change(box, { target: { value: "japan" } });
    expect(option(/Tokyo/)).toBeTruthy();
    expect(screen.queryByRole("option", { name: /Paris/ })).toBeNull();
    fireEvent.change(box, { target: { value: "america new york" } });
    expect(within(await list()).getByRole("option", { name: /New York/ })).toBeTruthy();
    fireEvent.change(box, { target: { value: "zzzzzz" } });
    expect(screen.getAllByText("No timezone matches. Try a city or country name.")).toHaveLength(2);
    expect(option(/Follow my browser/)).toBeTruthy();
  });

  it("search then Enter picks the match that is drawn, never the row the search hid", async () => {
    api.getRegionInput.mockResolvedValue(body("Europe/Paris"));
    renderSection();
    const box = await list();
    fireEvent.change(screen.getByLabelText("Search timezones"), { target: { value: "tokyo" } });
    const drawn = box.querySelector(".tz-opt.is-active");
    expect(drawn?.textContent).toMatch(/Tokyo/);
    expect(box.getAttribute("aria-activedescendant")).toBe(drawn?.id);
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(api.patchRegionInput).toHaveBeenCalledWith("t0k3n", { region: { timezone: "Asia/Tokyo" } }));
  });

  it("search then Space picks the same drawn match", async () => {
    api.getRegionInput.mockResolvedValue(body("Europe/Paris"));
    renderSection();
    const box = await list();
    fireEvent.change(screen.getByLabelText("Search timezones"), { target: { value: "tokyo" } });
    fireEvent.keyDown(box, { key: " " });
    await waitFor(() => expect(api.patchRegionInput).toHaveBeenCalledTimes(1));
    expect(api.patchRegionInput).toHaveBeenCalledWith("t0k3n", { region: { timezone: "Asia/Tokyo" } });
  });

  it("Enter does nothing on a saved zone outside the list when nothing is searched", async () => {
    api.getRegionInput.mockResolvedValue(body("Asia/Calcutta"));
    renderSection();
    const box = await list();
    expect(box.hasAttribute("aria-activedescendant")).toBe(false);
    expect(box.querySelector(".tz-opt.is-active")).toBeNull();
    fireEvent.keyDown(box, { key: "Enter" });
    fireEvent.keyDown(box, { key: " " });
    expect(api.patchRegionInput).not.toHaveBeenCalled();
  });

  it("Enter does nothing when the search matches nothing and the saved zone is hidden", async () => {
    api.getRegionInput.mockResolvedValue(body("Europe/Paris"));
    renderSection();
    const box = await list();
    fireEvent.change(screen.getByLabelText("Search timezones"), { target: { value: "zzzzzz" } });
    expect(box.hasAttribute("aria-activedescendant")).toBe(false);
    fireEvent.keyDown(box, { key: "Enter" });
    expect(api.patchRegionInput).not.toHaveBeenCalled();
  });

  it("says there is no match outside the listbox, in a live region", async () => {
    renderSection();
    const box = await list();
    fireEvent.change(screen.getByLabelText("Search timezones"), { target: { value: "zzzzzz" } });
    expect(within(box).queryByText(/No timezone matches/)).toBeNull();
    const live = document.querySelector('[aria-live="polite"]');
    expect(live?.textContent).toBe("No timezone matches. Try a city or country name.");
  });

  it("shows a saved zone this browser's Intl does not know by name alone, and the page still renders", async () => {
    api.getRegionInput.mockResolvedValue(body("Not/AZone"));
    renderSection();
    await list();
    expect(screen.getByText(/^Now: AZone \(Not\/AZone\)$/)).toBeTruthy();
    expect(option(/Follow my browser/).getAttribute("aria-selected")).toBe("false");
  });

  it("moves with the arrow keys and saves only on Enter", async () => {
    renderSection();
    const box = await list();
    fireEvent.keyDown(box, { key: "ArrowDown" });
    fireEvent.keyDown(box, { key: "ArrowDown" });
    expect(api.patchRegionInput).not.toHaveBeenCalled();
    const second = box.getAttribute("aria-activedescendant");
    expect(second).not.toBe("tz-o-follow");
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(api.patchRegionInput).toHaveBeenCalledTimes(1));
    expect(document.getElementById(second!)?.getAttribute("aria-selected")).toBe("true");
  });

  it("says so when the settings cannot be read", async () => {
    api.getRegionInput.mockRejectedValue(new ApiError(500, "internal", "boom"));
    renderSection();
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.queryByRole("listbox")).toBeNull();
  });
});

describe("Region map (#847)", () => {
  const svg = (c: HTMLElement) => c.querySelector(".tz-map svg")!;
  const marker = (c: HTMLElement) => c.querySelector(".mk-lbl text")?.textContent;
  // The svg is drawn 1080 by 420 here, so a pointer pixel is a viewBox unit.
  const clickAt = (c: HTMLElement, lon: number, lat: number) => {
    svg(c).getBoundingClientRect = () => ({ left: 0, top: 0, width: 1080, height: 420 }) as DOMRect;
    fireEvent.click(svg(c), { clientX: ((lon + 180) / 2) * 6, clientY: ((82 - lat) / 2) * 6 + 3 });
  };

  it("a click near Tokyo saves Asia/Tokyo through the list's save path", async () => {
    const { container } = renderSection();
    await list();
    clickAt(container, 139.7, 35.7);
    await waitFor(() => expect(api.patchRegionInput).toHaveBeenCalledWith("t0k3n", { region: { timezone: "Asia/Tokyo" } }));
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("Saved. Your next launch starts at Tokyo time."));
    expect(option(/Tokyo/).getAttribute("aria-selected")).toBe("true");
    expect(marker(container)).toMatch(/^Tokyo · /);
  });

  it("a click in the ocean picks the nearest point", async () => {
    const { container } = renderSection();
    await list();
    clickAt(container, -150, 20);
    await waitFor(() => expect(api.patchRegionInput).toHaveBeenCalledWith("t0k3n", { region: { timezone: "Pacific/Honolulu" } }));
  });

  it("marks and lights the clock of the saved zone, and the followed zone when none is saved", async () => {
    api.getRegionInput.mockResolvedValue(body("Asia/Tokyo"));
    const { container } = renderSection();
    await list();
    expect(marker(container)).toMatch(/^Tokyo · /);
    expect(container.querySelectorAll(".land.is-sel")).toHaveLength(1);
  });

  it("follows the browser's zone while none is saved", async () => {
    const { container } = renderSection();
    await list();
    expect(marker(container)).toMatch(/^London · /);
  });

  it("marks nothing for a saved zone outside the curated list, and does not crash", async () => {
    api.getRegionInput.mockResolvedValue(body("Asia/Calcutta"));
    const { container } = renderSection();
    await list();
    expect(container.querySelector(".mk")).toBeNull();
    expect(container.querySelector(".land.is-sel")).toBeNull();
    clickAt(container, 88.4, 22.6);
    await waitFor(() => expect(api.patchRegionInput).toHaveBeenCalledWith("t0k3n", { region: { timezone: "Asia/Kolkata" } }));
  });

  it("lights the hovered clock and says whose it is", async () => {
    const { container } = renderSection();
    await list();
    svg(container).getBoundingClientRect = () => ({ left: 0, top: 0, width: 1080, height: 420 }) as DOMRect;
    fireEvent.pointerMove(svg(container), { clientX: ((139.7 + 180) / 2) * 6, clientY: ((82 - 35.7) / 2) * 6 + 3 });
    expect(container.querySelector(".tz-tip")?.textContent).toMatch(/^Tokyo, Japan\s*UTC\+09:00$/);
    expect(container.querySelectorAll(".land.is-hover")).toHaveLength(1);
    fireEvent.pointerLeave(svg(container));
    expect(container.querySelector(".tz-tip")).toBeNull();
  });

  it("ignores clicks while a save runs", async () => {
    api.patchRegionInput.mockReturnValue(new Promise(() => {}));
    const { container } = renderSection();
    await list();
    clickAt(container, 139.7, 35.7);
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("Saving…"));
    clickAt(container, 2, 48);
    expect(api.patchRegionInput).toHaveBeenCalledTimes(1);
  });

  it("clears a search so the picked zone shows in the list", async () => {
    const { container } = renderSection();
    await list();
    const box = screen.getByLabelText("Search timezones") as HTMLInputElement;
    fireEvent.change(box, { target: { value: "paris" } });
    clickAt(container, 139.7, 35.7);
    await waitFor(() => expect(box.value).toBe(""));
    expect(option(/Tokyo/).getAttribute("aria-selected")).toBe("true");
  });

  it("keeps a search typed during a save when that save fails and the pick reverts", async () => {
    api.getRegionInput.mockResolvedValue(body("Europe/Paris"));
    api.patchRegionInput.mockRejectedValue(new ApiError(400, "validation_failed", "region.timezone: unknown zone"));
    renderSection();
    await list();
    const box = screen.getByLabelText("Search timezones") as HTMLInputElement;
    fireEvent.change(box, { target: { value: "tokyo" } });
    fireEvent.click(option(/Tokyo/));
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("region.timezone: unknown zone"));
    expect(box.value).toBe("tokyo");
  });

  it("keeps a search typed during a save that fails after a map pick", async () => {
    api.getRegionInput.mockResolvedValue(body("Europe/Paris"));
    let fail: (e: unknown) => void = () => {};
    api.patchRegionInput.mockReturnValue(new Promise((_r, rej) => (fail = rej)));
    const { container } = renderSection();
    await list();
    clickAt(container, 139.7, 35.7);
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("Saving…"));
    const box = screen.getByLabelText("Search timezones") as HTMLInputElement;
    fireEvent.change(box, { target: { value: "oslo" } });
    fail(new ApiError(500, "internal", "boom"));
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("boom"));
    expect(box.value).toBe("oslo");
  });

  it("a map click on the saved zone saves nothing but clears the search so the row shows", async () => {
    api.getRegionInput.mockResolvedValue(body("Asia/Tokyo"));
    const { container } = renderSection();
    await list();
    const box = screen.getByLabelText("Search timezones") as HTMLInputElement;
    fireEvent.change(box, { target: { value: "paris" } });
    expect(screen.queryByRole("option", { name: /Tokyo/ })).toBeNull();
    clickAt(container, 139.7, 35.7);
    await waitFor(() => expect(box.value).toBe(""));
    expect(option(/Tokyo/).getAttribute("aria-selected")).toBe("true");
    expect(api.patchRegionInput).not.toHaveBeenCalled();
  });

  it("still draws the label where the browser's text measuring throws", async () => {
    const proto = SVGElement.prototype as unknown as { getComputedTextLength?: () => number };
    proto.getComputedTextLength = () => {
      throw new Error("not rendered");
    };
    try {
      const { container } = renderSection();
      await list();
      expect(marker(container)).toMatch(/^London · /);
    } finally {
      delete proto.getComputedTextLength;
    }
  });

  it("leaves the search alone when the pick is made in the list", async () => {
    renderSection();
    await list();
    const box = screen.getByLabelText("Search timezones") as HTMLInputElement;
    fireEvent.change(box, { target: { value: "tokyo" } });
    fireEvent.click(option(/Tokyo/));
    await waitFor(() => expect(api.patchRegionInput).toHaveBeenCalled());
    expect(box.value).toBe("tokyo");
  });
});
