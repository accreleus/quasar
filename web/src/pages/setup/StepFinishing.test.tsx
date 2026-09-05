/**
 * StepFinishing — wizard step 5, wizard v2 §S1/S2/S3/S7. Covers: the artwork
 * SecretField renders and is skippable (including when fetching it fails
 * outright — the air-gapped/offline case S1 was built for), the .env backup
 * line is always present, the mic toggle PATCHes mic_capture_enabled, and
 * Finish setup triggers a best-effort library scan ONLY when
 * library_discovery_enabled is KNOWN true — showing the inert reason
 * verbatim on an inert result, an operator-language line on a thrown error
 * or a timed-out/aborted request, and never blocking onFinish either way.
 *
 * Also covers the three lifecycle findings from Alice's PR #480 review:
 *   1. Finish is disabled only while the FIRST settings load is in flight;
 *      a load FAILURE unblocks it (fail open) and skips the scan (unknown
 *      is not "on").
 *   2. A stalled scan is aborted after SCAN_TIMEOUT_MS and releases into
 *      the same best-effort failure path — it can never hold Finish
 *      hostage.
 *   3. The initial settings/secrets loads AND an in-flight scan are aborted
 *      on unmount.
 */

import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { StepFinishing, SCAN_TIMEOUT_MS } from "./StepFinishing";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { ToastProvider } from "../../components/Toast";
import { ApiError } from "../../api/client";
import type { InstanceSettings, SecretStatus, SecretsResponse, ForceScanResult } from "../../api/types";

vi.mock("../../api/admin", () => ({
  getSettings: vi.fn(),
  updateSettings: vi.fn(),
  listSecrets: vi.fn(),
  forceLibraryScan: vi.fn(),
}));

import * as adminApi from "../../api/admin";

function settings(over: Partial<InstanceSettings> = {}): { settings: InstanceSettings } {
  return {
    settings: {
      registration_mode: "closed",
      storage_provider: "local",
      mic_capture_enabled: false,
      library_discovery_enabled: false,
      library_discovery_interval_minutes: 360,
      library_discovery_appdetails_enabled: false,
      updated_by: null,
      updated_at: "2026-08-09T00:00:00Z",
      ...over,
    } as InstanceSettings,
  };
}

function artworkSecret(over: Partial<SecretStatus> = {}): SecretStatus {
  return {
    name: "artwork.steamgriddb.api_key",
    label: "SteamGridDB API key",
    description: "Looks up cover artwork for apps in your catalogue.",
    env_var: "QUASAR_STEAMGRIDDB_API_KEY",
    docs_url: "",
    configured: false,
    readable: false,
    hint: "",
    env_set: false,
    origin: "none",
    key_version: 0,
    updated_by: null,
    updated_at: null,
    ...over,
  } as SecretStatus;
}

function secretsEnvelope(secrets: SecretStatus[] = [artworkSecret()]): SecretsResponse {
  return { secrets, master_key_configured: true, key_versions: [1] } as SecretsResponse;
}

function scanResult(over: Partial<ForceScanResult> = {}): ForceScanResult {
  return { queued: 1, skipped: 0, eligible: 1, inert_reason: "", ...over } as ForceScanResult;
}

/** An AbortError shaped the way a real aborted `fetch` rejects with one —
 *  duck-typed (`name: "AbortError"`), matching `isAbortError` in the
 *  component rather than relying on a real `DOMException`. */
function abortError(): Error {
  const err = new Error("The operation was aborted.");
  err.name = "AbortError";
  return err;
}

function renderStep(onFinish = vi.fn()) {
  const authValue: AuthContextValue = {
    status: "authenticated",
    user: { id: "u1", email: "admin@example.com", username: "admin", role: "admin" },
    token: "tok",
    isAdmin: true,
    login: vi.fn(),
    claim: vi.fn(),
    logout: vi.fn(),
  };
  const result = render(
    <ToastProvider>
      <AuthContext.Provider value={authValue}>
        <StepFinishing onFinish={onFinish} />
      </AuthContext.Provider>
    </ToastProvider>,
  );
  return { onFinish, unmount: result.unmount };
}

