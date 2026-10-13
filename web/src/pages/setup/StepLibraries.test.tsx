/**
 * StepLibraries — the wizard's library-provider step (#454). Providers come
 * from the image catalog (GET /v1/admin/images), deduped by
 * `library_provider`; enabling one PATCHes library_discovery_enabled and
 * shows install progress off the same catalog signal, without ever blocking
 * finishing the wizard.
 *
 * #461 (live first-run exercise on a virgin box): a true virgin instance's
 * catalog has zero rows until a sync runs, and nothing ran one — so the step
 * must auto-sync (once) when it sees an empty provider list AND
 * `fetched_at: null`, and must NOT auto-sync an already-synced-but-genuinely-
 * empty catalog. Tests below encode both catalog shapes explicitly via
 * `fetched_at` on the envelope, matching what GET /v1/admin/images actually
 * distinguishes.
 */

import { StrictMode } from "react";
import { describe, expect, it, vi, beforeEach } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { StepLibraries } from "./StepLibraries";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { ApiError } from "../../api/client";
import type { CatalogImage } from "../../api/types";

vi.mock("../../api/admin", () => ({
  listImages: vi.fn(),
  syncImages: vi.fn(),
  updateSettings: vi.fn(),
  setProviderEntitlementMode: vi.fn(),
}));

import * as adminApi from "../../api/admin";

const SYNCED_AT = "2026-08-01T00:00:00Z";

function steamImage(overrides: Partial<CatalogImage> = {}): CatalogImage {
  return {
    id: "img-steam",
    display_name: "Steam",
    description: "Scan users' Steam installs and publish tiles automatically.",
    kind: "prebuilt",
    version: "1",
    library_provider: "steam",
    installed: false,
    hosts: [],
    ...overrides,
  } as unknown as CatalogImage;
}

/** An already-synced catalog envelope — the common case in these tests,
 *  where auto-sync must NOT fire. */
function syncedCatalog(images: CatalogImage[]) {
  return { images, fetched_at: SYNCED_AT } as never;
}

/** The true virgin-instance shape: no rows, never synced. */
function virginCatalog() {
  return { images: [], fetched_at: null } as never;
}

function renderStep(onNext = vi.fn(), { strict = false } = {}) {
  const authValue: AuthContextValue = {
    status: "authenticated",
    user: { id: "u1", email: "admin@example.com", username: "admin", role: "admin" },
    token: "tok",
    isAdmin: true,
    sessionExpired: false,
    login: vi.fn(),
    claim: vi.fn(),
    logout: vi.fn(),
  };
  const tree = (
    <AuthContext.Provider value={authValue}>
      <StepLibraries onNext={onNext} />
    </AuthContext.Provider>
  );
  render(strict ? <StrictMode>{tree}</StrictMode> : tree);
  return onNext;
}

