/**
 * Write one generated script per (gpu, access) pair so shellcheck can lint them
 * all. Not part of the site build; run it by hand or from CI:
 *
 *   node src/data/write-fixtures.mjs /tmp/quickstart-fixtures
 *   docker run --rm -v /tmp/quickstart-fixtures:/mnt koalaman/shellcheck:stable /mnt/*.sh
 */
import { mkdirSync, writeFileSync } from "node:fs";
import { DEFAULTS, generate } from "./stack-template.js";

const out = process.argv[2] ?? "/tmp/quickstart-fixtures";
mkdirSync(out, { recursive: true });

let n = 0;
for (const plat of ["fedora", "debian", "arch", "other", "unraid"]) {
  for (const gpu of ["nvidia", "amd-intel"]) {
    for (const access of ["self-signed", "proxy", "own-cert"]) {
      const answers = {
        ...DEFAULTS,
        platform: plat,
        gpu,
        access,
        tlsHosts: "192.168.1.50,quasar.lan",
        publicUrl: "https://quasar.example.com",
        certPath: "/etc/ssl/quasar/cert.pem",
        keyPath: "/etc/ssl/quasar/key.pem",
      };
      writeFileSync(
        `${out}/${plat}-${gpu}-${access}.sh`,
        generate(answers).script,
      );
      n++;
    }
  }
}
console.log(`wrote ${n} scripts to ${out}`);
