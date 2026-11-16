// Runs in headless Chromium on WebGPU (`make test-webgpu`). The marker read off a texture made like
// the PyroWave decoder's output (rgba8unorm, header size) by a real copy and map (#1344).
import { describe, expect, it, vi } from "vitest";
import { BenchController } from "./benchController";
import { syntheticFrame } from "./syntheticMarker";

describe("BenchController.onTexture on a real device", () => {
  it("reads the marker back off the texture and measures it", async () => {
    const adapter = (await navigator.gpu.requestAdapter())!;
    const device = await adapter.requestDevice();
    const [W, H] = [1920, 1080];
    const texture = device.createTexture({
      size: [W, H],
      format: "rgba8unorm",
      usage: GPUTextureUsage.RENDER_ATTACHMENT | GPUTextureUsage.TEXTURE_BINDING | GPUTextureUsage.COPY_SRC | GPUTextureUsage.COPY_DST,
    });
    const now = 1_800_000_000_100;
    const ctl = new BenchController({ sendInput: () => {}, emitWindow: () => {}, getClockOffsetMs: () => 0, nowEpochMs: () => now });
    ctl.start();
    for (const frame_index of [41, 42]) {
      const image = syntheticFrame(
        { frame_index, host_time_ms: 1_800_000_000_000, scene_id: 1, load_level: 0, event_flags: 0, render_w: W, render_h: H },
        { width: W, height: H, ox: 0, oy: 0, cell: 16 },
      );
      device.queue.writeTexture({ texture }, image.data as Uint8ClampedArray<ArrayBuffer>, { bytesPerRow: W * 4 }, [W, H]);
      ctl.onTexture(device, texture);
    }
    await vi.waitFor(() => expect(ctl.handle().stats().frames).toBe(2));
    expect(ctl.handle().dump().frames.map((f) => [f.frame_index, f.g2g_ms])).toEqual([[41, 100], [42, 100]]);
    ctl.stop();
    texture.destroy();
    device.destroy();
  });
});
