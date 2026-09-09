import { describe, expect, it } from "vitest";
import type { AdminActivityItem } from "../../../api/admin";
import { actionSentence, actionVerb, detailReadout, summaryLine, targetLabel } from "./describe";

const APP = "8b1116c8-fc33-4c01-9110-11601bab6be7";
const HOST = "4daeaa27-8f6f-4dad-aaae-475f1d304916";
const SESSION = "85d0b6a9-0000-4000-8000-000000000001";

function item(over: Partial<AdminActivityItem> = {}): AdminActivityItem {
  return {
    id: 1,
    actor_user_id: "u-1",
    actor_username: "salty2011",
    action: "host.drain",
    target_type: "host",
    target_id: HOST,
    details: {},
    created_at: "2026-09-09T14:02:11Z",
    severity: "info",
    names: {},
    ...over,
  };
}

/** The row from the screenshot that started this: a launch showing two bare
 *  uuids and no way to tell which app or host. */
const LAUNCH = item({
  action: "session.launched",
  target_type: "session",
  target_id: SESSION,
  details: { app_id: APP, host_id: HOST },
  names: { [SESSION]: "Steam · salty2011", [APP]: "Steam", [HOST]: "gpu-test" },
});

describe("targetLabel", () => {
  it("is the resolved name when the server resolved one", () => {
    expect(targetLabel(LAUNCH)).toBe("Steam · salty2011");
  });

  it("falls back to type + short id when nothing resolved", () => {
    expect(targetLabel(item({ names: {} }))).toBe("host 4daeaa27");
  });

  it("is the bare type when the row targets nothing", () => {
    expect(targetLabel(item({ target_type: "instance", target_id: null }))).toBe("instance");
  });
});

describe("actionSentence", () => {
  it("never sentence-cases the actor — a username is case-sensitive", () => {
    expect(actionSentence(LAUNCH)).toBe("salty2011 launched session Steam · salty2011");
  });

  it("attributes an actorless row to the system", () => {
    const failed = item({
      actor_user_id: null,
      actor_username: null,
      action: "session.failed",
      target_type: "session",
      target_id: SESSION,
      names: { [SESSION]: "Steam · kenji" },
    });
    expect(actionSentence(failed)).toBe("The system recorded a failure for session Steam · kenji");
  });

  it("omits the target for an action that has none", () => {
    const synced = item({ action: "image.synced", target_type: "image", target_id: null });
    expect(actionSentence(synced)).toBe("salty2011 synced the image catalogue");
  });

  it("humanises an action it has never seen, rather than dropping it", () => {
    expect(actionVerb("widget.frobnicated")).toBe("widget frobnicated");
  });

  it("never repeats the object noun the target type already supplies", () => {
    const minted = item({
      action: "invite.minted",
      target_type: "invite",
      target_id: "2fc39454-0000-4000-8000-000000000001",
      names: {},
    });
    expect(actionSentence(minted)).toBe("salty2011 minted invite 2fc39454");
  });

  it("omits the noun where the verb already names the object", () => {
    const tombstoned = item({
      action: "storage.home.tombstone",
      target_type: "storage_home",
      target_id: "3f2a1b9c-0000-4000-8000-000000000001",
      details: { username: "kenji", app_name: "Steam" },
      // The server seeds the target from the stamped name when the home is gone.
      names: { "3f2a1b9c-0000-4000-8000-000000000001": "kenji" },
    });
    expect(actionSentence(tombstoned)).toBe("salty2011 marked a home for cleanup kenji");
  });
});

describe("summaryLine", () => {
  it("shows names, not ids — the column is one line wide", () => {
    expect(summaryLine(LAUNCH)).toBe("app=Steam host=gpu-test");
  });

  it("keeps a value that resolves to no name", () => {
    const drained = item({ details: { force: true } });
    expect(summaryLine(drained)).toBe("force=true");
  });

  it("names the target when the row carries no details at all", () => {
    const disabled = item({
      action: "user.disabled",
      target_type: "user",
      target_id: "u-9",
      details: {},
      names: { "u-9": "kenji" },
    });
    expect(summaryLine(disabled)).toBe("user=kenji");
  });

  it("renders a structured value as compact JSON rather than [object Object]", () => {
    const row = item({ details: { rungs: ["1080p60", "720p60"] } });
    expect(summaryLine(row)).toBe('rungs=["1080p60","720p60"]');
  });
});

describe("detailReadout", () => {
  it("opens with the sentence, then keeps every id in full beside its name", () => {
    expect(detailReadout(LAUNCH)).toBe(
      [
        "salty2011 launched session Steam · salty2011",
        "",
        "action   session.launched",
        "actor    salty2011",
        `target   session ${SESSION} (Steam · salty2011)`,
        `app_id   ${APP} (Steam)`,
        `host_id  ${HOST} (gpu-test)`,
      ].join("\n"),
    );
  });

  it("still reads an unresolved row, without inventing a name", () => {
    const readout = detailReadout(item({ details: { force: true }, names: {} }));
    expect(readout).toContain(`target  host ${HOST}`);
    expect(readout).not.toContain("(");
  });
});
