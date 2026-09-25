import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Host, PlatformHostIdentity, PlatformIdentity } from "../../../api/types";
import { ServicesCard } from "./hostDetail/ServicesCard";
import { hostServices, isControlPlaneMachine } from "./hostServices";
import { ThisMachineBlock } from "./ThisMachine";
import { thisMachine } from "./thisMachine";

const binary: PlatformIdentity = {
  version: "0.5.2",
  source_commit: "3f9a2c1000000000000000000000000000000000",
  built_at: "2026-09-24T16:40:00Z",
  schema_version: 88,
};

const shape = (machine_role: "combined" | "control_only", machine_node_name: string) => ({
  machine_role,
  machine_node_name,
});

const owned = (
  database_mode: "owned" | "external",
  role: "combined" | "control_only" = "control_only",
  name = "attic-server",
): PlatformIdentity => ({
  ...binary,
  ...shape(role, name),
  install_mode: "owned",
  recovery_actor_version: "0.5.2",
  recovery_actor_source_commit: "3f9a2c1000000000000000000000000000000000",
  seed_version: "0.5.0",
  database_mode,
});

/** The actor has not answered: only the configured shape is known. */
const silent = (role: "combined" | "control_only", name: string): PlatformIdentity => ({
  ...binary,
  ...shape(role, name),
  install_mode: null,
  recovery_actor_version: null,
  recovery_actor_source_commit: null,
  seed_version: null,
  database_mode: null,
});

const notOwned: PlatformIdentity = {
  ...binary,
  install_mode: null,
  recovery_actor_version: null,
  recovery_actor_source_commit: null,
  seed_version: null,
  database_mode: null,
  machine_role: null,
  machine_node_name: null,
};

const hostId = (node_name: string, agent_version = "0.5.2"): PlatformHostIdentity =>
  ({
    host_id: "h1",
    node_name,
    status: "online",
    agent_version,
    source_commit: null,
    built_at: null,
    install_mode: "owned",
    updater_present: true,
    identity_known: true,
  }) as PlatformHostIdentity;

const asOf = () => "13:48";

describe("thisMachine", () => {
  it("draws nothing for a machine that is not owned, or an older server", () => {
    expect(thisMachine(notOwned, null, [], asOf).kind).toBe("none");
    expect(thisMachine(binary, null, [], asOf).kind).toBe("none");
  });

  it("a control-only machine: its name and shape, and no agent", () => {
    const m = thisMachine(owned("owned"), null, [hostId("attic-server")], asOf);
    if (m.kind === "none") throw new Error("none");
    expect(m.kind).toBe("reported");
    expect(m.title).toBe("attic-server · Control-only host");
    expect(m.rows.map((r) => [r.label, r.value, r.hint])).toEqual([
      ["Seed", "v0.5.0", "External manager · running"],
      ["Recovery actor", "v0.5.2", "Quasar · running"],
      ["Database", "Quasar’s own", "Quasar · running"],
      ["Control plane", "v0.5.2", "Quasar · running"],
      // Never matched by name on control-only, where a GPU host may share it.
      ["Node agent", null, "none on this machine"],
    ]);
  });

  it("a combined machine: its own agent, by node name", () => {
    const m = thisMachine(
      owned("owned", "combined", "living-room-pc"),
      null,
      [hostId("gpu-host-2", "0.4.0"), hostId("living-room-pc", "0.5.2")],
      asOf,
    );
    if (m.kind === "none") throw new Error("none");
    expect(m.title).toBe("living-room-pc · Combined host");
    expect(m.rows[4]).toMatchObject({ value: "v0.5.2", hint: "Quasar · running" });
  });

  it("names the operator's own database as theirs, reachable", () => {
    const m = thisMachine(owned("external"), null, [], asOf);
    if (m.kind === "none") throw new Error(m.kind);
    expect(m.external).toBe(true);
    expect(m.rows[2]).toMatchObject({ value: "Your own", hint: "You · reachable" });
  });

  it("not reported yet: the configured shape is named, the actor's rows wait", () => {
    const m = thisMachine(silent("control_only", "attic-server"), null, [], asOf);
    if (m.kind === "none") throw new Error("none");
    expect(m.kind).toBe("not_reported");
    expect(m.title).toBe("attic-server · Control-only host");
    expect(m.rows.slice(0, 3).map((r) => [r.value, r.hint])).toEqual([
      [null, "not reported yet"],
      [null, "not reported yet"],
      [null, "not reported yet"],
    ]);
    expect(m.rows[3]).toMatchObject({ value: "v0.5.2", hint: "Quasar · running" });
  });

  it("keeps the last report, with its time, once the actor stops answering", () => {
    const at = Date.parse("2026-09-25T13:48:02Z");
    const m = thisMachine(
      silent("control_only", "attic-server"),
      { identity: owned("external"), at },
      [],
      asOf,
    );
    if (m.kind === "none") throw new Error("none");
    expect(m.kind).toBe("not_answering");
    expect(m.since).toBe(at);
    expect(m.rows.map((r) => r.hint)).toEqual([
      "External manager · as of 13:48",
      "Quasar · as of 13:48",
      "You · as of 13:48",
      "Quasar · running",
      "none on this machine",
    ]);
  });
});

