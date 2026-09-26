/**
 * The UI and `docs/upgrading.md` must show the same commands. The page is what
 * an operator reads first and the guide is what they paste from later, so a
 * skeleton that exists in one and not the other is a bug in whichever moved.
 */

import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import {
  AGENT_IMAGE_VAR,
  CONTROL_IMAGE_VAR,
  REDEPLOY_SKELETON,
  REGISTRY_PULL_COMMAND,
  REGISTRY_RECREATE_COMMAND,
  ACTOR_CHECK_COMMAND,
  ACTOR_LOG_COMMAND,
  ACTOR_START_COMMAND,
} from "./manualUpdate";

// `import.meta.url` is not a file URL under vitest's transform, so the guide is
// found from the runner's cwd — `web/` normally, the repo root if invoked there.
const candidates = ["../docs/upgrading.md", "docs/upgrading.md"].map((p) =>
  resolve(process.cwd(), p),
);
const guidePath = candidates.find(existsSync);
if (!guidePath) throw new Error(`docs/upgrading.md not found from ${process.cwd()}`);
const guide = readFileSync(guidePath, "utf8");

describe("docs/upgrading.md and the manual-update commands", () => {
  it("documents the registry-install recipe the UI shows", () => {
    expect(guide).toContain("## Upgrading a registry install");
    for (const skeleton of [
      `${CONTROL_IMAGE_VAR}=`,
      `${AGENT_IMAGE_VAR}=`,
      REGISTRY_PULL_COMMAND,
      REGISTRY_RECREATE_COMMAND,
    ]) {
      expect(guide).toContain(skeleton);
    }
  });

  it("documents the recovery-actor check the UI shows", () => {
    for (const skeleton of [ACTOR_CHECK_COMMAND, ACTOR_LOG_COMMAND, ACTOR_START_COMMAND]) {
      expect(guide).toContain(skeleton);
    }
  });

  it("no longer documents the retired Compose updater", () => {
    expect(guide).not.toContain("quasar-updater");
    expect(guide).not.toContain("QUASAR_STACK_DIR");
  });

  it("documents the source-install redeploy the UI shows", () => {
    expect(guide).toContain(REDEPLOY_SKELETON);
  });
});
