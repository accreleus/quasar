// #522 remainder — the admin-wide (not just Settings-page) warning banner
// when the secret store's master key isn't configured. Reuses the SAME
// GET /v1/admin/secrets envelope the Settings page already reads; no new API.

import { render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import * as adminApi from "../../api/admin";
import type { SecretsResponse } from "../../api/types";
import { SecretStoreBanner } from "./SecretStoreBanner";

vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../api/admin");

const mocked = vi.mocked(adminApi);

function envelope(over: Partial<SecretsResponse> = {}): SecretsResponse {
  return {
    secrets: [],
    master_key_configured: true,
    key_versions: [1],
    ...over,
  } as SecretsResponse;
}

beforeEach(() => {
  vi.resetAllMocks();
  sessionStorage.clear();
});

afterEach(() => {
  sessionStorage.clear();
});

describe("SecretStoreBanner", () => {
  it("renders nothing while a master key is configured", async () => {
    mocked.listSecrets.mockResolvedValue(envelope({ master_key_configured: true }));
    render(<SecretStoreBanner />);

    await waitFor(() => expect(mocked.listSecrets).toHaveBeenCalled());
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("warns when no master key is configured, naming the variable", async () => {
    mocked.listSecrets.mockResolvedValue(envelope({ master_key_configured: false }));
    render(<SecretStoreBanner />);

    expect(
      await screen.findByText(/No master key is configured on this control plane/i),
    ).toBeInTheDocument();
    expect(screen.getByText(/QUASAR_SECRET_KEY/)).toBeInTheDocument();
  });

  it("Dismiss hides the banner and the dismissal survives a remount within the session", async () => {
    mocked.listSecrets.mockResolvedValue(envelope({ master_key_configured: false }));
    const { unmount } = render(<SecretStoreBanner />);

    const dismiss = await screen.findByRole("button", { name: /dismiss for this session/i });
    dismiss.click();
    await waitFor(() => expect(screen.queryByRole("status")).not.toBeInTheDocument());

    unmount();
    render(<SecretStoreBanner />);
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });
});
