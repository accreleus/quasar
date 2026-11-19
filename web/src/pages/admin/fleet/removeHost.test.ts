/**
 * The one rule the Fleet row menu and the host page share about offering removal of an
 * owned GPU host (#1240), and the sentence the page shows when it is not offered.
 */

import { describe, expect, it } from "vitest";
import type { Host, PlatformIdentity, ReadinessCheck } from "../../../api/types";
import { canRemoveHost, removable, removalBlockedReason } from "./removeHost";

function host(over: Partial<Host> = {}): Host {
  return {
    id: "h1",
    node_name: "gpu-host-4",
    status: "online",
    install_mode: "owned",
    updater_present: true,
    readiness: [],
    ...over,
  } as Host;
}

/** A Compose or source control plane: it has no machine of its own that a host could be. */
const MACHINE = { machine_role: null, machine_node_name: null } as PlatformIdentity;
const COMBINED = {
  machine_role: "combined",
  machine_node_name: "living-room-pc",
} as PlatformIdentity;
const CONFLICT = {
  id: "owner_conflict",
  status: "fail",
  summary: "x",
  remediation: "",
} as ReadinessCheck;

describe("canRemoveHost", () => {
  it("offers removal of an owned GPU host whose recovery actor is answering", () => {
    expect(removable(host(), MACHINE)).toBe(true);
    expect(canRemoveHost(host(), MACHINE)).toBe(true);
    expect(removalBlockedReason(host(), MACHINE)).toBeNull();
  });

  // A host this rule says nothing about is never sent here by the row menu; `?remove=1`
  // could still open the dialog, so the answer has to be no.
  it("never offers it for a host that is not owned", () => {
    expect(canRemoveHost(host({ install_mode: "registry" }), MACHINE)).toBe(false);
    expect(canRemoveHost(host({ install_mode: null }), MACHINE)).toBe(false);
    expect(removalBlockedReason(host({ install_mode: "registry" }), MACHINE)).toBeNull();
  });

  it("says why it is not offered, for each condition the row menu used to land on silently", () => {
    // The release read failed: the control plane could be this very machine.
    expect(canRemoveHost(host(), null)).toBe(false);
    expect(removalBlockedReason(host(), null)).toMatch(/could not say which machine it runs on/);

    expect(canRemoveHost(host({ readiness: [CONFLICT] }), MACHINE)).toBe(false);
    expect(removalBlockedReason(host({ readiness: [CONFLICT] }), MACHINE)).toMatch(
      /Another owner’s container is in the way/,
    );

    // `updater_present` is whether the recovery actor answered; only `true` is an answer.
    // #1499: the wording follows the services banner — "has not answered" once it went
    // silent (`false`), "has not reported" while it never answered (`null`).
    expect(canRemoveHost(host({ updater_present: false }), MACHINE)).toBe(false);
    expect(removalBlockedReason(host({ updater_present: false }), MACHINE)).toMatch(
      /recovery actor has not answered/,
    );
    expect(canRemoveHost(host({ updater_present: null }), MACHINE)).toBe(false);
    expect(removalBlockedReason(host({ updater_present: null }), MACHINE)).toMatch(
      /recovery actor has not reported/,
    );
  });

  it("leaves the control plane's own machine to the row menu's Forget dialog", () => {
    const own = host({ node_name: "living-room-pc" });
    expect(canRemoveHost(own, COMBINED)).toBe(false);
    expect(removalBlockedReason(own, COMBINED)).toBeNull();
  });
});
