// The WebGPU presenter (spec #1024): each decoded VideoFrame is imported as an
// external texture, with no copy, and drawn in the session view's scaling mode
// (the canvas's `data-scaling-mode`, as the <video> has it). Colour needs no code
// here: the import converts by the frame's own colour space, which the decoder
// takes from `hello_ack` `color_space` or the bitstream. Present telemetry is #1035.
//
// When the frame is smaller than the rect it is drawn into, the draw takes an
// upscale + sharpen pass instead of the plain bilinear one (#1349): the frame is
// reconstructed edge-aware and then sharpened contrast-adaptively, so a stream the
// ladder stepped down looks close to the size it is shown at. `data-upscale-sharpen`
// = "off" on the canvas turns it off (the HUD's switch); 1:1, a downscale and the
// pixel-exact `integer` mode keep the plain path whatever the setting says.

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
const shader = (texture: string, upscaleSharpen = false) => /* wgsl */ `
@group(0) @binding(0) var s: sampler;
@group(0) @binding(1) var t: ${texture};
// uv scale (xy) and offset (zw): the part of the frame a cover-scaled picture shows.
@group(0) @binding(2) var<uniform> crop: vec4f;
${
  upscaleSharpen
    ? `// One source texel in uv (xy), the reconstruction's edge gain (z) and the sharpen
// strength (w), for the upscale pass.
@group(0) @binding(3) var<uniform> texel: vec4f;`
    : ""
}
struct V { @builtin(position) p: vec4f, @location(0) uv: vec2f };
@vertex fn vs(@builtin(vertex_index) i: u32) -> V {
  let xy = vec2f(f32((i << 1u) & 2u), f32(i & 2u));
  var o: V;
  o.p = vec4f(xy * 2.0 - 1.0, 0.0, 1.0);
  o.uv = vec2f(xy.x, 1.0 - xy.y) * crop.xy + crop.zw;
  return o;
}
${
  upscaleSharpen
    ? `
fn tap(uv: vec2f) -> vec4f { return textureSampleBaseClampToEdge(t, s, uv); }
fn luma(c: vec3f) -> f32 { return dot(c, vec3f(0.2126, 0.7152, 0.0722)); }

