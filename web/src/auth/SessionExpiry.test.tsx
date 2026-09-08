/**
 * An expired token must not leave a signed-in page on screen (#154).
 *
 * The reported symptom was a red banner over a still-populated library, with a
 * "Try again" that could only fail again: the SPA handled a 401 in exactly one
 * place, AuthProvider's mount-time GET /v1/me, so every 401 after that was an
 * ordinary load error. This is the end-to-end shape of the fix — a 401 from any
 * request drives the provider to `unauthenticated`, which makes RequireAuth
 * navigate, which UNMOUNTS the subtree the stale data was in.
 */

import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { AuthProvider } from "./AuthProvider";
import { RequireAuth } from "./RequireAuth";
import { useAuth } from "./context";
import { saveSession, loadSession, type PersistedSession } from "./storage";
import { notifyUnauthorized } from "../api/unauthorized";

vi.mock("../api/auth", () => ({
  getMe: vi.fn(),
  login: vi.fn(),
  logout: vi.fn(),
  postDevice: vi.fn().mockResolvedValue({ device: { id: "d1", first_seen_at: "", last_seen_at: "" } }),
}));
vi.mock("../webrtc/capability", () => ({
  getOrCreateDeviceKey: () => "dev-key",
  probeCapabilities: vi.fn().mockResolvedValue({}),
  deviceProbeIsFresh: vi.fn(() => true),
  markDeviceProbePosted: vi.fn(),
}));

import * as authApi from "../api/auth";

const stored: PersistedSession = {
  token: "tok-1",
  expiresAt: new Date(Date.now() + 3_600_000).toISOString(),
  user: { id: "u1", email: "a@b.co", username: "ab", role: "user" },
};

/** Stands in for a page that has already loaded the user's data. */
function Library() {
  return <div>Cyberpunk 2077</div>;
}

function LoginStub() {
  const { sessionExpired } = useAuth();
  return <div>{sessionExpired ? "sign in — session expired" : "sign in"}</div>;
}

function renderApp() {
  return render(
    <AuthProvider>
      <MemoryRouter initialEntries={["/app"]}>
        <Routes>
          <Route path="/login" element={<LoginStub />} />
          <Route element={<RequireAuth />}>
            <Route path="/app" element={<Library />} />
          </Route>
        </Routes>
      </MemoryRouter>
    </AuthProvider>,
  );
}

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  vi.mocked(authApi.getMe).mockResolvedValue({ user: stored.user } as never);
});

describe("a token rejected mid-session", () => {
  it("navigates to /login and takes the page's contents with it", async () => {
    saveSession(stored, { remember: true });
    renderApp();
    // Signed in, with data on screen.
    expect(await screen.findByText("Cyberpunk 2077")).toBeInTheDocument();

    // The token expires; the next request 401s.
    notifyUnauthorized();

    await waitFor(() => expect(screen.getByText(/sign in/)).toBeInTheDocument());
    // The security requirement: nothing the expired token fetched is still shown.
    expect(screen.queryByText("Cyberpunk 2077")).toBeNull();
  });

  it("clears the stored session, so a reload cannot resurrect it", async () => {
    saveSession(stored, { remember: true });
    renderApp();
    await screen.findByText("Cyberpunk 2077");

    notifyUnauthorized();

    await waitFor(() => expect(loadSession()).toBeNull());
    expect(localStorage.getItem("quasar.auth.token")).toBeNull();
    expect(sessionStorage.getItem("quasar.auth.token")).toBeNull();
  });

  it("tells /login why the user is back there", async () => {
    saveSession(stored, { remember: true });
    renderApp();
    await screen.findByText("Cyberpunk 2077");

    notifyUnauthorized();

    expect(await screen.findByText("sign in — session expired")).toBeInTheDocument();
  });

  it("is idempotent — several requests can fail at once", async () => {
    saveSession(stored, { remember: true });
    renderApp();
    await screen.findByText("Cyberpunk 2077");

    notifyUnauthorized();
    notifyUnauthorized();
    notifyUnauthorized();

    await waitFor(() => expect(screen.getByText(/sign in/)).toBeInTheDocument());
    expect(loadSession()).toBeNull();
  });

  it("says nothing about an expiry when there was no session to lose", async () => {
    // Never signed in: a stray 401 must not invent an "expired" message.
    renderApp();
    expect(await screen.findByText("sign in")).toBeInTheDocument();

    notifyUnauthorized();

    await waitFor(() => expect(screen.getByText("sign in")).toBeInTheDocument());
    expect(screen.queryByText("sign in — session expired")).toBeNull();
  });
});
