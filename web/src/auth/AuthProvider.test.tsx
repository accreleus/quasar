/**
 * Rehydration must not change where the session lives. The provider re-writes
 * the session after GET /v1/me confirms the token (so the cached user is the
 * server's), and the store it writes to is the store it read from — otherwise
 * a user who unchecked "Keep me signed in on this device" would find their
 * token promoted to localStorage by the next page load, which is the exact
 * opposite of what they asked for.
 */

import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { AuthProvider } from "./AuthProvider";
import { useAuth } from "./context";
import { saveSession, type PersistedSession } from "./storage";

vi.mock("../api/auth", () => ({
  getMe: vi.fn(),
  login: vi.fn(),
  logout: vi.fn(),
  postDevice: vi.fn().mockResolvedValue({ device: { id: "d1", first_seen_at: "", last_seen_at: "" } }),
}));
vi.mock("../webrtc/capability", () => ({
  getOrCreateDeviceKey: () => "dev-key",
  probeCapabilities: vi.fn().mockResolvedValue({}),
  deviceProbeIsFresh: vi.fn(() => false),
  markDeviceProbePosted: vi.fn(),
}));

import * as authApi from "../api/auth";
import { deviceProbeIsFresh, markDeviceProbePosted } from "../webrtc/capability";

const TOKEN_KEY = "quasar.auth.token";

const stored: PersistedSession = {
  token: "tok-1",
  expiresAt: new Date(Date.now() + 3_600_000).toISOString(),
  user: { id: "u1", email: "a@b.co", username: "ab", role: "user" },
};

function Probe() {
  const { status, user } = useAuth();
  return <div>{status === "authenticated" ? `hi ${user?.username}` : status}</div>;
}

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  vi.mocked(authApi.postDevice).mockClear();
  vi.mocked(deviceProbeIsFresh).mockReturnValue(false);
  vi.mocked(authApi.getMe).mockResolvedValue({
    user: { ...stored.user, username: "ab-fresh" },
  } as never);
});

describe("AuthProvider rehydration", () => {
  it("keeps a session-only token out of localStorage", async () => {
    saveSession(stored, { remember: false });

    render(
      <AuthProvider>
        <Probe />
      </AuthProvider>,
    );

    await waitFor(() => expect(screen.getByText("hi ab-fresh")).toBeInTheDocument());
    expect(sessionStorage.getItem(TOKEN_KEY)).toBe("tok-1");
    expect(localStorage.getItem(TOKEN_KEY)).toBeNull();
  });

  it("leaves a remembered token in localStorage", async () => {
    saveSession(stored, { remember: true });

    render(
      <AuthProvider>
        <Probe />
      </AuthProvider>,
    );

    await waitFor(() => expect(screen.getByText("hi ab-fresh")).toBeInTheDocument());
    expect(localStorage.getItem(TOKEN_KEY)).toBe("tok-1");
    expect(sessionStorage.getItem(TOKEN_KEY)).toBeNull();
  });
});

/**
 * Rehydration runs on every full page load, so an ungated capability re-post
 * is a POST /v1/me/devices per navigation — enough of them on one token and
 * the endpoint answers 429.
 */
describe("AuthProvider device capability re-post", () => {
  it("posts on rehydration when the stored probe is stale", async () => {
    saveSession(stored, { remember: true });

    render(
      <AuthProvider>
        <Probe />
      </AuthProvider>,
    );

    await waitFor(() => expect(authApi.postDevice).toHaveBeenCalledTimes(1));
    expect(markDeviceProbePosted).toHaveBeenCalledWith("u1");
  });

  it("skips the post while the stored probe is still fresh", async () => {
    vi.mocked(deviceProbeIsFresh).mockReturnValue(true);
    saveSession(stored, { remember: true });

    render(
      <AuthProvider>
        <Probe />
      </AuthProvider>,
    );

    await waitFor(() => expect(screen.getByText("hi ab-fresh")).toBeInTheDocument());
    expect(authApi.postDevice).not.toHaveBeenCalled();
  });
});
