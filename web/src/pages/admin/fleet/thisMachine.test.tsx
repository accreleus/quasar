import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { PlatformIdentity } from "../../../api/types";
import { ThisMachineBlock } from "./ThisMachine";
import { thisMachine } from "./thisMachine";

const binary: PlatformIdentity = {
  version: "0.5.2",
  source_commit: "3f9a2c1000000000000000000000000000000000",
  built_at: "2026-09-24T16:40:00Z",
  schema_version: 88,
};

const owned = (database_mode: "owned" | "external"): PlatformIdentity => ({
  ...binary,
  install_mode: "owned",
  recovery_actor_version: "0.5.2",
  recovery_actor_source_commit: "3f9a2c1000000000000000000000000000000000",
  seed_version: "0.5.0",
  database_mode,
});

const notOwned: PlatformIdentity = {
  ...binary,
  install_mode: null,
  recovery_actor_version: null,
  recovery_actor_source_commit: null,
  seed_version: null,
  database_mode: null,
};

const asOf = () => "13:48";

describe("thisMachine", () => {
  it("draws nothing for a machine that is not owned, or an older server", () => {
    expect(thisMachine(notOwned, null, asOf).kind).toBe("none");
    expect(thisMachine(binary, null, asOf).kind).toBe("none");
  });

  it("lists the seed, the recovery actor, Quasar's own database and the control plane", () => {
    const m = thisMachine(owned("owned"), null, asOf);
    expect(m.kind).toBe("reported");
    if (m.kind !== "reported") return;
    expect(m.external).toBe(false);
    expect(m.rows.map((r) => [r.label, r.value, r.hint])).toEqual([
      ["Seed", "v0.5.0", "External manager · running"],
      ["Recovery actor", "v0.5.2", "Quasar · running"],
      ["Database", "Quasar’s own", "Quasar · running"],
      ["Control plane", "v0.5.2", "Quasar · running"],
    ]);
  });

  it("names the operator's own database as theirs, reachable", () => {
    const m = thisMachine(owned("external"), null, asOf);
    if (m.kind !== "reported") throw new Error(m.kind);
    expect(m.external).toBe(true);
    expect(m.rows[2]).toMatchObject({ value: "Your own", hint: "You · reachable" });
  });

  it("says no seed was found when the actor answered without one", () => {
    const m = thisMachine({ ...owned("owned"), seed_version: null }, null, asOf);
    if (m.kind !== "reported") throw new Error(m.kind);
    expect(m.rows[0]).toMatchObject({ value: null, hint: "not found" });
  });

  it("keeps the last report, with its time, once the actor stops answering", () => {
    const at = Date.parse("2026-09-25T13:48:02Z");
    const m = thisMachine(notOwned, { identity: owned("external"), at }, asOf);
    expect(m.kind).toBe("not_answering");
    if (m.kind !== "not_answering") return;
    expect(m.since).toBe(at);
    expect(m.rows.map((r) => r.hint)).toEqual([
      "External manager · as of 13:48",
      "Quasar · as of 13:48",
      "You · as of 13:48",
      "Quasar · running",
    ]);
  });
});

describe("ThisMachineBlock", () => {
  it("renders the rows and the operator's-database note", () => {
    render(<ThisMachineBlock machine={thisMachine(owned("external"), null, asOf)} now={0} />);
    expect(screen.getByText("This machine")).toBeInTheDocument();
    expect(screen.getByText("Your own")).toBeInTheDocument();
    expect(screen.getByText(/never dumps, restores or upgrades it/)).toBeInTheDocument();
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("says how long the recovery actor has not answered", () => {
    const at = Date.parse("2026-09-25T13:48:02Z");
    render(
      <ThisMachineBlock
        machine={thisMachine(notOwned, { identity: owned("owned"), at }, asOf)}
        now={at + 14 * 60_000}
      />,
    );
    expect(screen.getByRole("status")).toHaveTextContent(
      "Could not read this machine’s services. Its recovery actor has not answered for 14 minutes",
    );
    expect(screen.queryByText(/never dumps/)).toBeNull();
  });

  it("renders nothing for a machine that is not owned", () => {
    const { container } = render(
      <ThisMachineBlock machine={thisMachine(notOwned, null, asOf)} now={0} />,
    );
    expect(container).toBeEmptyDOMElement();
  });
});
