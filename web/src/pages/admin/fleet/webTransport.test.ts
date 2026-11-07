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

describe("certificateFact: which certificate, and bad data", () => {
  it("picks the earliest unexpired certificate whatever the report order", () => {
    const f = certificateFact(wt({}, [cert(12 * 24 * H, "later"), cert(5 * 24 * H, "sooner")]), NOW);
    expect(f.headline).toBe("Expires in 5 days");
    expect(f.fingerprint).toBe("sooner");
  });

  it("skips an expired certificate in the list", () => {
    expect(certificateFact(wt({}, [cert(-H, "gone"), cert(9 * 24 * H, "live")]), NOW)).toMatchObject({
      headline: "Expires in 9 days",
      fingerprint: "live",
      warn: false,
    });
  });

  it("says the expiry is unknown for an unparseable date, with no Invalid Date anywhere", () => {
    const bad = { ...cert(H), not_after: "soon" };
    const f = certificateFact(wt({}, [bad]), NOW);
    expect(f).toMatchObject({ headline: "Expiry unknown", warn: false });
    expect(JSON.stringify(f)).not.toContain("Invalid");
  });

  it("does not claim a failed renewal for a host that is offline", () => {
    const offline = wt({ usable: false, reason: "host_offline" }, [cert(3 * H)]);
    expect(certificateFact(offline, NOW)).toMatchObject({ warn: false, headline: "Expires in 3h" });
    expect(certificateFact(offline, NOW).sub).not.toContain("renewal");
    expect(certificateFact(wt({ usable: false, reason: "host_offline" }, [cert(-H)]), NOW)).toMatchObject({
      headline: "Expired",
      warn: false,
    });
  });
});

describe("webTransportMode", () => {
  it("is edge only for a public-certificate override", () => {
    expect(webTransportMode(wt())).toBe("direct");
    expect(webTransportMode(wt({ override: { public_url: "https://x.example/", certificate: "public" } }))).toBe("edge");
    expect(webTransportMode(wt({ override: { public_url: "https://x.example/", certificate: "agent" } }))).toBe("direct");
    // The resolved endpoint does not decide it: an edge endpoint without an override is not edge.
    expect(webTransportMode(wt({ endpoint: { url: "https://x:4433/", mode: "edge", cert_hashes: [] } }))).toBe("direct");
  });
});
