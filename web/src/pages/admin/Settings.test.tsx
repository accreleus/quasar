// Settings — handoff-v3-spec.md §A.21: one card per concern, one row per
// setting. Each row's PATCH assertion checks that only the one changed key is
// sent (control-api.md pointer-decode rule) — a row must never risk clobbering
// a sibling's value. Switches now share the generic "Enabled"/"Disabled"
// caption (review), so they are located by their `id`, not by label text.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { Settings } from "./Settings";
import { ToastProvider } from "../../components/Toast";
import { ThemeProvider } from "../../settings/ThemeContext";
import * as adminApi from "../../api/admin";
import { ApiError } from "../../api/client";
import type {
  InstanceSettings,
  LibraryStatus,
  SecretStatus,
  SecretsResponse,
  SettingsResponse,
} from "../../api/types";

vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../api/admin");

const mocked = vi.mocked(adminApi);

function settings(over: Partial<InstanceSettings> = {}): InstanceSettings {
  return {
    registration_mode: "closed",
    storage_provider: "local",
    mic_capture_enabled: false,
    library_discovery_enabled: false,
    library_discovery_interval_minutes: 360,
    library_discovery_appdetails_enabled: false,
    image_update_policy: "notify",
    allowed_origins: ["https://play.example.com"],
    updated_by: null,
    updated_at: "2026-07-29T00:00:00Z",
    ...over,
  } as InstanceSettings;
}

function settingsResponse(over: Partial<InstanceSettings> = {}): SettingsResponse {
  return { settings: settings(over) };
}

function libraryStatus(over: Partial<LibraryStatus> = {}): LibraryStatus {
  return {
    interval_overridden_by_env: false,
    appdetails_overridden_by_env: false,
    last_scan_completed_at: null,
    recent_scans: [],
    enabled: false,
    storage_provider: "local",
    scan_interval_secs: 21600,
    appdetails_lookup: false,
    inert_reason: "",
    scans: { pending: 0, claimed: 0, failed: 0 },
    ...over,
  } as LibraryStatus;
}

