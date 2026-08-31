/**
 * Save a blob to disk from a synthetic anchor click.
 *
 * The revoke must stay deferred: a same-tick revoke races the browser's own
 * read of the object URL and can leave a zero-byte file.
 */
export function downloadBlob(filename: string, blob: Blob): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

/** `downloadBlob` for a JSON payload, pretty-printed. */
export function downloadJson(filename: string, payload: unknown): void {
  downloadBlob(filename, new Blob([JSON.stringify(payload, null, 2)], { type: "application/json" }));
}
