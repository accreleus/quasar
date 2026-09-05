// The changelog parser, against the two real published bodies (v0.2.0-rc.1 and
// rc.2, saved verbatim under __fixtures__) plus the shapes those two do not
// happen to contain.

import { describe, expect, it } from "vitest";
import rc1 from "./__fixtures__/release-rc1.md?raw";
import rc2 from "./__fixtures__/release-rc2.md?raw";
import { parseReleaseNotes, releaseCountsLine } from "./releaseNotes";

const REPO = "accreleus/quasar";

describe("parseReleaseNotes on the real rc.2 body", () => {
  const parsed = parseReleaseNotes(rc2, REPO);

  it("finds every Fixed bullet and nothing from the install footer", () => {
    expect(parsed.counts).toEqual({ Fixed: 6 });
    expect(releaseCountsLine(parsed.counts)).toBe("6 fixed");
    // The footer is a `##` section of pins and shell blocks; a bullet from it
    // would be an entry that is not a change.
    expect(parsed.entries.map((e) => e.title).join(" ")).not.toMatch(/QUASAR_CONTROL_IMAGE/);
  });

  it("titles each entry from its leading bold run, marker stripped", () => {
    expect(parsed.entries[0].title).toBe(
      "Release detection follows the one redirect GitHub and GHCR answer with",
    );
    expect(parsed.entries[3].title).toBe(
      "Update Quasar is disabled when the control plane is not eligible",
    );
  });

  it("keeps a bold run that wraps across lines as one line of title", () => {
    expect(parsed.entries[5].title).toBe(
      "The leak scan no longer treats Unraid's generic appdata path as an operator fingerprint",
    );
  });

  it("tags Fixed as FIX in the info tone", () => {
    expect(parsed.entries[0].tag).toBe("FIX");
    expect(parsed.entries[0].tone).toBe("info");
  });

  it("links every issue the bullet references, de-duplicated", () => {
    expect(parsed.entries[0].issues).toEqual([
      { number: 110, url: `https://github.com/${REPO}/issues/110` },
      { number: 111, url: `https://github.com/${REPO}/issues/111` },
    ]);
    expect(parsed.entries[5].issues).toEqual([]);
  });

  it("keeps the rest of the bullet as the detail, with its inline code", () => {
    expect(parsed.entries[0].detail).toMatch(/^The stable channel downloaded/);
    expect(parsed.entries[0].detail).toContain("`platform-release-manifest.json`");
    // The comma that joined the title to its clause is not the detail's opener.
    expect(parsed.entries[3].detail).toBe(
      "with the\nreason in its tooltip, instead of answering a `409` after the click.",
    );
  });
});

describe("parseReleaseNotes on the real rc.1 body", () => {
  const parsed = parseReleaseNotes(rc1, REPO);

  it("keeps every category heading it meets, in heading order", () => {
    expect(parsed.counts).toEqual({ Security: 3, Fixed: 2, Removed: 1, Added: 3, Changed: 2 });
    expect(releaseCountsLine(parsed.counts)).toBe(
      "3 security · 2 fixed · 1 removed · 3 added · 2 changed",
    );
  });

  it("maps each known heading to its tag and tone", () => {
    const byCategory = new Map(parsed.entries.map((e) => [e.category, [e.tag, e.tone]]));
    expect(byCategory.get("Security")).toEqual(["SEC", "danger"]);
    expect(byCategory.get("Added")).toEqual(["NEW", "success"]);
    expect(byCategory.get("Fixed")).toEqual(["FIX", "info"]);
    expect(byCategory.get("Removed")).toEqual(["GONE", "neutral"]);
    expect(byCategory.get("Changed")).toEqual(["CHG", "warning"]);
  });

  it("strips a marker that carries a range and a semicolon", () => {
    // "**Quasar updates itself from the admin console (#104; #105-#119).**"
    const added = parsed.entries.find((e) => e.category === "Added");
    expect(added?.title).toBe("Quasar updates itself from the admin console");
    expect(added?.issues.map((i) => i.number)).toContain(105);
  });

  it("keeps a bullet's later paragraphs and nested bullets in its detail", () => {
    const sec = parsed.entries[0];
    expect(sec.title).toBe("A copy-pasted make line could run a second command");
    // The second paragraph of that bullet, two blank-line-separated blocks in.
    expect(sec.detail).toContain("No caller-settable variable reaches a recipe line any more");
    expect(sec.detail.split("\n\n").length).toBeGreaterThan(1);
  });
});

describe("parseReleaseNotes rules", () => {
  it("titles a bullet with no bold run by its first sentence", () => {
    const { entries } = parseReleaseNotes(
      "### Fixed\n- Boot race in the node agent. It raced the compositor. Twice.\n",
      REPO,
    );
    expect(entries[0].title).toBe("Boot race in the node agent");
    expect(entries[0].detail).toBe("It raced the compositor. Twice.");
  });

  it("uses the whole bullet as the title when it has no sentence break", () => {
    const { entries } = parseReleaseNotes("### Fixed\n- Boot race in the node agent\n", REPO);
    expect(entries[0].title).toBe("Boot race in the node agent");
    expect(entries[0].detail).toBe("");
  });

  it("does not end the first sentence on a dot inside a code span", () => {
    const { entries } = parseReleaseNotes(
      "### Fixed\n- `deploy/.env` is rewritten in place. The previous file is kept.\n",
      REPO,
    );
    expect(entries[0].title).toBe("deploy/.env is rewritten in place");
  });

  it("extracts a bare #NNN as well as a parenthesised one, once each", () => {
    const { entries } = parseReleaseNotes(
      "### Fixed\n- **A thing (#12).** See #12 and #7 and #12 again.\n",
      REPO,
    );
    expect(entries[0].title).toBe("A thing");
    expect(entries[0].issues.map((i) => i.number)).toEqual([12, 7]);
  });

  it("renders no issue link when no repo is configured", () => {
    const { entries } = parseReleaseNotes("### Fixed\n- **A thing (#12).** Detail.\n", REPO && "");
    expect(entries[0].issues).toEqual([{ number: 12, url: "" }]);
  });

  it("ends the changelog at the first `##` heading", () => {
    const { entries } = parseReleaseNotes(
      "### Fixed\n- **A real change.** Detail.\n\n## Install or upgrade\n\n- Not a change\n",
      REPO,
    );
    expect(entries).toHaveLength(1);
  });

  it("keeps an unknown heading's bullets under a tag of its own letters", () => {
    const { entries } = parseReleaseNotes("### Deprecated\n- **A thing.** Detail.\n", REPO);
    expect(entries[0].category).toBe("Deprecated");
    expect(entries[0].tag).toBe("DEPR");
    expect(entries[0].tone).toBe("neutral");
  });

  it("is empty for an edge build, which publishes no notes", () => {
    expect(parseReleaseNotes("", REPO)).toEqual({ entries: [], counts: {} });
  });
});