// The upscale + sharpen pass (#1349), drawn only when the frame is smaller than the
// rect it is drawn into, so every output pixel sits between source texels. Four filtered
// taps — the quad the bilinear tap would have blended — in the class of AMD's FSR 1 and
// CAS, written from the published descriptions of those algorithms rather than ported
// from either implementation:
//
//  - Edge-aware upscale: bilinear blends the two sides of an edge into a one-texel ramp,
//    which is the whole reason a stepped-down stream looks soft. This narrows the blend
//    weights toward the nearer texel, so an edge comes back tight instead of ramped. The
//    weights stay in [0,1], so the reconstruction can never leave the quad.
//  - Contrast-adaptive sharpening (CAS's formula): one term whose amplitude falls as
//    local contrast rises, so mid-contrast detail — texture, fine text, compression softness
//    — is sharpened while a hard edge is left to the reconstruction above instead of being
//    pushed into a halo. That falloff is what makes it adaptive to the picture.
//  - The anti-ringing bound (RCAS's): the result never leaves the quad's range, so no
//    amount of sharpening can invent a value the frame did not have.
fn upscaleSharpen(uv: vec2f) -> vec4f {
  let ts = texel.xy;
  let sp = uv / ts - vec2f(0.5);
  let base = (floor(sp) + vec2f(0.5)) * ts;
  let f = sp - floor(sp);
  let tl = tap(base);
  let tr = tap(base + vec2f(ts.x, 0.0));
  let bl = tap(base + vec2f(0.0, ts.y));
  let br = tap(base + vec2f(ts.x, ts.y));
  // 1. The reconstruction: narrowed bilinear weights, 1.0 being plain bilinear.
  let w = clamp((f - vec2f(0.5)) * texel.z + vec2f(0.5), vec2f(0.0), vec2f(1.0));
  let rec = mix(mix(tl, tr, w.x), mix(bl, br, w.x), w.y);
  // 2. The sharpen: amplitude from the quad's luma range, in the published CAS shape.
  let mn = min(min(luma(tl.rgb), luma(tr.rgb)), min(luma(bl.rgb), luma(br.rgb)));
  let mx = max(max(luma(tl.rgb), luma(tr.rgb)), max(luma(bl.rgb), luma(br.rgb)));
  let amp = sqrt(clamp(min(mn, 2.0 - mx) / max(mx, 1.0e-3), 0.0, 1.0));
  let cw = -amp * texel.w * 0.125;
  let o = (rec + (tl + tr + bl + br) * cw) / (1.0 + 4.0 * cw);
  return vec4f(clamp(o, min(min(tl, tr), min(bl, br)), max(max(tl, tr), max(bl, br))).rgb, rec.a);
}
`
    : ""
}
@fragment fn fs(v: V) -> @location(0) vec4f {
  return ${upscaleSharpen ? "upscaleSharpen(v.uv)" : "textureSampleBaseClampToEdge(t, s, v.uv)"};
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
 * How much smaller the frame is than the rect it is drawn into, per axis, in that
 * mode's geometry. 1 is 1:1; above 1 is upscaling.
 */
function upscaleFactor(mode: string | undefined, cw: number, ch: number, fw: number, fh: number) {
  const { rect, crop } = presentGeometry(mode, cw, ch, fw, fh);
  return { x: rect.w / (fw * crop[0]), y: rect.h / (fh * crop[1]) };
}

/**
 * Whether the upscale + sharpen pass draws this frame (#1349): only when the frame is
 * smaller than the picture on BOTH axes, so the pass never touches 1:1 or a downscale.
 * `integer` is pixel-exact by definition (a game drawing at 1:1 or a pixel-art look), so
 * it never takes the pass whatever the size. The 1% floor keeps a rounded layout — a
 * 1:1 frame in a canvas that reports a pixel more — from switching it on for nothing.
 */
const UPSCALE_SHARPEN_MIN_SCALE = 1.01;

/** How far the reconstruction narrows the bilinear blend toward the nearer texel:
 *  1 is plain bilinear, higher is tighter edges. */
const UPSCALE_SHARPEN_EDGE = 1.7;

/** CAS's own amount: the published formula's `w`, before the amplitude falloff. */
const UPSCALE_SHARPEN_STRENGTH = 1;

export function shouldUpscaleSharpen(
  mode: string | undefined,
  cw: number,
  ch: number,
  fw: number,
  fh: number,
  enabled = true,
): boolean {
  if (!enabled || mode === "integer" || fw <= 0 || fh <= 0) return false;
  const { x, y } = upscaleFactor(mode, cw, ch, fw, fh);
  return x > UPSCALE_SHARPEN_MIN_SCALE && y > UPSCALE_SHARPEN_MIN_SCALE;
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
  const makePipeline = (texture: string, upscaleSharpen = false) => {
    const module = device.createShaderModule({ code: shader(texture, upscaleSharpen) });
    return device.createRenderPipeline({
      layout: "auto",
      vertex: { module, entryPoint: "vs" },
      fragment: { module, entryPoint: "fs", targets: [{ format }] },
      primitive: { topology: "triangle-list" },
    });
  };

  /** Why the upscale + sharpen pass is unavailable, once one kind says so. */
  let sharpenUnavailable: string | null = null;
  /**
   * The upscale + sharpen pipeline for one texture kind, or null when it cannot be built
   * (#1349). Its shader is compiled here, at creation, so a browser that rejects it is known
   * before the first frame: the pass is then off for this session and every frame is drawn the
   * plain way. A shader nobody can run is not a broken session.
   */
  const makeSharpenPipeline = async (texture: string): Promise<GPURenderPipeline | null> => {
    try {
      const module = device.createShaderModule({ code: shader(texture, true) });
      const info = await module.getCompilationInfo?.();
      const errors = info?.messages.filter((m) => m.type === "error") ?? [];
      if (errors.length > 0) {
        sharpenUnavailable ??= errors[0].message;
        return null;
      }
      return device.createRenderPipeline({
        layout: "auto",
        vertex: { module, entryPoint: "vs" },
        fragment: { module, entryPoint: "fs", targets: [{ format }] },
        primitive: { topology: "triangle-list" },
      });
    } catch (err) {
      sharpenUnavailable ??= err instanceof Error ? err.message : String(err);
      return null;
    }
  };

  const external = makePipeline("texture_external");
  // Both kinds up front: the copy fallback below is synchronous, and knowing now whether the
  // pass compiles is what lets it be a fallback rather than a surprise. Either may be null.
  const [externalSharpen, textureSharpen] = await Promise.all([
    makeSharpenPipeline("texture_external"),
    makeSharpenPipeline("texture_2d<f32>"),
  ]);
  if (sharpenUnavailable != null) {
    console.warn("presenter: the upscale+sharpen pass is unavailable; drawing without it", sharpenUnavailable);
  }

  /** `texture_2d<f32>`: the copy fallback and the PyroWave decoder's output, one pipeline each. */
  let texture2d: GPURenderPipeline | null = null;
  const texture2dPipeline = () => (texture2d ??= makePipeline("texture_2d<f32>"));

  // Set once an import fails (seen on Chrome's software adapter, SwiftShader): from then on
  // each frame is copied into `copy.target`. The zero-copy import stays the normal path.
  let copy: { target: GPUTexture | null } | null = null;
  const linear = device.createSampler({ magFilter: "linear", minFilter: "linear" });
  let nearest: GPUSampler | null = null;
  const cropBuffer = device.createBuffer({ size: 16, usage: GPUBufferUsage.UNIFORM | GPUBufferUsage.COPY_DST });
  const cropData = new Float32Array(4);
  const texelBuffer = device.createBuffer({ size: 16, usage: GPUBufferUsage.UNIFORM | GPUBufferUsage.COPY_DST });
  const texelData = new Float32Array(4);

  const resize = new ResizeObserver(([entry]) => {
    const dpr = window.devicePixelRatio || 1;
    canvas.width = Math.max(1, Math.round(entry.contentRect.width * dpr));
    canvas.height = Math.max(1, Math.round(entry.contentRect.height * dpr));
  });
  resize.observe(canvas);

  /** The frame as the bind group takes it, and the two pipelines that can sample it. */
  const source = (frame: VideoFrame) => {
    if (!copy) {
      try {
        return {
          pipeline: external,
          sharpen: externalSharpen,
          texture: device.importExternalTexture({ source: frame }) as GPUExternalTexture | GPUTextureView,
        };
      } catch (err) {
        console.warn("presenter: importExternalTexture failed; copying each frame instead", err);
        copy = { target: null };
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
    return {
      pipeline: texture2dPipeline(),
      sharpen: textureSharpen,
      texture: copy.target.createView() as GPUExternalTexture | GPUTextureView,
    };
  };

  const draw = (
    plain: GPURenderPipeline,
    sharpenPipeline: GPURenderPipeline | null,
    texture: GPUExternalTexture | GPUTextureView,
    fw: number,
    fh: number,
  ) => {
    // Read per frame: the HUD changes the mode mid-session, and it is one attribute read.
    const mode = canvas.dataset?.scalingMode;
    const { rect: r, crop } = presentGeometry(mode, canvas.width, canvas.height, fw, fh);
    cropData.set(crop);
    device.queue.writeBuffer(cropBuffer, 0, cropData);
    const sharpen =
      sharpenPipeline != null &&
      canvas.dataset?.upscaleSharpen !== "off" &&
      shouldUpscaleSharpen(mode, canvas.width, canvas.height, fw, fh);
    const sampler = mode === "integer" ? (nearest ??= device.createSampler({ magFilter: "nearest", minFilter: "nearest" })) : linear;
    const entries: GPUBindGroupEntry[] = [
      { binding: 0, resource: sampler },
      { binding: 1, resource: texture },
      { binding: 2, resource: { buffer: cropBuffer } },
    ];
    if (sharpen) {
      texelData.set([1 / fw, 1 / fh, UPSCALE_SHARPEN_EDGE, UPSCALE_SHARPEN_STRENGTH]);
      device.queue.writeBuffer(texelBuffer, 0, texelData);
      entries.push({ binding: 3, resource: { buffer: texelBuffer } });
    }
    const pipeline = sharpen ? sharpenPipeline : plain;
    const bindGroup = device.createBindGroup({ layout: pipeline.getBindGroupLayout(0), entries });
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

  return {
    device,
    presentTexture(texture) {
      try {
        if (failed || destroyed) return false;
        draw(texture2dPipeline(), textureSharpen, texture.createView(), texture.width, texture.height);
        return true;
      } catch (err) {
        fail(`the presenter could not draw: ${err instanceof Error ? err.message : String(err)}`);
        return false;
      }
    },
    present(frame) {
      try {
        if (failed || destroyed) return false;
        const { pipeline, sharpen, texture } = source(frame);
        draw(pipeline, sharpen, texture, frame.displayWidth, frame.displayHeight);
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
