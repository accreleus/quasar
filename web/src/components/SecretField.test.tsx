// SecretField — the write-only credential control.
//
// The properties under test are the ones that make the facility honest rather
// than merely present: the value is never rendered, the two key-management
// failures read differently, and an operator can always tell WHICH source is in
// effect when both a stored key and an env var exist.
//
// The component is FULLY CONTROLLED: it is handed a `SecretStatus` and the
// master-key flag, and reads nothing from the server on its own. So these tests
// drive it with props, and mock only the two mutations it does call
// (`setSecret` / `clearSecret`). `listSecrets` is mocked solely so that the
// no-fetch property can be asserted rather than assumed.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { useState } from "react";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { SecretField } from "./SecretField";
import { ToastProvider } from "./Toast";
import * as adminApi from "../api/admin";
import { ApiError } from "../api/client";
import type { SecretStatus } from "../api/types";

vi.mock("../api/admin");
const mocked = vi.mocked(adminApi);

const NAME = "artwork.steamgriddb.api_key";

function secret(over: Partial<SecretStatus> = {}): SecretStatus {
  return {
    name: NAME,
    label: "SteamGridDB API key",
    description: "Looks up cover artwork for apps in your catalogue.",
    env_var: "QUASAR_STEAMGRIDDB_API_KEY",
    docs_url: "https://example.test/keys",
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

function renderField(
  opts: { secret?: SecretStatus; masterKeyConfigured?: boolean; onChange?: () => void } = {},
) {
  return render(
    <ToastProvider>
      <SecretField
        secret={opts.secret ?? secret()}
        masterKeyConfigured={opts.masterKeyConfigured ?? true}
        token="tok"
        onChange={opts.onChange ?? (() => {})}
      />
    </ToastProvider>,
  );
}

beforeEach(() => {
  vi.resetAllMocks();
});

describe("SecretField — the value never appears", () => {
  it("renders a password input that starts empty even when a key is stored", () => {
    renderField({
      secret: secret({ configured: true, readable: true, hint: "9f2c", origin: "database" }),
    });

    // Synchronous `getBy`, deliberately: the control is fully rendered on the
    // first commit because it fetches nothing. A `findBy` here would pass even
    // if a loading pass crept back in.
    const input = screen.getByLabelText("SteamGridDB API key") as HTMLInputElement;
    // Write-only: the server never sends the value, so there is nothing to
    // pre-fill and the field must not pretend otherwise.
    expect(input.value).toBe("");
    expect(input.type).toBe("password");
    // Only the masked tail is shown.
    expect(screen.getByText(/····9f2c/)).toBeTruthy();
    // And it re-reads nothing: the caller already holds the envelope this row
    // was rendered from, so a per-row GET would be a duplicate request.
    expect(mocked.listSecrets).not.toHaveBeenCalled();
  });

  it("clears the input after a successful save so the value does not linger in the DOM", async () => {
    mocked.setSecret.mockResolvedValue({
      secret: secret({ configured: true, readable: true, hint: "abcd", origin: "database" }),
      master_key_configured: true,
    } as never);
    renderField();

    const input = screen.getByLabelText("SteamGridDB API key") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "a-brand-new-credential" } });
    fireEvent.click(screen.getByRole("button", { name: "Save key" }));

    await waitFor(() =>
      expect(mocked.setSecret).toHaveBeenCalledWith("tok", NAME, "a-brand-new-credential"),
    );
    // The same node can be asserted on: there is no reload that unmounts it.
    await waitFor(() => expect(input.value).toBe(""));
  });
});

