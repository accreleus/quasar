// The PyroWave decoder (protocol/codec-pyrowave.md): one frame's blocks in, an RGBA texture out,
// on the caller's WebGPU device. Load it with import(), never statically: it is only for a
// session that streams PyroWave.
import { LEVELS, PyroWaveError, blockLayout, buildUpload, type BlockLayout, type PyroWaveHeader, type Upload } from "./bitstream";
import { dequantWGSL, idwtWGSL, yuvToRgbWGSL } from "./shaders";

export { PyroWaveError, type PyroWaveErrorReason } from "./bitstream";

/** fast: subgroups + shader-f16 storage. fallback (#1055): no subgroups, fp32 even with shader-f16. */
export type DecoderPath = "fast" | "subgroups-fp32" | "fallback";

const PATH_NEEDS: Record<DecoderPath, GPUFeatureName[]> = { fast: ["subgroups", "shader-f16"], "subgroups-fp32": ["subgroups"], fallback: [] };

/**
 * The subgroup paths assume every subgroup of a 128-lane workgroup has one size that divides 128
 * (the dequantise shader's lane index and its scan). Sizes the browser does not report are trusted.
 */
export function chooseDecoderPath(features: ReadonlySet<string>, info?: Pick<GPUAdapterInfo, "subgroupMinSize" | "subgroupMaxSize">): DecoderPath {
  if (!features.has("subgroups")) return "fallback";
  const min = info?.subgroupMinSize;
  const max = info?.subgroupMaxSize;
  if (min !== undefined && max !== undefined && (min !== max || !(max > 0 && max <= 128 && 128 % max === 0))) return "fallback";
  return features.has("shader-f16") ? "fast" : "subgroups-fp32";
}

/** The features to request on the device the decoder will share (Chrome on NVIDIA has no shader-f16). */
export function pyroWaveDeviceFeatures(adapter: GPUAdapter): GPUFeatureName[] {
  return (["subgroups", "shader-f16"] as const).filter((f) => adapter.features.has(f));
}

export interface DecoderOptions {
  /** The negotiated launch resolution's pixel count: a header asking for more is refused. */
  maxPixels: number;
  /** Tests: run a path the device supports other than the one it would choose. */
  forcePath?: DecoderPath;
  /** Tests: make one GPU call inside create() invalid. */
  faultForTest?: boolean;
}

export type DecodeStats = Pick<Upload, "received" | "zero" | "lost">;

const MAX_BANDS = 64;

interface Pipelines {
  dq: GPUComputePipeline;
  idwt: GPUComputePipeline;
  rgb: GPURenderPipeline;
}

// Compiled once per device and path; a decoder per configuration reuses them.
const pipelineCache = new WeakMap<GPUDevice, Map<DecoderPath, Promise<Pipelines>>>();
// create() runs one at a time per device, so no other create() pushes or pops between its own.
const createChain = new WeakMap<GPUDevice, Promise<unknown>>();
// Kept per device, not per decoder: a decoder's own lost handler would keep it alive as long as the device.
const lostDevices = new WeakSet<GPUDevice>();
const watchedDevices = new WeakSet<GPUDevice>();

async function checked(device: GPUDevice, code: string): Promise<GPUShaderModule> {
  const module = device.createShaderModule({ code });
  const errors = (await module.getCompilationInfo()).messages.filter((m) => m.type === "error");
  if (errors.length) throw new PyroWaveError("gpu_error", "WGSL: " + errors.map((e) => `${e.lineNum}:${e.linePos} ${e.message}`).join("; "));
  return module;
}

function pipelines(device: GPUDevice, path: DecoderPath): Promise<Pipelines> {
  let byPath = pipelineCache.get(device);
  if (!byPath) pipelineCache.set(device, (byPath = new Map()));
  let p = byPath.get(path);
  if (!p) {
    const f16 = path === "fast";
    p = (async () => {
      try {
        const rgbModule = await checked(device, yuvToRgbWGSL);
        const [dq, idwt, rgb] = await Promise.all([
          checked(device, dequantWGSL(f16, path !== "fallback")).then((module) => device.createComputePipelineAsync({ layout: "auto", compute: { module, entryPoint: "main" } })),
          checked(device, idwtWGSL(f16)).then((module) => device.createComputePipelineAsync({ layout: "auto", compute: { module, entryPoint: "main" } })),
          device.createRenderPipelineAsync({
            layout: "auto",
            vertex: { module: rgbModule, entryPoint: "vs" },
            fragment: { module: rgbModule, entryPoint: "fs", targets: [{ format: "rgba8unorm" }] },
            primitive: { topology: "triangle-list" },
          }),
        ]);
        return { dq, idwt, rgb };
      } catch (e) {
        // A GPUPipelineError (e.g. limits below what the auto layout needs) is a DOMException.
        if (e instanceof PyroWaveError) throw e;
        throw new PyroWaveError("gpu_error", `pipeline: ${e instanceof Error ? e.message : String(e)}`);
      }
    })();
    const cache = byPath;
    p.catch(() => cache.delete(path));
    byPath.set(path, p);
  }
  return p;
}

