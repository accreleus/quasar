import { act, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, useNavigate } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  OverlayPreferencesProvider,
  useNewDesign,
  useOverlayPreferences,
} from "./OverlayPreferencesContext";
import { DEFAULT_OVERLAY_PREFERENCES } from "./overlayPreferences";

const getUIPreferences = vi.fn();
const patchUIPreferences = vi.fn();

vi.mock("../api/me", () => ({
  getUIPreferences: (...a: unknown[]) => getUIPreferences(...a),
  patchUIPreferences: (...a: unknown[]) => patchUIPreferences(...a),
}));

vi.mock("../auth/context", () => ({
  useAuth: () => ({ token: "test-token", user: { id: "u1" } }),
}));

function Probe() {
  const { prefs, loaded, save } = useOverlayPreferences();
  return (
    <div>
      <span data-testid="pos">{prefs.stripPosition}</span>
      <span data-testid="loaded">{String(loaded)}</span>
      <button onClick={() => void save({ ...prefs, stripPosition: "top" })}>save</button>
    </div>
  );
}

// Exposes two independently-triggerable saves (different fields) plus the
// `error` slot, so a test can drive two overlapping `save()` calls and tell
// which one's value ended up winning.
function RaceProbe() {
  const { prefs, loaded, save, error } = useOverlayPreferences();
  return (
    <div>
      <span data-testid="pos">{prefs.stripPosition}</span>
      <span data-testid="autoHide">{prefs.stripAutoHide}</span>
      <span data-testid="loaded">{String(loaded)}</span>
      <span data-testid="error">{error ?? ""}</span>
      <button onClick={() => void save({ ...prefs, stripPosition: "top" })}>saveA</button>
      <button onClick={() => void save({ ...prefs, stripAutoHide: "always_visible" })}>saveB</button>
    </div>
  );
}

