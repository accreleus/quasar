// Releases ▸ Installed keeps the last report of this machine it saw, so a recovery actor
// that stops answering (the identity then reads null) shows that report with its time,
// not a machine that was never owned.

import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { PlatformIdentity, PlatformReleaseView } from "../../../api/types";
import { clockTime } from "../../../lib/format/clockTime";
import { InstalledCard } from "./ReleasesTab";

const commit = "3f9a2c1000000000000000000000000000000000";
const binary = {
  version: "0.5.2",
  source_commit: commit,
  built_at: "2026-09-24T16:40:00Z",
  schema_version: 88,
  machine_role: "control_only",
  machine_node_name: "attic-server",
} as const;

const reported: PlatformIdentity = {
  ...binary,
  install_mode: "owned",
  recovery_actor_version: "0.5.2",
  recovery_actor_source_commit: commit,
  seed_version: "0.5.0",
  database_mode: "external",
};

const silent: PlatformIdentity = {
  ...binary,
  install_mode: null,
  recovery_actor_version: null,
  recovery_actor_source_commit: null,
  seed_version: null,
  database_mode: null,
};

const view = (cp: PlatformIdentity) =>
  ({
    source_repo: "accreleus/quasar",
    last_error: null,
    installed: { control_plane: cp, hosts: [] },
  }) as unknown as PlatformReleaseView;

describe("InstalledCard", () => {
  it("shows the last good report, with its time, once the actor stops answering", () => {
    const first = Date.parse("2026-09-25T13:48:02Z");
    const { rerender } = render(<InstalledCard view={view(reported)} updatedAt={first} />);
    expect(screen.queryByRole("status")).toBeNull();
    expect(screen.getByText("Your own")).toBeInTheDocument();

    rerender(<InstalledCard view={view(silent)} updatedAt={first + 14 * 60_000} />);
    expect(screen.getByRole("status")).toHaveTextContent("Could not read this machine’s services.");
    const asOf = clockTime(new Date(first).toISOString(), { seconds: false });
    expect(screen.getByText(`You · as of ${asOf}`)).toBeInTheDocument();
    expect(screen.getByText("Your own")).toBeInTheDocument();
  });

  it("says not reported yet when the actor never answered on this page", () => {
    render(<InstalledCard view={view(silent)} updatedAt={Date.parse("2026-09-25T13:48:02Z")} />);
    expect(screen.getByRole("status")).toHaveTextContent(
      "This machine’s recovery actor has not reported its services yet.",
    );
    expect(screen.queryByText("Your own")).toBeNull();
  });
});
