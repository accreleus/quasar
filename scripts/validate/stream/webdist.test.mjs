// node --test scripts/validate/stream/webdist.test.mjs: WEB_DIST (#1284): the route decision, the directory check, the digest
// and the browser-context handler, against a fake route. No browser, no stack.
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { decide, loadWebDist, serveWebDist, webBuild } from "./webdist.mjs";

const ORIGIN = "https://stack.test";
const TMP = fs.mkdtempSync(path.join(os.tmpdir(), "e2e-webdist-"));
process.on("exit", () => fs.rmSync(TMP, { recursive: true, force: true }));
let n = 0;
/** A build directory shaped like web/dist. */
function build(over = {}) {
  const dir = path.join(TMP, `d${(n += 1)}`);
  const files = { "index.html": "<html>A</html>", "assets/index-abc.js": "export{}", "assets/index-abc.css": "a{}", "assets/font-1.woff2": "w", "icon.svg": "<svg/>", "manifest.webmanifest": "{}", "cert-probe-sw.js": "//", "enroll-host.sh": "#!/bin/sh", ...over };
  for (const [f, body] of Object.entries(files)) {
    if (body === null) continue;
    fs.mkdirSync(path.dirname(path.join(dir, f)), { recursive: true });
    fs.writeFileSync(path.join(dir, f), body);
  }
  return dir;
}
const dist = loadWebDist(build());
const go = (p, method = "GET", origin = ORIGIN) => decide(dist, method, origin + p, ORIGIN);

test("the route decision, over real paths of the stack", () => {
  // [path, expected]: "stack" = never answered here; a file name = served from the directory; a number = answered here with that status
  const table = [
    // the control plane's own routes (control-plane/cmd/quasar-control/app.go, internal/agentws, debug.go)
    ["/v1/me", "stack"], ["/v1/auth/login", "stack"], ["/v1/sessions?x=1", "stack"], ["/v1/signal", "stack"], ["/v1", "stack"],
    ["/v1/assets/index-abc.js", "stack"], ["/%76%31/me", "stack"], ["/health", "stack"], ["/agent/ws", "stack"], ["/wt", "stack"],
    ["/enroll-host.sh", "stack"], ["/debug/pprof/", "stack"],
    // assets, by their real names and with a query
    ["/assets/index-abc.js", "assets/index-abc.js"], ["/assets/index-abc.js?v=2#x", "assets/index-abc.js"], ["/assets/font-1.woff2", "assets/font-1.woff2"],
    ["/icon.svg", "icon.svg"], ["/manifest.webmanifest", "manifest.webmanifest"], ["/cert-probe-sw.js", "cert-probe-sw.js"], ["/index.html", "index.html"],
    // the SPA owns these: index.html, as the server's own fallback does
    ["/", "index.html"], ["/app", "index.html"], ["/app/", "index.html"], ["/app/game/42", "index.html"], ["/admin/hosts", "index.html"], ["/login", "index.html"],
    ["/nothing-here.js", "index.html"], ["/health/x", "index.html"], ["/V1/me", "index.html"], ["/assets", "index.html"],
    // an /assets miss is a 404, never index.html (it would MIME-block the module script)
    ["/assets/gone.js", 404], ["/assets/sub/gone.js", 404],
    // a path out of the directory, however it is spelled
    ["//etc/passwd", 400], ["/assets//x", 400], ["/%2e%2e/%2e%2e/etc/passwd", "index.html"], ["/assets/..%2f..%2fx", 400], ["/a%2fb", 400], ["/a%5cb", 400],
    ["/a%00b", 400], ["/a%zz", 400], ["/%5c..%5cx", 400],
  ];
  for (const [p, want] of table) {
    const d = go(p);
    const got = d.pass ? "stack" : d.status !== 200 ? d.status : d.file;
    assert.equal(got, want, `${p}: ${JSON.stringify({ ...d, body: undefined })}`);
  }
});

test("only a same-origin GET or HEAD is answered here; a write or another origin goes on", () => {
  for (const m of ["POST", "PUT", "PATCH", "DELETE", "OPTIONS", "get"]) assert.equal(go("/assets/index-abc.js", m).pass, true, m);
  assert.equal(go("/", "POST").pass, true);
  assert.equal(go("/assets/index-abc.js", "HEAD").file, "assets/index-abc.js");
  assert.equal(go("/assets/index-abc.js", "GET", "https://other.test").pass, true);
  assert.equal(go("/assets/index-abc.js", "GET", "http://stack.test").pass, true, "another scheme is another origin");
  assert.equal(decide(dist, "GET", "not a url", ORIGIN).pass, true);
});

test("content types come from the extension and the body is the file's", () => {
  assert.equal(go("/assets/index-abc.js").type, "text/javascript; charset=utf-8");
  assert.equal(go("/assets/index-abc.css").type, "text/css; charset=utf-8");
  assert.equal(go("/assets/font-1.woff2").type, "font/woff2");
  assert.equal(go("/icon.svg").type, "image/svg+xml");
  assert.equal(go("/app/x").type, "text/html; charset=utf-8");
  assert.equal(go("/app/x").body.toString(), "<html>A</html>");
});

