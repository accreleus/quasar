/**
 * Write one generated script per (platform, role, access, database) so
 * shellcheck can lint them all. A GPU host has no script: it joins with the
 * one-line command from Add host. Not part of the site build; run it by hand:
 *
 *   node src/data/write-fixtures.mjs /tmp/quickstart-fixtures
 *   docker run --rm -v /tmp/quickstart-fixtures:/mnt koalaman/shellcheck:stable /mnt/*.sh
 */
import { mkdirSync, writeFileSync } from "node:fs";
import { DEFAULTS, generate } from "./stack-template.js";

const out = process.argv[2] ?? "/tmp/quickstart-fixtures";
mkdirSync(out, { recursive: true });

let n = 0;
for (const platform of ["fedora", "ubuntu", "debian", "arch", "other", "unraid"]) {
  for (const [engine, mode] of [["docker", "rootful"], ["docker", "rootless"], ["podman", "rootful"], ["podman", "rootless"]]) {
    for (const role of ["combined", "control-only"]) {
      for (const access of ["self-signed", "proxy"]) {
        for (const database of ["owned", "external"]) {
          const answers = {
            ...DEFAULTS,
            platform,
            engine,
            mode,
            role,
            access,
            database,
            publicHost: "192.168.1.50",
            tlsHosts: "quasar.lan",
            publicUrl: "https://quasar.example.com",
            trustedProxies: "192.168.1.2",
            dbHost: "db.example.internal",
          };
          const { script } = generate(answers);
          if (!script) continue; // unsupported or rootless: no output
          writeFileSync(`${out}/${platform}-${engine}-${mode}-${role}-${access}-${database}.sh`, script);
          n++;
        }
      }
    }
  }
}
console.log(`wrote ${n} scripts to ${out}`);
