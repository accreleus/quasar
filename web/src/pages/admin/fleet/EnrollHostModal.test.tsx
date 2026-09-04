import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");

import * as adminApi from "../../../api/admin";
import { EnrollHostModal } from "./EnrollHostModal";

const mocked = vi.mocked(adminApi);
const FP =
  "0A:1B:2C:3D:4E:5F:60:71:82:93:A4:B5:C6:D7:E8:F9:0A:1B:2C:3D:4E:5F:60:71:82:93:A4:B5:C6:D7:E8:F9";
const PIN = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789+/AbCdE=";

function accessCheck(over: { self_signed?: boolean; in_use?: boolean } = {}) {
  const in_use = over.in_use ?? true;
  return {
    request: { host: "cp.example:8443", origin: "https://cp.example:8443", secure_context: true },
    certificate: in_use
      ? {
          in_use: true,
          host_covered: true,
          info: { self_signed: over.self_signed ?? true, fingerprint_sha256: FP, spki_sha256: PIN, source: "self_signed" },
        }
      : { in_use: false, not_in_use_reason: "a proxy terminates TLS" },
    origins: { source: "database" },
  } as never;
}

beforeEach(() => {
  vi.clearAllMocks();
  mocked.accessCheck.mockResolvedValue(accessCheck());
  mocked.mintHostEnrollment.mockResolvedValue({
    enrollment: {
      id: "e1",
      token: "tok.with.dots",
      node_name: null,
      max_uses: 1,
      used_count: 0,
      expires_at: "2026-09-03T13:00:00Z",
      created_at: "2026-09-03T12:00:00Z",
    },
  } as never);
});

const REF = "v1.2.3";
const SCRIPT = "https://cp.example:8443/enroll-host.sh";

