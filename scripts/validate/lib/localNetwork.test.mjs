// node --test lib/localNetwork.test.mjs (#1456): the journeys runner grants local network access to its contexts, as the e2e runner does.
import assert from "node:assert/strict";
import test from "node:test";
import { grantLocalNetwork } from "./localNetwork.mjs";

test("a context is granted local-network-access", async () => {
  const granted = [];
  await grantLocalNetwork({ grantPermissions: async (p) => { granted.push(...p); } });
  assert.deepEqual(granted, ["local-network-access"]);
});

test("a Chrome that does not know the permission still gets its context, with a warning", async () => {
  const warned = [];
  await grantLocalNetwork({ grantPermissions: async () => { throw new Error("Unknown permission: local-network-access\n  at ..."); } }, (m) => warned.push(m));
  assert.equal(warned.length, 1);
  assert.match(warned[0], /not granted \(Unknown permission: local-network-access\)/);
  await grantLocalNetwork({}); // a context with no grantPermissions at all is not an error
});

test("the journeys runner grants it on the context it makes", async () => {
  const fs = await import("node:fs");
  const src = fs.readFileSync(new URL("../runner.mjs", import.meta.url), "utf8");
  assert.match(src, /context = await browser\.newContext\([\s\S]*?\);\s*await grantLocalNetwork\(context\);/);
});
