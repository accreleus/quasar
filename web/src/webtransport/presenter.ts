// The WebGPU presenter (spec #1024): each decoded VideoFrame is imported as an
// external texture, with no copy, and drawn contain-scaled. Scaling modes,
// colour and present telemetry are #1034 and #1035.

/** What the runtime needs of a presenter; a test supplies a fake. */
export interface Presenter {
  /** Draws `frame` and closes it; false when nothing was drawn. */
  present(frame: VideoFrame): boolean;
  destroy(): void;
}

/** `texture_external` for the zero-copy import; `texture_2d<f32>` for the copy fallback. */
const shader = (texture: string) => /* wgsl */ `
@group(0) @binding(0) var s: sampler;
@group(0) @binding(1) var t: ${texture};
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

/**
 * Null when the browser has no WebGPU adapter. `onFatal` is called once when the device
 * is lost or reports an error nobody caught; the presenter draws nothing after that.
 */
export async function createWebGpuPresenter(
  canvas: HTMLCanvasElement,
  onFatal: (message: string) => void,
): Promise<Presenter | null> {
  const gpu = navigator.gpu as GPU | undefined;
  const adapter = await gpu?.requestAdapter();
  if (!gpu || !adapter) return null;
  const device = await adapter.requestDevice();
  const context = canvas.getContext("webgpu");
  if (!context) return null;

  let failed = false;
  let destroyed = false;
  const fail = (message: string) => {
    if (failed || destroyed) return;
    failed = true;
    onFatal(message);
  };
  void device.lost.then((info) => fail(`the GPU device was lost (${info.reason}): ${info.message}`));
  device.addEventListener("uncapturederror", (e) => fail(`WebGPU error: ${(e as GPUUncapturedErrorEvent).error.message}`));

  const format = gpu.getPreferredCanvasFormat();
  // Transparent outside the picture: the canvas's CSS background is the letterbox.
  context.configure({ device, format, alphaMode: "premultiplied" });
  const makePipeline = (texture: string) => {
    const module = device.createShaderModule({ code: shader(texture) });
    return device.createRenderPipeline({
      layout: "auto",
      vertex: { module, entryPoint: "vs" },
      fragment: { module, entryPoint: "fs", targets: [{ format }] },
      primitive: { topology: "triangle-list" },
    });
  };
  const external = makePipeline("texture_external");
  // Set once an import fails (seen on Chrome's software adapter, SwiftShader): from then on
  // each frame is copied into `copy.target`. The zero-copy import stays the normal path.
  let copy: { pipeline: GPURenderPipeline; target: GPUTexture | null } | null = null;
  const sampler = device.createSampler({ magFilter: "linear", minFilter: "linear" });

  const resize = new ResizeObserver(([entry]) => {
    const dpr = window.devicePixelRatio || 1;
    canvas.width = Math.max(1, Math.round(entry.contentRect.width * dpr));
    canvas.height = Math.max(1, Math.round(entry.contentRect.height * dpr));
  });
  resize.observe(canvas);

  /** The frame as something the bind group takes, and the pipeline that samples it. */
  const source = (frame: VideoFrame): [GPURenderPipeline, GPUExternalTexture | GPUTextureView] => {
    if (!copy) {
      try {
        return [external, device.importExternalTexture({ source: frame })];
      } catch (err) {
        console.warn("presenter: importExternalTexture failed; copying each frame instead", err);
        copy = { pipeline: makePipeline("texture_2d<f32>"), target: null };
      }
    }
    const [w, h] = [frame.displayWidth, frame.displayHeight];
    if (!copy.target || copy.target.width !== w || copy.target.height !== h) {
      copy.target?.destroy();
      copy.target = device.createTexture({
        size: [w, h],
        format: "rgba8unorm",
        usage: GPUTextureUsage.TEXTURE_BINDING | GPUTextureUsage.COPY_DST | GPUTextureUsage.RENDER_ATTACHMENT,
      });
    }
    device.queue.copyExternalImageToTexture({ source: frame }, { texture: copy.target }, [w, h]);
    return [copy.pipeline, copy.target.createView()];
  };

  return {
    present(frame) {
      try {
        if (failed || destroyed) return false;
        const [pipeline, texture] = source(frame);
        const bindGroup = device.createBindGroup({
          layout: pipeline.getBindGroupLayout(0),
          entries: [
            { binding: 0, resource: sampler },
            { binding: 1, resource: texture },
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
        return true;
      } catch (err) {
        fail(`the presenter could not draw: ${err instanceof Error ? err.message : String(err)}`);
        return false;
      } finally {
        // Submitted work keeps what it imported; the frame goes back to the decoder now.
        frame.close();
      }
    },
    destroy() {
      destroyed = true;
      resize.disconnect();
      device.destroy();
    },
  };
}
