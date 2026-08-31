/**
 * SetupWizard — regression test for the "dead-ends after a successful claim"
 * blocker (adversarial review round). A real claim flips the shared
 * AuthContext to authenticated (exactly like AuthProvider.claim does — see
 * FakeAuthProvider below, which mirrors that contract without touching real
 * network/localStorage plumbing). The wizard must advance to step 2
 * immediately, with no page reload, and the claim button must not stay
 * stuck disabled.
 *
 * StepClaim.test.tsx cannot catch this class of bug: it mocks `onClaimed`
 * directly, so it never exercises SetupWizard's own step-routing decision.
 * This test renders the real SetupWizard and drives a real claim through it.
 */

import { useState, type ReactNode } from "react";
import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { SetupWizard } from "./SetupWizard";
import { AuthContext, type AuthContextValue, type AuthStatus } from "../../auth/context";
import { SetupStatusProvider } from "../../setup/useSetupStatus";
// The wizard renders inside AuthCard, which holds a dark lock, so it needs a
// ThemeProvider above it exactly as the real app supplies one.
import { ThemeProvider } from "../../settings/ThemeContext";
import { RequireClaimedInstance } from "../../setup/RequireClaimedInstance";

vi.mock("../../api/setup", () => ({
  getSetupStatus: vi.fn(),
  completeSetup: vi.fn(),
}));
vi.mock("../../api/admin", () => ({
  getSettings: vi.fn(),
  updateSettings: vi.fn(),
  listHosts: vi.fn(),
  getHostGPUs: vi.fn(),
  listImages: vi.fn(),
  syncImages: vi.fn(),
  listSecrets: vi.fn(),
  forceLibraryScan: vi.fn(),
}));

import * as setupApi from "../../api/setup";
import * as adminApi from "../../api/admin";

/**
 * Mirrors AuthProvider.claim's real contract: on a successful claim it sets
 * status to "authenticated" and isAdmin true BEFORE the awaited promise
 * resolves back to the caller (StepClaim). A bare `AuthContext.Provider`
 * with a static value (as StepClaim.test.tsx uses) can't express that state
 * transition, which is exactly the gap that let this blocker ship.
 */
