// The device card's own contract, and the two page-level consequences that
// only AccountDevices can produce: a trust toggle reaching updateDevice, and
// revoking the current device signing this browser out.

import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { DeviceCard, buildCapChips, deviceLabel } from "./DeviceCard";
import { AccountDevices } from "./AccountDevices";
import { AuthContext, type AuthContextValue } from "../../../auth/context";
import { ToastProvider } from "../../../components/Toast";
import type { Device } from "../../../api/auth";

const listDevices = vi.hoisted(() => vi.fn());
const updateDevice = vi.hoisted(() => vi.fn());
const revokeDevice = vi.hoisted(() => vi.fn());
vi.mock("../../../api/auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../api/auth")>();
  return { ...actual, listDevices, updateDevice, revokeDevice };
});

const clearSession = vi.hoisted(() => vi.fn());
vi.mock("../../../auth/storage", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../auth/storage")>();
  return { ...actual, clearSession };
});

const navigate = vi.hoisted(() => vi.fn());
vi.mock("react-router-dom", async (importOriginal) => {
  const actual = await importOriginal<typeof import("react-router-dom")>();
  return { ...actual, useNavigate: () => navigate };
});

const logout = vi.fn();
const authValue = {
  status: "authenticated",
  user: { id: "u1", email: "me@example.com", username: "salty", role: "user" },
  token: "t0k3n",
  isAdmin: false,
  login: vi.fn(),
  claim: vi.fn(),
  logout,
} as unknown as AuthContextValue;

function device(over: Partial<Device> = {}): Device {
  return {
    id: "d1",
    device_key: "d1f9c40a8b72e5d3",
    name: "Studio desktop",
    trusted: true,
    first_seen_at: "2026-08-01T10:00:00Z",
    last_seen_at: new Date(Date.now() - 60_000).toISOString(),
    current: false,
    active_session_id: null,
    capabilities: {
      max_decode_height: 2160,
      codecs: { hevc: true, av1: true, vp9: true },
      features: { gamepad: true },
      measured_at: "2026-08-08T09:00:00Z",
    },
    ...over,
  } as Device;
}

const noop = async () => {};

function renderCard(d: Device, props: Partial<React.ComponentProps<typeof DeviceCard>> = {}) {
  return render(
    <DeviceCard
      device={d}
      busy={false}
      onRename={noop}
      onSetTrusted={noop}
      onRevoke={() => {}}
      {...props}
    />,
  );
}

function renderPage() {
  return render(
    <MemoryRouter>
      <AuthContext.Provider value={authValue}>
        <ToastProvider>
          <AccountDevices />
        </ToastProvider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  listDevices.mockResolvedValue({ devices: [device({ current: true })] });
  updateDevice.mockImplementation(async (_t: string, _id: string, patch: Partial<Device>) => ({
    device: device({ current: true, ...patch }),
  }));
  revokeDevice.mockResolvedValue(undefined);
});