describe("SecretField — which source is in effect", () => {
  it("names the environment variable when that is what is live", () => {
    renderField({ secret: secret({ origin: "environment", env_set: true }) });
    expect(screen.getByText(/from this server's environment/i)).toBeTruthy();
  });

  it("says the stored key wins when an env var is ALSO set, and how to fall back", () => {
    renderField({
      secret: secret({
        configured: true,
        readable: true,
        hint: "9f2c",
        origin: "database",
        env_set: true,
      }),
    });
    // Neither direction may be silent: the operator is told which one won and
    // that the other is still sitting there.
    expect(screen.getByText(/takes precedence/i)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Clear key" })).toBeTruthy();
  });

  it("offers no clear button when nothing is stored", () => {
    renderField();
    expect(screen.getByRole("button", { name: "Save key" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Clear key" })).toBeNull();
  });
});

describe("SecretField — the two key-management failures read differently", () => {
  it("explains a MISSING master key and keeps the field disabled", () => {
    renderField({ masterKeyConfigured: false });

    expect(screen.getByText(/No master key is configured/i)).toBeTruthy();
    // Offering an enabled field whose save always 409s would be the footgun.
    const input = screen.getByLabelText("SteamGridDB API key") as HTMLInputElement;
    expect(input.disabled).toBe(true);
    // And the operator is told the env var still works meanwhile.
    expect(screen.getByText(/still works/i)).toBeTruthy();
  });

  it("explains a WRONG master key as a decryption problem, not as 'not configured'", () => {
    renderField({
      secret: secret({
        configured: true,
        readable: false,
        hint: "9f2c",
        origin: "none",
        problem: "The configured master key does not match the stored value.",
      }),
    });

    expect(screen.getByText(/master key does not match/i)).toBeTruthy();
    expect(screen.getByText("Cannot be decrypted")).toBeTruthy();
    // Critically: this must NOT read as a missing master key.
    expect(screen.queryByText(/No master key is configured/i)).toBeNull();
  });
});

describe("SecretField — errors", () => {
  it("surfaces the server's own 409 text rather than a generic message", async () => {
    mocked.setSecret.mockRejectedValue(
      new ApiError(409, "conflict", "the master key does not match the stored secret"),
    );
    renderField();

    const input = screen.getByLabelText("SteamGridDB API key");
    fireEvent.change(input, { target: { value: "some-value" } });
    fireEvent.click(screen.getByRole("button", { name: "Save key" }));

    // Shown inline AND as a toast — both are the server's own words, not a
    // generic "that did not work".
    expect((await screen.findAllByText(/master key does not match the stored secret/i)).length)
      .toBeGreaterThan(0);
  });

  it("tells the caller to re-read its own state after a change (the no-restart property)", async () => {
    const onChange = vi.fn();
    mocked.setSecret.mockResolvedValue({
      secret: secret({ configured: true, readable: true, origin: "database" }),
      master_key_configured: true,
    } as never);
    renderField({ onChange });

    const input = screen.getByLabelText("SteamGridDB API key");
    fireEvent.change(input, { target: { value: "some-value" } });
    fireEvent.click(screen.getByRole("button", { name: "Save key" }));

    await waitFor(() => expect(onChange).toHaveBeenCalled());
  });
});

describe("SecretField — the refreshed state comes from the caller", () => {
  // `onChange` is the ONLY refresh path now, so it has to actually work end to
  // end: the parent re-reads, hands down a new SecretStatus, and the row
  // reflects it. A component that cached its own copy would still show "Not
  // configured" here, which is exactly the stale read this guards against.
  function Harness({ next }: { next: SecretStatus }) {
    const [s, setS] = useState<SecretStatus>(secret());
    return (
      <ToastProvider>
        <SecretField secret={s} masterKeyConfigured token="tok" onChange={() => setS(next)} />
      </ToastProvider>
    );
  }

  it("shows the parent's newly-loaded status after a save", async () => {
    mocked.setSecret.mockResolvedValue({
      secret: secret({ configured: true, readable: true, hint: "abcd", origin: "database" }),
      master_key_configured: true,
    } as never);
    render(
      <Harness
        next={secret({ configured: true, readable: true, hint: "abcd", origin: "database" })}
      />,
    );

    expect(screen.getByText("Not configured")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("SteamGridDB API key"), {
      target: { value: "some-value" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save key" }));

    await screen.findByText(/set here in the admin UI/i);
    expect(screen.getByText(/····abcd/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Replace key" })).toBeTruthy();
  });
});
