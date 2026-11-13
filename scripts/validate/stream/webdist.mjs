// make e2e-run WEB_DIST=<dir>: the web client's files for a run come from a local build directory, served
// by the runner inside its own browser context; the stack's API, signaling and media are untouched.
// docs/testing-e2e-suite.md "Testing a web build against a running stack" is the guide.
//
// The routing mirrors control-plane/internal/httpx/spa.go (SPAHandler) and the routes the control plane registers
// (cmd/quasar-control/app.go RegisterRoutes, internal/agentws/handler.go, cmd/quasar-control/debug.go): /v1/*, /health,
// /agent/*, /wt, /debug/* and /enroll-host.sh are server routes and are NEVER answered from the directory; an /assets/* miss is
// a 404; any other path with no file is the SPA fallback (index.html). No dependencies.
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";

export const MAX_BYTES = 200 * 1024 * 1024;
// What a Vite build emits (web/dist). An extension not here refuses the run: a guessed type could change how the page loads.
export const TYPES = {
  ".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8", ".json": "application/json", ".webmanifest": "application/manifest+json",
  ".svg": "image/svg+xml", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
  ".avif": "image/avif", ".ico": "image/x-icon", ".woff": "font/woff", ".woff2": "font/woff2", ".ttf": "font/ttf", ".otf": "font/otf",
  ".txt": "text/plain; charset=utf-8", ".map": "application/json", ".wasm": "application/wasm",
  ".sh": "text/x-shellscript; charset=utf-8", // enroll-host.sh rides in every build; it is a server route and is never served from here
};
// The headers the control plane's SecurityHeaders sets (internal/httpx/security.go): a document served from the directory
// carries the stack's own, so a build the stack's content security policy would block is blocked here too.
const SECURITY_HEADERS = ["content-security-policy", "x-content-type-options", "x-frame-options", "referrer-policy", "permissions-policy", "cross-origin-opener-policy"];
const SERVER_PREFIXES = ["v1", "agent", "debug"];
const SERVER_EXACT = ["health", "wt", "enroll-host.sh"];

/** Reads and checks `dir` once: the bytes served are the bytes digested. Throws a reason that names no path outside the directory. */
export function loadWebDist(dir, max = MAX_BYTES) {
  let root;
  try { root = fs.realpathSync(dir); } catch { throw new Error("WEB_DIST is not a directory that exists"); }
  if (!fs.statSync(root).isDirectory()) throw new Error("WEB_DIST is not a directory");
  const files = new Map();
  let bytes = 0;
  const walk = (rel) => {
    for (const e of fs.readdirSync(path.join(root, rel), { withFileTypes: true })) {
      const r = rel ? `${rel}/${e.name}` : e.name;
      if (e.isDirectory()) { walk(r); continue; }
      if (!e.isFile()) throw new Error(`WEB_DIST: ${r} is not a regular file (a link or special file is refused)`);
      const type = TYPES[path.extname(e.name).toLowerCase()];
      if (!type) throw new Error(`WEB_DIST: ${r} has an extension this runner has no content type for`);
      bytes += fs.lstatSync(path.join(root, r)).size;
      if (bytes > max) throw new Error(`WEB_DIST holds more than ${max / 1024 / 1024} MB`);
      files.set(r, { type, body: fs.readFileSync(path.join(root, r)) });
    }
  };
  // A file or folder that cannot be read: said with its place inside the directory, never the system's own message (it holds the full path).
  try { walk(""); } catch (e) { throw e.code ? new Error(`WEB_DIST: something in it cannot be read (${e.code})`) : e; }
  if (!files.has("index.html")) throw new Error("WEB_DIST has no index.html at its top");
  const h = crypto.createHash("sha256");
  for (const r of [...files.keys()].sort()) h.update(`${r}\0${crypto.createHash("sha256").update(files.get(r).body).digest("hex")}\n`);
  return { files, sha256: h.digest("hex"), count: files.size };
}

/** What builds.json records for the web build: the digest and the count, no path. */
export const webBuild = (dist) => ({ source: "web-dist", sha256: dist.sha256, files: dist.count });

/**
 * What to do with one request: `{ pass: true }` (the stack answers), `{ status, why }` (refused or missing), or `{ status: 200, file, type, body }`.
 * Only a same-origin GET or HEAD can be answered here; a write to any path goes to the stack, where the page-write watch sees it.
 */
export function decide(dist, method, url, origin) {
  let u;
  try { u = new URL(url); } catch { return { pass: true }; }
  if (u.origin !== origin || !["GET", "HEAD"].includes(method)) return { pass: true };
  // The query and fragment are not part of the path. Each segment is decoded as the server does, then must be a plain name.
  const segs = u.pathname === "/" ? [] : u.pathname.slice(1).replace(/\/$/, "").split("/");
  for (let i = 0; i < segs.length; i += 1) {
    try { segs[i] = decodeURIComponent(segs[i]); } catch { return { status: 400, why: "bad path" }; }
    if (segs[i] === "" || segs[i] === "." || segs[i] === ".." || /[\\/\0]/.test(segs[i])) return { status: 400, why: "bad path" };
  }
  if (SERVER_PREFIXES.includes(segs[0]) || (segs.length === 1 && SERVER_EXACT.includes(segs[0]))) return { pass: true };
  const rel = segs.join("/");
  const hit = rel && dist.files.get(rel);
  if (hit) return { status: 200, file: rel, ...hit };
  if (segs[0] === "assets" && segs.length > 1) return { status: 404, why: "not found" };
  return { status: 200, file: "index.html", ...dist.files.get("index.html") };
}

/** Answers the stack's static files from `dist` in `context` and nowhere else. Call before a page is opened. */
export async function serveWebDist(context, baseUrl, dist) {
  const origin = new URL(baseUrl).origin;
  // Only what this would answer is routed: a request the stack answers (the API, /health) never waits on the runner, so the
  // page's own timing probes of the stack read what they would without WEB_DIST.
  await context.route((u) => u.origin === origin && !decide(dist, "GET", u.href, origin).pass, async (route) => {
    const q = route.request();
    const d = decide(dist, q.method(), q.url(), origin);
    if (d.pass) return route.continue();
    const headers = { "cache-control": "no-store", "content-type": d.type ?? "text/plain; charset=utf-8" };
    if (d.type?.startsWith("text/html")) {
      // The stack's own answer for this address, for its security headers only; its body is not used.
      try {
        const theirs = (await route.fetch()).headers();
        for (const k of SECURITY_HEADERS) if (theirs[k] !== undefined) headers[k] = theirs[k];
      } catch { return route.fulfill({ status: 502, headers: { "cache-control": "no-store" }, body: "the stack did not answer" }); }
    }
    return route.fulfill({ status: d.status, headers, body: d.body ?? d.why });
  });
}
