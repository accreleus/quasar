// The WebGPU presenter (spec #1024): each decoded VideoFrame is imported as an
// external texture, with no copy, and drawn contain-scaled. Scaling modes,
// colour and present telemetry are #1034 and #1035.

/** What the runtime needs of a presenter; a test supplies a fake. */
export interface Presenter {
  /** Draws `frame` and closes it. */
  present(frame: VideoFrame): void;
  destroy(): void;
}

// The slice of WebGPU used here. TypeScript's DOM lib has no WebGPU types yet.
interface Gpu {
  requestAdapter(): Promise<{ requestDevice(): Promise<GpuDevice> } | null>;
  getPreferredCanvasFormat(): string;
}
interface GpuDevice {
  queue: { submit(buffers: unknown[]): void };
  createShaderModule(d: { code: string }): unknown;
  createRenderPipeline(d: unknown): { getBindGroupLayout(i: number): unknown };
  createSampler(d: unknown): unknown;
  importExternalTexture(d: { source: VideoFrame }): unknown;
  createBindGroup(d: unknown): unknown;
  createCommandEncoder(): {
    beginRenderPass(d: unknown): {
      setPipeline(p: unknown): void;
      setBindGroup(i: number, g: unknown): void;
      setViewport(x: number, y: number, w: number, h: number, min: number, max: number): void;
      draw(n: number): void;
      end(): void;
    };
    finish(): unknown;
  };
  destroy(): void;
}
interface GpuCanvasContext {
  configure(d: { device: GpuDevice; format: string; alphaMode: string }): void;
  getCurrentTexture(): { createView(): unknown };
}

const SHADER = /* wgsl */ `
@group(0) @binding(0) var s: sampler;
@group(0) @binding(1) var t: texture_external;
struct V { @builtin(position) p: vec4f, @location(0) uv: vec2f };
@vertex fn vs(@builtin(vertex_index) i: u32) -> V {
  let xy = vec2f(f32((i << 1u) & 2u), f32(i & 2u));
  var o: V;
  o.p = vec4f(xy * 2.0 - 1.0, 0.0, 1.0);
  o.uv = vec2f(xy.x, 1.0 - xy.y);
  return o;
}
@fragment fn fs(v: V) -> @location(0) vec4f {
  return textureSampleBaseClampToEdge(t, s, v.uv);
}`;

/** The largest rect of the frame's aspect that fits the canvas, centred. */
export function containRect(
  cw: number,
  ch: number,
  fw: number,
  fh: number,
): { x: number; y: number; w: number; h: number } {
  const scale = Math.min(cw / fw, ch / fh);
  const w = Math.round(fw * scale);
  const h = Math.round(fh * scale);
  return { x: Math.floor((cw - w) / 2), y: Math.floor((ch - h) / 2), w, h };
}

/** Null when the browser has no WebGPU adapter. */
export async function createWebGpuPresenter(canvas: HTMLCanvasElement): Promise<Presenter | null> {
  const gpu = (navigator as Navigator & { gpu?: Gpu }).gpu;
  const adapter = await gpu?.requestAdapter();
  if (!gpu || !adapter) return null;
  const device = await adapter.requestDevice();
  const context = canvas.getContext("webgpu") as unknown as GpuCanvasContext | null;
  if (!context) return null;
  const format = gpu.getPreferredCanvasFormat();
  // Transparent outside the picture: the canvas's CSS background is the letterbox.
  context.configure({ device, format, alphaMode: "premultiplied" });
  const module = device.createShaderModule({ code: SHADER });
  const pipeline = device.createRenderPipeline({
    layout: "auto",
    vertex: { module, entryPoint: "vs" },
    fragment: { module, entryPoint: "fs", targets: [{ format }] },
    primitive: { topology: "triangle-list" },
  });
  const sampler = device.createSampler({ magFilter: "linear", minFilter: "linear" });

  const resize = new ResizeObserver(([entry]) => {
    const dpr = window.devicePixelRatio || 1;
    canvas.width = Math.max(1, Math.round(entry.contentRect.width * dpr));
    canvas.height = Math.max(1, Math.round(entry.contentRect.height * dpr));
  });
  resize.observe(canvas);

  return {
    present(frame) {
      try {
        const bindGroup = device.createBindGroup({
          layout: pipeline.getBindGroupLayout(0),
          entries: [
            { binding: 0, resource: sampler },
            { binding: 1, resource: device.importExternalTexture({ source: frame }) },
          ],
        });
        const encoder = device.createCommandEncoder();
        const pass = encoder.beginRenderPass({
          colorAttachments: [
            {
              view: context.getCurrentTexture().createView(),
              clearValue: { r: 0, g: 0, b: 0, a: 0 },
              loadOp: "clear",
              storeOp: "store",
            },
          ],
        });
        const r = containRect(canvas.width, canvas.height, frame.displayWidth, frame.displayHeight);
        pass.setPipeline(pipeline);
        pass.setBindGroup(0, bindGroup);
        pass.setViewport(r.x, r.y, r.w, r.h, 0, 1);
        pass.draw(3);
        pass.end();
        device.queue.submit([encoder.finish()]);
      } finally {
        // Submitted work keeps what it imported; the frame goes back to the decoder now.
        frame.close();
      }
    },
    destroy() {
      resize.disconnect();
      device.destroy();
    },
  };
}
