/**
 * Reverse proxy snippets for people who ALREADY RUN a proxy.
 *
 * Every output here is a block to add to a config that already exists. None of
 * it stands a proxy up, and none of it assumes Quasar gets port 443. Somebody
 * with no proxy yet is on the self-signed path instead.
 *
 * All four target the control plane's HTTP listener rather than 8443, so the
 * reader's proxy never has to skip verification on a self-signed certificate.
 * Quasar keeps serving 8443 directly for LAN access.
 *
 * The four requirements every snippet has to meet come from
 * site/src/content/docs/network/reverse-proxy.mdx and are asserted in
 * stack-template.test.js:
 *
 *   1. WebSocket upgrade forwarded on /v1/signal and /agent/ws
 *   2. No aggressive buffering on those paths
 *   3. A read timeout above 60 seconds, or sessions get cut
 *   4. X-Forwarded-Proto https, which is what stops the control plane
 *      redirecting a request that already arrived over HTTPS
 */

export const PROXIES = {
  caddy: 'Caddy',
  traefik: 'Traefik',
  nginx: 'nginx',
  npm: 'NGINX Proxy Manager',
};

function host(o) {
  return `${o.host}:${o.port}`;
}

function domainOf(o) {
  try {
    return new URL(o.publicUrl).host;
  } catch {
    return 'quasar.example.com';
  }
}

function caddy(o) {
  return `# Add to your existing Caddyfile. Caddy forwards WebSocket upgrades and
# sets X-Forwarded-* on its own, so this stays short.
${domainOf(o)} {
	reverse_proxy http://${host(o)} {
		# Long-lived: /v1/signal and /agent/ws stay open for the whole session.
		transport http {
			read_timeout 3600s
			write_timeout 3600s
		}
		header_up X-Forwarded-Proto https
		flush_interval -1
	}
}
`;
}

function traefik(o) {
  const d = domainOf(o);
  return `# Traefik file provider. Drop this in the directory your existing
# providers.file.directory points at, then reload.
#
# Container labels are the alternative, but they only work when Traefik can
# reach the control plane's Docker network. A file provider pointed at
# host:port works whatever your topology is.
http:
  routers:
    quasar:
      rule: "Host(\`${d}\`)"
      service: quasar
      entryPoints: [websecure]
      tls:
        certResolver: your-resolver
      middlewares: [quasar-headers]

  middlewares:
    quasar-headers:
      headers:
        customRequestHeaders:
          X-Forwarded-Proto: https

  services:
    quasar:
      loadBalancer:
        # Covers /v1/signal and /agent/ws; Traefik proxies WebSockets natively.
        passHostHeader: true
        servers:
          - url: "http://${host(o)}"
        responseForwarding:
          flushInterval: -1

# Entry point read timeouts live in your static config, not here. Quasar's
# connections are long-lived, so make sure respondingTimeouts.readTimeout is 0
# (no limit) or well above 60s, or sessions get cut.
`;
}

function nginx(o) {
  const d = domainOf(o);
  return `# Add to your existing nginx sites-enabled. TLS directives omitted:
# keep whatever your other services already use.
server {
    listen 443 ssl;
    http2 on;
    server_name ${d};

    # ssl_certificate     /path/to/fullchain.pem;
    # ssl_certificate_key /path/to/privkey.pem;

    location / {
        proxy_pass http://${host(o)};
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }

    # Signaling and agent enrollment are WebSockets and stay open for the whole
    # session. Without the upgrade headers signaling breaks entirely; without
    # the timeouts sessions get cut after 60s.
    location ~ ^/(v1/signal|agent/ws)$ {
        proxy_pass http://${host(o)};
        proxy_http_version 1.1;
        proxy_set_header Upgrade           $http_upgrade;
        proxy_set_header Connection        "upgrade";
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-Proto https;
        proxy_buffering off;
        proxy_read_timeout  3600s;
        proxy_send_timeout  3600s;
    }
}
`;
}

function npm(o) {
  const d = domainOf(o);
  return `# NGINX Proxy Manager is GUI-driven, so there is no file to paste.
# Create a Proxy Host with these values:
#
#   Domain Names          ${d}
#   Scheme                http
#   Forward Hostname/IP   ${o.host}
#   Forward Port          ${o.port}
#   Websockets Support    ON      <- signaling breaks without this
#   Block Common Exploits OFF     <- it interferes with the WebSocket upgrade
#   SSL                   your certificate, Force SSL on
#
# Then paste the block below into the Advanced tab. It covers the two paths
# that stay open for a whole session, /v1/signal and /agent/ws: NPM's default
# 60s read timeout cuts them otherwise.

location ~ ^/(v1/signal|agent/ws)$ {
    proxy_pass http://${host(o)};
    proxy_http_version 1.1;
    proxy_set_header Upgrade           $http_upgrade;
    proxy_set_header Connection        "upgrade";
    proxy_set_header Host              $host;
    proxy_set_header X-Forwarded-Proto https;
    proxy_buffering off;
    proxy_read_timeout  3600s;
    proxy_send_timeout  3600s;
}
`;
}

const BUILDERS = {
  caddy: { filename: 'Caddyfile', language: 'caddyfile', build: caddy },
  traefik: { filename: 'quasar.yml', language: 'yaml', build: traefik },
  nginx: { filename: 'quasar.conf', language: 'nginx', build: nginx },
  npm: { filename: 'Advanced tab', language: 'nginx', build: npm },
};

/**
 * @param {'caddy'|'traefik'|'nginx'|'npm'} id
 * @param {{publicUrl: string, host: string, port: number}} opts
 * @returns {{name: string, filename: string, language: string, body: string}}
 */
export function proxyConfig(id, opts) {
  const entry = BUILDERS[id] ?? BUILDERS.caddy;
  return {
    name: PROXIES[id] ?? PROXIES.caddy,
    filename: entry.filename,
    language: entry.language,
    body: entry.build(opts),
  };
}
