// ProfilesTab — v3 handoff §A.17 (one card per codec, .qtable rows, row
// menu). What is asserted here is the tab's own wiring (states, delete
// confirm, toast copy); the load/cancel/poll races behind it are covered
// once in lib/resource/core.test.ts.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, act, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ProfilesTab } from "./ProfilesTab";
import { ToastProvider } from "../../../components/Toast";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { StreamProfile } from "../../../api/types";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");

const mocked = vi.mocked(adminApi);

function profile(over: Partial<StreamProfile> = {}): StreamProfile {
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

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/admin/streaming/profiles"]}>
      <ToastProvider>
        <SectionHeadProvider title="Streaming" tabs={[]}>
          <ProfilesTab />
        </SectionHeadProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  mocked.listStreamProfiles.mockResolvedValue({ items: [profile()] });
});

describe("ProfilesTab", () => {
  it("announces loading, then renders the profiles", async () => {
    renderPage();
    expect(screen.getByRole("status")).toHaveTextContent("Loading…");

    await act(async () => {});
    expect(screen.getByText("1080p60 H.264")).toBeInTheDocument();
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("shows the empty state only once the load settles", async () => {
    mocked.listStreamProfiles.mockResolvedValue({ items: [] });
    renderPage();
    expect(screen.queryByText(/No stream profiles yet/)).toBeNull(); // still loading

    await act(async () => {});
    expect(screen.getByText(/No stream profiles yet/)).toBeInTheDocument();
  });

  it("reports a load failure as an alert and not as an empty state", async () => {
    mocked.listStreamProfiles.mockRejectedValue(new ApiError(502, "internal", "gateway"));
    renderPage();
    await act(async () => {});

    expect(screen.getByRole("alert")).toHaveTextContent("gateway");
    expect(screen.queryByText(/No stream profiles yet/)).toBeNull();
  });

  it("groups by codec into one card per codec, with the fallback/HW chip and a rung count", async () => {
    mocked.listStreamProfiles.mockResolvedValue({
      items: [
        profile({ id: "av1-1", codec: "av1", display_name: "av1-1440p60" }),
        profile({ id: "h264-1", codec: "h264", display_name: "h264-1080p60" }),
      ],
    });
    const { container } = renderPage();
    await act(async () => {});

    const cards = container.querySelectorAll(".card");
    expect(cards).toHaveLength(2);
    expect(cards[0].querySelector(".panel-title")).toHaveTextContent("AV1");
    expect(within(cards[0] as HTMLElement).getByText("hardware required")).toBeInTheDocument();
    expect(within(cards[0] as HTMLElement).getByText("1 rung")).toBeInTheDocument();
    expect(cards[1].querySelector(".panel-title")).toHaveTextContent("H.264");
    expect(within(cards[1] as HTMLElement).getByText("universal fallback")).toBeInTheDocument();
  });

  it("deletes through the confirm modal, drops the row and toasts", async () => {
    mocked.deleteStreamProfile.mockResolvedValue(undefined);
    renderPage();
    await act(async () => {});

    await act(async () => {
      screen.getByRole("button", { name: "Actions for 1080p60 H.264" }).click();
    });
    await act(async () => {
      screen.getByRole("menuitem", { name: "Delete" }).click();
    });
    const modal = screen.getByRole("dialog");
    await act(async () => {
      within(modal).getByRole("button", { name: "Delete profile" }).click();
    });

    expect(mocked.deleteStreamProfile).toHaveBeenCalledWith("tok", "sp1");
    expect(screen.queryByText("1080p60 H.264")).toBeNull();
    expect(screen.getByText('"1080p60 H.264" deleted')).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("keeps the row and shows the server's message when delete is refused", async () => {
    mocked.deleteStreamProfile.mockRejectedValue(
      new ApiError(409, "profile_in_use", "profile is still used by 1 launch profile"),
    );
    const { container } = renderPage();
    await act(async () => {});

    await act(async () => {
      screen.getByRole("button", { name: "Actions for 1080p60 H.264" }).click();
    });
    await act(async () => {
      screen.getByRole("menuitem", { name: "Delete" }).click();
    });
    await act(async () => {
      within(screen.getByRole("dialog")).getByRole("button", { name: "Delete profile" }).click();
    });

    expect(screen.getByText("profile is still used by 1 launch profile")).toBeInTheDocument();
    // The modal repeats the name in its own copy, so match the row's own cell.
    expect(container.querySelector("td .mono.primary")).toHaveTextContent("1080p60 H.264");
  });

  it("opens the editor drawer from the row menu", async () => {
    renderPage();
    await act(async () => {});

    await act(async () => {
      screen.getByRole("button", { name: "Actions for 1080p60 H.264" }).click();
    });
    await act(async () => {
      screen.getByRole("menuitem", { name: "Edit" }).click();
    });

    expect(screen.getByRole("dialog", { name: "1080p60 H.264" })).toBeInTheDocument();
  });

  it("publishes the section head sub-line and primary action", async () => {
    renderPage();
    await act(async () => {});

    expect(screen.getByText("The encode rungs themselves, grouped by codec")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /New stream profile/ })).toBeInTheDocument();
  });

  it("disables Delete with a reason when a launch profile still lists the rung", async () => {
    mocked.listStreamProfiles.mockResolvedValue({
      items: [profile({ used_by: [{ id: "lp1", display_name: "Balanced" }] })],
    });
    renderPage();
    await act(async () => {});

    await act(async () => {
      screen.getByRole("button", { name: "Actions for 1080p60 H.264" }).click();
    });
    const deleteItem = screen.getByRole("menuitem", { name: "Delete" });
    expect(deleteItem).toBeDisabled();
    expect(deleteItem).toHaveAttribute("title", "In use. Remove it from every launch profile first.");
  });

  it("disables Delete with the session-recorded reason once sessions reference the rung", async () => {
    mocked.listStreamProfiles.mockResolvedValue({
      items: [profile({ session_count: 3 })],
    });
    renderPage();
    await act(async () => {});

    await act(async () => {
      screen.getByRole("button", { name: "Actions for 1080p60 H.264" }).click();
    });
    const deleteItem = screen.getByRole("menuitem", { name: "Delete" });
    expect(deleteItem).toBeDisabled();
    expect(deleteItem).toHaveAttribute(
      "title",
      "Recorded by past sessions as the rung they resolved to. It can no longer be deleted.",
    );
  });

  it("appends the session count to the Used by cell once sessions reference the rung", async () => {
    mocked.listStreamProfiles.mockResolvedValue({
      items: [profile({ used_by: [{ id: "lp1", display_name: "Balanced" }], session_count: 2 })],
    });
    renderPage();
    await act(async () => {});

    expect(screen.getByText("1 profile · 2 sessions")).toBeInTheDocument();
  });
});
