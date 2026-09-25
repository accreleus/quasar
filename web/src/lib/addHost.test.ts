import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import {
  composeSeedStack,
  EXPIRY_OPTIONS,
  isDigestRef,
  readServedImages,
  templateRootFor,
  tokenCaption,
  validNodeName,
} from "./addHost";

// `import.meta.url` is not a file URL under vitest's transform, so repo files are
// found from the runner's cwd — `web/` normally, the repo root if invoked there.
function repoFile(rel: string): string {
  const path = [`../${rel}`, rel].map((p) => resolve(process.cwd(), p)).find(existsSync);
  if (!path) throw new Error(`${rel} not found from ${process.cwd()}`);
  return readFileSync(path, "utf8");
}

const pins = JSON.parse(repoFile("testdata/enroll-host/pins.json")) as {
  seed_image: string;
  agent_image: string;
  lines: [string, string];
};
const script = repoFile("deploy/enroll-host.sh");
const served = script
  .replace(/^PINNED_SEED_IMAGE=''$/m, pins.lines[0])
  .replace(/^PINNED_AGENT_IMAGE=''$/m, pins.lines[1]);

describe("the served script and the stack name one seed (testdata/enroll-host/pins.json)", () => {
  it("reads back the images the control plane writes into /enroll-host.sh", () => {
    expect(served).not.toBe(script);
    expect(readServedImages(served)).toEqual({ seedImage: pins.seed_image, agentImage: pins.agent_image });
  });

  it("writes exactly those images into the stack", () => {
    const images = readServedImages(served)!;
    const stack = composeSeedStack({ ...images, enrollment: "qenr1..abc.tok" });
    expect(stack).toContain(`    image: ${pins.seed_image}\n`);
    expect(stack).toContain(`      QUASAR_AGENT_IMAGE: ${pins.agent_image}\n`);
  });

  it("finds nothing to install in the repository copy or in a tag", () => {
    expect(readServedImages(script)).toBeNull();
    expect(readServedImages(served.replace(pins.seed_image, "ghcr.io/x/quasar-recovery:0.6.0"))).toBeNull();
  });
});

describe("the seed-only stack", () => {
  it("is docs/configuration.md \"Seed\"'s Compose stack, field for field", () => {
    const doc = repoFile("docs/configuration.md");
    const section = doc.slice(doc.indexOf("The same seed as a Compose stack"));
    const block = /```yaml\n([\s\S]*?)\n```/.exec(section)?.[1];
    expect(block).toBeDefined();
    expect(
      composeSeedStack({
        seedImage: "<registry>/quasar-recovery@sha256:<digest>",
        agentImage: "<registry>/quasar-node-agent@sha256:<digest>",
        enrollment: "qenr1.…",
      }),
    ).toBe(block);
  });

  it("keeps the machine-state volume's own name, which the seed requires", () => {
    const stack = composeSeedStack({ seedImage: "s@x", agentImage: "a@x", enrollment: "qenr1..u.t" });
    expect(stack.endsWith("volumes:\n  quasar-machine:\n    name: quasar-machine")).toBe(true);
    expect(stack).toContain("      - quasar-machine:/var/lib/quasar-machine:ro");
  });

  it("names the node only when the token is bound to one, and always sets the template root", () => {
    const base = { seedImage: "s@x", agentImage: "a@x", enrollment: "qenr1..u.t" };
    expect(composeSeedStack(base)).not.toContain("QUASAR_NODE_NAME");
    expect(composeSeedStack({ ...base, nodeName: "gpu-host-6" })).toContain("      QUASAR_NODE_NAME: gpu-host-6\n");
    expect(composeSeedStack({ ...base, homeRoot: "/srv/quasar/homes" })).toContain(
      "      QUASAR_TEMPLATE_ROOT: /srv/quasar/templates\n",
    );
  });
});

describe("inputs", () => {
  it("puts templates beside the home root, as the one-line script does", () => {
    expect(templateRootFor("/var/lib/quasar/homes")).toBe("/var/lib/quasar/templates");
    expect(templateRootFor("/mnt/cache/appdata/quasar/homes/")).toBe("/mnt/cache/appdata/quasar/templates");
  });

  it("accepts the recovery actor's node names and digest pins only", () => {
    expect(validNodeName("gpu-host-6")).toBe(true);
    expect(validNodeName("gpu host")).toBe(false);
    expect(validNodeName("a".repeat(254))).toBe(false);
    expect(isDigestRef(pins.seed_image)).toBe(true);
    expect(isDigestRef("registry.example:5000/quasar-recovery@sha256:" + "0".repeat(64))).toBe(true);
    expect(isDigestRef("ghcr.io/x/quasar-recovery:1.0@sha256:" + "0".repeat(64))).toBe(false);
    expect(isDigestRef("ghcr.io/x/quasar-recovery@sha256:" + "A".repeat(64))).toBe(false);
  });

  it("offers expiries up to the control plane's 30-day cap, one hour first", () => {
    expect(EXPIRY_OPTIONS[0]).toEqual({ label: "in 1 hour", ms: 3_600_000 });
    expect(Math.max(...EXPIRY_OPTIONS.map((o) => o.ms))).toBeLessThan(30 * 24 * 3_600_000);
  });

  it("says single use, when it expires and for which node", () => {
    const now = new Date("2026-09-25T14:42:00");
    const at = new Date("2026-09-25T15:42:00");
    const time = at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    expect(tokenCaption(at.toISOString(), "gpu-host-6", now)).toBe(`Single use · expires at ${time} · only for gpu-host-6`);
    expect(tokenCaption(new Date("2026-10-02T15:42:00").toISOString(), null, now)).toMatch(/^Single use · expires .+ · any node name$/);
    expect(tokenCaption(null, null, now)).toBe("Single use · expires in an hour · any node name");
  });
});
