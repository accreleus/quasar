#!/usr/bin/env node
// Frozen autoresearch harness: serves a web/dist build over a static mock control
// plane. Every /v1 answer is a fixed fixture (fixtures.mjs), so two runs of the
// same build see byte-identical data. Unknown GETs answer 404 and are logged,
// so a page that starts calling something new shows up in the run log.
//
//   node server.mjs --dist ../../web/dist --port 4900
//
// Test hooks (harness-only, never in the SPA): index.html gets a script at the
// top of <head> that signs the browser in (token in localStorage) and applies
// ?ar_theme=light|dark, so URL-only tools (impeccable detect) can scan a
// signed-in page in either theme.
import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import zlib from "node:zlib";
import { fixtures } from "./fixtures.mjs";

const arg = (name, dflt) => {
  const i = process.argv.indexOf(name);
  return i > 0 ? process.argv[i + 1] : dflt;
};
const dist = path.resolve(arg("--dist", "../../web/dist"));
const PORT = Number(arg("--port", 4900));

const MIME = {
  ".html": "text/html; charset=utf-8", ".js": "text/javascript", ".css": "text/css",
  ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2", ".woff": "font/woff",
  ".json": "application/json", ".webmanifest": "application/manifest+json", ".sh": "text/plain",
};

const HOOK = `<script>(function(){try{
localStorage.setItem('quasar.auth.token','ar-admin-token');
localStorage.setItem('quasar.auth.expires_at','2099-01-01T00:00:00Z');
localStorage.setItem('quasar.auth.user',JSON.stringify({id:'00000000-0000-4000-8000-000000000001',email:'admin@example.com',username:'admin',role:'admin'}));
var t=new URLSearchParams(location.search).get('ar_theme');
if(t==='light'||t==='dark')localStorage.setItem('quasar-theme',t);
}catch(_){}})();</script>`;

function json(res, status, body) {
  res.writeHead(status, { "content-type": "application/json", "cache-control": "no-store" });
  res.end(body === undefined ? "" : JSON.stringify(body));
}

const unknown = new Set();
function api(req, res, url) {
  const p = url.pathname.slice(3);
  if (req.method !== "GET") {
    // Writes are not part of any measured flow; answer the fixture if one exists.
    const f = fixtures(p, req.method);
    return f === undefined ? json(res, 204) : json(res, 200, f);
  }
  const f = fixtures(p, "GET", url.searchParams);
  if (f === undefined) {
    if (!unknown.has(p)) console.error(`[mock] unknown GET ${p}`);
    unknown.add(p);
    return json(res, 404, { error: { code: "not_found", message: "not in harness fixtures" } });
  }
  return json(res, 200, f);
}

let indexHtml = null;
function serveIndex(res) {
  indexHtml ??= fs.readFileSync(path.join(dist, "index.html"), "utf8").replace("<head>", "<head>" + HOOK);
  res.writeHead(200, { "content-type": MIME[".html"], "cache-control": "no-cache" });
  res.end(indexHtml);
}

http
  .createServer((req, res) => {
    const url = new URL(req.url, "http://x");
    if (url.pathname === "/ar/unknown") return json(res, 200, [...unknown]);
    if (url.pathname.startsWith("/v1/")) return api(req, res, url);
    if (url.pathname === "/health") return json(res, 200, { ok: true });
    const file = path.join(dist, path.normalize(url.pathname).replace(/^(\.\.[/\\])+/, ""));
    if (url.pathname !== "/" && fs.existsSync(file) && fs.statSync(file).isFile()) {
      const type = MIME[path.extname(file)] ?? "application/octet-stream";
      const cache = url.pathname.startsWith("/assets/") ? "public, max-age=31536000, immutable" : "no-cache";
      // gzip text like the control plane does (httpx/compress.go, default level).
      if (/\.(js|css|svg|json|webmanifest)$/.test(file) && /gzip/.test(req.headers["accept-encoding"] ?? "")) {
        const body = zlib.gzipSync(fs.readFileSync(file), { level: 6 });
        res.writeHead(200, { "content-type": type, "content-encoding": "gzip", "content-length": body.length, "cache-control": cache, vary: "Accept-Encoding" });
        return res.end(body);
      }
      res.writeHead(200, { "content-type": type, "content-length": fs.statSync(file).size, "cache-control": cache });
      return fs.createReadStream(file).pipe(res);
    }
    if (url.pathname.startsWith("/assets/")) return json(res, 404, { error: "no asset" });
    return serveIndex(res);
  })
  .listen(PORT, "127.0.0.1", () => console.error(`[mock] ${dist} on :${PORT}`));
