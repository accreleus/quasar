// RuntimePresetDrawer — first-run-experience §S2's network field.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, type RenderResult } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RuntimePresetDrawer } from "./RuntimePresetDrawer";
import * as adminApi from "../../../api/admin";
import type { RuntimePreset } from "../../../api/types";
import type { ReactElement } from "react";

vi.mock("../../../api/admin");

const mocked = vi.mocked(adminApi);

// The drawer's "Used by" chips navigate via useNavigate() — every render
// needs a Router ancestor even when a test never clicks one.
function renderDrawer(el: ReactElement): RenderResult {
  return render(<MemoryRouter>{el}</MemoryRouter>);
}

function preset(over: Partial<RuntimePreset> = {}): RuntimePreset {
  return {
    id: "p1",
    name: "Steam preset",
    description: "",
    image: "quasar-steam:latest",
    args: [],
    env: {},
    mounts: [],
    managed_home: true,
    home_container_path: "/home/quasar",
    network: "",
    used_by: [],
    created_at: "2026-08-01T00:00:00Z",
    updated_at: "2026-08-01T00:00:00Z",
    ...over,
  } as RuntimePreset;
}

describe("RuntimePresetDrawer — network field", () => {
  beforeEach(() => {
    mocked.createRuntimePreset.mockReset();
    mocked.updateRuntimePreset.mockReset();
  });

  it("defaults a new preset's network select to Inherit host default", () => {
    renderDrawer(
      <RuntimePresetDrawer
        preset={null}
        token="tok"
        onClose={vi.fn()}
        onSaved={vi.fn()}
        onRequestDelete={vi.fn()}
      />,
    );
    expect(screen.getByLabelText("Network")).toHaveValue("");
  });

  it("initializes the select from an existing preset's stored network", () => {
    renderDrawer(
      <RuntimePresetDrawer
        preset={preset({ network: "bridge" })}
        token="tok"
        onClose={vi.fn()}
        onSaved={vi.fn()}
        onRequestDelete={vi.fn()}
      />,
    );
    expect(screen.getByLabelText("Network")).toHaveValue("bridge");
  });

  // Alice PR #464 round 2: host networking is operator-only
  // (QUASAR_CONTAINER_NETWORK), never app/preset-selectable — the select
  // offers only Inherit/None/Bridge.
  it("does not offer a Host option", () => {
    renderDrawer(
      <RuntimePresetDrawer
        preset={preset({ network: "bridge" })}
        token="tok"
        onClose={vi.fn()}
        onSaved={vi.fn()}
        onRequestDelete={vi.fn()}
      />,
    );
    const select = screen.getByLabelText("Network") as HTMLSelectElement;
    const optionValues = Array.from(select.options).map((o) => o.value);
    expect(optionValues).toEqual(["", "none", "bridge"]);
    expect(screen.queryByRole("option", { name: "Host" })).not.toBeInTheDocument();
  });

  // A preset carrying the no-longer-selectable "host" value (only reachable
  // today via a direct DB edit predating this tightening, since the write
  // path never offered it even before this round) must not render an
  // orphaned/blank selection — it falls back to Inherit rather than silently
  // matching nothing.
  it("falls back to Inherit host default when a preset carries the removed host value", () => {
    renderDrawer(
      <RuntimePresetDrawer
        preset={preset({ network: "host" as RuntimePreset["network"] })}
        token="tok"
        onClose={vi.fn()}
        onSaved={vi.fn()}
        onRequestDelete={vi.fn()}
      />,
    );
    expect(screen.getByLabelText("Network")).toHaveValue("");
  });

  it("round-trips a changed network value through create", () => {
    mocked.createRuntimePreset.mockResolvedValue({ runtime_preset: preset({ network: "bridge" }) });
    const onSaved = vi.fn();
    renderDrawer(
      <RuntimePresetDrawer
        preset={null}
        token="tok"
        onClose={vi.fn()}
        onSaved={onSaved}
        onRequestDelete={vi.fn()}
      />,
    );
    // The "Name" TextField's <span> label isn't a real <label for=...> (Field
    // wrapper, TextField.tsx) so it isn't reachable via getByLabelText —
    // target its id directly (TextField derives id="name" from label="Name").
    fireEvent.change(document.getElementById("name") as HTMLInputElement, {
      target: { value: "Steam preset" },
    });
    fireEvent.change(screen.getByLabelText("Network"), { target: { value: "bridge" } });
    fireEvent.click(screen.getByRole("button", { name: /save changes/i }));

    expect(mocked.createRuntimePreset).toHaveBeenCalledWith(
      "tok",
      expect.objectContaining({ network: "bridge" }),
    );
  });

  it("round-trips a changed network value through update, including back to inherit", () => {
    mocked.updateRuntimePreset.mockResolvedValue({ runtime_preset: preset({ network: "" }) });
    renderDrawer(
      <RuntimePresetDrawer
        preset={preset({ network: "bridge" })}
        token="tok"
        onClose={vi.fn()}
        onSaved={vi.fn()}
        onRequestDelete={vi.fn()}
      />,
    );
    fireEvent.change(screen.getByLabelText("Network"), { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: /save changes/i }));

    expect(mocked.updateRuntimePreset).toHaveBeenCalledWith(
      "tok",
      "p1",
      expect.objectContaining({ network: "" }),
    );
  });
});
