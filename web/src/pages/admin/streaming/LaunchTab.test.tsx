// LaunchTab — v3 handoff §A.16 (rung-editor cards, defaults card). What is
// asserted here is the tab's own wiring (states, delete confirm, toast copy,
// the section head it publishes); the load/cancel/poll races behind it are
// covered once in lib/resource/core.test.ts. The fan-out fetch (launch
// profiles + stream profiles + policy) is exercised indirectly: a settled
// load renders the profile using its rung's stream profile data.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, act, within, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { LaunchTab } from "./LaunchTab";
import { ToastProvider } from "../../../components/Toast";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { InstanceSettings, LaunchProfile, SettingsResponse, StreamProfile } from "../../../api/types";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");

const mocked = vi.mocked(adminApi);

function streamProfile(over: Partial<StreamProfile> = {}): StreamProfile {
  return {
    id: "sp1",
    codec: "h264",
    display_name: "1080p60 H.264",
    width: 1920,
    height: 1080,
    fps: 60,
    nominal_bitrate_kbps: 8000,
    abr_floor_kbps: 3000,
    hardware_encoder_required: false,
    browser_client: "recommended",
    used_by: [],
    session_count: 0,
    ...over,
  } as StreamProfile;
}

function launchProfile(over: Partial<LaunchProfile> = {}): LaunchProfile {
  return {
    id: "lp1",
    display_name: "Balanced",
    description: "",
    visibility: "user",
    rungs: [{ stream_profile: streamProfile() }],
    used_by: { apps: [], global_default: false, user_preferences: 0 },
    warnings: [],
    ...over,
  } as LaunchProfile;
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/admin/streaming/launch"]}>
      <ToastProvider>
        <SectionHeadProvider title="Streaming" tabs={[]}>
          <LaunchTab />
        </SectionHeadProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  mocked.listLaunchProfiles.mockResolvedValue({ items: [launchProfile()] });
  mocked.listStreamProfiles.mockResolvedValue({ items: [streamProfile()] });
  mocked.getProfilePolicy.mockResolvedValue({
    global_default_profile_id: null,
    user_overrides_allowed: true,
  });
  mocked.getSettings.mockResolvedValue(settingsWith({ default_transport: "webrtc" }));
});

// Only the field the tab reads; the rest of the envelope is the Settings page's.
function settingsWith(over: Partial<InstanceSettings>): SettingsResponse {
  return { settings: over as InstanceSettings };
}

describe("LaunchTab — default transport", () => {
  it("shows the instance default transport and PATCHes only default_transport", async () => {
    mocked.updateSettings.mockResolvedValue(settingsWith({ default_transport: "webtransport" }));
    renderPage();
    await act(async () => {});

    const select = screen.getByRole("combobox", { name: "Default transport" });
    expect(select).toHaveValue("webrtc");
    await act(async () => {
      fireEvent.change(select, { target: { value: "webtransport" } });
    });

    expect(mocked.updateSettings).toHaveBeenCalledWith("tok", { default_transport: "webtransport" });
  });

  it("reads WebRTC when a pre-amendment server sent no default_transport", async () => {
    mocked.getSettings.mockResolvedValue(settingsWith({ default_transport: undefined }));
    renderPage();
    await act(async () => {});

    expect(screen.getByRole("combobox", { name: "Default transport" })).toHaveValue("webrtc");
  });

  it("says the settings did not load rather than hiding the only transport control", async () => {
    mocked.getSettings.mockRejectedValue(new ApiError(502, "internal", "gateway"));
    renderPage();
    await act(async () => {});

    expect(screen.getByText("Default transport")).toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: "Default transport" })).toBeNull();
    expect(screen.getByRole("alert")).toBeInTheDocument();
  });
});