export class PyroWaveDecoder {
  private payload: GPUBuffer | null = null;
  private dqBindGroup: GPUBindGroup | null = null;
  private destroyed = false;
  private gpuError: string | null = null;

  private constructor(
    private readonly device: GPUDevice,
    readonly header: PyroWaveHeader,
    readonly path: DecoderPath,
    /** The decoded picture, rgba8unorm, header.width x header.height, rewritten by each decode(). */
    readonly texture: GPUTexture,
    private readonly layout: BlockLayout,
    private readonly pipes: Pipelines,
    private readonly dq: { params: GPUBuffer; blockBand: GPUBuffer; offsets: GPUBuffer; coefHi: GPUBuffer; coefLo: GPUBuffer; wgStride: number },
    private readonly idwt: { bindGroup: GPUBindGroup; wx: number; wy: number; wz: number }[],
    private readonly rgbBindGroup: GPUBindGroup,
    private readonly planes: GPUBuffer,
    private readonly owned: GPUBuffer[],
  ) {}

  /**
   * One decoder per configuration (`config_id`): concealment reuses the coefficients of earlier
   * frames decoded by this instance, and a new instance starts from zero (amendment 55 §Loss).
   */
  static create(device: GPUDevice, header: PyroWaveHeader, options: DecoderOptions): Promise<PyroWaveDecoder> {
    if (!watchedDevices.has(device)) {
      watchedDevices.add(device);
      void device.lost.then(() => lostDevices.add(device));
    }
    const run = (createChain.get(device) ?? Promise.resolve()).then(() => PyroWaveDecoder.createNow(device, header, options));
    createChain.set(
      device,
      run.catch(() => undefined),
    );
    return run;
  }

