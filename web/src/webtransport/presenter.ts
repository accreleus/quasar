// The WebGPU presenter (spec #1024): each decoded VideoFrame is imported as an
// external texture, with no copy, and drawn in the session view's scaling mode
// (the canvas's `data-scaling-mode`, as the <video> has it). Colour needs no code
// here: the import converts by the frame's own colour space, which the decoder
// takes from `hello_ack` `color_space` or the bitstream. Present telemetry is #1035.

/** What the runtime needs of a presenter; a test supplies a fake. */
export interface Presenter {
  /** Draws `frame` and closes it; false when nothing was drawn. */
  present(frame: VideoFrame): boolean;
  /** The device a PyroWave decoder shares, so its output is drawn with no copy. */
  readonly device?: GPUDevice;
  /** Draws a texture of `device` (the PyroWave decoder's rgba8unorm output). */
  presentTexture?(texture: GPUTexture): boolean;
  destroy(): void;
}

/** `texture_external` for the zero-copy import; `texture_2d<f32>` for the copy fallback. */
const shader = (texture: string) => /* wgsl */ `
@group(0) @binding(0) var s: sampler;
@group(0) @binding(1) var t: ${texture};
// uv scale (xy) and offset (zw): the part of the frame a cover-scaled picture shows.
@group(0) @binding(2) var<uniform> crop: vec4f;
struct V { @builtin(position) p: vec4f, @location(0) uv: vec2f };
@vertex fn vs(@builtin(vertex_index) i: u32) -> V {
  let xy = vec2f(f32((i << 1u) & 2u), f32(i & 2u));
  var o: V;
  o.p = vec4f(xy * 2.0 - 1.0, 0.0, 1.0);
  o.uv = vec2f(xy.x, 1.0 - xy.y) * crop.xy + crop.zw;
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
 * Where a frame is drawn and which part of it, for the session view's scaling modes
 * (`SCALING_MODES`): what `object-fit` does to the <video> of a WebRTC session. `integer`
 * is contain with nearest sampling there (`image-rendering: pixelated`), so it is here too;
 * an unknown mode is contain. Mouse scaling (mouseScale.ts) assumes exactly this.
 */
export function presentGeometry(
  mode: string | undefined,
  cw: number,
  ch: number,
  fw: number,
  fh: number,
): { rect: { x: number; y: number; w: number; h: number }; crop: [number, number, number, number] } {
  const full = { x: 0, y: 0, w: cw, h: ch };
  if (mode === "stretch") return { rect: full, crop: [1, 1, 0, 0] };
  if (mode === "cover") {
    const scale = Math.max(cw / fw, ch / fh);
    const [ux, uy] = [cw / (fw * scale), ch / (fh * scale)];
    return { rect: full, crop: [ux, uy, (1 - ux) / 2, (1 - uy) / 2] };
  }
  return { rect: containRect(cw, ch, fw, fh), crop: [1, 1, 0, 0] };
}

/**
 * Null when the browser has no WebGPU adapter. `onFatal` is called once when the device
 * is lost or reports an error nobody caught; the presenter draws nothing after that.
 */
export async function createWebGpuPresenter(
  canvas: HTMLCanvasElement,
  onFatal: (message: string) => void,
  opts: { pyrowave?: boolean } = {},
): Promise<Presenter | null> {
  const gpu = navigator.gpu as GPU | undefined;
  const adapter = await gpu?.requestAdapter();
  if (!gpu || !adapter) return null;
  // The PyroWave decoder's fast path wants these (pyroWaveDeviceFeatures in pyrowave/decoder.ts,
  // not imported: the decoder loads only for a PyroWave session).
  const requiredFeatures = opts.pyrowave ? (["subgroups", "shader-f16"] as const).filter((f) => adapter.features.has(f)) : [];
  const device = await adapter.requestDevice({ requiredFeatures });
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
  const linear = device.createSampler({ magFilter: "linear", minFilter: "linear" });
  let nearest: GPUSampler | null = null;
  const cropBuffer = device.createBuffer({ size: 16, usage: GPUBufferUsage.UNIFORM | GPUBufferUsage.COPY_DST });
  const cropData = new Float32Array(4);

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

  const draw = (pipeline: GPURenderPipeline, texture: GPUExternalTexture | GPUTextureView, fw: number, fh: number) => {
    // Read per frame: the HUD changes the mode mid-session, and it is one attribute read.
    const mode = canvas.dataset?.scalingMode;
    const { rect: r, crop } = presentGeometry(mode, canvas.width, canvas.height, fw, fh);
    cropData.set(crop);
    device.queue.writeBuffer(cropBuffer, 0, cropData);
    const sampler = mode === "integer" ? (nearest ??= device.createSampler({ magFilter: "nearest", minFilter: "nearest" })) : linear;
    const bindGroup = device.createBindGroup({
      layout: pipeline.getBindGroupLayout(0),
      entries: [
        { binding: 0, resource: sampler },
        { binding: 1, resource: texture },
        { binding: 2, resource: { buffer: cropBuffer } },
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
    pass.setPipeline(pipeline);
    pass.setBindGroup(0, bindGroup);
    pass.setViewport(r.x, r.y, r.w, r.h, 0, 1);
    pass.draw(3);
    pass.end();
    device.queue.submit([encoder.finish()]);
  };
  let texturePipeline: GPURenderPipeline | null = null;

  return {
    device,
    presentTexture(texture) {
      try {
        if (failed || destroyed) return false;
        texturePipeline ??= makePipeline("texture_2d<f32>");
        draw(texturePipeline, texture.createView(), texture.width, texture.height);
        return true;
      } catch (err) {
        fail(`the presenter could not draw: ${err instanceof Error ? err.message : String(err)}`);
        return false;
      }
    },
    present(frame) {
      try {
        if (failed || destroyed) return false;
        const [pipeline, texture] = source(frame);
        draw(pipeline, texture, frame.displayWidth, frame.displayHeight);
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
