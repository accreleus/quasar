/**
 * A deep link to the account area renders from the preloaded chunk with no route
 * skeleton. Its own file on purpose: React keeps a resolved lazy for the life of the
 * module registry, so after any earlier test in the same file opened the account
 * routes this one could no longer see the fallback (measured: a delayed lazy only fails
 * the test when it runs first).
 *
 * A deep link is wired in two places, and both are pinned here: index.html asks for
 * the chunk (ROUTE_PRELOADS in vite.config.ts) and App starts accountArea.preload() when
 * it loads on an account URL. The render test then fails if the account routes stop
 * going through accountArea (lib/preloadable) or its sync path breaks; the ~300 ms
 * reveal throttle itself only shows in a browser (lib/preloadable.test.tsx).
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { act, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { AuthContext, type AuthContextValue } from "./auth/context";
import { ToastProvider } from "./components/Toast";
import { accountArea } from "./pages/lazyPages";
import { ThemeProvider } from "./settings/ThemeContext";

vi.mock("./setup/useSetupStatus", async () => {
  const actual = await vi.importActual<typeof import("./setup/useSetupStatus")>("./setup/useSetupStatus");
  return {
    ...actual,
    useSetupStatus: () => ({
      status: { admin_exists: true, setup_completed: true },
      loading: false,
      error: false,
      refresh: vi.fn(),
      setStatus: vi.fn(),
    }),
  };
});

const user: AuthContextValue = {
  status: "authenticated",
  user: { id: "u1", email: "a@b.io", username: "admin", role: "admin" } as AuthContextValue["user"],
  token: "t",
  isAdmin: true,
  sessionExpired: false,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
};

beforeEach(() => {
  localStorage.setItem("quasar.ui.classic.u1", "0");
  vi.stubGlobal("fetch", vi.fn(async () => new Response("{}", { status: 200, headers: { "content-type": "application/json" } })));
});

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

it("index.html preloads the account chunk for an account deep link", () => {
  const config = readFileSync(resolve(__dirname, "../vite.config.ts"), "utf8");
  expect(config).toMatch(/["']\/app\/account["']:\s*["']src\/pages\/app\/account\/area\.ts["']/);
});

it("renders /app/account without the route skeleton once its chunk is in", async () => {
  // Load App the way a deep link does: on an account URL, so App itself starts the preload.
  window.history.replaceState(null, "", "/app/account/region");
  const preload = vi.spyOn(accountArea, "preload");
  const { App } = await import("./App");
  expect(preload).toHaveBeenCalled();
  await act(async () => {
    await import("./pages/app/account/area");
  });
  let skeleton = false;
  // Read the added nodes, not the DOM: a skeleton can be gone by the time this runs.
  const seen = new MutationObserver((records) => {
    for (const r of records)
      for (const n of r.addedNodes)
        if (n instanceof Element && (n.matches('[aria-label="Loading page"]') || n.querySelector('[aria-label="Loading page"]'))) skeleton = true;
  });
  seen.observe(document.body, { subtree: true, childList: true });
  render(
    <MemoryRouter initialEntries={["/app/account/region"]}>
      <ThemeProvider>
        <AuthContext.Provider value={user}>
          <ToastProvider>
            <App />
          </ToastProvider>
        </AuthContext.Provider>
      </ThemeProvider>
    </MemoryRouter>,
  );
  expect(await screen.findByRole("heading", { name: "Account", level: 1 })).toBeInTheDocument();
  seen.disconnect();
  expect(skeleton).toBe(false);
});
