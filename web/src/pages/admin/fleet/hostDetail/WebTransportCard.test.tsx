import { act, fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../../api/admin");

import * as adminApi from "../../../../api/admin";
import { ApiError } from "../../../../api/client";
import type { HostWebTransport } from "../../../../api/types";
import { ToastProvider } from "../../../../components/Toast";
import { WebTransportCard } from "./WebTransportCard";

const mocked = vi.mocked(adminApi);
const NOW = Date.parse("2026-11-07T12:00:00Z");
const cert = (leftMs: number) => ({
  sha256: "3f9a71c0aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaac21e",
  not_before: new Date(NOW - 13 * 86_400_000).toISOString(),
  not_after: new Date(NOW + leftMs).toISOString(),
});

function body(over: Partial<HostWebTransport> = {}): HostWebTransport {
  return {
    reported: {
      transports: ["webrtc", "webtransport"],
      port: 4433,
      addresses: ["198.51.100.24"],
      certificates: [cert(9 * 86_400_000)],
    },
    override: null,
    endpoint: { url: "https://198.51.100.24:4433/", mode: "direct", cert_hashes: [] },
    usable: true,
    reason: null,
    ...over,
  };
}

async function show(wt: HostWebTransport) {
  mocked.getHostWebTransport.mockResolvedValue(wt);
  render(
    <ToastProvider>
      <WebTransportCard hostId="h1" now={NOW} />
    </ToastProvider>,
  );
  await act(async () => {});
}

beforeEach(() => vi.clearAllMocks());

describe("WebTransportCard", () => {
  it("direct mode: address, read-only port, expiry, fingerprint; Clear off with no override", async () => {
    await show(body());
    expect(screen.getByText("198.51.100.24 · UDP 4433")).toBeInTheDocument();
    expect(screen.getByText("Expires in 9 days")).toBeInTheDocument();
    expect(screen.getByText(/3f9a71c0 … c21e/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Clear" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("proxy-managed: no expiry, no fingerprint", async () => {
    await show(
      body({
        override: { public_url: "https://play.example.com/wt", certificate: "public" },
        endpoint: { url: "https://play.example.com/wt", mode: "edge", cert_hashes: [] },
      }),
    );
    expect(screen.getByText("Managed by your proxy")).toBeInTheDocument();
    expect(screen.queryByText("Fingerprint")).toBeNull();
    expect(screen.getByText("edge proxy")).toBeInTheDocument();
  });

  it("warns that renewal failed when the certificate is nearly out with no successor", async () => {
    const wt = body();
    wt.reported!.certificates = [cert(3 * 3_600_000)];
    await show(wt);
    expect(screen.getByText(/Certificate renewal failed/)).toBeInTheDocument();
    expect(screen.getByText("renewal failed")).toBeInTheDocument();
  });

  it("agent without WebTransport: says so and offers no URL field", async () => {
    await show(body({ reported: null, endpoint: null, usable: false, reason: "not_reported" }));
    expect(screen.getByText(/does not offer WebTransport yet/)).toBeInTheDocument();
    expect(screen.queryByRole("textbox")).toBeNull();
  });

  it("saves the URL, keeping the override's certificate mode", async () => {
    mocked.setHostWebTransport.mockResolvedValue(
      body({ override: { public_url: "https://play.example.com/wt", certificate: "agent" } }),
    );
    await show(body({ override: { public_url: "https://old.example.com/", certificate: "agent" } }));
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "https://play.example.com/wt" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save" }));
    });
    expect(mocked.setHostWebTransport).toHaveBeenCalledWith("tok", "h1", {
      public_url: "https://play.example.com/wt",
      certificate: "agent",
    });
  });

  it("clears with null", async () => {
    mocked.setHostWebTransport.mockResolvedValue(body());
    await show(body({ override: { public_url: "https://old.example.com/", certificate: "public" } }));
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Clear" }));
    });
    expect(mocked.setHostWebTransport).toHaveBeenCalledWith("tok", "h1", { public_url: null, certificate: "public" });
  });

  it("shows the server's rejection under the field", async () => {
    mocked.setHostWebTransport.mockRejectedValue(new ApiError(400, "validation_failed", "Use a full https:// address."));
    await show(body());
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "play.example.com" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save" }));
    });
    expect(screen.getByRole("alert")).toHaveTextContent("Use a full https:// address.");
  });
});
