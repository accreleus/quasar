// Does this HTTPS origin's certificate actually chain to trust, or did the
// user click through a certificate warning to get here?
//
// Why it matters: a bypassed-certificate origin is the WORST-diagnosed state
// in the product. `window.isSecureContext` is true and every secure-context
// API object exists, so all of the "you need HTTPS" hints (Keyboard Lock in
// SessionDrawerInput, `unsupportedMicError` in webrtc/mic.ts) conclude the
// origin is fine and never render — precisely when the user is on the origin
// where features misbehave. The operator-facing symptoms are in-game Esc
// capture not holding and microphone permission that cannot be granted
// durably.
//
// How it detects (all measured live, Chrome 151, 2026-08-27, non-loopback
// origin behind a clicked-through NET::ERR_CERT_AUTHORITY_INVALID
// interstitial):
//   - isSecureContext === true, navigator.keyboard present, keyboard.lock()
//     RESOLVES, permissions.query({name:"keyboard-lock"}) === "granted" —
//     so neither API presence nor an observed lock() rejection can detect
//     this case.
//   - navigator.serviceWorker.register() is the one API that refuses:
//     Chromium rejects ANY service-worker registration on a cert-error
//     origin with a SecurityError, while Cache Storage and IndexedDB work.
//     On a trusted origin the same registration succeeds (control-tested).
// So the probe registers a no-op worker (public/cert-probe-sw.js) under a
// scope nothing uses and immediately unregisters it. SecurityError ⇒ the
// certificate is not trusted. Success ⇒ trusted. Anything else ⇒ unknown.
//
// The verdict is ADVISORY ONLY: it picks honest wording in hints and error
// toasts. It must never gate a feature — Firefox stores a permanent cert
// exception (which may or may not refuse service workers the same way), and
// a wrong "untrusted" verdict that only softens a hint is harmless where a
// wrong verdict that disables a feature is not.
//
// The SPA-fallback trap (#131) is guarded explicitly: a deploy that lost the
// probe script serves index.html with 200 for it, and Chrome rejects a
// text/html worker script with the SAME SecurityError a bad certificate
// produces. The pre-fetch below verifies we are looking at JavaScript before
// a SecurityError is allowed to mean "certificate".

export type CertTrust = "trusted" | "untrusted-cert" | "unknown";

const PROBE_SCRIPT = "/cert-probe-sw.js";
// A scope no route lives under, so the momentarily-registered worker can
// never intercept a real navigation even if unregister() loses a race.
const PROBE_SCOPE = "/__cert-trust-probe__/";

let cached: Promise<CertTrust> | null = null;

/**
 * Probe once per page load, cached — every caller (session hints, mic error
 * mapping) shares one verdict and one registration attempt.
 */
export function probeCertificateTrust(): Promise<CertTrust> {
  if (!cached) cached = runProbe();
  return cached;
}

/** Test seam: clear the cache so each test observes its own probe. */
export function resetCertTrustForTests(): void {
  cached = null;
}

async function runProbe(): Promise<CertTrust> {
  try {
    if (typeof window === "undefined" || window.isSecureContext !== true) {
      // Plain HTTP is not this module's question — the existing
      // insecure-context wording paths already own it.
      return "unknown";
    }
    const sw = navigator.serviceWorker;
    if (!sw || typeof sw.register !== "function") return "unknown";

    const head = await fetch(PROBE_SCRIPT, { cache: "no-store" });
    const contentType = head.headers.get("content-type") ?? "";
    if (!head.ok || !/javascript|ecmascript/i.test(contentType)) {
      // Probe script missing or SPA-fallback HTML — a SecurityError from
      // register() would be about the script, not the certificate.
      return "unknown";
    }

    const registration = await sw.register(PROBE_SCRIPT, { scope: PROBE_SCOPE });
    void Promise.resolve(registration.unregister()).catch(() => {});
    return "trusted";
  } catch (err) {
    const name =
      typeof err === "object" && err !== null && "name" in err
        ? String((err as { name: unknown }).name)
        : "";
    if (name === "SecurityError") {
      // One console line, in the one place the verdict is computed: this is
      // the diagnosability for every downstream symptom at once.
      console.info(
        "origin: this HTTPS address uses a certificate the browser does not " +
          "trust (a bypassed certificate warning). In-game Esc capture and " +
          "microphone permission are unreliable here — download the server " +
          "certificate from /v1/tls/certificate.pem, trust it on this " +
          "device, then reload.",
      );
      return "untrusted-cert";
    }
    return "unknown";
  }
}
