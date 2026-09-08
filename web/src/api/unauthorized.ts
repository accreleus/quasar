// The one place the SPA learns that its bearer token is no longer good.
//
// Before this existed, a 401 was handled in exactly one place: AuthProvider's
// mount-time GET /v1/me. Every later 401 — the token expiring while a tab sat
// open, or an admin revoking the device — surfaced as an ordinary load error, so
// the page kept rendering the data it had already fetched behind a red banner
// whose "Try again" could only ever fail again (#154). An expired token must not
// leave a signed-in page on screen.
//
// It lives in its own module rather than in client.ts so that AuthProvider can
// subscribe without importing the client, and client.ts can publish without
// importing anything from auth/ — the cycle that would otherwise force this into
// a React context and out of reach of a plain fetch wrapper.

type Listener = () => void;

const listeners = new Set<Listener>();

/**
 * Subscribe to "the bearer token was rejected". Returns the unsubscribe.
 *
 * Handlers must be idempotent: a page that fires several requests at once gets
 * one notification per rejected request, not one per session.
 */
export function onUnauthorized(fn: Listener): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

/**
 * Publish a rejected bearer token. Called by apiFetch, and only for a request
 * that actually carried one — a 401 from the sign-in form is a statement about
 * the credentials just typed, not about a session, and must stay in the form.
 */
export function notifyUnauthorized(): void {
  // Copy first: a handler is allowed to unsubscribe itself.
  for (const fn of [...listeners]) fn();
}
