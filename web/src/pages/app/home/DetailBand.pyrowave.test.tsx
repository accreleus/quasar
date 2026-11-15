// Ultra-low latency (PyroWave) as the fifth Codec entry (amendment 58), driven
// through the real band and overlay: the five states of the approved mock, the
// Auto note, and what Play sends.

import { createRef } from "react";
import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { DetailBand } from "./DetailBand";
import { AuthContext, type AuthContextValue } from "../../../auth/context";
import type { App, LaunchProfilePyroWave, ProfilesResponse } from "../../../api/types";

const CAPS = { h264: true, hevc: true, av1: true, vp9: false };
const NAME = "Ultra-low latency (PyroWave)";

const block = (over: Partial<LaunchProfilePyroWave> = {}): LaunchProfilePyroWave => ({
  mode: "prefer",
  available: true,
  reason: null,
  auto: false,
  auto_reason: null,
  ...over,
});

function profiles(pyrowave: LaunchProfilePyroWave | undefined): ProfilesResponse {
  return {
    recommended_id: "p-1440",
    confidence: "high",
    notes: [],
    profiles: [
      {
        id: "p-1440",
        display_name: "1440p60",
        description: "",
        nominal: { width: 2560, height: 1440, fps: 60, bitrate_kbps: 16000 },
        eligibility: "eligible",
        reasons: [],
        pyrowave,
        rungs: [
          {
            id: "r1",
            display_name: "1440p60",
            codec: "hevc",
            width: 2560,
            height: 1440,
            fps: 60,
            nominal_bitrate_kbps: 16000,
            position: 1,
            eligibility: "eligible",
            reasons: [],
          },
        ],
      },
    ],
  } as unknown as ProfilesResponse;
}

const auth: AuthContextValue = {
  status: "authenticated",
  user: { id: "u1", email: "a@b.c", username: "u", role: "user" } as never,
  token: "tok",
  isAdmin: false,
  sessionExpired: false,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
};

function renderOpen(pyrowave: LaunchProfilePyroWave | undefined) {
  const props = {
    app: { id: "a1", name: "Hades II", kind: "game", profile_policy: "inherit", default_profile_id: null } as unknown as App,
    codecCaps: CAPS,
    launching: false,
    waitingForSlot: false,
    profiles: profiles(pyrowave),
    optionsOpen: true,
    optionsToggleRef: createRef<HTMLButtonElement>(),
    onToggleOptions: vi.fn(),
    onCloseOptions: vi.fn(),
    onRetryProfiles: vi.fn(),
    onClose: vi.fn(),
    onConfirmProfile: vi.fn(),
    onToggleFavourite: vi.fn(),
    isBlocked: false,
    blockedByName: null,
    liveSessionId: null,
    isLive: false,
    onResume: vi.fn(),
  };
  render(
    <MemoryRouter>
      <AuthContext.Provider value={auth}>
        <DetailBand {...props} />
      </AuthContext.Provider>
    </MemoryRouter>,
  );
  return props;
}

const codecGroup = () => screen.getByRole("radiogroup", { name: "Codec" });
const entry = () => within(codecGroup()).getByRole("radio", { name: new RegExp(`^${NAME.replace(/[()]/g, "\\$&")}`) });
const panelHead = () => document.querySelector(".lo .qp-spec")!.textContent;

describe("Launch options: Ultra-low latency (PyroWave)", () => {
  it("lists it after the codecs the device can decode, with its one-line note", () => {
    renderOpen(block({ mode: "allow" }));
    const rows = within(codecGroup()).getAllByRole("radio");
    expect(rows.map((r) => r.querySelector(".qr-label")?.textContent)).toEqual(["Auto", "HEVC", NAME]);
    expect(rows[2]).toBe(entry());
    expect(entry()).toBeEnabled();
    expect(entry().querySelector(".qr-sub")?.textContent).toBe("Local network only");
  });

  it("1. Auto picked it: the head reads Auto · PyroWave and the note says why", () => {
    renderOpen(block({ auto: true }));
    expect(panelHead()).toContain("Auto · PyroWave");
    expect(codecGroup()).toHaveTextContent("Here it lands on PyroWave. Auto chose PyroWave: you're on the local network.");
  });

  it("2. Auto did not: it lands on the normal order and PyroWave stays pickable", () => {
    renderOpen(block({ auto: false, auto_reason: "link_too_slow" }));
    expect(panelHead()).toContain("Auto · HEVC");
    expect(codecGroup()).toHaveTextContent("Here it lands on HEVC. Not PyroWave: the link is too slow.");
    expect(entry()).toBeEnabled();
  });

  it("3. picked: the head reads PyroWave, and Play asks for it with no codec", () => {
    const props = renderOpen(block({ mode: "allow" }));
    fireEvent.click(entry());
    expect(entry()).toHaveAttribute("aria-checked", "true");
    expect(panelHead()).toContain("PyroWave");
    expect(panelHead()).not.toContain("Auto");
    expect(codecGroup()).toHaveTextContent("Ultra-low latency has the lowest delay of any codec, and needs a fast local network.");
    fireEvent.click(screen.getByRole("button", { name: /Play now/ }));
    expect(props.onConfirmProfile).toHaveBeenCalledWith("p-1440", undefined, true);
  });

  it("4. disabled when the device cannot decode it, with the reason", () => {
    const props = renderOpen(block({ available: false, reason: "device_no_webgpu" }));
    expect(entry()).toBeDisabled();
    expect(entry().querySelector(".qr-sub")?.textContent).toBe("Your browser can't decode PyroWave");
    expect(entry()).toHaveAttribute("title", "Your browser can't decode PyroWave.");
    fireEvent.click(entry());
    expect(entry()).toHaveAttribute("aria-checked", "false");
    fireEvent.click(screen.getByRole("button", { name: /Play now/ }));
    expect(props.onConfirmProfile).toHaveBeenCalledWith("p-1440", undefined, false);
  });

  it("disabled with its own line for a reason the client does not know", () => {
    renderOpen(block({ available: false, reason: "something_new" }));
    expect(entry()).toBeDisabled();
    expect(entry().querySelector(".qr-sub")?.textContent).toBe("Ultra-low latency isn't available here");
    expect(entry().querySelector(".qr-sub")?.textContent).not.toMatch(/host flagged/);
  });

  it("5. disabled when the profile does not allow it, with the reason", () => {
    renderOpen(block({ mode: "off", available: false, reason: "profile_disallows" }));
    expect(entry()).toBeDisabled();
    expect(entry().querySelector(".qr-sub")?.textContent).toBe("This profile doesn't allow PyroWave");
  });

  it("is shown disabled, not hidden, by a server that sends no block", () => {
    renderOpen(undefined);
    expect(entry()).toBeDisabled();
    expect(entry().querySelector(".qr-sub")?.textContent).toBe("This profile doesn't allow PyroWave");
  });

  it("a launch from Auto and from an explicit codec does not ask for it", () => {
    const props = renderOpen(block({ mode: "allow" }));
    fireEvent.click(screen.getByRole("button", { name: /Play now/ }));
    fireEvent.click(within(codecGroup()).getByRole("radio", { name: /HEVC/ }));
    fireEvent.click(screen.getByRole("button", { name: /Play now/ }));
    expect(props.onConfirmProfile).toHaveBeenNthCalledWith(1, "p-1440", undefined, false);
    expect(props.onConfirmProfile).toHaveBeenNthCalledWith(2, "p-1440", "h265", false);
  });
});
