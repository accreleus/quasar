// The reload prompt (#117): it must fire when the served build moved under an
// open tab, and never on a build whose SOURCE_REF is a tag — a tag can never
// equal a commit, so comparing one would nag every admin on every load.

import { act, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as adminApi from "../../api/admin";
import type { PlatformIdentity } from "../../api/types";
import { ToastProvider } from "../../components/Toast";
import { ReloadPrompt } from "./ReloadPrompt";

vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../api/admin");

const build = vi.hoisted(() => ({ ref: "" }));
vi.mock("../../lib/buildInfo", () => ({
  get SOURCE_REF() {
    return build.ref;
  },
}));

const mocked = vi.mocked(adminApi);

const A = "a".repeat(40);
const B = "b".repeat(40);

function identity(commit: string | null): { identity: PlatformIdentity } {
  return {
    identity: { version: "0.3.0", source_commit: commit, built_at: null, schema_version: 75 },
  };
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
  build.ref = A;
});
afterEach(() => vi.useRealTimers());

describe("ReloadPrompt", () => {
  it("prompts when the served commit is not the one this bundle was built from", async () => {
    mocked.getPlatformIdentity.mockResolvedValue(identity(B));
    renderPrompt();

    expect(await screen.findByText("Quasar was updated")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reload" })).toBeInTheDocument();
  });

  it("stays quiet when the served commit is this bundle's", async () => {
    mocked.getPlatformIdentity.mockResolvedValue(identity(A));
    renderPrompt();

    await waitFor(() => expect(mocked.getPlatformIdentity).toHaveBeenCalled());
    expect(screen.queryByText("Quasar was updated")).not.toBeInTheDocument();
  });

  it("takes the first served commit as the baseline when SOURCE_REF is a tag", async () => {
    build.ref = "v0.3.0";
    vi.useFakeTimers();
    mocked.getPlatformIdentity
      .mockResolvedValueOnce(identity(A))
      .mockResolvedValue(identity(B));
    renderPrompt();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.queryByText("Quasar was updated")).not.toBeInTheDocument();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000);
    });
    expect(screen.getByText("Quasar was updated")).toBeInTheDocument();
  });
});
