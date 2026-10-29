/**
 * A deep link to the account area renders from the preloaded chunk with no route
 * skeleton. Its own file on purpose: React keeps a resolved lazy for the life of the
 * module registry, so after any earlier test in the same file opened the account
 * routes this one could no longer see the fallback (measured: a delayed lazy only fails
 * the test when it runs first).
 *
 * index.html preloads the chunk for an account URL; once it is in, the route is served
 * from it without the skeleton. Under act() React flushes a microtask-resolved lazy
 * before committing a fallback, so the synchronous path itself (the ~300 ms reveal
 * throttle in a browser) is guarded by lib/preloadable.test.tsx, not here. This test
 * fails if the account routes stop going through accountArea (lib/preloadable).
 */

import { act, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { App } from "./App";
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

it("renders /app/account without the route skeleton once its chunk is in", async () => {
  accountArea.preload();
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