test("the directory check: what is refused at start", () => {
  const refused = [
    ["no index.html", () => build({ "index.html": null }), /no index\.html/],
    ["an index.html only in a sub-directory", () => build({ "index.html": null, "sub/index.html": "x" }), /no index\.html/],
    ["an extension with no content type", () => build({ "assets/thing.xyz": "x" }), /no content type/],
    ["no directory", () => path.join(TMP, "missing"), /not a directory that exists/],
    ["a file, not a directory", () => path.join(build(), "index.html"), /not a directory/],
    ["a link out of the directory", () => { const d = build(); const out = path.join(TMP, "secret.js"); fs.writeFileSync(out, "x"); fs.symlinkSync(out, path.join(d, "assets/link.js")); return d; }, /not a regular file/],
    ["a link to a directory out", () => { const d = build(); fs.symlinkSync(TMP, path.join(d, "assets/dir")); return d; }, /not a regular file/],
  ];
  for (const [name, make, re] of refused) assert.throws(() => loadWebDist(make()), re, name);
  assert.throws(() => loadWebDist(build(), 10), /more than/, "over the size bound");
  assert.doesNotThrow(() => loadWebDist(build(), 1024));
});

test("the digest names the content, not the place: same files same digest, one byte or one name changes it", () => {
  const a = loadWebDist(build());
  assert.match(a.sha256, /^[0-9a-f]{64}$/);
  assert.equal(a.sha256, loadWebDist(build()).sha256, "another directory, same files");
  assert.notEqual(a.sha256, loadWebDist(build({ "assets/index-abc.js": "export{};" })).sha256, "a byte");
  assert.notEqual(a.sha256, loadWebDist(build({ "assets/index-abc.js": null, "assets/index-abd.js": "export{}" })).sha256, "a name");
  assert.notEqual(a.sha256, loadWebDist(build({ "extra.txt": "x" })).sha256, "a file added");
  assert.equal(a.count, 8);
  assert.deepEqual(webBuild(a), { source: "web-dist", sha256: a.sha256, files: 8 });
  // The bytes served are the bytes digested: a change on disk after the load is not served.
  const d = build();
  const loaded = loadWebDist(d);
  fs.writeFileSync(path.join(d, "assets/index-abc.js"), "changed");
  assert.equal(decide(loaded, "GET", `${ORIGIN}/assets/index-abc.js`, ORIGIN).body.toString(), "export{}");
});

// ── the handler in a browser context, against a fake route ──────────────────

async function handler(fetchImpl) {
  let match;
  let fn;
  await serveWebDist({ route: async (m, f) => { match = m; fn = f; } }, `${ORIGIN}/`, dist);
  const send = async (method, url) => {
    const out = {};
    const route = {
      request: () => ({ method: () => method, url: () => url }),
      continue: async () => { out.continued = true; },
      fulfill: async (o) => { out.fulfilled = o; },
      fetch: fetchImpl ?? (async () => ({ headers: () => ({ "content-security-policy": "default-src 'self'", "x-frame-options": "DENY", "content-length": "1200", "content-encoding": "gzip", etag: "x", "set-cookie": "a=b" }) })),
    };
    await fn(route);
    return out;
  };
  return { match, send };
}

test("the handler: only the stack's origin is matched; the API goes on, a file is answered with no-store", async () => {
  const { match, send } = await handler();
  assert.equal(match(new URL(`${ORIGIN}/x`)), true);
  assert.equal(match(new URL("https://other.test/x")), false);
  assert.equal((await send("GET", `${ORIGIN}/v1/me`)).continued, true);
  assert.equal((await send("POST", `${ORIGIN}/icon.svg`)).continued, true, "a write to a static path is the stack's");
  const js = (await send("GET", `${ORIGIN}/assets/index-abc.js`)).fulfilled;
  assert.deepEqual([js.status, js.headers["cache-control"], js.headers["content-type"], js.body.toString()], [200, "no-store", "text/javascript; charset=utf-8", "export{}"]);
  assert.equal(js.headers["content-security-policy"], undefined, "a script needs no copy of the document's headers");
  const miss = (await send("GET", `${ORIGIN}/assets/gone.js`)).fulfilled;
  assert.deepEqual([miss.status, miss.headers["cache-control"]], [404, "no-store"]);
});

test("the handler: a document carries the stack's security headers and nothing else of its answer", async () => {
  const { send } = await handler();
  const doc = (await send("GET", `${ORIGIN}/app/game/1`)).fulfilled;
  assert.equal(doc.status, 200);
  assert.equal(doc.body.toString(), "<html>A</html>");
  assert.deepEqual(Object.keys(doc.headers).sort(), ["cache-control", "content-security-policy", "content-type", "x-frame-options"]);
  const down = (await (await handler(async () => { throw new Error("down"); })).send("GET", `${ORIGIN}/`)).fulfilled;
  assert.equal(down.status, 502, "a stack that does not answer is not papered over with the directory's page");
});
