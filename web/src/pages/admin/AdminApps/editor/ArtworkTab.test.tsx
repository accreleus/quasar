import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ArtworkTab } from "./ArtworkTab";
import { ToastProvider } from "../../../../components/Toast";
import * as adminApi from "../../../../api/admin";
import { ApiError } from "../../../../api/client";
import type { AppArtworkEnvelope } from "../../../../api/types";

vi.mock("../../../../api/admin");
vi.mock("../../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));

const mocked = vi.mocked(adminApi);

function envelope(over: Partial<AppArtworkEnvelope> = {}): AppArtworkEnvelope {
  return {
    artwork: null,
    provider_configured: false,
    provider_name: "",
    provider_origin: "none",
    ...over,
  } as AppArtworkEnvelope;
}

// The tab renders a <Link to="/admin/settings"> (the credential lives on the
// Settings page, not here) — needs a Router in scope, hence MemoryRouter.
function renderPanel(kind = "game") {
  return render(
    <MemoryRouter>
      <ToastProvider>
        <ArtworkTab appId="app-1" appName="Portal 2" token="tok" kind={kind} />
      </ToastProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.resetAllMocks();
});

describe("Artwork tab — provider not configured (the shipped default)", () => {
  it("explains why nothing is fetched instead of offering a dead search button", async () => {
    mocked.getAppArtwork.mockResolvedValue(envelope());
    renderPanel();

    await screen.findByText(/No artwork provider is configured/i);
    // The provider-backed controls must be ABSENT, not present-and-broken.
    expect(screen.queryByRole("button", { name: "Search" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Match automatically" })).toBeNull();
  });

  it("still offers upload — the local path needs no third party", async () => {
    mocked.getAppArtwork.mockResolvedValue(envelope());
    renderPanel();

    expect(await screen.findByRole("button", { name: "Upload tile image" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Upload hero image" })).toBeTruthy();
  });

  it("shows the gradient-tile state for an app with no artwork", async () => {
    mocked.getAppArtwork.mockResolvedValue(envelope());
    renderPanel();

    await screen.findByText(/No artwork, showing the gradient tile/i);
    // No <img> at all: an unmatched app must look deliberate, never broken.
    expect(document.querySelectorAll("img.cover-img")).toHaveLength(0);
  });
});

describe("Artwork tab — the provider credential is read-only here", () => {
  it("shows a 'not configured' indicator and a link to Settings, with no key-entry field", async () => {
    mocked.getAppArtwork.mockResolvedValue(envelope());
    renderPanel();

    await screen.findByText(/No artwork provider key is configured/i);
    // No editable credential control of any kind — that facility moved to
    // /admin/settings, and this panel must not offer a second place to set it.
    expect(screen.queryByLabelText(/SteamGridDB API key/i)).toBeNull();
    expect(screen.queryByRole("button", { name: /save key/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /replace key/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /clear key/i })).toBeNull();
    // At least one link points at the Settings page (the note above the
    // indicator links there too, so more than one is expected and fine).
    const settingsLinks = screen.getAllByRole("link", { name: /settings/i });
    expect(settingsLinks.length).toBeGreaterThan(0);
    for (const link of settingsLinks) {
      expect(link.getAttribute("href")).toBe("/admin/settings");
    }
  });

  it("shows a 'configured' indicator and its origin when a provider key is in effect", async () => {
    mocked.getAppArtwork.mockResolvedValue(
      envelope({ provider_configured: true, provider_origin: "environment" }),
    );
    renderPanel();

    await screen.findByText(/An artwork provider key is configured/i);
    await screen.findByText(/from this server's environment/i);
  });
});

describe("Artwork tab — a desktop app", () => {
  it("says why it is never looked up automatically", async () => {
    mocked.getAppArtwork.mockResolvedValue(
      envelope({ provider_configured: true, provider_name: "steamgriddb" }),
    );
    renderPanel("desktop");

    await screen.findByText(/never looked up automatically/i);
    expect(screen.getByText("Desktop")).toBeTruthy();
  });
});

// steam-library-discovery spec §4.5.4: the server short-circuits a launcher
// app's artwork EXACTLY as it does a desktop app's (both are apps a games
// database will never have — "Steam (Dev)" matching "Steam Dev Days" is the
// headline evidence). The client hint must say so too, and must name the
// right kind rather than always saying "Desktop".
describe("Artwork tab — a launcher app (spec §4.5.4)", () => {
  it("says why it is never looked up automatically, same as a desktop app", async () => {
    mocked.getAppArtwork.mockResolvedValue(
      envelope({ provider_configured: true, provider_name: "steamgriddb" }),
    );
    renderPanel("launcher");

    await screen.findByText(/never looked up automatically/i);
  });

  it("names the app a Launcher, not a Desktop", async () => {
    mocked.getAppArtwork.mockResolvedValue(
      envelope({ provider_configured: true, provider_name: "steamgriddb" }),
    );
    renderPanel("launcher");

    await screen.findByText(/never looked up automatically/i);
    expect(screen.getByText("Launcher")).toBeTruthy();
    expect(screen.queryByText("Desktop")).toBeNull();
  });
});

describe("Artwork tab — a game app shows no artworkless hint", () => {
  it("never renders the never-looked-up-automatically hint for kind=game", async () => {
    mocked.getAppArtwork.mockResolvedValue(
      envelope({ provider_configured: true, provider_name: "steamgriddb" }),
    );
    renderPanel("game");

    await screen.findByRole("button", { name: "Search" });
    expect(screen.queryByText(/never looked up automatically/i)).toBeNull();
  });
});

describe("Artwork tab — showing an existing match", () => {
  it("renders both crops and names the matched title so a wrong match is visible", async () => {
    mocked.getAppArtwork.mockResolvedValue(
      envelope({
        provider_configured: true,
        provider_name: "steamgriddb",
        artwork: {
          app_id: "app-1",
          source: "provider",
          provider: "steamgriddb",
          provider_ref: "1234",
          matched_name: "Portal Knights",
          tile_url: "/v1/artwork/aaa.jpg",
          hero_url: "/v1/artwork/bbb.jpg",
          attribution: "Artwork via SteamGridDB",
          locked: false,
          updated_at: "2026-07-28T00:00:00Z",
        },
      }),
    );
    renderPanel();

    await screen.findByText(/Matched automatically/i);
    expect(screen.getByText(/Portal Knights/)).toBeTruthy();
    expect(screen.getByText("Artwork via SteamGridDB")).toBeTruthy();

    const imgs = Array.from(document.querySelectorAll("img.cover-img")) as HTMLImageElement[];
    expect(imgs).toHaveLength(2);
    // Both crops are served locally — never hotlinked from the provider.
    for (const img of imgs) {
      expect(img.getAttribute("src")).toMatch(/^\/v1\/artwork\//);
    }
    // And they are DIFFERENT assets, not one image reused.
    expect(imgs[0].getAttribute("src")).not.toBe(imgs[1].getAttribute("src"));
  });

  it("marks an admin override as locked", async () => {
    mocked.getAppArtwork.mockResolvedValue(
      envelope({
        provider_configured: true,
        artwork: {
          app_id: "app-1",
          source: "manual",
          provider: "",
          provider_ref: "",
          matched_name: "",
          tile_url: "/v1/artwork/aaa.jpg",
          hero_url: null,
          attribution: "",
          locked: true,
          updated_at: "2026-07-28T00:00:00Z",
        },
      }),
    );
    renderPanel();

    await screen.findByText(/Set by an admin/i);
    expect(screen.getByText(/Locked/i)).toBeTruthy();
  });
});

describe("Artwork tab — the override", () => {
  it("searches, lists candidates, and applies the one the operator picks", async () => {
    mocked.getAppArtwork.mockResolvedValue(
      envelope({ provider_configured: true, provider_name: "steamgriddb" }),
    );
    mocked.searchAppArtwork.mockResolvedValue({
      candidates: [
        { ref: "1", name: "Portal Knights", thumb_url: "https://cdn.example.test/a.jpg" },
        { ref: "2", name: "Portal 2", thumb_url: "" },
      ],
      provider_configured: true,
      provider_name: "steamgriddb",
      provider_origin: "database",
    });
    mocked.setAppArtwork.mockResolvedValue(
      envelope({
        provider_configured: true,
        artwork: {
          app_id: "app-1",
          source: "manual",
          provider: "steamgriddb",
          provider_ref: "2",
          matched_name: "Portal 2",
          tile_url: "/v1/artwork/ccc.jpg",
          hero_url: null,
          attribution: "",
          locked: true,
          updated_at: "2026-07-28T00:00:00Z",
        },
      }),
    );
    renderPanel();

    fireEvent.click(await screen.findByRole("button", { name: "Search" }));
    const pick = await screen.findByRole("button", { name: /Portal 2/ });
    fireEvent.click(pick);

    await waitFor(() =>
      expect(mocked.setAppArtwork).toHaveBeenCalledWith("tok", "app-1", { provider_ref: "2" }),
    );
    await screen.findByText(/Locked/i);
  });

  it("sends only the crop the operator filled in, leaving the other untouched", async () => {
    mocked.getAppArtwork.mockResolvedValue(envelope());
    mocked.setAppArtwork.mockResolvedValue(envelope());
    renderPanel();

    const heroField = (await screen.findByLabelText("Hero image URL")) as HTMLInputElement;
    fireEvent.change(heroField, { target: { value: " https://example.test/hero.jpg " } });
    fireEvent.click(screen.getByRole("button", { name: "Fetch from URL" }));

    await waitFor(() =>
      expect(mocked.setAppArtwork).toHaveBeenCalledWith("tok", "app-1", {
        hero_url: "https://example.test/hero.jpg",
      }),
    );
  });

  it("keeps Fetch from URL disabled until a URL is entered", async () => {
    mocked.getAppArtwork.mockResolvedValue(envelope());
    renderPanel();

    const btn = (await screen.findByRole("button", { name: "Fetch from URL" })) as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Tile image URL"), {
      target: { value: "https://example.test/a.png" },
    });
    expect(btn.disabled).toBe(false);
  });

  it("surfaces a server rejection rather than silently doing nothing", async () => {
    mocked.getAppArtwork.mockResolvedValue(envelope());
    mocked.setAppArtwork.mockRejectedValue(
      new ApiError(400, "validation_failed", "that URL resolves to a non-public address and will not be fetched"),
    );
    renderPanel();

    fireEvent.change(await screen.findByLabelText("Tile image URL"), {
      target: { value: "http://10.0.0.5/a.png" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Fetch from URL" }));

    // Surfaced twice on purpose: inline next to the field AND as a toast.
    expect((await screen.findAllByText(/non-public address/i)).length).toBeGreaterThan(0);
  });
});

describe("Artwork tab — upload and clear", () => {
  it("uploads the chosen file for the chosen crop", async () => {
    mocked.getAppArtwork.mockResolvedValue(envelope());
    mocked.uploadAppArtwork.mockResolvedValue(envelope());
    renderPanel();

    const input = (await screen.findByLabelText("Upload hero image")) as HTMLInputElement;
    const file = new File([new Uint8Array([1, 2, 3])], "hero.png", { type: "image/png" });
    fireEvent.change(input, { target: { files: [file] } });

    await waitFor(() =>
      expect(mocked.uploadAppArtwork).toHaveBeenCalledWith("tok", "app-1", "hero", file),
    );
  });

  it("offers Reset to gradient only once the app has artwork", async () => {
    mocked.getAppArtwork.mockResolvedValue(envelope());
    const { unmount } = renderPanel();
    await screen.findByRole("button", { name: "Upload tile image" });
    expect(screen.queryByRole("button", { name: "Reset to gradient" })).toBeNull();
    unmount();

    mocked.getAppArtwork.mockResolvedValue(
      envelope({
        artwork: {
          app_id: "app-1",
          source: "manual",
          provider: "",
          provider_ref: "",
          matched_name: "",
          tile_url: "/v1/artwork/aaa.jpg",
          hero_url: null,
          attribution: "",
          locked: true,
          updated_at: "2026-07-28T00:00:00Z",
        },
      }),
    );
    mocked.clearAppArtwork.mockResolvedValue(undefined);
    renderPanel();

    fireEvent.click(await screen.findByRole("button", { name: "Reset to gradient" }));
    await waitFor(() => expect(mocked.clearAppArtwork).toHaveBeenCalledWith("tok", "app-1"));
  });
});
