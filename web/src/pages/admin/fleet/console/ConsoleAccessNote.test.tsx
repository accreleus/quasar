import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { ConsoleAccessNote } from "./ConsoleAccessNote";
import type { ConsoleAccess } from "../../../../api/types";

const REQUEST_ID = "3f0812c4-6e1d-4f7a-9d55-8c1b0e2d44a2";
const UNREAD_SUMMARY = "The node agent has not yet read the recovery actor's console status.";

function note(access: Partial<ConsoleAccess>) {
  render(
    <ConsoleAccessNote
      access={{
        state: "applying", target: null, request_id: null, reason: null,
        started_at: null, finished_at: null, summary: UNREAD_SUMMARY, ...access,
      }}
      host={{ node_name: "host-a", engine: "docker", engine_mode: "rootful" }}
      liveSessions={0}
      onTryAgain={() => {}}
      tryAgainPending={false}
    />,
  );
}

describe("ConsoleAccessNote applying (#797)", () => {
  it("target null (no attempt yet): shows the agent's summary, not a replacement", () => {
    note({});
    expect(screen.getByText(UNREAD_SUMMARY, { exact: false })).toBeTruthy();
    expect(screen.queryByText(/Turning/)).toBeNull();
    expect(screen.queryByText(/replacing the node agent/)).toBeNull();
  });

  it("target null with an empty summary: still says the host is waiting", () => {
    note({ summary: "" });
    expect(screen.getByText(/Waiting to hear from the recovery actor/)).toBeTruthy();
    expect(screen.queryByText(/Turning/)).toBeNull();
  });

  it("target false with a request_id: turning off, unchanged", () => {
    note({ target: false, request_id: REQUEST_ID });
    expect(screen.getByText(/Turning off console mode/)).toBeTruthy();
    expect(screen.getByText(/replacing the node agent/)).toBeTruthy();
  });

  it("target true: turning on, unchanged", () => {
    note({ target: true, request_id: REQUEST_ID });
    expect(screen.getByText(/Turning on console mode/)).toBeTruthy();
  });
});
