import { describe, expect, it } from "vitest";
import { hostImageState, imageFailure, imgRollout, imgVersion } from "./imageRollout";
import type { CatalogImage, Host } from "../../../api/types";

function image(over: Partial<CatalogImage> = {}): CatalogImage {
  return {
    id: "x",
    display_name: "X",
    kind: "prebuilt",
    version: "1.0.0",
    installed: false,
    installed_version: null,
    update_available: false,
    hosts: [],
    ...over,
  } as CatalogImage;
}

function host(id: string, node_name: string): Host {
  return { id, node_name } as Host;
}

describe("imgVersion", () => {
  it("not installed", () => {
    expect(imgVersion(image({ version: "2.0.0" }))).toEqual({ version: "2.0.0", sub: "not installed" });
  });

  it("in flight beats every other state, even mid-update, and keeps the Downloading…/Building… distinction", () => {
    expect(
      imgVersion(
        image({
          version: "2.0.0",
          installed: true,
          update_available: true,
          hosts: [{ host_id: "h1", state: "pulling", version: null, error: null }],
        }),
      ),
    ).toEqual({ version: "2.0.0", sub: "Downloading…", tone: "info" });

    expect(
      imgVersion(
        image({ version: "2.0.0", installed: false, hosts: [{ host_id: "h1", state: "building", version: null, error: null }] }),
      ),
    ).toEqual({ version: "2.0.0", sub: "Building…", tone: "info" });
  });

  it("update available", () => {
    expect(
      imgVersion(image({ version: "2.0.0", installed: true, installed_version: "1.9.0", update_available: true })),
    ).toEqual({ version: "2.0.0", sub: "running 1.9.0", tone: "warning" });
  });

  it("up to date", () => {
    expect(imgVersion(image({ version: "2.0.0", installed: true, installed_version: "2.0.0" }))).toEqual({
      version: "2.0.0",
      sub: "up to date",
    });
  });
});

describe("imgRollout", () => {
  const hosts = [host("h1", "node-1"), host("h2", "node-2"), host("h3", "node-3")];

  it("ready on every host when every host reports ready at the installed version", () => {
    const img = image({
      installed: true,
      installed_version: "1.0.0",
      hosts: hosts.map((h) => ({ host_id: h.id, state: "ready", version: "1.0.0", error: null })),
    });
    expect(imgRollout(img, hosts)).toEqual({
      ready: 3,
      total: 3,
      tone: "success",
      exceptions: [],
      note: "ready on every host",
    });
  });

  it("a ready host whose own version trails installed_version reads as stale, and does not count as ready", () => {
    const img = image({
      installed: true,
      installed_version: "1.1.0",
      hosts: [
        { host_id: "h1", state: "ready", version: "1.1.0", error: null },
        { host_id: "h2", state: "ready", version: "1.0.0", error: null },
        { host_id: "h3", state: "ready", version: "1.1.0", error: null },
      ],
    });
    const roll = imgRollout(img, hosts);
    expect(roll.ready).toBe(2);
    expect(roll.tone).toBe("warning");
    expect(roll.exceptions).toEqual(["node-2 stale"]);
    expect(roll.note).toBe("node-2 stale");
  });

  it("pulling and failed hosts are named exceptions, not counted as ready", () => {
    const img = image({
      installed: true,
      installed_version: "1.0.0",
      hosts: [
        { host_id: "h1", state: "ready", version: "1.0.0", error: null },
        { host_id: "h2", state: "pulling", version: null, error: null },
        { host_id: "h3", state: "failed", version: null, error: "pull timeout" },
      ],
    });
    const roll = imgRollout(img, hosts);
    expect(roll.ready).toBe(1);
    expect(roll.exceptions).toEqual(["node-2 pulling", "node-3 failed"]);
  });

  it("hosts the image never reported (absent, or missing entirely) collapse to a count above two", () => {
    const fourHosts = [...hosts, host("h4", "node-4")];
    const img = image({
      installed: true,
      installed_version: "1.0.0",
      hosts: [{ host_id: "h1", state: "ready", version: "1.0.0", error: null }],
    });
    const roll = imgRollout(img, fourHosts);
    expect(roll.ready).toBe(1);
    expect(roll.exceptions).toEqual(["not on 3 hosts"]);
  });

  it("lists absent hosts by name when two or fewer are missing", () => {
    const img = image({
      installed: true,
      installed_version: "1.0.0",
      hosts: [
        { host_id: "h1", state: "ready", version: "1.0.0", error: null },
        { host_id: "h2", state: "absent", version: null, error: null },
      ],
    });
    const roll = imgRollout(img, hosts);
    expect(roll.ready).toBe(1);
    expect(roll.exceptions).toEqual(["not on node-2, node-3"]);
  });

  it("a never-synced image (no host entries at all) reports zero ready with everyone missing", () => {
    const img = image({ hosts: [] });
    const roll = imgRollout(img, hosts);
    expect(roll.ready).toBe(0);
    expect(roll.exceptions).toEqual(["not on 3 hosts"]);
  });
});