describe("ThisMachineBlock", () => {
  it("renders the rows and the operator's-database note", () => {
    render(<ThisMachineBlock machine={thisMachine(owned("external"), null, [], asOf)} now={0} />);
    expect(screen.getByText("This machine")).toBeInTheDocument();
    expect(screen.getByText("attic-server · Control-only host")).toBeInTheDocument();
    expect(screen.getByText("Your own")).toBeInTheDocument();
    expect(screen.getByText("none on this machine")).toBeInTheDocument();
    expect(screen.getByText(/never dumps, restores or upgrades it/)).toBeInTheDocument();
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("says the actor has not reported yet", () => {
    render(
      <ThisMachineBlock
        machine={thisMachine(silent("control_only", "attic-server"), null, [], asOf)}
        now={0}
      />,
    );
    expect(screen.getByRole("status")).toHaveTextContent(
      "This machine’s recovery actor has not reported its services yet.",
    );
  });

  it("says how long the recovery actor has not answered", () => {
    const at = Date.parse("2026-09-25T13:48:02Z");
    render(
      <ThisMachineBlock
        machine={thisMachine(
          silent("control_only", "attic-server"),
          { identity: owned("owned"), at },
          [],
          asOf,
        )}
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
      <ThisMachineBlock machine={thisMachine(notOwned, null, [], asOf)} now={0} />,
    );
    expect(container).toBeEmptyDOMElement();
  });
});

describe("the combined host's own page", () => {
  const host = {
    id: "5a1c90e2",
    node_name: "living-room-pc",
    status: "online",
    install_mode: "owned",
    updater_present: true,
    agent_version: "0.5.2",
    recovery_actor_version: "0.5.2",
    recovery_actor_source_commit: null,
    seed_version: "0.5.0",
    last_registered_at: "2026-09-25T13:48:02Z",
  } as unknown as Host;

  it("is identified only on combined, by node name", () => {
    expect(isControlPlaneMachine(host, owned("owned", "combined", "living-room-pc"))).toBe(true);
    expect(isControlPlaneMachine(host, owned("owned", "control_only", "living-room-pc"))).toBe(
      false,
    );
    expect(isControlPlaneMachine(host, owned("owned", "combined", "other"))).toBe(false);
    expect(isControlPlaneMachine(host, { ...notOwned, machine_role: "sideways" as never })).toBe(
      false,
    );
  });

  it("shows the control plane and database running here, and the footnote", () => {
    const services = hostServices(host, {
      agentOlder: false,
      machine: silent("combined", "living-room-pc"),
    })!;
    expect(services.shape).toBe("Combined host");
    render(<ServicesCard nodeName="living-room-pc" services={services} connectedSince={null} now={0} />);
    expect(screen.getByText("Combined host")).toBeInTheDocument();
    expect(screen.getByText("Accounts, the console, scheduling and signaling.")).toBeInTheDocument();
    expect(screen.getByText(/schema 88/)).toBeInTheDocument();
    expect(screen.queryByText("Runs on another machine.")).toBeNull();
    expect(screen.queryByText("No database runs on a GPU host.")).toBeNull();
    expect(screen.getByText(/so it is not removed from here/)).toBeInTheDocument();
  });

  it("a GPU host sharing the name of a control-only machine stays a GPU host", () => {
    const services = hostServices(host, {
      agentOlder: false,
      machine: owned("owned", "control_only", "living-room-pc"),
    })!;
    expect(services.shape).toBe("GPU host");
    expect(services.rows.find((r) => r.key === "control_plane")?.description).toBe(
      "Runs on another machine.",
    );
  });
});