  private static async createNow(device: GPUDevice, header: PyroWaveHeader, options: DecoderOptions): Promise<PyroWaveDecoder> {
    const path = options.forcePath ?? chooseDecoderPath(device.features, device.adapterInfo as GPUAdapterInfo | undefined);
    if (!Object.hasOwn(PATH_NEEDS, path)) throw new PyroWaveError("unsupported", `unknown decoder path ${String(path)}`);
    const missing = PATH_NEEDS[path].find((f) => !device.features.has(f));
    if (missing) throw new PyroWaveError("unsupported", `the ${path} path needs ${missing}, which the device lacks`);
    const f16 = path === "fast";
    const { width: w, height: h } = header;
    const tooLarge = (what: string) => new PyroWaveError("too_large", `${w}x${h}: ${what}`);
    if (!(w * h <= options.maxPixels)) throw tooLarge(`more than the negotiated ${options.maxPixels} pixels`);
    const limits = device.limits;
    if (Math.max(w, h) > limits.maxTextureDimension2D) throw tooLarge(`over the device's ${limits.maxTextureDimension2D}-pixel texture limit`);
    const layout = blockLayout(w, h);
    if (layout.bands.length > MAX_BANDS) throw tooLarge("too many bands");
    const sizes = { coefHi: layout.hiWords * (f16 ? 2 : 4), coefLo: layout.loWords * 4, planes: layout.planeWords * 4, offsets: layout.totalBlocks * 4 };
    const maxBytes = Math.min(limits.maxStorageBufferBindingSize, limits.maxBufferSize);
    for (const [name, bytes] of Object.entries(sizes)) if (bytes > maxBytes) throw tooLarge(`${name} needs ${bytes} bytes, the device allows ${maxBytes}`);

    const pipes = await pipelines(device, path);
    const owned: GPUBuffer[] = [];
    let texture: GPUTexture | undefined;
    device.pushErrorScope("out-of-memory");
    device.pushErrorScope("validation");
    let scopes = 2;
    try {
      const { STORAGE, COPY_SRC, COPY_DST, UNIFORM } = GPUBufferUsage;
      const buffer = (size: number, usage: number, data?: Uint32Array<ArrayBuffer>) => {
        const b = device.createBuffer({ size: Math.max(4, Math.ceil(size / 4) * 4), usage });
        owned.push(b);
        if (data) device.queue.writeBuffer(b, 0, data);
        return b;
      };
      const coefHi = buffer(sizes.coefHi, STORAGE);
      const coefLo = buffer(sizes.coefLo, STORAGE);
      const planes = buffer(sizes.planes, STORAGE | COPY_SRC);
      const offsets = buffer(sizes.offsets, STORAGE | COPY_DST);
      texture = device.createTexture({
        size: [w, h],
        format: "rgba8unorm",
        usage: GPUTextureUsage.RENDER_ATTACHMENT | GPUTextureUsage.TEXTURE_BINDING | GPUTextureUsage.COPY_SRC,
      });

      const wgStride = Math.min(layout.totalBlocks, 65535);
      const params = new Uint32Array(4 + MAX_BANDS * 8);
      params.set([layout.bands.length, wgStride, layout.totalBlocks]);
      const bandOf = new Uint32Array(layout.totalBlocks);
      layout.bands.forEach((b, i) => {
        bandOf.fill(i, b.blockOffset, b.blockOffset + b.blockCount);
        params.set([b.blockOffset, b.blocksX, b.blockCount, b.off, b.width, b.height, b.lo ? 1 : 0, 0], 4 + 8 * i);
      });
      const dq = { params: buffer(params.byteLength, UNIFORM | COPY_DST, params), blockBand: buffer(bandOf.byteLength, STORAGE | COPY_DST, bandOf), offsets, coefHi, coefLo, wgStride };

      // One dispatch per level with z = component: Dawn puts a barrier between dispatches (B00:
      // 13 dispatches -> 5 took the 1080p iDWT from 114 to 47 us on the RTX 5090).
      const idwt = [];
      for (let l = LEVELS - 1; l >= 0; l--) {
        const bw = layout.aw >> (l + 1);
        const bh = layout.ah >> (l + 1);
        const comps = l === 0 ? [0] : [0, 1, 2];
        const u = new Uint32Array(3 * 8);
        for (const c of comps) {
          const final = l === 0 || (c !== 0 && l === 1); // 4:2:0 chroma ends at level 1
          const outKind = final ? 2 : l - 1 >= 2 ? 1 : 0;
          const b = layout.base[c][l];
          u.set([b[0].off, b[1].off, b[2].off, b[3].off, bw, bh, final ? layout.planeOff[c] : layout.base[c][l - 1][0].off, (l >= 2 ? 1 : 0) | (outKind << 1)], 8 * c);
        }
        const bindGroup = device.createBindGroup({
          layout: pipes.idwt.getBindGroupLayout(0),
          entries: [buffer(u.byteLength, UNIFORM | COPY_DST, u), coefHi, coefLo, planes].map((b, binding) => ({ binding, resource: { buffer: b } })),
        });
        idwt.push({ bindGroup, wx: Math.ceil(bw / 16), wy: Math.ceil(bh / 16), wz: comps.length });
      }

      const rgbParams = new Uint32Array([layout.aw, w, h, header.fullRange ? 0 : 1, layout.planeOff[1], layout.planeOff[2], layout.planeStride[1], 0]);
      const rgbBindGroup = device.createBindGroup({
        layout: pipes.rgb.getBindGroupLayout(0),
        entries: [buffer(rgbParams.byteLength, UNIFORM | COPY_DST, rgbParams), planes].map((b, binding) => ({ binding, resource: { buffer: b } })),
      });
      if (options.faultForTest) device.createBuffer({ size: 6, usage: GPUBufferUsage.STORAGE | GPUBufferUsage.MAP_READ });
      scopes = 0;
      // Both pops issued back to back, before any await.
      const [validation, oom] = await Promise.all([device.popErrorScope(), device.popErrorScope()]);
      const error = validation ?? oom;
      if (error) throw new PyroWaveError("gpu_error", error.message);
      return new PyroWaveDecoder(device, header, path, texture, layout, pipes, dq, idwt, rgbBindGroup, planes, owned);
    } catch (e) {
      while (scopes-- > 0) void device.popErrorScope();
      for (const b of owned) b.destroy();
      texture?.destroy();
      throw e;
    }
  }

  private usable(): void {
    if (this.destroyed) throw new PyroWaveError("destroyed", "the decoder was destroyed");
    if (lostDevices.has(this.device)) throw new PyroWaveError("device_lost", "the WebGPU device was lost");
    if (this.gpuError) throw new PyroWaveError("gpu_error", `an earlier decode failed: ${this.gpuError}`);
  }