describe("hostImageState — null guards", () => {
  it("is absent when img.hosts is undefined", () => {
    const img = image({});
    delete (img as { hosts?: unknown }).hosts;
    expect(hostImageState(img, "h1")).toBe("absent");
  });

  it("is absent when img.hosts is an empty array", () => {
    expect(hostImageState(image({ hosts: [] }), "h1")).toBe("absent");
  });

  it("is absent when the host id is not among the reported hosts", () => {
    const img = image({ hosts: [{ host_id: "h2", state: "ready", version: "1.0.0", error: null }] });
    expect(hostImageState(img, "h1")).toBe("absent");
  });

  it("a ready host with a null version is never stale (nothing to compare against)", () => {
    const img = image({
      installed_version: "1.0.0",
      hosts: [{ host_id: "h1", state: "ready", version: null, error: null }],
    });
    expect(hostImageState(img, "h1")).toBe("ready");
  });

  it("a ready host is never stale when the image itself has no installed_version", () => {
    const img = image({
      installed_version: null,
      hosts: [{ host_id: "h1", state: "ready", version: "1.0.0", error: null }],
    });
    expect(hostImageState(img, "h1")).toBe("ready");
  });
});

describe("imageFailure", () => {
  const failedOn = (...states: string[]) =>
    image({ installed: true, hosts: states.map((state, i) => ({ host_id: `h${i}`, state, version: null, error: null })) as CatalogImage["hosts"] });

  it("is null when no host failed, however the rest look", () => {
    expect(imageFailure(failedOn("ready", "building", "absent"), 3)).toBeNull();
    expect(imageFailure(image({ hosts: undefined }), 4)).toBeNull();
  });

  it("counts failed hosts against the whole fleet, never more failures than hosts", () => {
    expect(imageFailure(failedOn("failed", "failed", "failed", "absent"), 4)).toBe("Failed on 3 of 4 hosts");
    expect(imageFailure(failedOn("failed", "ready"), 4)).toBe("Failed on 1 of 4 hosts");
    expect(imageFailure(failedOn("failed"), 1)).toBe("Failed on 1 of 1 host");
    expect(imageFailure(failedOn("failed", "failed"), 0)).toBe("Failed on 2 of 2 hosts");
  });

  it("imgVersion reads failed, not up to date, and failure beats in-flight and update-available", () => {
    const done = { version: "2.0.0", installed_version: "2.0.0" };
    const up = { ...failedOn("failed", "failed", "failed", "ready"), ...done };
    expect(imgVersion(up, 4)).toEqual({ version: "2.0.0", sub: "Failed on 3 of 4 hosts", tone: "danger" });
    const busy = { ...failedOn("failed", "building"), ...done, update_available: true };
    expect(imgVersion(busy, 2).tone).toBe("danger");
  });
});
