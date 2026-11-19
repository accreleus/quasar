// A preset-linked app stores managed_home=false, but the launch ORs the preset's in
// (control-api.md, runtime presets), so the editor must show what runs.

import { describe, expect, it } from "vitest";
import { useState } from "react";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RuntimeTab } from "./RuntimeTab";
import { draftFromApp } from "./appDraft";
import type { RuntimePreset } from "../../../../api/types";

type Draft = ReturnType<typeof draftFromApp>;

function renderTab(presetHome: boolean, draftOver: Partial<Draft> = {}, presetPath = "/home/kde") {
  const preset = {
    id: "preset-1",
    name: "KDE Desktop",
    managed_home: presetHome,
    home_container_path: presetPath,
    env: {},
  } as unknown as RuntimePreset;
  const initial = {
    ...draftFromApp(null),
    kind: "desktop" as const,
    runtimePresetId: "preset-1",
    managedHome: false,
    ...draftOver,
  };
  // Stateful, so an edit round-trips through the draft like the page does.
  function Harness() {
    const [draft, setDraft] = useState<Draft>(initial);
    return (
      <RuntimeTab
        draft={draft}
        onChange={setDraft}
        errors={{}}
        app={null}
        parent={null}
        presets={[preset]}
        storageProvider={null}
      />
    );
  }
  render(
    <MemoryRouter>
      <Harness />
    </MemoryRouter>,
  );
}

const pathField = () => screen.getByLabelText("Mount path inside container") as HTMLInputElement;

describe("RuntimeTab managed home", () => {
  it("shows the preset's managed home as on and fixed, and offers the Steam library switch", () => {
    renderTab(true);
    const home = screen.getByLabelText("Managed home") as HTMLInputElement;
    expect(home.checked).toBe(true);
    expect(home.disabled).toBe(true);
    expect(screen.getByLabelText("Share the user's Steam library")).toBeTruthy();
  });

  it("leaves it off and editable when the preset has none", () => {
    renderTab(false);
    const home = screen.getByLabelText("Managed home") as HTMLInputElement;
    expect(home.checked).toBe(false);
    expect(home.disabled).toBe(false);
    expect(screen.queryByLabelText("Share the user's Steam library")).toBeNull();
  });

  it("names the preset's home path in the hint and keeps the field on what the app stores", () => {
    renderTab(true);
    expect(pathField().value).toBe("/home/quasar");
    expect(screen.getByText(/The runtime preset mounts the home at \/home\/kde/)).toBeTruthy();
  });

  it("follows the preset's path even when the preset's own managed home is off", () => {
    renderTab(false, { managedHome: true });
    expect(screen.getByText(/The runtime preset mounts the home at \/home\/kde/)).toBeTruthy();
  });

  it("shows no preset hint when the app has its own path", () => {
    renderTab(true, { containerPath: "/srv/own" });
    expect(pathField().value).toBe("/srv/own");
    expect(screen.queryByText(/The runtime preset mounts/)).toBeNull();
  });

  it("shows exactly what the admin types, including an empty field", () => {
    renderTab(true);
    fireEvent.change(pathField(), { target: { value: "" } });
    expect(pathField().value).toBe("");
    expect(screen.getByText(/while this field is empty or \/home\/quasar\./)).toBeTruthy();
    fireEvent.change(pathField(), { target: { value: "/srv/own" } });
    expect(pathField().value).toBe("/srv/own");
  });

  it("hides the Steam library switch for a library-provider desktop app", () => {
    renderTab(true, { libraryProvider: "steam" });
    expect(screen.queryByLabelText("Share the user's Steam library")).toBeNull();
  });

  it("keeps the switch on a library-provider app that already shares, so it can be turned off", () => {
    renderTab(true, { libraryProvider: "steam", shareSteamLibrary: true });
    expect(screen.getByLabelText("Share the user's Steam library")).toBeTruthy();
  });
});

describe("RuntimeTab container flags (#179)", () => {
  let latest: Draft;
  function renderFlags(draftOver: Partial<Draft> = {}) {
    function Harness() {
      const [draft, setDraft] = useState<Draft>({ ...draftFromApp(null), ...draftOver });
      latest = draft;
      return (
        <RuntimeTab
          draft={draft}
          onChange={setDraft}
          errors={{}}
          app={null}
          parent={null}
          presets={[]}
          storageProvider={null}
        />
      );
    }
    render(
      <MemoryRouter>
        <Harness />
      </MemoryRouter>,
    );
  }
  const flag = (name: string) => screen.getByLabelText(name) as HTMLInputElement;

  it("offers the three flags with their defaults on an app with no preset", () => {
    renderFlags();
    expect(flag("GPU access").checked).toBe(false);
    expect(flag("Block privilege escalation").checked).toBe(true);
    expect(flag("Unmask /proc and /sys").checked).toBe(false);
  });

  it("writes each flag to the draft, and drops a key that goes back to its default", () => {
    renderFlags();
    fireEvent.click(flag("GPU access"));
    expect(latest.gpu).toBe(true);
    fireEvent.click(flag("Block privilege escalation"));
    expect(latest.specExtras).toEqual({ no_new_privileges: false });
    fireEvent.click(flag("Unmask /proc and /sys"));
    expect(latest.specExtras).toEqual({ no_new_privileges: false, systempaths_unconfined: true });
    fireEvent.click(flag("Block privilege escalation"));
    expect(latest.specExtras).toEqual({ systempaths_unconfined: true });
  });

  it("shows a stored opt-out as it is", () => {
    renderFlags({ gpu: true, specExtras: { no_new_privileges: false } });
    expect(flag("GPU access").checked).toBe(true);
    expect(flag("Block privilege escalation").checked).toBe(false);
  });

  it("is absent when a runtime preset supplies the runtime", () => {
    renderTab(false);
    expect(screen.queryByLabelText("GPU access")).toBeNull();
    expect(screen.queryByLabelText("Block privilege escalation")).toBeNull();
  });
});
