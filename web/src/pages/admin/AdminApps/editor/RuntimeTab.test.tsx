// A preset-linked app stores managed_home=false, but the launch ORs the preset's in
// (control-api.md, runtime presets), so the editor must show what runs.

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RuntimeTab } from "./RuntimeTab";
import { draftFromApp } from "./appDraft";
import type { RuntimePreset } from "../../../../api/types";

function renderTab(presetHome: boolean, draftOver: Partial<ReturnType<typeof draftFromApp>> = {}) {
  const preset = {
    id: "preset-1",
    name: "KDE Desktop",
    managed_home: presetHome,
    home_container_path: "/home/kde",
    env: {},
  } as unknown as RuntimePreset;
  const draft = {
    ...draftFromApp(null),
    kind: "desktop" as const,
    runtimePresetId: "preset-1",
    managedHome: false,
    ...draftOver,
  };
  render(
    <MemoryRouter>
      <RuntimeTab
        draft={draft}
        onChange={() => {}}
        errors={{}}
        app={null}
        parent={null}
        presets={[preset]}
        storageProvider={null}
      />
    </MemoryRouter>,
  );
}

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

  it("shows the preset's home path when the app is still on the default path", () => {
    renderTab(true);
    expect((screen.getByLabelText("Mount path inside container") as HTMLInputElement).value).toBe("/home/kde");
  });

  it("keeps the app's own home path over the preset's", () => {
    renderTab(true, { containerPath: "/srv/own" });
    expect((screen.getByLabelText("Mount path inside container") as HTMLInputElement).value).toBe("/srv/own");
  });

  it("hides the Steam library switch for a library-provider desktop app", () => {
    renderTab(true, { libraryProvider: "steam" });
    expect(screen.queryByLabelText("Share the user's Steam library")).toBeNull();
  });
});
