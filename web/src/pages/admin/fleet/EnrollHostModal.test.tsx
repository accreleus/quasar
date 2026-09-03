import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");

import * as adminApi from "../../../api/admin";
import { EnrollHostModal } from "./EnrollHostModal";

const mocked = vi.mocked(adminApi);
const FP =
  "0A:1B:2C:3D:4E:5F:60:71:82:93:A4:B5:C6:D7:E8:F9:0A:1B:2C:3D:4E:5F:60:71:82:93:A4:B5:C6:D7:E8:F9";

function accessCheck(over: { self_signed?: boolean; in_use?: boolean } = {}) {
  const in_use = over.in_use ?? true;
  return {
    request: { host: "cp.example:8443", origin: "https://cp.example:8443", secure_context: true },
    certificate: in_use
      ? { in_use: true, host_covered: true, info: { self_signed: over.self_signed ?? true, fingerprint_sha256: FP, source: "self_signed" } }
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

describe("EnrollHostModal", () => {
  it("refuses to compose from a plain-http page and never mints", () => {
    render(<EnrollHostModal open onClose={() => {}} origin="http://cp.example:8080" />);
    expect(screen.getByTestId("enroll-needs-https")).toBeTruthy();
    expect(screen.queryByText("Mint enrollment string")).toBeNull();
    expect(mocked.accessCheck).not.toHaveBeenCalled();
    expect(mocked.mintHostEnrollment).not.toHaveBeenCalled();
  });

  it("shows the served self-signed fingerprint, then mints and composes a pinned string", async () => {
    render(<EnrollHostModal open onClose={() => {}} origin="https://cp.example:8443" />);
    await waitFor(() => expect(screen.getByTestId("enroll-fingerprint").textContent).toBe(FP));
    expect(screen.getByText("wss://cp.example:8443")).toBeTruthy();

    fireEvent.click(screen.getByText("Mint enrollment string"));
    await waitFor(() => expect(screen.getByTestId("enroll-string")).toBeTruthy());
    const value = screen.getByTestId("enroll-string").textContent ?? "";
    expect(value.startsWith(`qenr1.${FP}.`)).toBe(true);
    expect(value.endsWith(".tok.with.dots")).toBe(true);
    expect(mocked.mintHostEnrollment).toHaveBeenCalledWith("tok", {});
  });

  it("emits no pin for a real-CA certificate", async () => {
    mocked.accessCheck.mockResolvedValue(accessCheck({ self_signed: false }));
    render(<EnrollHostModal open onClose={() => {}} origin="https://play.example.com" />);
    await waitFor(() => expect(screen.getByText(/public CA/)).toBeTruthy());
    fireEvent.click(screen.getByText("Mint enrollment string"));
    await waitFor(() => expect(screen.getByTestId("enroll-string")).toBeTruthy());
    expect((screen.getByTestId("enroll-string").textContent ?? "").startsWith("qenr1..")).toBe(true);
  });

  it("surfaces a mint failure instead of a half string", async () => {
    mocked.mintHostEnrollment.mockRejectedValue(new Error("boom"));
    render(<EnrollHostModal open onClose={() => {}} origin="https://cp.example:8443" />);
    await waitFor(() => expect(screen.getByText("Mint enrollment string")).toBeTruthy());
    fireEvent.click(screen.getByText("Mint enrollment string"));
    await waitFor(() => expect(screen.getByRole("alert")).toBeTruthy());
    expect(screen.queryByTestId("enroll-string")).toBeNull();
  });

  it("forgets the previous host's token on close and re-checks access on reopen", async () => {
    const { rerender } = render(
      <EnrollHostModal open onClose={() => {}} origin="https://cp.example:8443" />,
    );
    await waitFor(() => expect(screen.getByTestId("enroll-fingerprint").textContent).toBe(FP));
    fireEvent.click(screen.getByText("Mint enrollment string"));
    await waitFor(() => expect(screen.getByTestId("enroll-string")).toBeTruthy());
    expect(mocked.accessCheck).toHaveBeenCalledTimes(1);

    rerender(<EnrollHostModal open={false} onClose={() => {}} origin="https://cp.example:8443" />);
    rerender(<EnrollHostModal open onClose={() => {}} origin="https://cp.example:8443" />);

    expect(screen.queryByTestId("enroll-string")).toBeNull();
    await waitFor(() => expect(screen.getByText("Mint enrollment string")).toBeTruthy());
    await waitFor(() => expect(mocked.accessCheck).toHaveBeenCalledTimes(2));
  });
});
