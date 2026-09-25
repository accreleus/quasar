/**
 * Add host (#359): what the dialog puts in front of an operator besides the
 * one-line command — the seed-only stack for Dockge or Arcane, and the choices
 * both carry. The stack's fields are the seed's inputs as docs/configuration.md
 * "Seed" lists them; a test holds the two identical.
 *
 * Both paths install the same images: the served /enroll-host.sh carries them
 * (control-plane/internal/enrollscript writes them in), and the dialog reads them
 * back from it rather than from a second source (testdata/enroll-host/pins.json).
 */

export const DEFAULT_HOME_ROOT = "/var/lib/quasar/homes";

/** Always written out: the recovery actor and the agent default it differently. */
export function templateRootFor(homeRoot: string): string {
  const trimmed = homeRoot.replace(/\/+$/, "");
  const parent = trimmed.slice(0, trimmed.lastIndexOf("/"));
  return `${parent}/templates`;
}

/** The recovery actor's node-name rule (recipe::validate). */
export function validNodeName(name: string): boolean {
  return /^[A-Za-z0-9._-]{1,253}$/.test(name);
}

/** repository@sha256:<64 lowercase hex>, never a tag: the seed refuses anything else. */
export function isDigestRef(ref: string): boolean {
  if (!/^[A-Za-z0-9][A-Za-z0-9._/:-]*@sha256:[0-9a-f]{64}$/.test(ref)) return false;
  const repo = ref.slice(0, ref.indexOf("@"));
  return !repo.slice(repo.lastIndexOf("/") + 1).includes(":");
}

export type ServedImages = { seedImage: string; agentImage: string };

/** The two images the served script installs, or null when the control plane
 *  names none (or an image is not a digest pin). */
export function readServedImages(script: string): ServedImages | null {
  const pin = (name: string) => {
    const m = new RegExp(`^${name}='([^']*)'$`, "m").exec(script);
    return m && isDigestRef(m[1]) ? m[1] : null;
  };
  const seedImage = pin("PINNED_SEED_IMAGE");
  const agentImage = pin("PINNED_AGENT_IMAGE");
  return seedImage && agentImage ? { seedImage, agentImage } : null;
}

export type Expiry = { label: string; ms: number };

const HOUR = 3_600_000;
const DAY = 24 * HOUR;

/** The control plane caps expiry at 30 days from its own clock; the longest option
 *  stays a minute inside it so clock skew cannot turn it into a 400. */
export const EXPIRY_OPTIONS: Expiry[] = [
  { label: "in 1 hour", ms: HOUR },
  { label: "in 6 hours", ms: 6 * HOUR },
  { label: "in 1 day", ms: DAY },
  { label: "in 7 days", ms: 7 * DAY },
  { label: "in 30 days", ms: 30 * DAY - 60_000 },
];

/** "Single use · expires at 15:42 · only for gpu-host-6". */
export function tokenCaption(expiresAt: string | null, nodeName: string | null, now = new Date()): string {
  let when = "expires in an hour";
  if (expiresAt) {
    const at = new Date(expiresAt);
    const sameDay = at.toDateString() === now.toDateString();
    const time = at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    when = sameDay ? `expires at ${time}` : `expires ${at.toLocaleDateString()} ${time}`;
  }
  return `Single use · ${when} · ${nodeName ? `only for ${nodeName}` : "any node name"}`;
}

export type SeedStackInputs = {
  seedImage: string;
  agentImage: string;
  enrollment: string;
  nodeName?: string | null;
  homeRoot?: string;
};

/** The seed alone, as a Compose stack. The top-level `name:` keeps the volume
 *  `quasar-machine` rather than `<project>_quasar-machine`, which the seed refuses. */
export function composeSeedStack(i: SeedStackInputs): string {
  const home = i.homeRoot ?? DEFAULT_HOME_ROOT;
  const env = [
    "QUASAR_ROLE: gpu",
    `QUASAR_ENROLLMENT: ${i.enrollment}`,
    ...(i.nodeName ? [`QUASAR_NODE_NAME: ${i.nodeName}`] : []),
    `QUASAR_HOME_ROOT: ${home}`,
    `QUASAR_TEMPLATE_ROOT: ${templateRootFor(home)}`,
    `QUASAR_AGENT_IMAGE: ${i.agentImage}`,
  ];
  return [
    "services:",
    "  quasar-seed:",
    `    image: ${i.seedImage}`,
    "    command: seed",
    "    restart: unless-stopped",
    "    security_opt: [label=disable]",
    "    environment:",
    ...env.map((line) => `      ${line}`),
    "    volumes:",
    "      - /var/run/docker.sock:/var/run/docker.sock",
    "      - quasar-machine:/var/lib/quasar-machine:ro",
    "volumes:",
    "  quasar-machine:",
    "    name: quasar-machine",
  ].join("\n");
}
