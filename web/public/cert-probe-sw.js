// Deliberately empty service worker. It is never activated for real work:
// lib/certTrust.ts registers it (narrow scope, immediately unregistered) as a
// probe, because Chromium refuses ANY service-worker registration on an origin
// whose certificate the user bypassed — the only page-observable signal that
// distinguishes "trusted HTTPS" from "HTTPS with a bypassed certificate
// warning" (both report window.isSecureContext === true). Do not add logic
// here; a probe that does work is a probe with side effects.
self.addEventListener("install", () => {});