describe("EnrollHostModal", () => {
  it("refuses to compose from a plain-http page and never mints", () => {
    render(<EnrollHostModal open onClose={() => {}} origin="http://cp.example:8080" sourceRef={REF} />);
    expect(screen.getByTestId("enroll-needs-https")).toBeTruthy();
    expect(screen.queryByText("Mint enrollment string")).toBeNull();
    expect(mocked.accessCheck).not.toHaveBeenCalled();
    expect(mocked.mintHostEnrollment).not.toHaveBeenCalled();
  });

  it("shows the self-signed fingerprint, then mints and hands over a key-pinned one-liner served by this control plane", async () => {
    render(<EnrollHostModal open onClose={() => {}} origin="https://cp.example:8443" sourceRef={REF} />);
    await waitFor(() => expect(screen.getByTestId("enroll-fingerprint").textContent).toBe(FP));

    fireEvent.click(screen.getByText("Mint enrollment string"));
    await waitFor(() => expect(screen.getByTestId("enroll-command")).toBeTruthy());
    const cmd = screen.getByTestId("enroll-command").textContent ?? "";
    expect(cmd.startsWith(`curl -fsSL -k --pinnedpubkey 'sha256//${PIN}' ${SCRIPT} | QUASAR_ENROLLMENT='qenr1.${FP}.`)).toBe(true);
    expect(cmd.endsWith(`.tok.with.dots' QUASAR_REF=${REF} sh`)).toBe(true);
    expect(cmd).not.toMatch(/githubusercontent/);
    expect(screen.getByText(/--pinnedpubkey/)).toBeTruthy();
    expect(mocked.mintHostEnrollment).toHaveBeenCalledWith("tok", {});
    // The fingerprint stays on screen next to the command for the eye check.
    expect(screen.getByTestId("enroll-fingerprint").textContent).toBe(FP);
  });

  it("keeps the bare enrollment string one click away, behind the command", async () => {
    render(<EnrollHostModal open onClose={() => {}} origin="https://cp.example:8443" sourceRef={REF} />);
    await waitFor(() => expect(screen.getByText("Mint enrollment string")).toBeTruthy());
    fireEvent.click(screen.getByText("Mint enrollment string"));
    await waitFor(() => expect(screen.getByTestId("enroll-command")).toBeTruthy());

    const summary = screen.getByText("Show the enrollment string");
    expect(summary.closest("details")?.hasAttribute("open")).toBe(false);
    fireEvent.click(summary);
    const value = screen.getByTestId("enroll-string").textContent ?? "";
    expect(value.startsWith(`qenr1.${FP}.`)).toBe(true);
    expect(value.endsWith(".tok.with.dots")).toBe(true);
  });

  it("says nothing about a fingerprint for a real-CA certificate, and pins nothing in the string", async () => {
    mocked.accessCheck.mockResolvedValue(accessCheck({ self_signed: false }));
    render(<EnrollHostModal open onClose={() => {}} origin="https://play.example.com" sourceRef={REF} />);
    await waitFor(() => expect(screen.getByText("Mint enrollment string")).toBeTruthy());
    expect(screen.queryByTestId("enroll-fingerprint")).toBeNull();
    fireEvent.click(screen.getByText("Mint enrollment string"));
    await waitFor(() => expect(screen.getByTestId("enroll-command")).toBeTruthy());
    expect(screen.queryByTestId("enroll-fingerprint")).toBeNull();
    const cmd = screen.getByTestId("enroll-command").textContent ?? "";
    expect(cmd).toContain("QUASAR_ENROLLMENT='qenr1..");
    expect(cmd.startsWith("curl -fsSL https://play.example.com/enroll-host.sh |")).toBe(true);
    expect(cmd).not.toMatch(/ -k|pinnedpubkey/);
  });

  it("still composes the command when the build carries no source ref, just without QUASAR_REF", async () => {
    render(<EnrollHostModal open onClose={() => {}} origin="https://cp.example:8443" sourceRef="" />);
    await waitFor(() => expect(screen.getByText("Mint enrollment string")).toBeTruthy());
    fireEvent.click(screen.getByText("Mint enrollment string"));
    await waitFor(() => expect(screen.getByTestId("enroll-command")).toBeTruthy());
    const cmd = screen.getByTestId("enroll-command").textContent ?? "";
    expect(cmd).not.toContain("QUASAR_REF");
    expect(cmd.endsWith("' sh")).toBe(true);
    expect(screen.queryByTestId("enroll-no-installer")).toBeNull();
  });

  it("no longer carries the runtime-contract prose the installer replaced", async () => {
    render(<EnrollHostModal open onClose={() => {}} origin="https://cp.example:8443" sourceRef={REF} />);
    await waitFor(() => expect(screen.getByText("Mint enrollment string")).toBeTruthy());
    fireEvent.click(screen.getByText("Mint enrollment string"));
    await waitFor(() => expect(screen.getByTestId("enroll-command")).toBeTruthy());
    const text = screen.getByRole("dialog").textContent ?? "";
    expect(text).not.toMatch(/operator work|host networking|no supported agent-only package/);
    expect(screen.getByRole("link", { name: "Add a second GPU host" }).getAttribute("href")).toBe(
      "https://accreleus.github.io/quasar/install/second-host/",
    );
  });

  it("surfaces a mint failure instead of a half string", async () => {
    mocked.mintHostEnrollment.mockRejectedValue(new Error("boom"));
    render(<EnrollHostModal open onClose={() => {}} origin="https://cp.example:8443" sourceRef={REF} />);
    await waitFor(() => expect(screen.getByText("Mint enrollment string")).toBeTruthy());
    fireEvent.click(screen.getByText("Mint enrollment string"));
    await waitFor(() => expect(screen.getByRole("alert")).toBeTruthy());
    expect(screen.queryByTestId("enroll-command")).toBeNull();
    expect(screen.queryByTestId("enroll-string")).toBeNull();
  });

  it("forgets the previous host's token on close and re-checks access on reopen", async () => {
    const { rerender } = render(
      <EnrollHostModal open onClose={() => {}} origin="https://cp.example:8443" sourceRef={REF} />,
    );
    await waitFor(() => expect(screen.getByTestId("enroll-fingerprint").textContent).toBe(FP));
    fireEvent.click(screen.getByText("Mint enrollment string"));
    await waitFor(() => expect(screen.getByTestId("enroll-command")).toBeTruthy());
    expect(mocked.accessCheck).toHaveBeenCalledTimes(1);

    rerender(<EnrollHostModal open={false} onClose={() => {}} origin="https://cp.example:8443" sourceRef={REF} />);
    rerender(<EnrollHostModal open onClose={() => {}} origin="https://cp.example:8443" sourceRef={REF} />);

    expect(screen.queryByTestId("enroll-command")).toBeNull();
    await waitFor(() => expect(screen.getByText("Mint enrollment string")).toBeTruthy());
    await waitFor(() => expect(mocked.accessCheck).toHaveBeenCalledTimes(2));
  });
});
