/**
 * The v3 route table (spec §3.3): every old admin path still resolves, and the
 * four section containers render the head + tabs their pages sit inside.
 *
 * Pages are allowed to fail their data fetches here — the tree stubs `fetch`
 * with an empty object and each page is wrapped in its own error boundary. The
 * assertions are about routing, so a page that renders an error line still
 * proves the URL landed where it should.
 */

import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, useLocation, Route, Routes } from "react-router-dom";

import { App, RedirectWithParams } from "./App";
import { AuthContext, type AuthContextValue } from "./auth/context";
import { ThemeProvider } from "./settings/ThemeContext";
import { ToastProvider } from "./components/Toast";

vi.mock("./setup/useSetupStatus", async () => {
  const actual = await vi.importActual<typeof import("./setup/useSetupStatus")>(
    "./setup/useSetupStatus",
  );
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

// The admin pages are stubbed: this file is about the route table, and a real
// page would drag its data fetches (and their unhandled rejections) into every
// case. The section containers and the route wiring are the real thing.
vi.mock("./pages/admin/AdminLayout", async () => {
  const { Outlet } = await import("react-router-dom");
  return { AdminLayout: () => <Outlet /> };
});
vi.mock("./pages/admin/Overview", () => ({ Overview: () => "overview page" }));
vi.mock("./pages/admin/Settings", () => ({ Settings: () => "settings page" }));
vi.mock("./pages/admin/Sessions", () => ({ Sessions: () => "sessions page" }));
vi.mock("./pages/admin/SessionDetail", () => ({ SessionDetail: () => "session detail page" }));
vi.mock("./pages/admin/fleet/HostsTab", () => ({ HostsTab: () => "hosts page" }));
vi.mock("./pages/admin/HostDetail", () => ({ HostDetail: () => "host detail page" }));
vi.mock("./pages/admin/fleet/HostSettings", () => ({ HostSettings: () => "host settings page" }));
vi.mock("./pages/admin/fleet/HostConsole", () => ({ HostConsole: () => "host console page" }));
vi.mock("./pages/admin/fleet/StorageTab", () => ({ StorageTab: () => "storage page" }));
vi.mock("./pages/admin/fleet/JobsTab", () => ({ JobsTab: () => "jobs page" }));
vi.mock("./pages/admin/library/AppsTab", () => ({ AppsTab: () => "apps page" }));
vi.mock("./pages/admin/AdminApps/AppEditorPage", () => ({ AppEditorPage: () => "app editor page" }));
vi.mock("./pages/admin/library/PresetsTab", () => ({ PresetsTab: () => "presets page" }));
vi.mock("./pages/admin/library/ImagesTab", () => ({ ImagesTab: () => "images page" }));
vi.mock("./pages/admin/ImageDetail", () => ({ ImageDetail: () => "image detail page" }));
vi.mock("./pages/admin/library/SourcesTab", () => ({ SourcesTab: () => "sources page" }));
vi.mock("./pages/admin/streaming/LaunchTab", () => ({ LaunchTab: () => "launch profiles page" }));
vi.mock("./pages/admin/streaming/ProfilesTab", () => ({ ProfilesTab: () => "stream profiles page" }));
vi.mock("./pages/admin/people/UsersTab", () => ({ UsersTab: () => "users page" }));
vi.mock("./pages/admin/people/InvitesTab", () => ({ InvitesTab: () => "invites page" }));
vi.mock("./pages/admin/Audit", () => ({ Audit: () => "audit page" }));

const admin: AuthContextValue = {
  status: "authenticated",
  user: { id: "u1", email: "a@b.io", username: "admin", role: "admin" } as AuthContextValue["user"],
  token: "t",
  isAdmin: true,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
};

let path = "";

function Probe() {
  const loc = useLocation();
  path = loc.pathname + loc.search;
  return null;
}

function renderAt(entry: string) {
  path = "";
  return render(
    <MemoryRouter initialEntries={[entry]}>
      <ThemeProvider>
        <AuthContext.Provider value={admin}>
          <ToastProvider>
            <App />
            <Probe />
          </ToastProvider>
        </AuthContext.Provider>
      </ThemeProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response("{}", { status: 200, headers: { "content-type": "application/json" } })),
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("legacy admin paths", () => {
  it.each([
    ["/admin/hosts", "/admin/fleet/hosts"],
    ["/admin/hosts/h1/settings", "/admin/fleet/hosts/h1/settings"],
    ["/admin/hosts/h1/console", "/admin/fleet/hosts/h1/console"],
    ["/admin/storage", "/admin/fleet/storage"],
    ["/admin/jobs", "/admin/fleet/jobs"],
    ["/admin/apps", "/admin/library/apps"],
    ["/admin/apps/a1", "/admin/library/apps/a1"],
    ["/admin/runtime-presets", "/admin/library/presets"],
    ["/admin/images", "/admin/library/images"],
    ["/admin/libraries/steam", "/admin/library/sources"],
    ["/admin/launch-profiles", "/admin/streaming/launch"],
    ["/admin/stream-profiles", "/admin/streaming/profiles"],
    ["/admin/users", "/admin/people/users"],
    ["/admin/invites", "/admin/people/invites"],
    ["/admin/activity", "/admin/audit"],
    ["/app/account", "/app/account/profile"],
    ["/app/account/security", "/app/account/profile"],
  ])("redirects %s to %s", async (from, to) => {
    renderAt(from);
    await waitFor(() => expect(path).toBe(to));
  });

  it("carries the query string across a redirect", async () => {
    renderAt("/admin/apps?preset=proton");
    await waitFor(() => expect(path).toBe("/admin/library/apps?preset=proton"));
  });

  it("sends a bare section to its first tab", async () => {
    renderAt("/admin/fleet");
    await waitFor(() => expect(path).toBe("/admin/fleet/hosts"));
  });
});

describe("RedirectWithParams", () => {
  it("substitutes the matched params into the target", async () => {
    render(
      <MemoryRouter initialEntries={["/old/h1/settings?x=1"]}>
        <Routes>
          <Route
            path="/old/:id/settings"
            element={<RedirectWithParams to="/new/:id/settings" />}
          />
          <Route path="/new/:id/settings" element={<Probe />} />
        </Routes>
      </MemoryRouter>,
    );
    await waitFor(() => expect(path).toBe("/new/h1/settings?x=1"));
  });
});

describe("section containers", () => {
  it("renders the section tabs on a tab route", async () => {
    renderAt("/admin/fleet/storage");
    const tabs = await screen.findAllByRole("tab");
    expect(tabs.map((t) => t.textContent)).toEqual(["Hosts", "Storage", "Jobs", "Releases"]);
    expect(screen.getByRole("tab", { name: "Storage" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tab", { name: "Hosts" })).toHaveAttribute("aria-selected", "false");
    expect(screen.getAllByRole("heading", { name: "Fleet", level: 1 })).toHaveLength(1);
  });

  it("names each section once, at the head", async () => {
    renderAt("/admin/people/invites");
    await screen.findByRole("heading", { name: "People", level: 1 });
    const tabs = await screen.findAllByRole("tab");
    expect(tabs.map((t) => t.textContent)).toEqual(["Users", "Invites"]);
  });

  it("routes an image detail id to its own page, outside the Library tab row", async () => {
    renderAt("/admin/library/images/steam");
    await screen.findByText("image detail page");
    expect(screen.queryAllByRole("tab")).toHaveLength(0);
  });
});
