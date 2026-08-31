/**
 * Unit tests for the certificate-trust probe (certTrust.ts).
 *
 * The probe's whole reason to exist is a state jsdom cannot be in — an HTTPS
 * origin behind a clicked-through certificate warning — so what is tested here
 * is the DECISION TABLE around the browser boundary: which register() outcomes
 * map to which verdicts, the SPA-fallback guard (#131), the insecure-context
 * short-circuit, and the one-probe-per-page cache. The underlying browser fact
 * (Chromium refuses service-worker registration on cert-error origins with a
 * SecurityError, while resolving keyboard.lock() and passing isSecureContext)
 * was measured live in Chrome 151 — see the module header.
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { probeCertificateTrust, resetCertTrustForTests } from "./certTrust";

function stubSecureContext(value: boolean) {
  Object.defineProperty(window, "isSecureContext", { value, configurable: true });
}

function stubServiceWorker(register: (() => Promise<unknown>) | undefined) {
  Object.defineProperty(navigator, "serviceWorker", {
    value: register ? { register } : undefined,
    configurable: true,
  });
}

function stubFetch(ok: boolean, contentType: string) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => ({
      ok,
      headers: { get: (name: string) => (name.toLowerCase() === "content-type" ? contentType : null) },
    })),
  );
}

function securityError(): Error {
  const err = new Error("Failed to register a ServiceWorker: an unknown error occurred when fetching the script.");
  err.name = "SecurityError";
  return err;
}

afterEach(() => {
  resetCertTrustForTests();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  delete (navigator as unknown as { serviceWorker?: unknown }).serviceWorker;
});

describe("probeCertificateTrust", () => {
  it("reports trusted when the probe worker registers (and unregisters it)", async () => {
    stubSecureContext(true);
    stubFetch(true, "text/javascript");
    const unregister = vi.fn(async () => true);
    const register = vi.fn(async () => ({ unregister }));
    stubServiceWorker(register);

    await expect(probeCertificateTrust()).resolves.toBe("trusted");
    expect(register).toHaveBeenCalledWith("/cert-probe-sw.js", { scope: "/__cert-trust-probe__/" });
    expect(unregister).toHaveBeenCalled();
  });

  it("reports untrusted-cert when register() rejects with SecurityError", async () => {
    stubSecureContext(true);
    stubFetch(true, "application/javascript; charset=utf-8");
    stubServiceWorker(vi.fn(async () => Promise.reject(securityError())));

    await expect(probeCertificateTrust()).resolves.toBe("untrusted-cert");
  });

  it("reports unknown for a non-SecurityError rejection (flaky network, odd engines)", async () => {
    stubSecureContext(true);
    stubFetch(true, "text/javascript");
    stubServiceWorker(vi.fn(async () => Promise.reject(new TypeError("network went away"))));

    await expect(probeCertificateTrust()).resolves.toBe("unknown");
  });

  it("reports unknown — NOT untrusted — when the probe script comes back as SPA-fallback HTML (#131)", async () => {
    stubSecureContext(true);
    // The deploy lost the file: index.html with 200. register() would reject
    // with the SAME SecurityError a bad certificate produces, so the verdict
    // must bail out before register() is ever called.
    stubFetch(true, "text/html");
    const register = vi.fn(async () => ({ unregister: vi.fn() }));
    stubServiceWorker(register);

    await expect(probeCertificateTrust()).resolves.toBe("unknown");
    expect(register).not.toHaveBeenCalled();
  });

  it("reports unknown on an insecure (plain-HTTP) context — that case has its own wording paths", async () => {
    stubSecureContext(false);
    await expect(probeCertificateTrust()).resolves.toBe("unknown");
  });

  it("reports unknown where the service-worker API is absent", async () => {
    stubSecureContext(true);
    stubServiceWorker(undefined);
    await expect(probeCertificateTrust()).resolves.toBe("unknown");
  });

  it("probes once per page load — every caller shares one verdict", async () => {
    stubSecureContext(true);
    stubFetch(true, "text/javascript");
    const register = vi.fn(async () => ({ unregister: vi.fn(async () => true) }));
    stubServiceWorker(register);

    await probeCertificateTrust();
    await probeCertificateTrust();
    expect(register).toHaveBeenCalledTimes(1);
  });
});