function FakeAuthProvider({
  children,
  claimImpl,
}: {
  children: ReactNode;
  claimImpl: (setupToken: string, email: string, username: string, password: string) => Promise<void>;
}) {
  const [status, setStatus] = useState<AuthStatus>("unauthenticated");
  const [isAdmin, setIsAdmin] = useState(false);

  const claim: AuthContextValue["claim"] = async (setupToken, email, username, password) => {
    await claimImpl(setupToken, email, username, password);
    setStatus("authenticated");
    setIsAdmin(true);
  };

  const value: AuthContextValue = {
    status,
    user: isAdmin ? { id: "u1", email: "admin@example.com", username: "admin", role: "admin" } : null,
    token: isAdmin ? "test-token" : null,
    isAdmin,
    login: vi.fn(),
    claim,
    logout: vi.fn(),
  };

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

function fillAndSubmitClaim() {
  fireEvent.change(screen.getByLabelText(/setup token/i), { target: { value: "the-token" } });
  fireEvent.change(screen.getByLabelText(/email/i), { target: { value: "admin@example.com" } });
  fireEvent.change(screen.getByLabelText(/username/i), { target: { value: "admin" } });
  fireEvent.change(screen.getByLabelText(/^password$/i), { target: { value: "a-strong-password-123" } });
  fireEvent.click(screen.getByRole("button", { name: /create admin account/i }));
}

describe("SetupWizard — claim success advances to step 2", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.mocked(setupApi.getSetupStatus).mockResolvedValue({ admin_exists: false, setup_completed: false });
    vi.mocked(setupApi.completeSetup).mockResolvedValue({ admin_exists: true, setup_completed: true });
    vi.mocked(adminApi.getSettings).mockResolvedValue({
      settings: {
        registration_mode: "closed",
        storage_provider: "auto",
        mic_capture_enabled: false,
        library_discovery_interval_minutes: 360,
        library_discovery_appdetails_enabled: false,
        library_discovery_enabled: false,
        updated_by: null,
        updated_at: null,
      },
    } as never);
  });

  it("shows the claim form for a virgin instance, then advances to instance basics on success — no reload required", async () => {
    const claimImpl = vi.fn().mockResolvedValue(undefined);

    render(
      <ThemeProvider>
        <MemoryRouter initialEntries={["/setup"]}>
          <FakeAuthProvider claimImpl={claimImpl}>
            <SetupStatusProvider>
              <SetupWizard />
            </SetupStatusProvider>
          </FakeAuthProvider>
        </MemoryRouter>
      </ThemeProvider>,
    );

    await waitFor(() => {
      expect(screen.getByRole("button", { name: /create admin account/i })).toBeInTheDocument();
    });

    fillAndSubmitClaim();

    // The claim form must be gone and step 2 must be visible, with no
    // manual reload. This is the assertion the blocker breaks: without the
    // fix, StepClaim re-renders in place, still populated, submit button
    // permanently stuck on "Creating admin account…".
    await waitFor(() => {
      expect(screen.queryByLabelText(/setup token/i)).not.toBeInTheDocument();
    });
    expect(screen.getByRole("heading", { name: /instance basics/i })).toBeInTheDocument();
    expect(claimImpl).toHaveBeenCalledWith("the-token", "admin@example.com", "admin", "a-strong-password-123");
  });

  /**
   * Review finding 2: after a successful claim, the SHARED SetupStatus cache
   * still said admin_exists:false (the status fetch is not re-run). SetupWizard
   * itself worked around that locally via authStatus, but RequireClaimedInstance
   * reads the same stale global value — so following a StepHosts link to
   * /admin/fleet/hosts before completing setup bounced the just-created admin
   * straight back to /setup. The claim-success path must update the shared
   * cache so leaving /setup mid-wizard works.
   */
  it("after a claim, navigating to an admin route is NOT redirected back to /setup (shared status cache updated)", async () => {
    vi.mocked(setupApi.getSetupStatus).mockResolvedValue({ admin_exists: false, setup_completed: false });
    vi.mocked(adminApi.updateSettings).mockResolvedValue({} as never);
    vi.mocked(adminApi.listHosts).mockResolvedValue({ items: [] } as never);

    render(
      <ThemeProvider>
        <MemoryRouter initialEntries={["/setup"]}>
          <FakeAuthProvider claimImpl={vi.fn().mockResolvedValue(undefined)}>
            <SetupStatusProvider>
              <RequireClaimedInstance>
                <Routes>
                  <Route path="/setup" element={<SetupWizard />} />
                  <Route path="/admin/fleet/hosts" element={<div>hosts admin page</div>} />
                </Routes>
              </RequireClaimedInstance>
            </SetupStatusProvider>
          </FakeAuthProvider>
        </MemoryRouter>
      </ThemeProvider>,
    );

    // Step 1 — claim.
    await waitFor(() => {
      expect(screen.getByRole("button", { name: /create admin account/i })).toBeInTheDocument();
    });
    fillAndSubmitClaim();

    // Step 2 — instance basics; continue through it.
    await waitFor(() => {
      expect(screen.getByRole("heading", { name: /instance basics/i })).toBeInTheDocument();
    });
    await waitFor(() => expect(screen.getByRole("button", { name: /continue/i })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));

    // Step 3 — no hosts registered yet; the warning links to /admin/fleet/hosts.
    await waitFor(() => {
      expect(screen.getByRole("heading", { name: /host & gpu check/i })).toBeInTheDocument();
    });
    const [link] = await screen.findAllByRole("link", { name: /hosts/i });
    fireEvent.click(link);

    // The admin route must render — NOT bounce back to the wizard.
    await waitFor(() => {
      expect(screen.getByText("hosts admin page")).toBeInTheDocument();
    });
    expect(screen.queryByRole("heading", { name: /host & gpu check/i })).not.toBeInTheDocument();
  });
});

