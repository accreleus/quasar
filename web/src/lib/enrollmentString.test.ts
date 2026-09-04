import { describe, expect, it } from "vitest";
import {
  agentWssUrl,
  base64UrlNoPad,
  canMintFrom,
  composeEnrollmentString,
  composeInstallCommand,
  installerScriptUrl,
  normalizeFingerprint,
} from "./enrollmentString";

const FP =
  "0A:1B:2C:3D:4E:5F:60:71:82:93:A4:B5:C6:D7:E8:F9:0A:1B:2C:3D:4E:5F:60:71:82:93:A4:B5:C6:D7:E8:F9";

describe("enrollment string", () => {
  it("shares its wire vector with the agent's decoder test", () => {
    // node-agent/src/enrollment.rs asserts `qenr1.<FP>.d3NzOi8vY3A.tok` decodes to wss://cp.
    expect(base64UrlNoPad("wss://cp")).toBe("d3NzOi8vY3A");
    const r = composeEnrollmentString({ origin: "https://cp", fingerprint: FP, token: "tok" });
    expect(r).toEqual({ ok: true, value: `qenr1.${FP}.d3NzOi8vY3A.tok`, url: "wss://cp", fingerprint: FP });
  });

  it("keeps the fingerprint first and verbatim in the control plane's form", () => {
    for (const form of [FP, FP.toLowerCase(), FP.replace(/:/g, ""), `sha256:${FP}`]) {
      expect(normalizeFingerprint(form)).toBe(FP);
    }
    expect(normalizeFingerprint("not a fingerprint")).toBeNull();
    expect(normalizeFingerprint(FP.slice(0, -3))).toBeNull();
    const r = composeEnrollmentString({
      origin: "https://cp.example:8443",
      fingerprint: FP.toLowerCase(),
      token: "t",
    });
    expect(r.ok && r.value.startsWith(`qenr1.${FP}.`)).toBe(true);
  });

  it("refuses a plain-http origin rather than emit a ws:// string", () => {
    expect(agentWssUrl("http://cp.example:8080")).toBeNull();
    const r = composeEnrollmentString({ origin: "http://cp.example:8080", fingerprint: FP, token: "t" });
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.reason).toMatch(/HTTPS/);
  });

  it("emits an empty fingerprint segment for a real-CA certificate", () => {
    const r = composeEnrollmentString({ origin: "https://play.example.com", fingerprint: null, token: "t" });
    expect(r.ok && r.value.startsWith("qenr1..")).toBe(true);
    expect(r.ok && r.fingerprint).toBeNull();
  });

  it("survives a token containing dots and a URL with a port", () => {
    const r = composeEnrollmentString({ origin: "https://cp.lan:8443", fingerprint: FP, token: "a.b.c" });
    expect(r.ok).toBe(true);
    if (r.ok) {
      const parts = r.value.split(".");
      // prefix, fingerprint, url, then the token (which itself has dots) — max-3 split on the agent side
      expect(parts.slice(0, 3)).toEqual(["qenr1", FP, base64UrlNoPad("wss://cp.lan:8443")]);
      expect(parts.slice(3).join(".")).toBe("a.b.c");
    }
  });

  it("gates mint()'s pre-flight guard on the same http-origin check", () => {
    expect(canMintFrom("http://cp.example:8080")).toBe(false);
    expect(canMintFrom("https://cp.example:8443")).toBe(true);
  });
});

describe("one-line installer (#100)", () => {
  const FULL = `qenr1.${FP}.d3NzOi8vY3A.tok`;
  const PIN = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789+/AbCdE=";

  it("fetches the script from the control plane itself, at the origin this page was opened at", () => {
    expect(installerScriptUrl("https://cp.example:8443")).toBe("https://cp.example:8443/enroll-host.sh");
    expect(installerScriptUrl("https://cp.example:8443/")).toBe("https://cp.example:8443/enroll-host.sh");
    expect(installerScriptUrl("http://cp.example:8080")).toBeNull();
  });

  it("pins the self-signed control plane's key: -k is only ever paired with --pinnedpubkey", () => {
    const cmd = composeInstallCommand({ origin: "https://cp.example:8443", enrollment: FULL, ref: "v1.2.3", spkiPin: PIN });
    expect(cmd).toBe(
      `curl -fsSL -k --pinnedpubkey 'sha256//${PIN}' https://cp.example:8443/enroll-host.sh | QUASAR_ENROLLMENT='${FULL}' QUASAR_REF=v1.2.3 sh`,
    );
  });

  it("uses plain CA verification for a real certificate: no -k, no pin", () => {
    const cmd = composeInstallCommand({ origin: "https://play.example.com", enrollment: FULL, ref: "v1.2.3", spkiPin: null });
    expect(cmd).toBe(
      `curl -fsSL https://play.example.com/enroll-host.sh | QUASAR_ENROLLMENT='${FULL}' QUASAR_REF=v1.2.3 sh`,
    );
    expect(cmd).not.toMatch(/ -k| --insecure|pinnedpubkey/);
  });

  it("passes the string and the ref as environment, never in the URL; drops the ref when the build has none", () => {
    const cmd = composeInstallCommand({ origin: "https://cp.example:8443", enrollment: FULL, ref: "", spkiPin: PIN }) ?? "";
    expect(cmd.endsWith(`| QUASAR_ENROLLMENT='${FULL}' sh`)).toBe(true);
    expect(cmd).not.toContain("QUASAR_REF");
    expect(cmd.indexOf(FULL)).toBeGreaterThan(cmd.indexOf("|"));
  });

  it("refuses anything that would break out of the shell quoting or is not a real pin", () => {
    const base = { origin: "https://cp.example:8443", enrollment: FULL, ref: "v1.2.3", spkiPin: PIN };
    expect(composeInstallCommand({ ...base, ref: "v1; rm -rf /" })).toBeNull();
    expect(composeInstallCommand({ ...base, enrollment: "qenr1..abc.tok'; id #" })).toBeNull();
    expect(composeInstallCommand({ ...base, enrollment: "" })).toBeNull();
    expect(composeInstallCommand({ ...base, spkiPin: "not-a-pin'" })).toBeNull();
    expect(composeInstallCommand({ ...base, origin: "http://cp.example:8080" })).toBeNull();
  });
});
