import { describe, expect, it } from "vitest";
import type { HostWebTransport } from "../../../api/types";
import { certificateFact, webTransportMode } from "./webTransport";

const NOW = Date.parse("2026-11-07T12:00:00Z");
const H = 3_600_000;
const cert = (leftMs: number, sha256 = "AAAAAAAABBBB") => ({
  sha256,
  not_before: new Date(NOW - 13 * 24 * H).toISOString(),
  not_after: new Date(NOW + leftMs).toISOString(),
});

function wt(over: Partial<HostWebTransport> = {}, certificates = [cert(9 * 24 * H)]): HostWebTransport {
  return {
    reported: { transports: ["webrtc", "webtransport"], port: 4433, addresses: ["198.51.100.24"], certificates },
    override: null,
    endpoint: null,
    usable: true,
    reason: null,
    ...over,
  };
}

describe("certificateFact", () => {
  it("is quiet while the expiry is far off", () => {
    const f = certificateFact(wt(), NOW);
    expect(f.headline).toBe("Expires in 9 days");
    expect(f.warn).toBe(false);
    expect(f.fingerprint).toBe("AAAAAAAABBBB");
  });

  it("warns only when under 24 h are left and no successor is reported", () => {
    expect(certificateFact(wt({}, [cert(3 * H)]), NOW)).toMatchObject({ warn: true });
    // Rotation overlap: the next certificate is reported, so renewal worked.
    expect(certificateFact(wt({}, [cert(3 * H), cert(12 * 24 * H, "next")]), NOW).warn).toBe(false);
    expect(certificateFact(wt({}, [cert(25 * H)]), NOW).warn).toBe(false);
  });

  it("says renewal failed once expired", () => {
    const f = certificateFact(wt({}, [cert(-H)]), NOW);
    expect(f).toMatchObject({ headline: "Expired", warn: true });
    expect(f.sub).toContain("automatic renewal failed");
  });

  it("shows no expiry, and never warns, behind a proxy with a public certificate", () => {
    const f = certificateFact(
      wt({ override: { public_url: "https://play.example.com/wt", certificate: "public" } }, [cert(H)]),
      NOW,
    );
    expect(f).toMatchObject({ headline: "Managed by your proxy", warn: false, fingerprint: null });
  });

  it("shows no expiry for an operator-supplied certificate (reported certificates: [])", () => {
    expect(certificateFact(wt({}, []), NOW)).toMatchObject({ headline: "Supplied by you", warn: false });
  });
});

describe("webTransportMode", () => {
  it("follows the resolved endpoint, else the override", () => {
    expect(webTransportMode(wt())).toBe("direct");
    expect(webTransportMode(wt({ override: { public_url: "https://x.example/", certificate: "public" } }))).toBe("edge");
    expect(
      webTransportMode(wt({ endpoint: { url: "https://x:4433/", mode: "direct", cert_hashes: [] } })),
    ).toBe("direct");
  });
});