describe("StepLibraries", () => {
  beforeEach(() => {
    vi.mocked(adminApi.listImages).mockReset();
    vi.mocked(adminApi.syncImages).mockReset();
    vi.mocked(adminApi.updateSettings).mockReset();
    vi.mocked(adminApi.setProviderEntitlementMode).mockReset();
    vi.mocked(adminApi.setProviderEntitlementMode).mockResolvedValue({
      entitlement_mode: { provider: "steam", app_id: "app-1", mode: "all", items: [] },
    });
  });

  it("shows a loading state while the catalog is in flight", () => {
    vi.mocked(adminApi.listImages).mockReturnValue(new Promise(() => {}) as never);
    renderStep();

    expect(screen.getByText(/loading library providers/i)).toBeInTheDocument();
  });

  it("lists available providers from the catalog, deduped by library_provider", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(
      syncedCatalog([steamImage(), steamImage({ id: "img-steam-2" })]),
    );
    renderStep();

    await waitFor(() => {
      expect(screen.getByText("Steam")).toBeInTheDocument();
    });
    // Deduped: only one row despite two catalog rows sharing library_provider.
    expect(screen.getAllByText("Steam")).toHaveLength(1);
    expect(screen.getByText(/scan users' steam installs/i)).toBeInTheDocument();
    // Already synced — no auto-sync should have fired.
    expect(adminApi.syncImages).not.toHaveBeenCalled();
  });

  it("genuinely empty after a sync: shows the plain empty-state copy, no auto-sync", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(syncedCatalog([]));
    const onNext = renderStep();

    await waitFor(() => {
      expect(screen.getByText(/no library providers are in the image catalog/i)).toBeInTheDocument();
    });
    expect(adminApi.syncImages).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: /skip and continue/i }));
    expect(onNext).toHaveBeenCalled();
    expect(adminApi.updateSettings).not.toHaveBeenCalled();
  });

  it("virgin instance (empty + never synced): auto-syncs, then lists the providers the sync found", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(virginCatalog());
    // Held open, so the in-flight state is a state the DOM actually passes
    // through: with an already-resolved sync, React batches the start and the
    // finish into one commit and the message never renders.
    let finishSync: (v: unknown) => void = () => {};
    vi.mocked(adminApi.syncImages).mockReturnValue(
      new Promise((res) => {
        finishSync = res;
      }) as never,
    );
    renderStep();

    // The catalog-fetch state, in the step's own idiom.
    await waitFor(() => {
      expect(screen.getByText(/fetching the image catalog/i)).toBeInTheDocument();
    });

    await act(async () => {
      finishSync(syncedCatalog([steamImage()]));
    });
    await waitFor(() => {
      expect(screen.getByText("Steam")).toBeInTheDocument();
    });
    expect(adminApi.syncImages).toHaveBeenCalledWith("tok");
    expect(adminApi.syncImages).toHaveBeenCalledTimes(1);
    expect(screen.queryByText(/fetching the image catalog/i)).not.toBeInTheDocument();
  });

  it("a sync failure on a virgin instance degrades to the empty-state copy plus an operator-language error line", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(virginCatalog());
    vi.mocked(adminApi.syncImages).mockRejectedValue(
      new ApiError(502, "bad_gateway", "could not reach the image catalog"),
    );
    const onNext = renderStep();

    await waitFor(() => {
      expect(screen.getByText(/no library providers are in the image catalog/i)).toBeInTheDocument();
    });
    expect(screen.getByText(/could not reach the image catalog/i)).toBeInTheDocument();

    // Still finishable — a sync failure is not a dead end.
    fireEvent.click(screen.getByRole("button", { name: /skip and continue/i }));
    expect(onNext).toHaveBeenCalled();
  });

  it("does not fire the auto-sync more than once per mount (StrictMode double-invoke)", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(virginCatalog());
    vi.mocked(adminApi.syncImages).mockResolvedValue(syncedCatalog([steamImage()]));
    renderStep(vi.fn(), { strict: true });

    await waitFor(() => {
      expect(screen.getByText("Steam")).toBeInTheDocument();
    });
    expect(adminApi.syncImages).toHaveBeenCalledTimes(1);
  });

  it("continuing with none selected skips — finishes without a PATCH", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(syncedCatalog([steamImage()]));
    const onNext = renderStep();

    await waitFor(() => expect(screen.getByText("Steam")).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: /skip and continue/i }));

    expect(onNext).toHaveBeenCalled();
    expect(adminApi.updateSettings).not.toHaveBeenCalled();
  });

  it("selecting a provider and continuing PATCHes library_discovery_enabled, then shows in-flight progress", async () => {
    vi.mocked(adminApi.listImages)
      .mockResolvedValueOnce(syncedCatalog([steamImage()]))
      .mockResolvedValue(
        syncedCatalog([steamImage({ installed: false, hosts: [{ host_id: "h1", state: "pulling" }] })]),
      );
    vi.mocked(adminApi.updateSettings).mockResolvedValue({} as never);
    const onNext = renderStep();

    await waitFor(() => expect(screen.getByText("Steam")).toBeInTheDocument());
    fireEvent.click(screen.getByLabelText(/^enable$/i));
    fireEvent.click(screen.getByRole("button", { name: /^continue$/i }));

    await waitFor(() => {
      expect(adminApi.updateSettings).toHaveBeenCalledWith("tok", { library_discovery_enabled: true });
    });
    await waitFor(() => {
      expect(screen.getByText(/downloading…/i)).toBeInTheDocument();
    });

    // Finishing must remain available while the install is still in flight —
    // it does not block on the background install.
    const finishBtn = screen.getByRole("button", { name: /^continue$/i });
    expect(finishBtn).toBeEnabled();
    fireEvent.click(finishBtn);
    expect(onNext).toHaveBeenCalled();
  });

  // Alice PR #464 round 2: the aggregate chip used to pick whichever row's
  // in-flight state came first in the catalog array, so a "building" row
  // ahead of a "pulling" row in `images` wrongly won the chip — pulling must
  // win regardless of row order (imageStatus.ts's documented precedence).
  it("prefers Downloading… over Building… regardless of which row comes first", async () => {
    vi.mocked(adminApi.listImages)
      .mockResolvedValueOnce(syncedCatalog([steamImage()]))
      .mockResolvedValue(
        syncedCatalog([
          steamImage({
            id: "img-steam-building",
            installed: false,
            hosts: [{ host_id: "h1", state: "building" }],
          }),
          steamImage({
            id: "img-steam-pulling",
            installed: false,
            hosts: [{ host_id: "h2", state: "pulling" }],
          }),
        ]),
      );
    vi.mocked(adminApi.updateSettings).mockResolvedValue({} as never);
    renderStep();

    await waitFor(() => expect(screen.getByText("Steam")).toBeInTheDocument());
    fireEvent.click(screen.getByLabelText(/^enable$/i));
    fireEvent.click(screen.getByRole("button", { name: /^continue$/i }));

    await waitFor(() => {
      expect(screen.getByText(/downloading…/i)).toBeInTheDocument();
    });
    expect(screen.queryByText(/building…/i)).not.toBeInTheDocument();
  });

  it("a failed install renders the failure state in operator language and does not block finishing", async () => {
    vi.mocked(adminApi.listImages)
      .mockResolvedValueOnce(syncedCatalog([steamImage()]))
      .mockResolvedValue(
        syncedCatalog([
          steamImage({ installed: false, hosts: [{ host_id: "h1", state: "failed", error: "no space left on device" }] }),
        ]),
      );
    vi.mocked(adminApi.updateSettings).mockResolvedValue({} as never);
    const onNext = renderStep();

    await waitFor(() => expect(screen.getByText("Steam")).toBeInTheDocument());
    fireEvent.click(screen.getByLabelText(/^enable$/i));
    fireEvent.click(screen.getByRole("button", { name: /^continue$/i }));

    await waitFor(() => {
      expect(screen.getByText("Failed")).toBeInTheDocument();
    });
    // Operator language, not the raw per-host error body.
    expect(screen.queryByText(/no space left on device/i)).not.toBeInTheDocument();
    expect(screen.getByText(/didn't complete automatically/i)).toBeInTheDocument();

    const finishBtn = screen.getByRole("button", { name: /^continue$/i });
    expect(finishBtn).toBeEnabled();
    fireEvent.click(finishBtn);
    expect(onNext).toHaveBeenCalled();
  });

  it("a PATCH failure surfaces an inline error but still reaches the progress view", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(syncedCatalog([steamImage()]));
    vi.mocked(adminApi.updateSettings).mockRejectedValue(new ApiError(500, "internal", "could not save settings"));
    renderStep();

    await waitFor(() => expect(screen.getByText("Steam")).toBeInTheDocument());
    fireEvent.click(screen.getByLabelText(/^enable$/i));
    fireEvent.click(screen.getByRole("button", { name: /^continue$/i }));

    await waitFor(() => {
      expect(screen.getByText(/could not save settings/i)).toBeInTheDocument();
    });
    expect(screen.getByRole("button", { name: /^continue$/i })).toBeEnabled();
  });

  // First-run-experience §S3 — "who can see it". #465 wired a real control:
  // POST /v1/admin/library-providers/{provider}/entitlement-mode, submitted
  // alongside the settings PATCH on Continue.
  it("does not show the who-can-play picker before a provider is enabled", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(syncedCatalog([steamImage()]));
    renderStep();

    await waitFor(() => expect(screen.getByText("Steam")).toBeInTheDocument());
    expect(screen.queryByText(/available to all users/i)).not.toBeInTheDocument();
    expect(screen.queryAllByRole("tab")).toHaveLength(0);
  });

  it("shows the who-can-play picker (defaulted to Everyone) once a provider is toggled on", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(syncedCatalog([steamImage()]));
    renderStep();

    await waitFor(() => expect(screen.getByText("Steam")).toBeInTheDocument());
    fireEvent.click(screen.getByLabelText(/^enable$/i));

    expect(screen.getByText(/available to all users/i)).toBeInTheDocument();
    const everyoneTab = screen.getByRole("tab", { name: "Everyone" });
    expect(everyoneTab).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tab", { name: "Only me" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Nobody yet" })).toBeInTheDocument();
  });

  // #490: "all" is sent too, so it replaces a restriction an earlier pass stored.
  it("leaving the picker on Everyone (the default) still sends mode 'all'", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(syncedCatalog([steamImage()]));
    vi.mocked(adminApi.updateSettings).mockResolvedValue({} as never);
    renderStep();

    await waitFor(() => expect(screen.getByText("Steam")).toBeInTheDocument());
    fireEvent.click(screen.getByLabelText(/^enable$/i));
    fireEvent.click(screen.getByRole("button", { name: /^continue$/i }));

    await waitFor(() => {
      expect(adminApi.setProviderEntitlementMode).toHaveBeenCalledWith("tok", "steam", "all");
    });
    expect(adminApi.updateSettings).toHaveBeenCalledWith("tok", { library_discovery_enabled: true });
    expect(screen.queryByText(/could not switch/i)).not.toBeInTheDocument();
  });

  it("picking Only me submits the settings PATCH and then the entitlement-mode call", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(syncedCatalog([steamImage()]));
    vi.mocked(adminApi.updateSettings).mockResolvedValue({} as never);
    vi.mocked(adminApi.setProviderEntitlementMode).mockResolvedValue({
      entitlement_mode: { provider: "steam", app_id: "app-1", mode: "user", items: [] },
    } as never);
    renderStep();

    await waitFor(() => expect(screen.getByText("Steam")).toBeInTheDocument());
    fireEvent.click(screen.getByLabelText(/^enable$/i));
    fireEvent.click(screen.getByRole("tab", { name: "Only me" }));
    expect(screen.getByText(/available to your account only/i)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /^continue$/i }));

    await waitFor(() => {
      expect(adminApi.setProviderEntitlementMode).toHaveBeenCalledWith("tok", "steam", "user");
    });
    // Ordering: the mode call only fires after the settings PATCH resolved.
    const settingsOrder = vi.mocked(adminApi.updateSettings).mock.invocationCallOrder[0];
    const modeOrder = vi.mocked(adminApi.setProviderEntitlementMode).mock.invocationCallOrder[0];
    expect(settingsOrder).toBeLessThan(modeOrder);
    expect(screen.queryByText(/as soon as it finishes installing/i)).not.toBeInTheDocument();
  });

  // #490: before the provider app exists the server keeps the mode (202) and
  // applies it at create — one call, no retry, and the step says so.
  it("a stored (202) mode says it applies once installed, without retrying", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(syncedCatalog([steamImage()]));
    vi.mocked(adminApi.updateSettings).mockResolvedValue({} as never);
    vi.mocked(adminApi.setProviderEntitlementMode).mockResolvedValue({
      pending_entitlement_mode: { provider: "steam", mode: "user" },
    });
    const onNext = renderStep();

    await waitFor(() => expect(screen.getByText("Steam")).toBeInTheDocument());
    fireEvent.click(screen.getByLabelText(/^enable$/i));
    fireEvent.click(screen.getByRole("tab", { name: "Only me" }));
    fireEvent.click(screen.getByRole("button", { name: /^continue$/i }));

    await waitFor(() => {
      expect(screen.getByText(/“Only me” will apply to Steam as soon as it finishes installing/)).toBeInTheDocument();
    });
    expect(adminApi.setProviderEntitlementMode).toHaveBeenCalledTimes(1);
    expect(screen.queryByText(/visible to all users for now/i)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /^continue$/i }));
    expect(onNext).toHaveBeenCalled();
  });

  it("picking Nobody yet submits mode 'none'", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(syncedCatalog([steamImage()]));
    vi.mocked(adminApi.updateSettings).mockResolvedValue({} as never);
    vi.mocked(adminApi.setProviderEntitlementMode).mockResolvedValue({
      entitlement_mode: { provider: "steam", app_id: "app-1", mode: "none", items: [] },
    } as never);
    renderStep();

    await waitFor(() => expect(screen.getByText("Steam")).toBeInTheDocument());
    fireEvent.click(screen.getByLabelText(/^enable$/i));
    fireEvent.click(screen.getByRole("tab", { name: "Nobody yet" }));

    fireEvent.click(screen.getByRole("button", { name: /^continue$/i }));

    await waitFor(() => {
      expect(adminApi.setProviderEntitlementMode).toHaveBeenCalledWith("tok", "steam", "none");
    });
  });

  // A failure is not retried and degrades to honest copy rather than blocking
  // Continue (the provider IS enabled).
  it("an entitlement-mode failure surfaces inline without blocking finish", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(syncedCatalog([steamImage()]));
    vi.mocked(adminApi.updateSettings).mockResolvedValue({} as never);
    vi.mocked(adminApi.setProviderEntitlementMode).mockRejectedValue(
      new ApiError(500, "internal", "could not set entitlement mode"),
    );
    const onNext = renderStep();

    await waitFor(() => expect(screen.getByText("Steam")).toBeInTheDocument());
    fireEvent.click(screen.getByLabelText(/^enable$/i));
    fireEvent.click(screen.getByRole("tab", { name: "Only me" }));
    fireEvent.click(screen.getByRole("button", { name: /^continue$/i }));

    await waitFor(() => {
      expect(screen.getByText(/could not switch it to only me/i)).toBeInTheDocument();
    });
    expect(adminApi.setProviderEntitlementMode).toHaveBeenCalledTimes(1);

    const finishBtn = screen.getByRole("button", { name: /^continue$/i });
    expect(finishBtn).toBeEnabled();
    fireEvent.click(finishBtn);
    expect(onNext).toHaveBeenCalled();
  });

});
