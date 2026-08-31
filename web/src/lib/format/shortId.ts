/**
 * The 8-character head of an id, which is how the mock's fixtures render one
 * everywhere except a fact row (`design_handoff_v3/screens/assets/data.js`).
 * A full uuid crowds a breadcrumb and wraps a table sub-line, so the callers
 * that shorten also carry the full id in a `title`.
 */
export const SHORT_ID_LENGTH = 8;

export function shortId(id: string | null | undefined): string {
  return id ? id.slice(0, SHORT_ID_LENGTH) : "";
}
