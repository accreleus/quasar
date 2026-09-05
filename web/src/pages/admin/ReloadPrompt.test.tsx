// The reload prompt (#117). The question it answers is "would a reload fetch a
// different bundle?", so it compares hashed bundle names and NOT the served
// commit: a source-built stack rebuilds the SPA and the control-plane image
// from different commits, and a commit comparison would nag forever.

import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as adminApi from "../../api/admin";
import type { PlatformIdentity } from "../../api/types";
import { ToastProvider } from "../../components/Toast";
import { ReloadPrompt } from "./ReloadPrompt";

vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../api/admin");

const mocked = vi.mocked(adminApi);

const LOADED = "index-aaaaaaaa.js";
const A = "a".repeat(40);
const B = "b".repeat(40);

function identity(commit: string | null): { identity: PlatformIdentity } {
  return {
    identity: { version: "0.3.0", source_commit: commit, built_at: null, schema_version: 75 },
  };
}

/** What the server answers for /index.html. */
function serves(bundle: string) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => ({
      ok: true,
      text: async () => `<html><script type="module" src="/assets/${bundle}"></script></html>`,
    })),
  );
}

function renderPrompt() {
  return render(
    <ToastProvider>
      <ReloadPrompt />
    </ToastProvider>,
  );
}

beforeEach(() => {
  vi.resetAllMocks();
  sessionStorage.clear();
  // The bundle this document is running.
  const script = document.createElement("script");
  script.setAttribute("src", `/assets/${LOADED}`);
  script.dataset.testBundle = "1";
  document.head.appendChild(script);
  mocked.getPlatformIdentity.mockResolvedValue(identity(A));
});

afterEach(() => {
  document.querySelectorAll("script[data-test-bundle]").forEach((el) => el.remove());
  vi.unstubAllGlobals();
});

describe("ReloadPrompt", () => {
  it("prompts when the served bundle is not the one this tab is running", async () => {
    serves("index-bbbbbbbb.js");
    renderPrompt();

    expect(await screen.findByText("Quasar was updated")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reload" })).toBeInTheDocument();
  });

  // The source-built stack: web/dist and the control-plane image were built
  // from different commits, and a reload would change nothing.
  it("stays quiet when the bundle is the same, whatever commit the server reports", async () => {
    mocked.getPlatformIdentity.mockResolvedValue(identity(B));
    serves(LOADED);
    renderPrompt();

    await waitFor(() => expect(mocked.getPlatformIdentity).toHaveBeenCalled());
    await waitFor(() => expect(fetch).toHaveBeenCalled());
    expect(screen.queryByText("Quasar was updated")).not.toBeInTheDocument();
  });

  it("dismisses, and does not come back for the same bundle", async () => {
    serves("index-bbbbbbbb.js");
    const { unmount } = renderPrompt();

    (await screen.findByRole("button", { name: "Dismiss" })).click();
    await waitFor(() => expect(screen.queryByText("Quasar was updated")).not.toBeInTheDocument());

    // A fresh mount is a fresh component: only the remembered dismissal can
    // keep it quiet.
    unmount();
    serves("index-bbbbbbbb.js");
    renderPrompt();
    await waitFor(() => expect(fetch).toHaveBeenCalled());
    expect(screen.queryByText("Quasar was updated")).not.toBeInTheDocument();
  });

  it("prompts again once the served bundle moves past the dismissed one", async () => {
    serves("index-bbbbbbbb.js");
    const { unmount } = renderPrompt();
    (await screen.findByRole("button", { name: "Dismiss" })).click();
    unmount();

    serves("index-cccccccc.js");
    renderPrompt();
    expect(await screen.findByText("Quasar was updated")).toBeInTheDocument();
  });

  it("says nothing when this document names no bundle", async () => {
    document.querySelectorAll("script[data-test-bundle]").forEach((el) => el.remove());
    serves("index-bbbbbbbb.js");
    renderPrompt();

    await waitFor(() => expect(mocked.getPlatformIdentity).toHaveBeenCalled());
    expect(fetch).not.toHaveBeenCalled();
    expect(screen.queryByText("Quasar was updated")).not.toBeInTheDocument();
  });
});