describe("OverlayPreferencesProvider", () => {
  beforeEach(() => {
    localStorage.clear();
    getUIPreferences.mockReset();
    patchUIPreferences.mockReset();
  });

  it("serves defaults before the fetch resolves, then the server value", async () => {
    let resolve!: (v: unknown) => void;
    getUIPreferences.mockReturnValue(new Promise((r) => { resolve = r; }));

    render(
      <OverlayPreferencesProvider>
        <Probe />
      </OverlayPreferencesProvider>,
      { wrapper: MemoryRouter },
    );
    expect(screen.getByTestId("pos").textContent).toBe(DEFAULT_OVERLAY_PREFERENCES.stripPosition);
    expect(screen.getByTestId("loaded").textContent).toBe("false");

    await act(async () => {
      resolve({ session_overlay: { strip_position: "top" } });
    });
    await waitFor(() => expect(screen.getByTestId("pos").textContent).toBe("top"));
    expect(screen.getByTestId("loaded").textContent).toBe("true");
  });

  it("applies a save optimistically and persists it", async () => {
    getUIPreferences.mockResolvedValue({ session_overlay: {} });
    patchUIPreferences.mockResolvedValue({ session_overlay: { strip_position: "top" } });

    render(
      <OverlayPreferencesProvider>
        <Probe />
      </OverlayPreferencesProvider>,
      { wrapper: MemoryRouter },
    );
    await waitFor(() => expect(screen.getByTestId("loaded").textContent).toBe("true"));

    await act(async () => {
      screen.getByText("save").click();
    });
    expect(screen.getByTestId("pos").textContent).toBe("top");
    expect(patchUIPreferences).toHaveBeenCalledWith("test-token", {
      session_overlay: expect.objectContaining({ strip_position: "top" }),
    });
  });

  it("rolls back and reports when the save fails", async () => {
    getUIPreferences.mockResolvedValue({ session_overlay: { strip_position: "bottom" } });
    patchUIPreferences.mockRejectedValue(new Error("network down"));

    render(
      <OverlayPreferencesProvider>
        <Probe />
      </OverlayPreferencesProvider>,
      { wrapper: MemoryRouter },
    );
    await waitFor(() => expect(screen.getByTestId("loaded").textContent).toBe("true"));

    await act(async () => {
      screen.getByText("save").click();
    });
    await waitFor(() => expect(screen.getByTestId("pos").textContent).toBe("bottom"));

    // The cache must roll back too — otherwise a reload after a failed save
    // would resurrect the write the server never accepted.
    const cached = JSON.parse(localStorage.getItem("quasar.ui.overlay.u1") ?? "{}");
    expect(cached.strip_position).toBe("bottom");
  });

  it("does not let an earlier save's rollback clobber a later, still-in-flight save", async () => {
    getUIPreferences.mockResolvedValue({
      session_overlay: { strip_position: "bottom", strip_auto_hide: "on_capture" },
    });

    let rejectA!: (e: unknown) => void;
    let resolveB!: (v: unknown) => void;
    patchUIPreferences
      .mockImplementationOnce(
        () =>
          new Promise((_resolve, reject) => {
            rejectA = reject;
          }),
      )
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            resolveB = resolve;
          }),
      );

    render(
      <OverlayPreferencesProvider>
        <RaceProbe />
      </OverlayPreferencesProvider>,
      { wrapper: MemoryRouter },
    );
    await waitFor(() => expect(screen.getByTestId("loaded").textContent).toBe("true"));

    // Save A starts (position -> top) but its PATCH does not settle yet.
    await act(async () => {
      screen.getByText("saveA").click();
    });
    expect(screen.getByTestId("pos").textContent).toBe("top");

    // Save B starts before A settles (auto-hide -> always_visible) — the
    // ordinary "flip two toggles back to back" interaction from Task 15.
    await act(async () => {
      screen.getByText("saveB").click();
    });
    expect(screen.getByTestId("autoHide").textContent).toBe("always_visible");

    // A's PATCH now fails. Its rollback must not win: B has already
    // superseded it, so A's failure may only surface via `error`.
    await act(async () => {
      rejectA(new Error("network down"));
    });

    expect(screen.getByTestId("pos").textContent).toBe("top");
    expect(screen.getByTestId("autoHide").textContent).toBe("always_visible");
    expect(screen.getByTestId("error").textContent).toBe("network down");

    const cached = JSON.parse(localStorage.getItem("quasar.ui.overlay.u1") ?? "{}");
    expect(cached.strip_position).toBe("top");
    expect(cached.strip_auto_hide).toBe("always_visible");

    // Let B's own PATCH settle so no promise is left dangling.
    await act(async () => {
      resolveB({ session_overlay: {} });
    });
  });

  it("hydrates from the local cache on the next mount so the strip does not flash", async () => {
    localStorage.setItem(
      "quasar.ui.overlay.u1",
      JSON.stringify({ strip_position: "top", strip_auto_hide: "always_visible" }),
    );
    getUIPreferences.mockReturnValue(new Promise(() => {})); // never resolves

    render(
      <OverlayPreferencesProvider>
        <Probe />
      </OverlayPreferencesProvider>,
      { wrapper: MemoryRouter },
    );
    expect(screen.getByTestId("pos").textContent).toBe("top");
  });

  describe("useNewDesign", () => {
    function Design() {
      const { setClassicLayout, classicSaving } = useOverlayPreferences();
      return (
        <div>
          <span data-testid="nd">{String(useNewDesign())}</span>
          <span data-testid="saving">{String(classicSaving)}</span>
          <button onClick={() => void setClassicLayout(true).catch(() => {})}>classic</button>
          <button onClick={() => void setClassicLayout(false).catch(() => {})}>modern</button>
        </div>
      );
    }
    const mount = (path = "/app") =>
      render(
        <MemoryRouter initialEntries={[path]}>
          <OverlayPreferencesProvider>
            <Design />
          </OverlayPreferencesProvider>
        </MemoryRouter>,
      );
    const nd = () => screen.getByTestId("nd").textContent;
    const click = (name: string) =>
      act(async () => {
        screen.getByText(name).click();
      });

    it("is the new design by default, before and after the load", async () => {
      let resolve!: (v: unknown) => void;
      getUIPreferences.mockReturnValue(new Promise((r) => { resolve = r; }));
      mount();
      expect(nd()).toBe("true");
      await act(async () => {
        resolve({ session_overlay: {} });
      });
      expect(nd()).toBe("true");
      expect(localStorage.getItem("quasar.ui.classic.u1")).toBe("0");
    });

    it("is the new design outside a provider", () => {
      render(<Design />);
      expect(nd()).toBe("true");
    });

    it("reads classic_layout: true from the server", async () => {
      getUIPreferences.mockResolvedValue({ session_overlay: {}, classic_layout: true });
      mount();
      await waitFor(() => expect(nd()).toBe("false"));
    });

    it("starts from this device's mirror so a classic user sees no flip", () => {
      localStorage.setItem("quasar.ui.classic.u1", "1");
      getUIPreferences.mockReturnValue(new Promise(() => {}));
      mount();
      expect(nd()).toBe("false");
    });

    it("lets the server value override the mirror once it arrives", async () => {
      localStorage.setItem("quasar.ui.classic.u1", "1");
      getUIPreferences.mockResolvedValue({ session_overlay: {} }); // key absent = new design
      mount();
      await waitFor(() => expect(nd()).toBe("true"));
      expect(localStorage.getItem("quasar.ui.classic.u1")).toBe("0");
    });

    it("ignores a second toggle while one is pending", async () => {
      getUIPreferences.mockResolvedValue({ session_overlay: {} });
      let resolvePatch!: (v: unknown) => void;
      patchUIPreferences.mockReturnValue(new Promise((r) => { resolvePatch = r; }));
      mount();
      await waitFor(() => expect(getUIPreferences).toHaveBeenCalled());
      await click("classic");
      expect(screen.getByTestId("saving").textContent).toBe("true");
      await click("modern");
      expect(patchUIPreferences).toHaveBeenCalledTimes(1);
      expect(nd()).toBe("false");
      await act(async () => {
        resolvePatch({});
      });
      expect(screen.getByTestId("saving").textContent).toBe("false");
      // Settled: the next toggle goes through.
      await click("modern");
      expect(patchUIPreferences).toHaveBeenCalledTimes(2);
      expect(nd()).toBe("true");
    });

    it("reverts to the value before the call when its PATCH fails", async () => {
      getUIPreferences.mockResolvedValue({ session_overlay: {} });
      patchUIPreferences.mockRejectedValue(new Error("down"));
      mount();
      await waitFor(() => expect(getUIPreferences).toHaveBeenCalled());
      await click("classic");
      expect(nd()).toBe("true");
      expect(screen.getByTestId("saving").textContent).toBe("false");
      expect(localStorage.getItem("quasar.ui.classic.u1")).toBe("0");
    });

    it("does not let a GET that answers after the user toggled overwrite the choice", async () => {
      let resolveGet!: (v: unknown) => void;
      getUIPreferences.mockReturnValue(new Promise((r) => { resolveGet = r; }));
      patchUIPreferences.mockResolvedValue({});
      mount();
      await click("classic");
      expect(nd()).toBe("false");
      await act(async () => {
        resolveGet({ session_overlay: { strip_position: "top" }, classic_layout: false });
      });
      expect(nd()).toBe("false");
      expect(localStorage.getItem("quasar.ui.classic.u1")).toBe("1");
      // Overlay prefs from the same answer are still applied.
      expect(JSON.parse(localStorage.getItem("quasar.ui.overlay.u1") ?? "{}").strip_position).toBe("top");
    });

    describe("data-design on the page root", () => {
      const design = () => document.documentElement.dataset.design;

      it("is set under /app, from the mirror, and follows the server answer", async () => {
        getUIPreferences.mockResolvedValue({ session_overlay: {}, classic_layout: true });
        const { unmount } = mount("/app");
        expect(design()).toBe("new");
        await waitFor(() => expect(design()).toBe("classic"));
        unmount();
        expect(design()).toBeUndefined();
      });

      it("is absent under /admin, and the hook reports false there whatever the choice", async () => {
        getUIPreferences.mockResolvedValue({ session_overlay: {} });
        mount("/admin/fleet");
        await waitFor(() => expect(getUIPreferences).toHaveBeenCalled());
        expect(design()).toBeUndefined();
        expect(nd()).toBe("false");
      });

      it("matches /admin the way the router does: case-insensitive, bare /admin too", async () => {
        getUIPreferences.mockReturnValue(new Promise(() => {}));
        for (const path of ["/admin", "/Admin/Fleet"]) {
          const { unmount } = mount(path);
          expect(nd()).toBe("false");
          unmount();
        }
        mount("/administrator");
        expect(nd()).toBe("true");
      });

      it("appears when navigating out of admin and goes away on entering it", async () => {
        getUIPreferences.mockReturnValue(new Promise(() => {}));
        function Nav() {
          const nav = useNavigate();
          return <button onClick={() => nav("/admin")}>to-admin</button>;
        }
        render(
          <MemoryRouter initialEntries={["/app"]}>
            <OverlayPreferencesProvider>
              <Nav />
            </OverlayPreferencesProvider>
          </MemoryRouter>,
        );
        expect(design()).toBe("new");
        await click("to-admin");
        expect(design()).toBeUndefined();
      });
    });
  });
});
