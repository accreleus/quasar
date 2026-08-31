#!/usr/bin/env node
// scripts/ui-v3/tlsbridge.mjs — plain-HTTP front door for a control plane that
// only speaks HTTPS with a self-signed certificate.
//
// Why this exists: web/vite.config.ts proxies /v1 and /health to
// QUASAR_CONTROL_ORIGIN, and vite's proxy hard-sets `rejectUnauthorized: true`
// when `secure` is undefined (node_modules/vite .. common.js), so a self-signed
// control plane cannot be proxied without editing web/vite.config.ts. The
// capture driver must not edit web/, so it points vite at this bridge instead:
//
//   node scripts/ui-v3/tlsbridge.mjs --port 43111 --target https://127.0.0.1:8443
//   QUASAR_CONTROL_ORIGIN=http://127.0.0.1:43111 npx vite ...
//
// It forwards requests and WebSocket upgrades verbatim with certificate
// verification off. Loopback only, dev only. No dependencies.

import http from "node:http";
import https from "node:https";
import net from "node:net";
import { URL } from "node:url";

function arg(name, fallback) {
  const i = process.argv.indexOf(name);
  return i >= 0 && i + 1 < process.argv.length ? process.argv[i + 1] : fallback;
}

const port = Number(arg("--port", "0"));
const target = new URL(arg("--target", "https://127.0.0.1:8443"));
const agent = new https.Agent({ rejectUnauthorized: false, keepAlive: true });

const server = http.createServer((req, res) => {
  const upstream = https.request(
    {
      host: target.hostname,
      port: target.port || 443,
      path: req.url,
      method: req.method,
      headers: { ...req.headers, host: target.host },
      agent,
      rejectUnauthorized: false,
    },
    (up) => {
      res.writeHead(up.statusCode ?? 502, up.headers);
      up.pipe(res);
    }
  );
  upstream.on("error", (err) => {
    res.writeHead(502, { "content-type": "text/plain" });
    res.end(`tlsbridge: ${err.message}`);
  });
  req.pipe(upstream);
});

// WebSocket (signaling) upgrades: open a TLS socket to the control plane and
// splice it to the client socket after replaying the upgrade request.
server.on("upgrade", (req, socket) => {
  const tls = https.request({
    host: target.hostname,
    port: target.port || 443,
    path: req.url,
    method: req.method,
    headers: { ...req.headers, host: target.host },
    rejectUnauthorized: false,
    agent: false,
  });
  tls.on("upgrade", (upRes, upSocket, upHead) => {
    const head = [`HTTP/1.1 ${upRes.statusCode} ${upRes.statusMessage}`];
    for (const [k, v] of Object.entries(upRes.headers)) head.push(`${k}: ${v}`);
    socket.write(head.join("\r\n") + "\r\n\r\n");
    if (upHead?.length) socket.write(upHead);
    upSocket.pipe(socket);
    socket.pipe(upSocket);
  });
  tls.on("error", () => socket.destroy());
  tls.end();
});

server.on("error", (err) => {
  console.error(`tlsbridge: ${err.message}`);
  process.exit(1);
});

server.listen(port, "127.0.0.1", () => {
  const actual = /** @type {net.AddressInfo} */ (server.address()).port;
  console.log(`tlsbridge listening on http://127.0.0.1:${actual} -> ${target.origin}`);
});
