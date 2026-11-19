// A preset-linked app stores managed_home=false, but the launch ORs the preset's in
// (control-api.md, runtime presets), so the editor must show what runs.

import { describe, expect, it } from "vitest";
import { useState } from "react";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RuntimeTab } from "./RuntimeTab";
import { draftFromApp } from "./appDraft";
import type { AdminApp, RuntimePreset } from "../../../../api/types";

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

// A derived tile has no runtime of its own, so the editor must not derive its
// managed home client-side from the parent: it reads effective_managed_home /
// effective_home_container_path off the app the API returned (amendment 82).
describe("RuntimeTab derived tile", () => {
  function renderDerived(opts: { parent: Partial<AdminApp>; app: Partial<AdminApp> }) {
    const preset = {
      id: "preset-1",
      name: "KDE Desktop",
      managed_home: true,
      home_container_path: "/home/kde",
      env: {},
    } as unknown as RuntimePreset;
    function Harness() {
      const [draft, setDraft] = useState<Draft>(draftFromApp(null));
      return (
        <RuntimeTab
          draft={draft}
          onChange={setDraft}
          errors={{}}
          app={{ id: "app-1", ...opts.app } as unknown as AdminApp}
          parent={{ id: "parent-1", ...opts.parent } as unknown as AdminApp}
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

  it("shows the app's effective managed home and its path for a derived tile", () => {
    renderDerived({
      parent: {
        id: "parent-1",
        name: "KDE Desktop",
        runtime_spec: {},
        runtime_preset_id: "preset-1",
      },
      app: {
        id: "app-1",
        runtime_spec: {},
        effective_managed_home: true,
        effective_home_container_path: "/home/kde",
      },
    });
    expect(screen.getByText("Managed home")).toBeInTheDocument();
    expect(screen.getByText(/\/home\/kde/)).toBeInTheDocument();
  });

  it("reads the home from the API, not a client-side merge of the parent", () => {
    renderDerived({
      parent: {
        id: "parent-1",
        name: "KDE Desktop",
        runtime_spec: {},
        runtime_preset_id: "preset-1",
      },
      app: {
        id: "app-1",
        runtime_spec: {},
        // The parent's preset provides no managed home, so a client-side merge
        // of parent + preset would show none; the API says otherwise.
        effective_managed_home: true,
        effective_home_container_path: "/srv/effective",
      },
    });
    expect(screen.getByText("/srv/effective")).toBeInTheDocument();
  });

  it("shows None when the app's effective managed home is off", () => {
    renderDerived({
      parent: {
        id: "parent-1",
        name: "KDE Desktop",
        runtime_spec: {},
        runtime_preset_id: "preset-1",
      },
      app: {
        id: "app-1",
        runtime_spec: {},
        effective_managed_home: false,
      },
    });
    expect(screen.getByText("Managed home")).toBeInTheDocument();
    expect(screen.getByText("None")).toBeInTheDocument();
  });
});