describe("LaunchTab", () => {
  it("announces loading, then renders the launch profile cards", async () => {
    const { container } = renderPage();
    expect(screen.getAllByRole("status")[0]).toHaveTextContent("Loading…");

    await act(async () => {});
    // "Balanced" also appears as an <option> in the defaults select, so match
    // the card's own title element rather than the text.
    expect(container.querySelector(".panel-title")).toHaveTextContent("Balanced");
  });

  it("publishes the section head sub-line and primary action", async () => {
    renderPage();
    await act(async () => {});
    expect(screen.getByText("What a user picks, and the quality chain behind it")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /New launch profile/ })).toBeInTheDocument();
  });

  it("shows the empty state only once the load settles", async () => {
    mocked.listLaunchProfiles.mockResolvedValue({ items: [] });
    renderPage();
    expect(screen.queryByText(/No launch profiles yet/)).toBeNull(); // still loading

    await act(async () => {});
    expect(screen.getByText(/No launch profiles yet/)).toBeInTheDocument();
  });

  it("reports a load failure as an alert and not as an empty state", async () => {
    mocked.listLaunchProfiles.mockRejectedValue(new ApiError(502, "internal", "gateway"));
    renderPage();
    await act(async () => {});

    expect(screen.getByRole("alert")).toHaveTextContent("gateway");
    expect(screen.queryByText(/No launch profiles yet/)).toBeNull();
  });

  it("renders one .rung row per rung, ranked, with the trailing H.264 rung locked", async () => {
    mocked.listLaunchProfiles.mockResolvedValue({
      items: [
        launchProfile({
          rungs: [
            { position: 0, stream_profile: streamProfile({ id: "av1", codec: "av1", display_name: "AV1 1440p", height: 1440, fps: 60, nominal_bitrate_kbps: 35000 }) },
            { position: 1, stream_profile: streamProfile({ id: "h264", codec: "h264", display_name: "H264 1080p" }) },
          ],
        }),
      ],
    });
    const { container } = renderPage();
    await act(async () => {});

    const rungs = container.querySelectorAll(".rung");
    expect(rungs).toHaveLength(2);
    expect(rungs[0].querySelector(".rank")).toHaveTextContent("1");
    expect(rungs[0].querySelector(".nm")).toHaveTextContent("AV1 1440p60");
    expect(rungs[0].querySelector(".br")).toHaveTextContent("35 Mb/s");
    expect(rungs[1].className).toContain("locked");

    // First rung: up disabled, down enabled. Last (locked) rung: down disabled,
    // remove hidden from a11y.
    const firstCtl = rungs[0].querySelectorAll(".ctl button");
    expect(firstCtl[0]).toBeDisabled();
    expect(firstCtl[1]).not.toBeDisabled();
    const lastCtl = rungs[1].querySelectorAll(".ctl button");
    expect(lastCtl[1]).toBeDisabled();
    expect(lastCtl[2]).toHaveAttribute("title", "The last rung must be H.264");
  });

  it("opens the actions menu and renames through the existing modal", async () => {
    mocked.updateLaunchProfile.mockResolvedValue(launchProfile({ display_name: "Renamed" }));
    renderPage();
    await act(async () => {});

    await act(async () => {
      screen.getByRole("button", { name: "Actions for Balanced" }).click();
    });
    await act(async () => {
      screen.getByRole("menuitem", { name: "Rename profile" }).click();
    });
    const modal = screen.getByRole("dialog", { name: "Rename launch profile" });
    await act(async () => {
      within(modal).getByRole("button", { name: "Save" }).click();
    });

    expect(mocked.updateLaunchProfile).toHaveBeenCalled();
    expect(screen.getByText('"Renamed" saved')).toBeInTheDocument();
  });

  it("deletes through the confirm modal, drops the card and toasts", async () => {
    mocked.deleteLaunchProfile.mockResolvedValue(undefined);
    renderPage();
    await act(async () => {});

    await act(async () => {
      screen.getByRole("button", { name: "Actions for Balanced" }).click();
    });
    await act(async () => {
      screen.getByRole("menuitem", { name: "Delete profile" }).click();
    });
    const modal = screen.getByRole("dialog");
    await act(async () => {
      within(modal).getByRole("button", { name: "Delete profile" }).click();
    });

    expect(mocked.deleteLaunchProfile).toHaveBeenCalledWith("tok", "lp1");
    expect(screen.queryByText("Balanced")).toBeNull();
    expect(screen.getByText('"Balanced" deleted')).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("keeps the card and shows the server's message when delete is refused", async () => {
    mocked.deleteLaunchProfile.mockRejectedValue(
      new ApiError(409, "profile_in_use", "profile is still used by an app"),
    );
    const { container } = renderPage();
    await act(async () => {});

    await act(async () => {
      screen.getByRole("button", { name: "Actions for Balanced" }).click();
    });
    await act(async () => {
      screen.getByRole("menuitem", { name: "Delete profile" }).click();
    });
    await act(async () => {
      within(screen.getByRole("dialog")).getByRole("button", { name: "Delete profile" }).click();
    });

    expect(screen.getByText("profile is still used by an app")).toBeInTheDocument();
    expect(container.querySelector(".panel-title")).toHaveTextContent("Balanced");
  });

  it("changing the default-profile select PATCHes the policy", async () => {
    mocked.updateProfilePolicy.mockResolvedValue({
      global_default_profile_id: "lp1",
      user_overrides_allowed: true,
    });
    renderPage();
    await act(async () => {});

    const select = screen.getByRole("combobox", { name: "Default profile" });
    await act(async () => {
      fireEvent.change(select, { target: { value: "lp1" } });
    });

    expect(mocked.updateProfilePolicy).toHaveBeenCalledWith("tok", {
      global_default_profile_id: "lp1",
      user_overrides_allowed: true,
    });
  });

  it("toggling the overrides switch PATCHes the policy, preserving the current default", async () => {
    mocked.getProfilePolicy.mockResolvedValue({
      global_default_profile_id: "lp1",
      user_overrides_allowed: true,
    });
    mocked.updateProfilePolicy.mockResolvedValue({
      global_default_profile_id: "lp1",
      user_overrides_allowed: false,
    });
    renderPage();
    await act(async () => {});

    const toggle = screen.getByRole("checkbox", { name: "Let users choose a profile" });
    await act(async () => {
      fireEvent.click(toggle);
    });

    expect(mocked.updateProfilePolicy).toHaveBeenCalledWith("tok", {
      global_default_profile_id: "lp1",
      user_overrides_allowed: false,
    });
  });

  it("'Set as default' in the card menu PATCHes the policy to that profile", async () => {
    mocked.listLaunchProfiles.mockResolvedValue({
      items: [launchProfile(), launchProfile({ id: "lp2", display_name: "Other" })],
    });
    mocked.updateProfilePolicy.mockResolvedValue({
      global_default_profile_id: "lp2",
      user_overrides_allowed: true,
    });
    renderPage();
    await act(async () => {});

    await act(async () => {
      screen.getByRole("button", { name: "Actions for Other" }).click();
    });
    await act(async () => {
      screen.getByRole("menuitem", { name: "Set as default" }).click();
    });

    expect(mocked.updateProfilePolicy).toHaveBeenCalledWith("tok", {
      global_default_profile_id: "lp2",
      user_overrides_allowed: true,
    });
  });

  it("moving a rung down PATCHes the full reordered id array", async () => {
    mocked.listLaunchProfiles.mockResolvedValue({
      items: [
        launchProfile({
          rungs: [
            { position: 0, stream_profile: streamProfile({ id: "av1", codec: "av1", display_name: "AV1 1440p" }) },
            { position: 1, stream_profile: streamProfile({ id: "hevc", codec: "hevc", display_name: "HEVC 1440p" }) },
            { position: 2, stream_profile: streamProfile({ id: "h264", codec: "h264", display_name: "H264 1080p" }) },
          ],
        }),
      ],
    });
    mocked.updateLaunchProfile.mockResolvedValue(launchProfile());
    const { container } = renderPage();
    await act(async () => {});

    const firstRungDown = container.querySelectorAll(".rung")[0].querySelectorAll(".ctl button")[1];
    await act(async () => {
      fireEvent.click(firstRungDown);
    });

    expect(mocked.updateLaunchProfile).toHaveBeenCalledWith("tok", "lp1", {
      display_name: "Balanced",
      description: "",
      rungs: ["hevc", "av1", "h264"],
    });
  });

  it("removing a rung PATCHes the remaining ids, in order", async () => {
    mocked.listLaunchProfiles.mockResolvedValue({
      items: [
        launchProfile({
          rungs: [
            { position: 0, stream_profile: streamProfile({ id: "av1", codec: "av1", display_name: "AV1 1440p" }) },
            { position: 1, stream_profile: streamProfile({ id: "hevc", codec: "hevc", display_name: "HEVC 1440p" }) },
            { position: 2, stream_profile: streamProfile({ id: "h264", codec: "h264", display_name: "H264 1080p" }) },
          ],
        }),
      ],
    });
    mocked.updateLaunchProfile.mockResolvedValue(launchProfile());
    const { container } = renderPage();
    await act(async () => {});

    const middleRungRemove = container.querySelectorAll(".rung")[1].querySelector(".ctl button.rm")!;
    await act(async () => {
      fireEvent.click(middleRungRemove);
    });

    expect(mocked.updateLaunchProfile).toHaveBeenCalledWith("tok", "lp1", {
      display_name: "Balanced",
      description: "",
      rungs: ["av1", "h264"],
    });
  });

  it("adding a rung PATCHes the existing ids plus the picked one, appended", async () => {
    mocked.listLaunchProfiles.mockResolvedValue({
      items: [
        launchProfile({
          rungs: [{ position: 0, stream_profile: streamProfile({ id: "h264", codec: "h264" }) }],
        }),
      ],
    });
    mocked.listStreamProfiles.mockResolvedValue({
      items: [
        streamProfile({ id: "h264" }),
        streamProfile({ id: "av1-new", codec: "av1", display_name: "AV1 new" }),
      ],
    });
    mocked.updateLaunchProfile.mockResolvedValue(launchProfile());
    renderPage();
    await act(async () => {});

    const addSelect = screen.getByRole("combobox", { name: "Add a stream profile" });
    await act(async () => {
      fireEvent.change(addSelect, { target: { value: "av1-new" } });
    });
    await act(async () => {
      screen.getByRole("button", { name: "Add" }).click();
    });

    expect(mocked.updateLaunchProfile).toHaveBeenCalledWith("tok", "lp1", {
      display_name: "Balanced",
      description: "",
      rungs: ["h264", "av1-new"],
    });
  });

  it("saves the Delivery transport as soon as it changes, with the rungs untouched", async () => {
    mocked.updateLaunchProfile.mockResolvedValue(launchProfile({ transport: "webtransport" }));
    renderPage();
    await act(async () => {});

    const select = screen.getByRole("combobox", { name: "Transport for Balanced" });
    expect(select).toHaveValue("webrtc"); // an absent field reads as webrtc
    await act(async () => {
      fireEvent.change(select, { target: { value: "webtransport" } });
    });

    expect(mocked.updateLaunchProfile).toHaveBeenCalledWith("tok", "lp1", {
      display_name: "Balanced",
      description: "",
      rungs: ["sp1"],
      transport: "webtransport",
    });
    expect(screen.getByRole("combobox", { name: "Transport for Balanced" })).toHaveValue("webtransport");
  });

  it("offers Use instance default and saves it as the value default", async () => {
    mocked.listLaunchProfiles.mockResolvedValue({ items: [launchProfile({ transport: "webtransport" })] });
    mocked.updateLaunchProfile.mockResolvedValue(launchProfile({ transport: "default" }));
    renderPage();
    await act(async () => {});

    const select = screen.getByRole("combobox", { name: "Transport for Balanced" });
    expect(within(select).getByRole("option", { name: "Use instance default" })).toHaveValue("default");
    await act(async () => {
      fireEvent.change(select, { target: { value: "default" } });
    });

    expect(mocked.updateLaunchProfile.mock.calls[0][2]).toMatchObject({ transport: "default" });
    expect(screen.getByRole("combobox", { name: "Transport for Balanced" })).toHaveValue("default");
  });

  it("keeps a developer opt-in profile visible, and a rung edit leaves its transport alone", async () => {
    mocked.listLaunchProfiles.mockResolvedValue({ items: [launchProfile({ transport: "webtransport_opt_in" })] });
    mocked.listStreamProfiles.mockResolvedValue({
      items: [streamProfile(), streamProfile({ id: "av1-new", codec: "av1", display_name: "AV1 new" })],
    });
    mocked.updateLaunchProfile.mockResolvedValue(launchProfile({ transport: "webtransport_opt_in" }));
    renderPage();
    await act(async () => {});
    expect(screen.getByRole("combobox", { name: "Transport for Balanced" })).toHaveValue("webtransport_opt_in");

    await act(async () => {
      fireEvent.change(screen.getByRole("combobox", { name: "Add a stream profile" }), { target: { value: "av1-new" } });
    });
    await act(async () => {
      screen.getByRole("button", { name: "Add" }).click();
    });
    expect(mocked.updateLaunchProfile).toHaveBeenCalledTimes(1);
    expect(mocked.updateLaunchProfile.mock.calls[0][2]).not.toHaveProperty("transport");
  });

  it("shows a transport it does not know as itself and disabled, not as WebRTC", async () => {
    mocked.listLaunchProfiles.mockResolvedValue({ items: [launchProfile({ transport: "quic" as never })] });
    renderPage();
    await act(async () => {});
    const select = screen.getByRole("combobox", { name: "Transport for Balanced" });
    expect(select).toHaveValue("quic");
    expect(select).toBeDisabled();
    expect(within(select).getByRole("option", { name: "quic" })).toBeInTheDocument();
  });

  it("puts the select back and toasts when the transport save fails", async () => {
    mocked.updateLaunchProfile.mockRejectedValue(new ApiError(500, "internal", "disk full"));
    renderPage();
    await act(async () => {});
    const select = screen.getByRole("combobox", { name: "Transport for Balanced" });
    await act(async () => {
      fireEvent.change(select, { target: { value: "webtransport" } });
    });
    expect(screen.getByRole("combobox", { name: "Transport for Balanced" })).toHaveValue("webrtc");
    expect(await screen.findByText("disk full")).toBeInTheDocument();
  });

  describe("Ultra-low latency (PyroWave)", () => {
    const modeSelect = () => screen.getByRole("combobox", { name: "Ultra-low latency (PyroWave) for Balanced" });
    const bitrate = () => screen.queryByRole("spinbutton", { name: "Maximum bitrate for Balanced" });
    const patched = () => mocked.updateLaunchProfile.mock.calls[0][2];

    it("reads Off when the server sent no mode, and shows no Maximum bitrate row", async () => {
      renderPage();
      await act(async () => {});
      expect(modeSelect()).toHaveValue("off");
      expect(within(modeSelect()).getAllByRole("option").map((o) => o.textContent)).toEqual([
        "Off",
        "Allow",
        "Prefer when possible",
      ]);
      expect(bitrate()).toBeNull();
    });

    it("saves the mode alone as soon as it changes, then shows the bitrate row", async () => {
      mocked.updateLaunchProfile.mockResolvedValue(launchProfile({ pyrowave_mode: "allow", pyrowave_kbps: null }));
      renderPage();
      await act(async () => {});
      await act(async () => {
        fireEvent.change(modeSelect(), { target: { value: "allow" } });
      });

      expect(patched()).toEqual({ display_name: "Balanced", description: "", rungs: ["sp1"], pyrowave_mode: "allow" });
      expect(modeSelect()).toHaveValue("allow");
      expect(bitrate()).toHaveValue(null); // blank is automatic
      expect(bitrate()).toHaveAttribute("placeholder", "Auto");
    });

    it("shows Prefer with its bitrate in Mb/s, and Off hides the row again", async () => {
      mocked.listLaunchProfiles.mockResolvedValue({
        items: [launchProfile({ pyrowave_mode: "prefer", pyrowave_kbps: 200000 })],
      });
      mocked.updateLaunchProfile.mockResolvedValue(launchProfile({ pyrowave_mode: "off", pyrowave_kbps: 200000 }));
      renderPage();
      await act(async () => {});
      expect(modeSelect()).toHaveValue("prefer");
      expect(bitrate()).toHaveValue(200);

      await act(async () => {
        fireEvent.change(modeSelect(), { target: { value: "off" } });
      });
      expect(patched()).toMatchObject({ pyrowave_mode: "off" });
      expect(patched()).not.toHaveProperty("pyrowave_kbps");
      expect(bitrate()).toBeNull();
    });

    it("saves a number as kbit/s on blur", async () => {
      mocked.listLaunchProfiles.mockResolvedValue({ items: [launchProfile({ pyrowave_mode: "allow" })] });
      mocked.updateLaunchProfile.mockResolvedValue(launchProfile({ pyrowave_mode: "allow", pyrowave_kbps: 150500 }));
      renderPage();
      await act(async () => {});
      fireEvent.change(bitrate()!, { target: { value: "150.5" } });
      expect(mocked.updateLaunchProfile).not.toHaveBeenCalled(); // not per keystroke
      await act(async () => {
        fireEvent.blur(bitrate()!);
      });

      expect(patched()).toMatchObject({ pyrowave_kbps: 150500 });
      expect(patched()).not.toHaveProperty("pyrowave_mode");
      expect(bitrate()).toHaveValue(150.5);
    });

    it("saves a cleared field as null (automatic) on Enter", async () => {
      mocked.listLaunchProfiles.mockResolvedValue({
        items: [launchProfile({ pyrowave_mode: "prefer", pyrowave_kbps: 200000 })],
      });
      mocked.updateLaunchProfile.mockResolvedValue(launchProfile({ pyrowave_mode: "prefer", pyrowave_kbps: null }));
      renderPage();
      await act(async () => {});
      fireEvent.change(bitrate()!, { target: { value: "" } });
      await act(async () => {
        fireEvent.keyDown(bitrate()!, { key: "Enter" });
      });

      expect(patched()).toMatchObject({ pyrowave_kbps: null });
      expect(bitrate()).toHaveValue(null);
    });

    it("saves nothing when the bitrate did not change", async () => {
      mocked.listLaunchProfiles.mockResolvedValue({
        items: [launchProfile({ pyrowave_mode: "prefer", pyrowave_kbps: 200000 })],
      });
      renderPage();
      await act(async () => {});
      await act(async () => {
        fireEvent.blur(bitrate()!);
      });
      expect(mocked.updateLaunchProfile).not.toHaveBeenCalled();
    });

    it.each(["10", "1001", "0"])("refuses %s Mb/s with a message and saves nothing", async (value) => {
      mocked.listLaunchProfiles.mockResolvedValue({ items: [launchProfile({ pyrowave_mode: "allow" })] });
      renderPage();
      await act(async () => {});
      fireEvent.change(bitrate()!, { target: { value } });
      await act(async () => {
        fireEvent.blur(bitrate()!);
      });

      expect(screen.getByRole("alert")).toHaveTextContent("Enter 50 to 1000 Mb/s, or leave it blank for automatic.");
      expect(bitrate()).toHaveAttribute("aria-invalid", "true");
      expect(mocked.updateLaunchProfile).not.toHaveBeenCalled();
    });

    it("puts the mode back and toasts when the save fails", async () => {
      mocked.updateLaunchProfile.mockRejectedValue(new ApiError(500, "internal", "disk full"));
      renderPage();
      await act(async () => {});
      await act(async () => {
        fireEvent.change(modeSelect(), { target: { value: "prefer" } });
      });
      expect(modeSelect()).toHaveValue("off");
      expect(await screen.findByText("disk full")).toBeInTheDocument();
    });

    it("toasts the server's warning that PyroWave needs WebTransport", async () => {
      mocked.updateLaunchProfile.mockResolvedValue(
        launchProfile({
          pyrowave_mode: "allow",
          warnings: [{ code: "pyrowave_needs_webtransport", message: "PyroWave needs WebTransport." }],
        }),
      );
      renderPage();
      await act(async () => {});
      await act(async () => {
        fireEvent.change(modeSelect(), { target: { value: "allow" } });
      });
      expect(await screen.findByText("PyroWave needs WebTransport.")).toBeInTheDocument();
    });
  });
});
