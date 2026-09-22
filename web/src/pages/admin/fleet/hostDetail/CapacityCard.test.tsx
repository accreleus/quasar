/**
 * The host-detail Capacity card's GPU rows (mock §A.5). Only the codec-chip
 * addition (#296/#302) is covered here — the gauge/bar behaviour for the rest
 * of the card has no prior test file to extend.
 */
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";

import type { GPUAvailability, Host } from "../../../../api/types";
import { CapacityCard } from "./CapacityCard";

const NOW = Date.parse("2026-08-29T12:00:00Z");

function host(over: Partial<Host> = {}): Host {
  return {
    id: "c2059601",
    node_name: "quasar-node-1",
    status: "online",
    agent_version: "0.1.0",
    cpu_cores: 16,
    cpu_model: "AMD Ryzen 9 9950X3D",
    mem_mb: 131072,
    capacity_detection: "ok",
    capacity_reason: null,
    readiness: [],
    readiness_reported_at: null,
    last_registered_at: "2026-08-01T00:00:00Z",
    last_heartbeat_at: new Date(NOW - 4000).toISOString(),
    storage: [{ label: "agent-data", path: "/var/lib/quasar", total_mb: 122880, available_mb: 98304 }],
    capacity: { slots_total: 3, slots_used: 2, vram_mb_total: 32768, vram_mb_used: 21504, active_sessions: 2, gpu_count: 1 },
    agent_connected_since: new Date(NOW - 90 * 60 * 1000).toISOString(),
    agent_restart_count: 0,
    agent_last_restart_at: null,
    ...over,
  } as Host;
}

function gpu(over: Partial<GPUAvailability> = {}): GPUAvailability {
  return {
    gpu_id: "g1",
    gpu_index: 0,
    vendor: "NVIDIA",
    model: "NVIDIA GeForce RTX 5090",
    vram_mb_total: 32768,
    vram_mb_reserved: 0,
    vram_mb_used: 21504,
    vram_mb_free: 11264,
    vram_sampled_at: new Date(NOW).toISOString(),
    slots_total: 3,
    slots_reserved: 2,
    active_sessions: 2,
    render_node: "/dev/dri/renderD128",
    codecs: ["av1", "h264", "h265"],
    ...over,
  } as GPUAvailability;
}

function renderCard(gpus: GPUAvailability[] | null) {
  return render(
    <MemoryRouter>
      <CapacityCard host={host()} gpus={gpus} now={NOW} />
    </MemoryRouter>,
  );
}

describe("CapacityCard — GPU codec chips (#302)", () => {
  it("shows a GPU's codecs, in fixed order, beside its slots", () => {
    renderCard([gpu()]);

    const row = screen.getByTestId("cap-row-gpu-g1");
    const chipLabels = within(row)
      .getAllByText(/^(H\.264|HEVC|AV1)$/)
      .map((el) => el.textContent);
    expect(chipLabels).toEqual(["H.264", "HEVC", "AV1"]);
  });

  it("shows a muted 'Not reported' chip for a GPU inheriting an unreported host set", () => {
    renderCard([gpu({ codecs: null })]);

    const row = screen.getByTestId("cap-row-gpu-g1");
    expect(within(row).getByText("Not reported")).toBeTruthy();
  });
});