/**
 * Step 5 (StepFinishing) shell wiring — Alice PR #480 review item 5. The
 * unit tests in StepLibraries.test.tsx / StepFinishing.test.tsx only ever
 * invoke their injected `onNext`/`onFinish` spies directly, so they cannot
 * catch a broken `goTo(5)` call in SetupWizard.tsx, a missing `step === 5`
 * render branch, or a failure to restore a persisted
 * `quasar.setup.step = "5"` on mount — the last of which would strand a
 * half-finished first-run operator on step 2 after a refresh. These render
 * the REAL SetupWizard (not an injected callback) to close that gap.
 */
describe("SetupWizard — step 5 (finishing touches) wiring", () => {
  function authenticatedAdmin(): AuthContextValue {
    return {
      status: "authenticated",
      user: { id: "u1", email: "admin@example.com", username: "admin", role: "admin" },
      token: "test-token",
      isAdmin: true,
      login: vi.fn(),
      claim: vi.fn(),
      logout: vi.fn(),
    };
  }

  beforeEach(() => {
    localStorage.clear();
    vi.mocked(setupApi.getSetupStatus).mockResolvedValue({ admin_exists: true, setup_completed: false });
    vi.mocked(adminApi.listSecrets).mockResolvedValue({
      secrets: [],
      master_key_configured: true,
      key_versions: [1],
    } as never);
    vi.mocked(adminApi.getSettings).mockResolvedValue({
      settings: {
        registration_mode: "closed",
        storage_provider: "auto",
        mic_capture_enabled: false,
        library_discovery_interval_minutes: 360,
        library_discovery_appdetails_enabled: false,
        library_discovery_enabled: false,
        updated_by: null,
        updated_at: null,
      },
    } as never);
  });

  it("advances from step 4 (libraries) to step 5 (finishing touches) via the wizard's own goTo(5) wiring", async () => {
    // A valid persisted step-4 marker — this is StepHosts -> StepLibraries
    // wiring, already covered elsewhere; starting here isolates the thing
    // actually under test: does Libraries' "skip" callback land on step 5.
    localStorage.setItem("quasar.setup.step", "4");
    vi.mocked(adminApi.listImages).mockResolvedValue({ images: [], fetched_at: "2026-08-01T00:00:00Z" } as never);

    render(
      <ThemeProvider>
        <MemoryRouter initialEntries={["/setup"]}>
          <AuthContext.Provider value={authenticatedAdmin()}>
            <SetupStatusProvider>
              <SetupWizard />
            </SetupStatusProvider>
          </AuthContext.Provider>
        </MemoryRouter>
      </ThemeProvider>,
    );

    await waitFor(() => {
      expect(screen.getByRole("heading", { name: /libraries/i })).toBeInTheDocument();
    });
    fireEvent.click(await screen.findByRole("button", { name: /skip and continue/i }));

    await waitFor(() => {
      expect(screen.getByRole("heading", { name: /finishing touches/i })).toBeInTheDocument();
    });
    // The step indicator must have followed — this is what a missed
    // `visibleStep = 5` render branch would get wrong even if the heading
    // happened to still be right.
    expect(screen.getByText(/5\. finishing touches/i)).toBeInTheDocument();
  });

  it("restores directly to step 5 from a persisted quasar.setup.step, with no step 2-4 UI ever appearing", async () => {
    localStorage.setItem("quasar.setup.step", "5");

    render(
      <ThemeProvider>
        <MemoryRouter initialEntries={["/setup"]}>
          <AuthContext.Provider value={authenticatedAdmin()}>
            <SetupStatusProvider>
              <SetupWizard />
            </SetupStatusProvider>
          </AuthContext.Provider>
        </MemoryRouter>
      </ThemeProvider>,
    );

    await waitFor(() => {
      expect(screen.getByRole("heading", { name: /finishing touches/i })).toBeInTheDocument();
    });
    expect(screen.queryByRole("heading", { name: /instance basics/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /host & gpu check/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /^libraries$/i })).not.toBeInTheDocument();
  });
});