describe("DeviceCard", () => {
  it("renders the measured capabilities, highlighting the decode ceiling", () => {
    const { container } = renderCard(device());
    expect(screen.getByText("4K H.264")).toBeTruthy();
    expect(screen.getByText("HEVC decode")).toBeTruthy();
    expect(screen.getByText("AV1 decode")).toBeTruthy();
    expect(screen.getByText("VP9")).toBeTruthy();
    expect(screen.getByText("Gamepad API")).toBeTruthy();
    expect(container.querySelector(".cap.hl")?.textContent).toBe("4K H.264");
  });

  it("says so, rather than showing an optimistic default, when nothing was measured", () => {
    renderCard(device({ capabilities: {} }));
    expect(screen.getByText("Not yet measured")).toBeTruthy();
    expect(screen.queryByText(/H\.264/)).toBeNull();
  });

  it("marks the current device and nothing else", () => {
    const { unmount } = renderCard(device({ current: true }));
    expect(screen.getByText("this device")).toBeTruthy();
    unmount();
    renderCard(device({ current: false }));
    expect(screen.queryByText("this device")).toBeNull();
  });

  it("shows a streaming marker only while a session is live", () => {
    const { unmount } = renderCard(device({ active_session_id: "s1" }));
    expect(screen.getByText("streaming now")).toBeTruthy();
    unmount();
    renderCard(device());
    expect(screen.queryByText("streaming now")).toBeNull();
  });

  it("falls back to a readable label when the device has no name", () => {
    renderCard(device({ name: null }));
    expect(screen.getByRole("button", { name: /Device d1f9c40a/ })).toBeTruthy();
  });

  it("reports a trust change and a revoke to its owner", () => {
    const onSetTrusted = vi.fn(async () => {});
    const onRevoke = vi.fn();
    renderCard(device({ trusted: false }), { onSetTrusted, onRevoke });

    fireEvent.click(screen.getByRole("checkbox", { name: "Trusted device" }));
    expect(onSetTrusted).toHaveBeenCalledWith("d1", true);

    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
    expect(onRevoke).toHaveBeenCalled();
  });

  it("goes inert while its own request is in flight", () => {
    renderCard(device(), { busy: true });
    expect(screen.getByRole("checkbox", { name: "Trusted device" })).toHaveProperty(
      "disabled",
      true,
    );
    expect(screen.getByRole("button", { name: "Revoke" })).toHaveProperty("disabled", true);
  });
});

describe("buildCapChips", () => {
  it("labels the decode ceiling by height and highlights it", () => {
    expect(buildCapChips({ max_decode_height: 2160 })[0]).toEqual({
      label: "4K H.264",
      highlight: true,
    });
    expect(buildCapChips({ max_decode_height: 1080 })[0].label).toBe("1080p H.264");
    expect(buildCapChips({ max_decode_height: 720 })[0].label).toBe("720p H.264");
  });

  it("is empty for an unmeasured device", () => {
    expect(buildCapChips({})).toEqual([]);
  });
});

describe("deviceLabel", () => {
  it("prefers the user's name and falls back to a key prefix", () => {
    expect(deviceLabel(device({ name: "Living room TV" }))).toBe("Living room TV");
    expect(deviceLabel(device({ name: null }))).toBe("Device d1f9c40a");
  });
});

describe("AccountDevices", () => {
  it("sends a trust change to the API", async () => {
    renderPage();
    await screen.findByText("Studio desktop");
    fireEvent.click(screen.getByRole("checkbox", { name: "Trusted device" }));
    await waitFor(() =>
      expect(updateDevice).toHaveBeenCalledWith("t0k3n", "d1", { trusted: false }),
    );
  });

  it("confirms before revoking", async () => {
    renderPage();
    await screen.findByText("Studio desktop");
    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
    expect(screen.getByRole("button", { name: "Revoke device" })).toBeTruthy();
    expect(revokeDevice).not.toHaveBeenCalled();
  });

  it("signs this browser out when the revoked device is the current one", async () => {
    renderPage();
    await screen.findByText("Studio desktop");
    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
    fireEvent.click(screen.getByRole("button", { name: "Revoke device" }));

    await waitFor(() => expect(revokeDevice).toHaveBeenCalledWith("t0k3n", "d1"));
    await waitFor(() => expect(clearSession).toHaveBeenCalled());
    expect(logout).toHaveBeenCalled();
    expect(navigate).toHaveBeenCalledWith("/login", { replace: true });
  });

  it("keeps the session when another device is revoked", async () => {
    listDevices.mockResolvedValue({ devices: [device({ current: false })] });
    renderPage();
    await screen.findByText("Studio desktop");
    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
    fireEvent.click(screen.getByRole("button", { name: "Revoke device" }));

    await waitFor(() => expect(revokeDevice).toHaveBeenCalled());
    expect(clearSession).not.toHaveBeenCalled();
    expect(navigate).not.toHaveBeenCalled();
  });

  it("explains where capabilities come from", async () => {
    renderPage();
    expect(
      await screen.findByText(/measured at sign-in, not advertised by the device/i),
    ).toBeTruthy();
  });
});