  /**
   * Decodes one frame into `texture`. `packets` and `covered` are as for buildUpload: a lost
   * block index keeps the coefficients of the last frame this decoder had it in. A WebGPU error in
   * this decode is reported asynchronously, so the next decode() throws gpu_error: waiting for it
   * here would put the GPU's completion on every frame's path.
   */
  decode(packets: readonly Uint8Array[], covered: Uint8Array | null): DecodeStats {
    this.usable();
    const d = this.device;
    const up = buildUpload(this.layout, packets, covered);
    const bytes = up.payload.byteLength;
    const limit = Math.min(d.limits.maxStorageBufferBindingSize, d.limits.maxBufferSize) & ~3;
    if (bytes > limit) throw new PyroWaveError("too_large", `a ${bytes}-byte frame`);
    d.pushErrorScope("validation");
    try {
      // Grow with headroom; shrink again once frames are far smaller, so one burst is not kept.
      if (!this.payload || this.payload.size < bytes || this.payload.size > 4 * bytes + 65536) {
        this.payload?.destroy();
        this.payload = d.createBuffer({ size: Math.min(limit, Math.ceil((bytes * 1.25) / 4) * 4), usage: GPUBufferUsage.STORAGE | GPUBufferUsage.COPY_DST });
        this.dqBindGroup = d.createBindGroup({
          layout: this.pipes.dq.getBindGroupLayout(0),
          entries: [this.dq.params, this.dq.offsets, this.payload, this.dq.coefHi, this.dq.coefLo, this.dq.blockBand].map((b, binding) => ({ binding, resource: { buffer: b } })),
        });
      }
      d.queue.writeBuffer(this.dq.offsets, 0, up.offsets);
      d.queue.writeBuffer(this.payload, 0, up.payload);

      const enc = d.createCommandEncoder();
      const pass = enc.beginComputePass();
      pass.setPipeline(this.pipes.dq);
      pass.setBindGroup(0, this.dqBindGroup);
      pass.dispatchWorkgroups(this.dq.wgStride, Math.ceil(this.layout.totalBlocks / this.dq.wgStride));
      pass.setPipeline(this.pipes.idwt);
      for (const s of this.idwt) {
        pass.setBindGroup(0, s.bindGroup);
        pass.dispatchWorkgroups(s.wx, s.wy, s.wz);
      }
      pass.end();
      const draw = enc.beginRenderPass({ colorAttachments: [{ view: this.texture.createView(), loadOp: "clear", storeOp: "store", clearValue: [0, 0, 0, 1] }] });
      draw.setPipeline(this.pipes.rgb);
      draw.setBindGroup(0, this.rgbBindGroup);
      draw.draw(3);
      draw.end();
      d.queue.submit([enc.finish()]);
    } finally {
      void d.popErrorScope().then((e) => {
        if (e) this.gpuError ??= e.message;
      });
    }
    return { received: up.received, zero: up.zero, lost: up.lost };
  }

  /** The last decode's Y, Cb, Cr planes as 8-bit I420, for tests and diagnostics (a GPU readback). */
  async readI420(): Promise<Uint8Array> {
    this.usable();
    const d = this.device;
    const rb = d.createBuffer({ size: this.planes.size, usage: GPUBufferUsage.MAP_READ | GPUBufferUsage.COPY_DST });
    let f: Float32Array;
    try {
      const enc = d.createCommandEncoder();
      enc.copyBufferToBuffer(this.planes, 0, rb, 0, this.planes.size);
      d.queue.submit([enc.finish()]);
      await rb.mapAsync(GPUMapMode.READ);
      f = new Float32Array(rb.getMappedRange().slice(0));
    } finally {
      rb.destroy();
    }
    const { width: w, height: h } = this.header;
    const out = new Uint8Array(w * h + 2 * (w >> 1) * (h >> 1));
    let o = 0;
    for (let c = 0; c < 3; c++) {
      const pw = c ? w >> 1 : w;
      const ph = c ? h >> 1 : h;
      const { planeStride: st, planeOff: po } = this.layout;
      for (let y = 0; y < ph; y++)
        for (let x = 0; x < pw; x++) out[o++] = Math.round(Math.min(1, Math.max(0, f[po[c] + y * st[c] + x])) * 255);
    }
    return out;
  }

  destroy(): void {
    if (this.destroyed) return;
    this.destroyed = true;
    for (const b of this.owned) b.destroy();
    this.payload?.destroy();
    this.texture.destroy();
  }
}
