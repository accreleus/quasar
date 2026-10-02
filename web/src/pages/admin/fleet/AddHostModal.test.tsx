import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");

import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import { AddHostModal } from "./AddHostModal";

const mocked = vi.mocked(adminApi);
const FP =
  "0A:1B:2C:3D:4E:5F:60:71:82:93:A4:B5:C6:D7:E8:F9:0A:1B:2C:3D:4E:5F:60:71:82:93:A4:B5:C6:D7:E8:F9";
const PIN = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789+/AbCdE=";
const SEED = "registry.example.invalid/quasar/quasar-recovery@sha256:" + "4c1d".padEnd(64, "0");
const AGENT = "registry.example.invalid/quasar/quasar-node-agent@sha256:" + "bb22".padEnd(64, "0");
const SERVED = `#!/bin/sh\nset -eu\nPINNED_SEED_IMAGE='${SEED}'\nPINNED_AGENT_IMAGE='${AGENT}'\n`;
const UNPINNED = "#!/bin/sh\nset -eu\nPINNED_SEED_IMAGE=''\nPINNED_AGENT_IMAGE=''\n";
const ORIGIN = "https://cp.example:8443";

function accessCheck(over: { self_signed?: boolean; in_use?: boolean } = {}) {
  const in_use = over.in_use ?? true;
  return {
    request: { host: "cp.example:8443", origin: ORIGIN, secure_context: true },
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

const served = vi.fn(async () => SERVED);

beforeEach(() => {
  vi.clearAllMocks();
  served.mockImplementation(async () => SERVED);
  mocked.accessCheck.mockResolvedValue(accessCheck());
  mocked.mintHostEnrollment.mockImplementation(async (_t, req) => ({
    enrollment: {
      id: "e1",
      token: "tok.with.dots",
      node_name: req?.node_name ?? null,
      max_uses: 1,
      used_count: 0,
      expires_at: req?.expires_at ?? null,
      created_at: "2026-09-25T12:00:00Z",
    },
  }) as never);
});

function renderModal(props: Partial<Parameters<typeof AddHostModal>[0]> = {}) {
  return render(<AddHostModal open onClose={() => {}} origin={ORIGIN} fetchScript={served} {...props} />);
}

const create = async (label = "Create command") => {
  const button = await screen.findByRole("button", { name: label });
  await waitFor(() => expect((button as HTMLButtonElement).disabled).toBe(false));
  fireEvent.click(button);
};

describe("AddHostModal", () => {
  it("refuses a plain-http page and never mints or reads anything", () => {
    renderModal({ origin: "http://cp.example:8080" });
    expect(screen.getByTestId("enroll-needs-https").textContent).toMatch(/Open this page over HTTPS to add a host/);
    expect(screen.queryByRole("button", { name: "Create command" })).toBeNull();
    expect(mocked.accessCheck).not.toHaveBeenCalled();
    expect(served).not.toHaveBeenCalled();
    expect(mocked.mintHostEnrollment).not.toHaveBeenCalled();
  });

  it("opens on the one-line command with the pinned certificate, and cannot create before it is read", async () => {
    let resolve: (v: never) => void = () => {};
    mocked.accessCheck.mockReturnValue(new Promise((r) => (resolve = r)));
    renderModal();
    expect(screen.getByRole("tab", { name: "One-line command" }).getAttribute("aria-selected")).toBe("true");
    expect(screen.getByText(/reading this control plane/)).toBeTruthy();
    expect((screen.getByRole("button", { name: "Create command" }) as HTMLButtonElement).disabled).toBe(true);
    resolve(accessCheck());
    await waitFor(() => expect(screen.getByTestId("enroll-fingerprint").textContent).toBe("SHA256 0A:1B:…:E8:F9"));
  });

  it("mints one single-use token bound to the node name and hands over the key-pinned one-liner", async () => {
    renderModal();
    fireEvent.change(screen.getByLabelText(/Node name/), { target: { value: "gpu-host-6" } });
    await create();
    await waitFor(() => expect(screen.getByTestId("enroll-command")).toBeTruthy());

    const req = mocked.mintHostEnrollment.mock.calls[0][1]!;
    expect(req.max_uses).toBe(1);
    expect(req.node_name).toBe("gpu-host-6");
    const ms = Date.parse(req.expires_at!) - Date.now();
    expect(ms).toBeGreaterThan(3_500_000);
    expect(ms).toBeLessThanOrEqual(3_600_000);

    const cmd = screen.getByTestId("enroll-command").textContent!;
    expect(cmd).toBe(
      `curl -fsSL -k --pinnedpubkey 'sha256//${PIN}' ${ORIGIN}/enroll-host.sh | QUASAR_ENROLLMENT='qenr1.${FP}.d3NzOi8vY3AuZXhhbXBsZTo4NDQz.tok.with.dots' QUASAR_NODE_NAME=gpu-host-6 sh`,
    );
    expect(screen.getByText(/only for gpu-host-6/)).toBeTruthy();
    expect(screen.getByText(/writes no compose file/)).toBeTruthy();
    expect((screen.getByLabelText(/Node name/) as HTMLInputElement).disabled).toBe(true);
    expect(screen.getByRole("link", { name: "Add a second GPU host" }).getAttribute("href")).toBe(
      "https://accreleus.github.io/quasar/install/second-host/",
    );
    expect(screen.getByText("Show the enrollment string")).toBeTruthy();
  });

  it("sends the chosen expiry and leaves an unnamed token unbound", async () => {
    renderModal();
    fireEvent.change(screen.getByLabelText("Expires"), { target: { value: "3" } });
    await create();
    await waitFor(() => expect(mocked.mintHostEnrollment).toHaveBeenCalled());
    const req = mocked.mintHostEnrollment.mock.calls[0][1]!;
    expect(req).not.toHaveProperty("node_name");
    expect(Date.parse(req.expires_at!) - Date.now()).toBeGreaterThan(7 * 24 * 3_600_000 - 60_000);
    await waitFor(() => expect(screen.getByText(/any node name/)).toBeTruthy());
    expect(screen.getByTestId("enroll-command").textContent).not.toContain("QUASAR_NODE_NAME");
  });

  it("gives the Dockge / Arcane stack from the same token, pinned to the served seed", async () => {
    renderModal();
    await create();
    await waitFor(() => expect(screen.getByTestId("enroll-command")).toBeTruthy());
    fireEvent.click(screen.getByRole("tab", { name: "Dockge or Arcane" }));
    expect(screen.getByText("Using Dockge or Arcane? Paste this stack instead.")).toBeTruthy();
    const stack = screen.getByTestId("addhost-stack").textContent!;
    expect(stack).toContain(`image: "${SEED}"`);
    expect(stack).toContain(`QUASAR_AGENT_IMAGE: "${AGENT}"`);
    expect(stack).toContain(`QUASAR_ENROLLMENT: "qenr1.${FP}.`);
    expect(stack).toContain("name: quasar-machine");
    expect(screen.getByText(/Preparing the host is then your job/)).toBeTruthy();
    expect(mocked.mintHostEnrollment).toHaveBeenCalledTimes(1);
  });

  it("can start on the stack tab", async () => {
    renderModal();
    fireEvent.click(screen.getByRole("tab", { name: "Dockge or Arcane" }));
    await create("Create stack");
    await waitFor(() => expect(screen.getByTestId("addhost-stack").textContent).toContain(`image: "${SEED}"`));
  });

  it("refuses a node name whose agent is connected now, before a token is spent", async () => {
    renderModal({ connectedNodeNames: ["gpu-host-2"] });
    fireEvent.change(screen.getByLabelText(/Node name/), { target: { value: "gpu-host-2" } });
    await create();
    expect(screen.getByRole("alert").textContent).toMatch(
      /Could not create the command\. gpu-host-2 is connected right now, so a command bound to its name would be refused/,
    );
    expect(mocked.mintHostEnrollment).not.toHaveBeenCalled();
  });

  it("refuses a node name the recovery actor would, before a token is spent", async () => {
    renderModal();
    fireEvent.change(screen.getByLabelText(/Node name/), { target: { value: "gpu host" } });
    await create();
    expect(screen.getByRole("alert").textContent).toMatch(/1 to 253 letters/);
    expect(mocked.mintHostEnrollment).not.toHaveBeenCalled();
  });

  it("says the control plane names no images, and creates nothing", async () => {
    served.mockImplementation(async () => UNPINNED);
    renderModal();
    await waitFor(() => expect(screen.getByTestId("addhost-no-images").textContent).toMatch(/QUASAR_ENROLL_SEED_IMAGE/));
    expect((screen.getByRole("button", { name: "Create command" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("shows a mint failure as could-not-create", async () => {
    mocked.mintHostEnrollment.mockRejectedValue(new ApiError(400, "validation_failed", "expires_at must be at most 30 days out"));
    renderModal();
    await create();
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toMatch(/Could not create the command\. expires_at must be at most 30 days out/),
    );
  });

  it("adds no -k and no pin for a real-CA certificate", async () => {
    mocked.accessCheck.mockResolvedValue(accessCheck({ self_signed: false }));
    renderModal();
    await create();
    await waitFor(() => expect(screen.getByTestId("enroll-command")).toBeTruthy());
    expect(screen.getByTestId("enroll-command").textContent).toMatch(/^curl -fsSL https:\/\/cp\.example:8443\/enroll-host\.sh \| /);
    expect(screen.queryByTestId("enroll-fingerprint")).toBeNull();
  });

  it("forgets the previous token when closed and opened again", async () => {
    const { rerender } = renderModal();
    await create();
    await waitFor(() => expect(screen.getByTestId("enroll-command")).toBeTruthy());
    rerender(<AddHostModal open={false} onClose={() => {}} origin={ORIGIN} fetchScript={served} />);
    rerender(<AddHostModal open onClose={() => {}} origin={ORIGIN} fetchScript={served} />);
    expect(screen.queryByTestId("enroll-command")).toBeNull();
    expect(screen.getByRole("button", { name: "Create command" })).toBeTruthy();
  });
});