/** Waits past both initial loads AND the Finish-button loading gate — the
 *  common "the step is ready to interact with" point most tests start from. */
async function waitUntilReady() {
  await waitFor(() => expect(screen.getByRole("button", { name: /^finish setup$/i })).toBeEnabled());
}

describe("StepFinishing", () => {
  beforeEach(() => {
    vi.mocked(adminApi.getSettings).mockReset();
    vi.mocked(adminApi.updateSettings).mockReset();
    vi.mocked(adminApi.listSecrets).mockReset();
    vi.mocked(adminApi.forceLibraryScan).mockReset();
  });

  it("renders the artwork SecretField and the .env backup warning", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settings());
    vi.mocked(adminApi.listSecrets).mockResolvedValue(secretsEnvelope());
    renderStep();

    await waitFor(() => expect(screen.getByLabelText("SteamGridDB API key")).toBeInTheDocument());
    expect(screen.getByText(/back up deploy\/\.env now/i)).toBeInTheDocument();
    expect(screen.getByText(/QUASAR_SECRET_KEY/)).toBeInTheDocument();
  });

  it("is skippable: Finish setup works with no key entered and discovery off", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settings({ library_discovery_enabled: false }));
    vi.mocked(adminApi.listSecrets).mockResolvedValue(secretsEnvelope());
    const { onFinish } = renderStep();

    await waitUntilReady();
    fireEvent.click(screen.getByRole("button", { name: /^finish setup$/i }));

    await waitFor(() => expect(onFinish).toHaveBeenCalled());
    expect(adminApi.forceLibraryScan).not.toHaveBeenCalled();
  });

  it("requires operator confirmation before calling the first stream checked", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settings());
    vi.mocked(adminApi.listSecrets).mockResolvedValue(secretsEnvelope());
    renderStep();
    await waitUntilReady();
    expect(screen.getByText(/Streaming is not yet confirmed/)).toBeInTheDocument();
    const link = screen.getByRole("link", { name: /Open library for a test stream/ });
    expect(link).toHaveAttribute("href", "/app");
    expect(link).toHaveAttribute("target", "_blank");
    for (const name of [/I can see the app/, /I can hear sound/, /Keyboard, mouse or controller/]) {
      fireEvent.click(screen.getByRole("checkbox", { name }));
    }
    expect(screen.getByText(/You have confirmed video, audio and input/)).toBeInTheDocument();
  });

  // Item 4 (Alice PR #480 review): the requirement is that an air-gapped or
  // offline install can finish — and offline is precisely where
  // GET /v1/admin/secrets REJECTS, not where it happens to return an empty
  // list. This is the test that actually encodes that requirement.
  it("is skippable even when fetching credentials fails entirely (air-gapped/offline)", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settings({ library_discovery_enabled: false }));
    vi.mocked(adminApi.listSecrets).mockRejectedValue(
      new ApiError(0, "network_error", "could not reach the control plane"),
    );
    const { onFinish } = renderStep();

    await waitFor(() => expect(screen.getByText(/could not reach the control plane/i)).toBeInTheDocument());
    // No secrets envelope ever arrived, so the artwork field itself never
    // renders — the operator sees the error, not a broken/stuck field.
    expect(screen.queryByLabelText("SteamGridDB API key")).not.toBeInTheDocument();

    await waitUntilReady();
    fireEvent.click(screen.getByRole("button", { name: /^finish setup$/i }));

    await waitFor(() => expect(onFinish).toHaveBeenCalled());
    expect(adminApi.forceLibraryScan).not.toHaveBeenCalled();
  });

  it("triggers a library scan on finish when discovery is enabled, then requires a second click to continue", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settings({ library_discovery_enabled: true }));
    vi.mocked(adminApi.listSecrets).mockResolvedValue(secretsEnvelope());
    vi.mocked(adminApi.forceLibraryScan).mockResolvedValue(scanResult());
    const { onFinish } = renderStep();

    await waitUntilReady();
    fireEvent.click(screen.getByRole("button", { name: /^finish setup$/i }));

    await waitFor(() =>
      expect(adminApi.forceLibraryScan).toHaveBeenCalledWith("tok", {}, expect.any(AbortSignal)),
    );
    await waitFor(() => {
      expect(screen.getByText(/a library scan has started/i)).toBeInTheDocument();
    });
    expect(onFinish).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: /continue to quasar/i }));
    expect(onFinish).toHaveBeenCalled();
  });

  it("shows the inert reason verbatim when the scan comes back inert", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settings({ library_discovery_enabled: true }));
    vi.mocked(adminApi.listSecrets).mockResolvedValue(secretsEnvelope());
    vi.mocked(adminApi.forceLibraryScan).mockResolvedValue(
      scanResult({ queued: 0, eligible: 0, inert_reason: "no app is marked as a library provider yet" }),
    );
    renderStep();

    await waitUntilReady();
    fireEvent.click(screen.getByRole("button", { name: /^finish setup$/i }));

    await waitFor(() => {
      expect(screen.getByText(/no app is marked as a library provider yet/i)).toBeInTheDocument();
    });
  });

  it("a scan failure is best-effort — reported inline, never blocking finish", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settings({ library_discovery_enabled: true }));
    vi.mocked(adminApi.listSecrets).mockResolvedValue(secretsEnvelope());
    vi.mocked(adminApi.forceLibraryScan).mockRejectedValue(
      new ApiError(500, "internal", "could not reach the database"),
    );
    const { onFinish } = renderStep();

    await waitUntilReady();
    fireEvent.click(screen.getByRole("button", { name: /^finish setup$/i }));

    await waitFor(() => {
      expect(screen.getByText(/could not start the initial library scan automatically/i)).toBeInTheDocument();
    });

    const continueBtn = screen.getByRole("button", { name: /continue to quasar/i });
    expect(continueBtn).toBeEnabled();
    fireEvent.click(continueBtn);
    expect(onFinish).toHaveBeenCalled();
  });

  // Item 2 (Alice PR #480 review): a scan that never resolves must not hold
  // Finish hostage forever — it is aborted after SCAN_TIMEOUT_MS and treated
  // exactly like a thrown error.
  it("aborts a stalled scan after the timeout and releases into the best-effort failure path", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settings({ library_discovery_enabled: true }));
    vi.mocked(adminApi.listSecrets).mockResolvedValue(secretsEnvelope());
    vi.mocked(adminApi.forceLibraryScan).mockImplementation(
      (_token, _scope, signal) =>
        new Promise((_resolve, reject) => {
          signal?.addEventListener("abort", () => reject(abortError()));
        }),
    );
    renderStep();

    await waitUntilReady();

    vi.useFakeTimers();
    try {
      fireEvent.click(screen.getByRole("button", { name: /^finish setup$/i }));
      await vi.advanceTimersByTimeAsync(SCAN_TIMEOUT_MS);
    } finally {
      vi.useRealTimers();
    }

    expect(await screen.findByText(/could not start the initial library scan automatically/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /continue to quasar/i })).toBeEnabled();
  });

  // Item 3 (Alice PR #480 review): unmounting mid-scan must not leave a
  // best-effort request running past the step's own lifetime.
  it("aborts an in-flight scan when the step unmounts", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settings({ library_discovery_enabled: true }));
    vi.mocked(adminApi.listSecrets).mockResolvedValue(secretsEnvelope());
    let capturedSignal: AbortSignal | undefined;
    vi.mocked(adminApi.forceLibraryScan).mockImplementation((_token, _scope, signal) => {
      capturedSignal = signal;
      return new Promise(() => {
        /* never resolves — simulates a stalled request */
      });
    });
    const { unmount } = renderStep();

    await waitUntilReady();
    fireEvent.click(screen.getByRole("button", { name: /^finish setup$/i }));
    await waitFor(() => expect(adminApi.forceLibraryScan).toHaveBeenCalled());

    expect(capturedSignal?.aborted).toBe(false);
    unmount();
    expect(capturedSignal?.aborted).toBe(true);
  });

  // Item 3 (Alice PR #480 review): the two initial loads share one
  // AbortController, aborted on unmount.
  it("aborts the initial settings/secrets loads on unmount", async () => {
    let settingsSignal: AbortSignal | undefined;
    let secretsSignal: AbortSignal | undefined;
    vi.mocked(adminApi.getSettings).mockImplementation((_token, signal) => {
      settingsSignal = signal;
      return new Promise(() => {
        /* never resolves before unmount */
      });
    });
    vi.mocked(adminApi.listSecrets).mockImplementation((_token, signal) => {
      secretsSignal = signal;
      return new Promise(() => {
        /* never resolves before unmount */
      });
    });
    const { unmount } = renderStep();

    await waitFor(() => {
      expect(settingsSignal).toBeDefined();
      expect(secretsSignal).toBeDefined();
    });

    unmount();

    expect(settingsSignal?.aborted).toBe(true);
    expect(secretsSignal?.aborted).toBe(true);
  });

  // Item 1 (Alice PR #480 review): Finish must be disabled ONLY while the
  // first settings load is actually in flight.
  it("disables Finish only while the initial settings load is in flight", async () => {
    let resolveSettings!: (v: { settings: InstanceSettings }) => void;
    vi.mocked(adminApi.getSettings).mockReturnValue(
      new Promise((resolve) => {
        resolveSettings = resolve;
      }),
    );
    vi.mocked(adminApi.listSecrets).mockResolvedValue(secretsEnvelope());
    renderStep();

    await waitFor(() => expect(screen.getByLabelText("SteamGridDB API key")).toBeInTheDocument());
    expect(screen.getByRole("button", { name: /loading/i })).toBeDisabled();
    expect(adminApi.forceLibraryScan).not.toHaveBeenCalled();

    resolveSettings(settings({ library_discovery_enabled: false }));
    await waitFor(() => expect(screen.getByRole("button", { name: /^finish setup$/i })).toBeEnabled());
  });

  // Item 1 (Alice PR #480 review): a settings-load FAILURE must not trap the
  // operator either — Finish unblocks, and (since "unknown" is not "on")
  // the scan is skipped rather than guessed at.
  it("a settings-load failure does not trap the operator — Finish still completes and the scan is skipped", async () => {
    vi.mocked(adminApi.getSettings).mockRejectedValue(
      new ApiError(500, "internal", "could not reach settings"),
    );
    vi.mocked(adminApi.listSecrets).mockResolvedValue(secretsEnvelope());
    const { onFinish } = renderStep();

    await waitFor(() => expect(screen.getByText(/could not reach settings/i)).toBeInTheDocument());
    await waitUntilReady();

    fireEvent.click(screen.getByRole("button", { name: /^finish setup$/i }));
    await waitFor(() => expect(onFinish).toHaveBeenCalled());
    expect(adminApi.forceLibraryScan).not.toHaveBeenCalled();
  });

  it("toggling the mic switch PATCHes mic_capture_enabled and reflects the response", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settings({ mic_capture_enabled: false }));
    vi.mocked(adminApi.listSecrets).mockResolvedValue(secretsEnvelope());
    vi.mocked(adminApi.updateSettings).mockResolvedValue(settings({ mic_capture_enabled: true }));
    renderStep();

    const toggle = (await screen.findByLabelText(/enable microphone capture/i)) as HTMLInputElement;
    expect(toggle.checked).toBe(false);
    fireEvent.click(toggle);

    await waitFor(() => {
      expect(adminApi.updateSettings).toHaveBeenCalledWith("tok", { mic_capture_enabled: true });
    });
    // Assert the CONTROL reflects the PATCH response, not just the call args.
    await waitFor(() => expect(toggle.checked).toBe(true));
    expect(screen.getByLabelText(/^enabled$/i)).toBeInTheDocument();
  });

  it("states the secure-context precondition for the microphone", async () => {
    vi.mocked(adminApi.getSettings).mockResolvedValue(settings());
    vi.mocked(adminApi.listSecrets).mockResolvedValue(secretsEnvelope());
    renderStep();

    await waitFor(() => expect(screen.getByLabelText("SteamGridDB API key")).toBeInTheDocument());
    expect(screen.getByText(/secure context/i)).toBeInTheDocument();
  });
});
