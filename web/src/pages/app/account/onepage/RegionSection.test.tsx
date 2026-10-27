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
