// useSettings — the pointer-PATCH contract: `patch(key, value)` sends only the
// named key (never a full snapshot), never throws (toast is the error
// report), and `patchOrThrow` is the raw escape hatch Allowed origins needs
// for an inline 400.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { useSettings, type UseSettingsResult } from "./useSettings";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { InstanceSettings, SettingsResponse } from "../../../api/types";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
const addToast = vi.fn();
vi.mock("../../../components/Toast", () => ({ useToast: () => ({ addToast, removeToast: vi.fn() }) }));
vi.mock("../../../api/admin");

const mocked = vi.mocked(adminApi);

function settings(over: Partial<InstanceSettings> = {}): InstanceSettings {
  return {
    registration_mode: "closed",
    storage_provider: "local",
    mic_capture_enabled: false,
    library_discovery_enabled: false,
    library_discovery_interval_minutes: 360,
    library_discovery_appdetails_enabled: false,
    allowed_origins: [],
    updated_by: null,
    updated_at: "2026-07-29T00:00:00Z",
    ...over,
  } as InstanceSettings;
}

function envelope(s: InstanceSettings): SettingsResponse {
  return { settings: s };
}

// `onApi` hands the test the live hook result each render — needed for
// patchOrThrow, whose whole point is that it rejects (a button-click handler
// would have to swallow that rejection itself, which is exactly what the
// caller under test — Settings.tsx's Allowed origins row — does not want).
function Probe({ onApi }: { onApi?: (api: UseSettingsResult) => void }) {
  const s = useSettings();
  onApi?.(s);
  return (
    <div>
      <span data-testid="mode">{s.settings?.registration_mode ?? ""}</span>
      <span data-testid="pending">{s.pending ?? "idle"}</span>
      <button onClick={() => void s.patch("registration_mode", "open")}>patch</button>
    </div>
  );
}

beforeEach(() => {
  vi.resetAllMocks();
  mocked.getSettings.mockResolvedValue(envelope(settings()));
});

describe("useSettings", () => {
  it("loads the envelope on mount", async () => {
    render(<Probe />);
    await screen.findByText("closed");
  });

  it("patch() sends only the one changed key", async () => {
    mocked.updateSettings.mockResolvedValue(envelope(settings({ registration_mode: "open" })));
    render(<Probe />);
    await screen.findByText("closed");

    await act(async () => {
      screen.getByText("patch").click();
    });

    expect(mocked.updateSettings).toHaveBeenCalledWith("tok", { registration_mode: "open" });
    await screen.findByText("open");
  });

  it("pending reports the key currently saving", async () => {
    let resolve!: (v: SettingsResponse) => void;
    mocked.updateSettings.mockReturnValue(
      new Promise((r) => {
        resolve = r;
      }),
    );
    render(<Probe />);
    await screen.findByText("closed");

    await act(async () => {
      screen.getByText("patch").click();
    });
    expect(screen.getByTestId("pending").textContent).toBe("registration_mode");

    await act(async () => {
      resolve(envelope(settings({ registration_mode: "open" })));
    });
    expect(screen.getByTestId("pending").textContent).toBe("idle");
  });

  it("patch() never throws — failure goes to a toast", async () => {
    mocked.updateSettings.mockRejectedValue(new ApiError(500, "internal", "could not reach the database"));
    render(<Probe />);
    await screen.findByText("closed");

    await act(async () => {
      screen.getByText("patch").click();
    });

    expect(addToast).toHaveBeenCalledWith(
      expect.objectContaining({ variant: "danger", title: "could not reach the database" }),
    );
  });

  it("patchOrThrow() rejects with the raw error and fires no toast — the caller owns the message", async () => {
    const apiError = new ApiError(400, "validation_failed", "entry 2 is not a valid origin");
    mocked.updateSettings.mockRejectedValue(apiError);
    let api: UseSettingsResult | undefined;
    render(<Probe onApi={(a) => { api = a; }} />);
    await screen.findByText("closed");

    await act(async () => {
      await expect(
        api!.patchOrThrow({ allowed_origins: ["https://a.example"] }),
      ).rejects.toBeInstanceOf(ApiError);
    });

    expect(mocked.updateSettings).toHaveBeenCalledWith("tok", { allowed_origins: ["https://a.example"] });
    expect(addToast).not.toHaveBeenCalled();
  });
});