// A generic non-artwork secret — the artwork key (Task 25) moved to Library >
// Sources, so Settings' Secrets card must never render it; tests that need
// that specific descriptor use artworkSecret() below instead.
function secret(over: Partial<SecretStatus> = {}): SecretStatus {
  return {
    name: "backup.encryption_key",
    label: "Backup encryption key",
    description: "Encrypts scheduled backup archives.",
    env_var: "QUASAR_BACKUP_ENCRYPTION_KEY",
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

function artworkSecret(over: Partial<SecretStatus> = {}): SecretStatus {
  return secret({
    name: "artwork.steamgriddb.api_key",
    label: "SteamGridDB API key",
    description: "Looks up cover artwork for apps in your catalogue.",
    env_var: "QUASAR_STEAMGRIDDB_API_KEY",
    ...over,
  });
}

function secretsResponse(secrets: SecretStatus[], over: Partial<SecretsResponse> = {}): SecretsResponse {
  return { secrets, master_key_configured: true, key_versions: [1], ...over };
}

function renderPage() {
  return render(
    <ToastProvider>
      <ThemeProvider>
        <Settings />
      </ThemeProvider>
    </ToastProvider>,
  );
}

beforeEach(() => {
  vi.resetAllMocks();
  localStorage.clear();
  mocked.getSettings.mockResolvedValue(settingsResponse());
  mocked.getLibraryStatus.mockResolvedValue(libraryStatus());
  mocked.listSecrets.mockResolvedValue(secretsResponse([secret()]));
});

describe("Settings — Instance card", () => {
  it("renders Registration reflecting the current mode", async () => {
    renderPage();
    const btn = await screen.findByRole("tab", { name: "Invite only" });
    expect(btn).toHaveAttribute("aria-selected", "false");
    expect(screen.getByRole("tab", { name: "Closed" })).toHaveAttribute("aria-selected", "true");
  });

  it("PATCHes only registration_mode when changed", async () => {
    mocked.updateSettings.mockResolvedValue(settingsResponse({ registration_mode: "open" }));
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Open" }));

    await waitFor(() =>
      expect(mocked.updateSettings).toHaveBeenCalledWith("tok", { registration_mode: "open" }),
    );
  });

  it("Allowed origins Save sends the textarea as an array", async () => {
    mocked.updateSettings.mockResolvedValue(settingsResponse());
    renderPage();

    const textarea = (await screen.findByLabelText("Allowed origins")) as HTMLTextAreaElement;
    expect(textarea.value).toBe("https://play.example.com");
    fireEvent.change(textarea, { target: { value: "https://a.example\nhttps://b.example" } });
    fireEvent.click(screen.getByRole("button", { name: "Save allowed origins" }));

    await waitFor(() =>
      expect(mocked.updateSettings).toHaveBeenCalledWith("tok", {
        allowed_origins: ["https://a.example", "https://b.example"],
      }),
    );
  });

  it("shows the server's 400 inline on the Allowed origins row", async () => {
    mocked.updateSettings.mockRejectedValue(
      new ApiError(400, "validation_failed", "entry 1 is not a valid origin"),
    );
    renderPage();

    const textarea = await screen.findByLabelText("Allowed origins");
    fireEvent.change(textarea, { target: { value: "not-a-url" } });
    fireEvent.click(screen.getByRole("button", { name: "Save allowed origins" }));

    await screen.findByText("entry 1 is not a valid origin");
  });

  it("shows an error line, never a permanent spinner, when GET /v1/admin/settings fails", async () => {
    mocked.getSettings.mockRejectedValue(new ApiError(500, "internal", "could not reach the database"));
    renderPage();

    // Instance, Voice and Images all key off the one settings resource, so
    // its error line renders once per card — assert presence, not count.
    await waitFor(() =>
      expect(screen.getAllByText("could not reach the database").length).toBeGreaterThan(0),
    );
    expect(screen.queryByText(/loading/i)).toBeNull();
    expect(screen.queryByRole("tab", { name: "Closed" })).toBeNull();
  });
});

describe("Settings — Region defaults card", () => {
  it("shows the saved default timezone, grouped by area", async () => {
    mocked.getSettings.mockResolvedValue(settingsResponse({ default_timezone: "Europe/Berlin" }));
    renderPage();
    const select = (await screen.findByLabelText("Default timezone")) as HTMLSelectElement;
    expect(select.value).toBe("Europe/Berlin");
    expect(select.querySelector('optgroup[label="Europe"]')).toBeTruthy();
  });

  it("PATCHes only default_timezone when changed", async () => {
    mocked.updateSettings.mockResolvedValue(settingsResponse({ default_timezone: "Asia/Tokyo" }));
    renderPage();
    const select = await screen.findByLabelText("Default timezone");
    fireEvent.change(select, { target: { value: "Asia/Tokyo" } });
    await waitFor(() => expect(mocked.updateSettings).toHaveBeenCalledWith("tok", { default_timezone: "Asia/Tokyo" }));
  });

  it("keeps a saved zone the list does not carry, and defaults to UTC for an older server", async () => {
    mocked.getSettings.mockResolvedValue(settingsResponse({ default_timezone: "Asia/Calcutta" }));
    const { unmount } = renderPage();
    expect(((await screen.findByLabelText("Default timezone")) as HTMLSelectElement).value).toBe("Asia/Calcutta");
    unmount();
    mocked.getSettings.mockResolvedValue(settingsResponse());
    renderPage();
    expect(((await screen.findByLabelText("Default timezone")) as HTMLSelectElement).value).toBe("UTC");
  });
});

describe("Settings — Region and Input defaults", () => {
  const cursorTab = (name: string) => within(screen.getByRole("tablist", { name: "Match cursor to my window" })).getByRole("tab", { name });
  const sent = (body: object) => expect(mocked.updateSettings).toHaveBeenCalledWith("tok", body);

  it("shows the saved defaults", async () => {
    mocked.getSettings.mockResolvedValue(
      settingsResponse({
        default_language: "de-DE",
        default_region_format: "en-GB",
        default_keyboard: { layout: "fr", variant: "bepo" },
        default_match_cursor: "on",
        default_mouse_sensitivity: 2,
        default_raw_mouse: true,
      }),
    );
    renderPage();
    expect(((await screen.findByLabelText("Default language")) as HTMLSelectElement).value).toBe("de-DE");
    expect((screen.getByLabelText("Default region format") as HTMLSelectElement).value).toBe("en-GB");
    expect((screen.getByLabelText("Default keyboard layout") as HTMLSelectElement).value).toBe("fr");
    expect((screen.getByLabelText("Default keyboard variant") as HTMLSelectElement).value).toBe("bepo");
    expect(cursorTab("Always on").getAttribute("aria-selected")).toBe("true");
    expect(screen.getByLabelText("Default mouse sensitivity").getAttribute("aria-valuetext")).toBe("2×");
    expect((document.getElementById("default-raw-mouse") as HTMLInputElement).checked).toBe(true);
  });

  it("reads an older server's missing fields as the built-in defaults", async () => {
    renderPage();
    expect(((await screen.findByLabelText("Default language")) as HTMLSelectElement).value).toBe("en-US");
    expect((screen.getByLabelText("Default region format") as HTMLSelectElement).value).toBe("");
    expect(screen.getByRole("option", { name: "Same as language (English (US))" })).toBeTruthy();
    expect((screen.getByLabelText("Default keyboard layout") as HTMLSelectElement).value).toBe("us");
    expect(cursorTab("Auto").getAttribute("aria-selected")).toBe("true");
    expect(screen.getByLabelText("Default mouse sensitivity").getAttribute("aria-valuetext")).toBe("1×");
  });

  it("PATCHes only the changed key for each control", async () => {
    mocked.updateSettings.mockResolvedValue(settingsResponse());
    renderPage();
    fireEvent.change(await screen.findByLabelText("Default language"), { target: { value: "ja-JP" } });
    await waitFor(() => sent({ default_language: "ja-JP" }));
    fireEvent.change(screen.getByLabelText("Default region format"), { target: { value: "de-DE" } });
    await waitFor(() => sent({ default_region_format: "de-DE" }));
    fireEvent.change(screen.getByLabelText("Default region format"), { target: { value: "" } });
    await waitFor(() => sent({ default_region_format: null }));
    fireEvent.click(cursorTab("Off"));
    await waitFor(() => sent({ default_match_cursor: "off" }));
    fireEvent.click(document.getElementById("default-raw-mouse")!);
    await waitFor(() => sent({ default_raw_mouse: true }));
    fireEvent.click(document.getElementById("default-flip-scroll")!);
    await waitFor(() => sent({ default_flip_scroll: true }));
    fireEvent.click(document.getElementById("default-nintendo-layout")!);
    await waitFor(() => sent({ default_nintendo_layout: true }));
  });

  it("sends the whole keyboard, and the layout's basic variant when the layout changes", async () => {
    mocked.getSettings.mockResolvedValue(settingsResponse({ default_keyboard: { layout: "de", variant: "neo" } }));
    mocked.updateSettings.mockResolvedValue(settingsResponse({ default_keyboard: { layout: "de", variant: "neo" } }));
    renderPage();
    fireEvent.change(await screen.findByLabelText("Default keyboard variant"), { target: { value: "dvorak" } });
    await waitFor(() => sent({ default_keyboard: { layout: "de", variant: "dvorak" } }));
    fireEvent.change(screen.getByLabelText("Default keyboard layout"), { target: { value: "fr" } });
    await waitFor(() => sent({ default_keyboard: { layout: "fr", variant: "" } }));
  });

  it("saves the sensitivity once the slider rests, on the log scale", async () => {
    mocked.updateSettings.mockResolvedValue(settingsResponse());
    renderPage();
    const slider = await screen.findByLabelText("Default mouse sensitivity");
    fireEvent.change(slider, { target: { value: "1" } });
    fireEvent.change(slider, { target: { value: "-1" } });
    expect(slider.getAttribute("aria-valuetext")).toBe("0.5×");
    expect(mocked.updateSettings).not.toHaveBeenCalled();
    await waitFor(() => sent({ default_mouse_sensitivity: 0.5 }));
    expect(mocked.updateSettings).toHaveBeenCalledTimes(1);
  });

  it.each([
    ["Default language", "default_language", "ja-JP", "en-US"],
    ["Default timezone", "default_timezone", "Asia/Tokyo", "UTC"],
  ])("shows the server's refusal under the %s row, not as a toast, and keeps the saved value", async (label, key, pick, saved) => {
    mocked.updateSettings.mockRejectedValue(new ApiError(400, "validation_failed", `${key} is not an accepted value`));
    renderPage();
    const select = (await screen.findByLabelText(label)) as HTMLSelectElement;
    fireEvent.change(select, { target: { value: pick } });
    const row = select.closest(".setting-row") as HTMLElement;
    expect((await within(row).findByRole("alert")).textContent).toBe(`${key} is not an accepted value`);
    expect(select.value).toBe(saved);
  });

  it("sends one PATCH at a time, so a late response cannot revert a row saved after it", async () => {
    const answers: ((r: SettingsResponse) => void)[] = [];
    mocked.updateSettings.mockImplementation(() => new Promise((resolve) => answers.push(resolve)));
    renderPage();
    const raw = (await screen.findByLabelText("Raw mouse input by default")) as HTMLInputElement;
    const flip = screen.getByLabelText("Flip scroll direction by default") as HTMLInputElement;
    fireEvent.click(raw);
    fireEvent.click(flip);
    await waitFor(() => expect(mocked.updateSettings).toHaveBeenCalledTimes(1));
    await new Promise((r) => setTimeout(r, 20));
    expect(mocked.updateSettings).toHaveBeenCalledTimes(1);
    expect(raw.disabled).toBe(true);
    expect(flip.disabled).toBe(true);
    answers[0](settingsResponse({ default_raw_mouse: true }));
    await waitFor(() => expect(mocked.updateSettings).toHaveBeenCalledTimes(2));
    expect(mocked.updateSettings).toHaveBeenLastCalledWith("tok", { default_flip_scroll: true });
    answers[1](settingsResponse({ default_raw_mouse: true, default_flip_scroll: true }));
    await waitFor(() => expect(flip.checked).toBe(true));
    expect(raw.checked).toBe(true);
    expect(raw.disabled).toBe(false);
  });

  it("keeps one row's error when another row saves", async () => {
    mocked.updateSettings.mockImplementation(async (_t, body) => {
      if ("default_language" in body) throw new ApiError(400, "validation_failed", "default_language is not an accepted value");
      return settingsResponse({ default_flip_scroll: true });
    });
    renderPage();
    const select = await screen.findByLabelText("Default language");
    fireEvent.change(select, { target: { value: "ja-JP" } });
    await screen.findByRole("alert");
    fireEvent.click(screen.getByLabelText("Flip scroll direction by default"));
    await waitFor(() => expect(mocked.updateSettings).toHaveBeenCalledTimes(2));
    await waitFor(() => expect((screen.getByLabelText("Flip scroll direction by default") as HTMLInputElement).checked).toBe(true));
    expect(within(select.closest(".setting-row") as HTMLElement).getByRole("alert")).toBeTruthy();
  });
});

describe("Settings — Libraries card", () => {
  it("renders the discovery hint and PATCHes library_discovery_enabled when the switch flips", async () => {
    mocked.updateSettings.mockResolvedValue(settingsResponse({ library_discovery_enabled: true }));
    const { container } = renderPage();

    await screen.findByText("Enabling discovery installs the Steam image on your hosts.");
    const toggle = container.querySelector("#library-discovery-enabled") as HTMLInputElement;
    expect(toggle).not.toBeNull();
    fireEvent.click(toggle);

    await waitFor(() =>
      expect(mocked.updateSettings).toHaveBeenCalledWith("tok", { library_discovery_enabled: true }),
    );
  });

  it("disables the scan interval with an explanatory hint when env-overridden", async () => {
    mocked.getLibraryStatus.mockResolvedValue(libraryStatus({ interval_overridden_by_env: true }));
    renderPage();

    const input = (await screen.findByLabelText("Scan interval")) as HTMLInputElement;
    expect(input).toBeDisabled();
    expect(screen.getByText("Set by QUASAR_LIBRARY_SCAN_INTERVAL on the server")).toBeInTheDocument();
  });

  it("saves the scan interval on Save, right-aligned under the input", async () => {
    mocked.updateSettings.mockResolvedValue(
      settingsResponse({ library_discovery_interval_minutes: 90 }),
    );
    renderPage();

    const input = await screen.findByLabelText("Scan interval");
    fireEvent.change(input, { target: { value: "90" } });
    fireEvent.click(screen.getByRole("button", { name: "Save scan interval" }));

    await waitFor(() =>
      expect(mocked.updateSettings).toHaveBeenCalledWith("tok", {
        library_discovery_interval_minutes: 90,
      }),
    );
  });

  it("disables the app-details switch when env-overridden", async () => {
    mocked.getLibraryStatus.mockResolvedValue(libraryStatus({ appdetails_overridden_by_env: true }));
    const { container } = renderPage();

    await screen.findByText("Set by QUASAR_STEAM_APPDETAILS_LOOKUP on the server");
    const toggle = container.querySelector("#appdetails-lookup-enabled") as HTMLInputElement;
    expect(toggle).toBeDisabled();
  });

  it("shows the inert_reason note when present", async () => {
    mocked.getLibraryStatus.mockResolvedValue(
      libraryStatus({ inert_reason: "No app is marked as a library provider." }),
    );
    renderPage();

    await screen.findByText("No app is marked as a library provider.");
  });

  it("shows an error line, never a permanent spinner, when GET /v1/admin/library/status fails", async () => {
    mocked.getLibraryStatus.mockRejectedValue(new ApiError(500, "internal", "could not read library status"));
    renderPage();

    await screen.findByText("could not read library status");
    expect(screen.queryByLabelText("Scan interval")).toBeNull();
  });
});

describe("Settings — Voice card", () => {
  it("renders the secure-context caveat and PATCHes mic_capture_enabled when the switch flips", async () => {
    mocked.updateSettings.mockResolvedValue(settingsResponse({ mic_capture_enabled: true }));
    const { container } = renderPage();

    await screen.findByText(/Needs a secure context \(HTTPS, or localhost\)/);
    const toggle = container.querySelector("#mic-capture-enabled") as HTMLInputElement;
    fireEvent.click(toggle);

    await waitFor(() =>
      expect(mocked.updateSettings).toHaveBeenCalledWith("tok", { mic_capture_enabled: true }),
    );
  });
});

describe("Settings — Images card", () => {
  it("describes all three modes in the hint and PATCHes image_update_policy from the segmented control", async () => {
    mocked.updateSettings.mockResolvedValue(settingsResponse({ image_update_policy: "auto" }));
    renderPage();

    await screen.findByText(/left alone \(manual\)/);
    const list = await screen.findByRole("tablist", { name: "Update policy" });
    fireEvent.click(within(list).getByRole("tab", { name: "Auto" }));

    await waitFor(() =>
      expect(mocked.updateSettings).toHaveBeenCalledWith("tok", { image_update_policy: "auto" }),
    );
  });

  it("selects nothing when the server sent no image_update_policy", async () => {
    mocked.getSettings.mockResolvedValue(settingsResponse({ image_update_policy: undefined }));
    renderPage();

    const list = await screen.findByRole("tablist", { name: "Update policy" });
    const tabs = within(list).getAllByRole("tab");
    expect(tabs.every((t) => t.getAttribute("aria-selected") === "false")).toBe(true);
  });
});

describe("Settings — Secrets card", () => {
  it("renders one SecretField row per declared secret", async () => {
    mocked.listSecrets.mockResolvedValue(
      secretsResponse([secret(), secret({ name: "b", label: "Another secret" })]),
    );
    renderPage();

    await screen.findByLabelText("Backup encryption key");
    expect(screen.getByLabelText("Another secret")).toBeInTheDocument();
  });

  it("excludes the artwork key — it moved to Library > Sources (Task 25)", async () => {
    mocked.listSecrets.mockResolvedValue(secretsResponse([secret(), artworkSecret()]));
    renderPage();

    await screen.findByLabelText("Backup encryption key");
    expect(screen.queryByLabelText("SteamGridDB API key")).toBeNull();
  });

  it("keeps the master-key note but skips the 'no credentials' copy when the artwork key was the only descriptor", async () => {
    mocked.listSecrets.mockResolvedValue(
      secretsResponse([artworkSecret()], { master_key_configured: true, key_versions: [1] }),
    );
    renderPage();

    await screen.findByText("Master key configured. Decryptable key version 1.");
    expect(screen.queryByText("This deployment does not declare any credentials yet.")).toBeNull();
    expect(screen.queryByLabelText("SteamGridDB API key")).toBeNull();
  });

  it("shows the resolved key versions as a mono hint under the head", async () => {
    mocked.listSecrets.mockResolvedValue(
      secretsResponse([secret()], { master_key_configured: true, key_versions: [1, 2] }),
    );
    renderPage();

    await screen.findByText("Master key configured. Decryptable key versions 1, 2.");
  });

  it("shows the master-key warning, including that 'Not configured' may just be waiting on it", async () => {
    mocked.listSecrets.mockResolvedValue(secretsResponse([secret()], { master_key_configured: false }));
    renderPage();

    // The card-level note and SecretField's own per-row note both say the
    // headline sentence — deliberately (SecretField.tsx) — so match on "any".
    await waitFor(() =>
      expect(
        screen.getAllByText(/No master key is configured on this control plane/i).length,
      ).toBeGreaterThan(0),
    );
    expect(screen.getByText(/may simply be waiting on this, not actually unset by choice/i)).toBeInTheDocument();
  });

  it("shows an error line, never a permanent spinner, when GET /v1/admin/secrets fails", async () => {
    mocked.listSecrets.mockRejectedValue(new ApiError(500, "internal", "could not read secrets"));
    renderPage();

    await screen.findByText("could not read secrets");
    expect(screen.queryByLabelText("Backup encryption key")).toBeNull();
  });
});

describe("Settings — Appearance card", () => {
  it("calls setTheme when Light is picked", async () => {
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Light" }));

    await waitFor(() => expect(localStorage.getItem("quasar-theme")).toBe("light"));
  });

  it("calls setDensity when Dense is picked", async () => {
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Dense" }));

    await waitFor(() => expect(localStorage.getItem("quasar-density")).toBe("dense"));
  });
});
