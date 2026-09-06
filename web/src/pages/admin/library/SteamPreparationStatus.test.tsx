import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { SteamPreparationStatus as Status } from "../../../api/types";
import { SteamPreparationStatus } from "./SteamPreparationStatus";

const status = (over: Partial<Status> = {}): Status => ({
  eligible: true, supported: true, desired_enabled: true, desired_revision: "2", applied_revision: "2",
  policy_pending: false, preparation_enabled: true, consumption_enabled: true,
  state: "preparing", reason: "none", detail: "", template: null, clone_mode: null, clone_reason: null, reported_at: "2026-09-06T12:00:00Z", ...over,
});

describe("SteamPreparationStatus", () => {
  it("does not claim a prepared home from running preparation", () => {
    render(<SteamPreparationStatus status={status()} />);
    expect(screen.getByText("Preparing")).toBeInTheDocument();
    expect(screen.queryByText("Prepared")).not.toBeInTheDocument();
    expect(screen.queryByText(/Home cloning/)).not.toBeInTheDocument();
  });
  it("requires a reported template before claiming ready", () => {
    render(<SteamPreparationStatus status={status({ state: "ready" })} />);
    expect(screen.getByText("Not reported")).toBeInTheDocument();
    expect(screen.queryByText("Prepared")).not.toBeInTheDocument();
  });
  it("reports full copy as a measured fallback without calling preparation failed", () => {
    render(<SteamPreparationStatus status={status({ state: "ready", template: { version: "v1", registry_ref: "image@sha256:abc" }, clone_mode: "copy", clone_reason: "The mounted filesystem does not support reflinks." })} />);
    expect(screen.getByText("Prepared")).toBeInTheDocument();
    expect(screen.getByText(/Home cloning: full copy/)).toHaveTextContent("does not support reflinks");
    expect(screen.queryByText("Failed")).not.toBeInTheDocument();
  });
  it("does not present an old effective report as confirmation of a changed policy", () => {
    render(<SteamPreparationStatus status={status({ policy_pending: true, desired_enabled: false, applied_revision: "1", state: "ready", template: { version: "v1", registry_ref: "image@sha256:abc" }, clone_mode: "reflink" })} />);
    expect(screen.getByText("Applying setting")).toBeInTheDocument();
    expect(screen.getByText(/Steam setting: off/)).toHaveTextContent("not confirmed");
    expect(screen.queryByText("Prepared")).not.toBeInTheDocument();
    expect(screen.getByText(/Last observed home cloning: reflink/)).toBeInTheDocument();
  });
  it("keeps failure diagnosis actionable without promoting preparation to ready", () => {
    render(<SteamPreparationStatus status={status({ state: "failed", reason: "storage_unavailable", detail: "Template mount is read-only." })} />);
    expect(screen.getByText("Template mount is read-only.")).toBeInTheDocument();
    expect(screen.queryByText("Prepared")).not.toBeInTheDocument();
  });
  it("does not confirm an old report immediately after the source setting is saved", () => {
    render(<SteamPreparationStatus desiredEnabled={false} status={status({ state: "ready", template: { version: "v1", registry_ref: "image@sha256:abc" } })} />);
    expect(screen.getByText("Applying setting")).toBeInTheDocument();
    expect(screen.queryByText("Prepared")).not.toBeInTheDocument();
  });
  it("names legacy support instead of claiming the feature is disabled", () => {
    render(<SteamPreparationStatus status={status({ supported: false, state: "unknown", reason: "agent_upgrade_required", preparation_enabled: null, consumption_enabled: null })} />);
    expect(screen.getByText(/Upgrade this node agent/)).toBeInTheDocument();
    expect(screen.queryByText("Disabled")).not.toBeInTheDocument();
  });
  it("distinguishes an unsupported image from an administrator disabling Steam", () => {
    render(<SteamPreparationStatus status={status({ eligible: false, state: "unsupported", reason: "unsupported_image", preparation_enabled: false, consumption_enabled: false })} />);
    expect(screen.getByText("Not supported")).toBeInTheDocument();
    expect(screen.queryByText("Disabled")).not.toBeInTheDocument();
  });
  it("shows separate host production and consumption permissions", () => {
    render(<SteamPreparationStatus status={status({ preparation_enabled: false, reason: "host_warmup_disabled" })} />);
    expect(screen.getByText(/This host: preparation off; prepared homes on/)).toBeInTheDocument();
    expect(screen.getByText(/host has opted out of background preparation/)).toBeInTheDocument();
  });
});
