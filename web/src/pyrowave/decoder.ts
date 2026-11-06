// The PyroWave decoder (protocol/codec-pyrowave.md): one frame's blocks in, an RGBA texture out,
// on the caller's WebGPU device. Load it with import(), never statically: it is only for a
// session that streams PyroWave.
import { LEVELS, blockLayout, buildUpload, type BlockLayout, type PyroWaveHeader, type Upload } from "./bitstream";
import { dequantWGSL, idwtWGSL, yuvToRgbWGSL } from "./shaders";

/** fast: subgroups + shader-f16. fallback: neither, or f16 alone (B06, #1055; not built yet). */
export type DecoderPath = "fast" | "subgroups-fp32" | "fallback";

export function chooseDecoderPath(features: ReadonlySet<string>): DecoderPath {
  if (!features.has("subgroups")) return "fallback";
  return features.has("shader-f16") ? "fast" : "subgroups-fp32";
}

/** The features to request on the device the decoder will share (Chrome on NVIDIA has no shader-f16). */
export function pyroWaveDeviceFeatures(adapter: GPUAdapter): GPUFeatureName[] {
  return (["subgroups", "shader-f16"] as const).filter((f) => adapter.features.has(f));
}

export type DecodeStats = Pick<Upload, "received" | "zero" | "lost">;

const MAX_BANDS = 64;

async function checked(device: GPUDevice, code: string): Promise<GPUShaderModule> {
  const module = device.createShaderModule({ code });
  const errors = (await module.getCompilationInfo()).messages.filter((m) => m.type === "error");
  if (errors.length) throw new Error("PyroWave WGSL: " + errors.map((e) => `${e.lineNum}:${e.linePos} ${e.message}`).join("; "));
  return module;
}

export class PyroWaveDecoder {
  /** The decoded picture, rgba8unorm, header.width x header.height, rewritten by each decode(). */
  readonly texture: GPUTexture;

  private payload: GPUBuffer | null = null;
  private dqBindGroup: GPUBindGroup | null = null;

  private constructor(
    private readonly device: GPUDevice,
    readonly header: PyroWaveHeader,
    readonly path: DecoderPath,
    private readonly layout: BlockLayout,
    private readonly dq: { pipe: GPUComputePipeline; params: GPUBuffer; blockBand: GPUBuffer; offsets: GPUBuffer; coefHi: GPUBuffer; coefLo: GPUBuffer; wgStride: number },
    private readonly idwt: { pipe: GPUComputePipeline; bindGroup: GPUBindGroup; wx: number; wy: number; wz: number }[],
    private readonly rgb: { pipe: GPURenderPipeline; bindGroup: GPUBindGroup },
    private readonly planes: GPUBuffer,
    private readonly owned: GPUBuffer[],
  ) {
    this.texture = device.createTexture({
      size: [header.width, header.height],
      format: "rgba8unorm",
      usage: GPUTextureUsage.RENDER_ATTACHMENT | GPUTextureUsage.TEXTURE_BINDING | GPUTextureUsage.COPY_SRC,
    });
  }

  /**
   * One decoder per configuration (`config_id`): concealment reuses the coefficients of earlier
   * frames decoded by this instance, and a new instance starts from zero (amendment 55 §Loss).
   */
  static async create(device: GPUDevice, header: PyroWaveHeader): Promise<PyroWaveDecoder> {
    const path = chooseDecoderPath(device.features);
    if (path === "fallback") throw new Error("PyroWave: this device has no subgroups; the fallback decoder is not built yet (#1055)");
    const f16 = path === "fast";
    const layout = blockLayout(header.width, header.height);
    if (layout.bands.length > MAX_BANDS) throw new Error("PyroWave: too many bands");
    const { STORAGE, COPY_SRC, COPY_DST, UNIFORM } = GPUBufferUsage;
    const owned: GPUBuffer[] = [];
    const buffer = (size: number, usage: number, data?: Uint32Array<ArrayBuffer>) => {
      const b = device.createBuffer({ size: Math.max(4, Math.ceil(size / 4) * 4), usage });
      if (data) device.queue.writeBuffer(b, 0, data);
      owned.push(b);
      return b;
    };
    const coefHi = buffer(layout.hiWords * (f16 ? 2 : 4), STORAGE);
    const coefLo = buffer(layout.loWords * 4, STORAGE);
    const planes = buffer(layout.planeWords * 4, STORAGE | COPY_SRC);
    const offsets = buffer(layout.totalBlocks * 4, STORAGE | COPY_DST);

    const wgStride = Math.min(layout.totalBlocks, 65535);
    const params = new Uint32Array(4 + MAX_BANDS * 8);
    params.set([layout.bands.length, wgStride, layout.totalBlocks]);
    const bandOf = new Uint32Array(layout.totalBlocks);
    layout.bands.forEach((b, i) => {
      bandOf.fill(i, b.blockOffset, b.blockOffset + b.blockCount);
      params.set([b.blockOffset, b.blocksX, b.blockCount, b.off, b.width, b.height, b.lo ? 1 : 0, 0], 4 + 8 * i);
    });
    const sgMin = device.adapterInfo.subgroupMinSize ?? 0;
    const dqPipe = await device.createComputePipelineAsync({
      layout: "auto",
      compute: { module: await checked(device, dequantWGSL(f16)), entryPoint: "main", constants: { SG_AT_LEAST_16: sgMin >= 16 ? 1 : 0 } },
    });
    const dq = { pipe: dqPipe, params: buffer(params.byteLength, UNIFORM | COPY_DST, params), blockBand: buffer(bandOf.byteLength, STORAGE | COPY_DST, bandOf), offsets, coefHi, coefLo, wgStride };

    // One dispatch per level with z = component: Dawn puts a barrier between dispatches (B00:
    // 13 dispatches -> 5 took the 1080p iDWT from 114 to 47 us on the RTX 5090).
    const idwtPipe = await device.createComputePipelineAsync({ layout: "auto", compute: { module: await checked(device, idwtWGSL(f16)), entryPoint: "main" } });
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
        layout: idwtPipe.getBindGroupLayout(0),
        entries: [buffer(u.byteLength, UNIFORM | COPY_DST, u), coefHi, coefLo, planes].map((b, binding) => ({ binding, resource: { buffer: b } })),
      });
      idwt.push({ pipe: idwtPipe, bindGroup, wx: Math.ceil(bw / 16), wy: Math.ceil(bh / 16), wz: comps.length });
    }

    const rgbModule = await checked(device, yuvToRgbWGSL);
    const rgbPipe = await device.createRenderPipelineAsync({
      layout: "auto",
      vertex: { module: rgbModule, entryPoint: "vs" },
      fragment: { module: rgbModule, entryPoint: "fs", targets: [{ format: "rgba8unorm" }] },
      primitive: { topology: "triangle-list" },
    });
    const rgbParams = new Uint32Array([layout.aw, header.width, header.height, header.fullRange ? 0 : 1, layout.planeOff[1], layout.planeOff[2], layout.planeStride[1], 0]);
    const rgb = {
      pipe: rgbPipe,
      bindGroup: device.createBindGroup({
        layout: rgbPipe.getBindGroupLayout(0),
        entries: [buffer(rgbParams.byteLength, UNIFORM | COPY_DST, rgbParams), planes].map((b, binding) => ({ binding, resource: { buffer: b } })),
      }),
    };
    return new PyroWaveDecoder(device, header, path, layout, dq, idwt, rgb, planes, owned);
  }

  /**
   * Decodes one frame into `texture`. `packets` and `covered` are as for buildUpload: a lost
   * block index keeps the coefficients of the last frame this decoder had it in.
   */
  decode(packets: readonly Uint8Array[], covered: Uint8Array | null): DecodeStats {
    const d = this.device;
    const up = buildUpload(this.layout, packets, covered);
    if (!this.payload || this.payload.size < up.payload.byteLength) {
      this.payload?.destroy();
      this.payload = d.createBuffer({ size: Math.ceil((up.payload.byteLength * 1.25) / 4) * 4, usage: GPUBufferUsage.STORAGE | GPUBufferUsage.COPY_DST });
      this.dqBindGroup = d.createBindGroup({
        layout: this.dq.pipe.getBindGroupLayout(0),
        entries: [this.dq.params, this.dq.offsets, this.payload, this.dq.coefHi, this.dq.coefLo, this.dq.blockBand].map((b, binding) => ({ binding, resource: { buffer: b } })),
      });
    }
    d.queue.writeBuffer(this.dq.offsets, 0, up.offsets);
    d.queue.writeBuffer(this.payload, 0, up.payload);

    const enc = d.createCommandEncoder();
    const pass = enc.beginComputePass();
    pass.setPipeline(this.dq.pipe);
    pass.setBindGroup(0, this.dqBindGroup);
    pass.dispatchWorkgroups(this.dq.wgStride, Math.ceil(this.layout.totalBlocks / this.dq.wgStride));
    for (const s of this.idwt) {
      pass.setPipeline(s.pipe);
      pass.setBindGroup(0, s.bindGroup);
      pass.dispatchWorkgroups(s.wx, s.wy, s.wz);
    }
    pass.end();
    const draw = enc.beginRenderPass({ colorAttachments: [{ view: this.texture.createView(), loadOp: "clear", storeOp: "store", clearValue: [0, 0, 0, 1] }] });
    draw.setPipeline(this.rgb.pipe);
    draw.setBindGroup(0, this.rgb.bindGroup);
    draw.draw(3);
    draw.end();
    d.queue.submit([enc.finish()]);
    return { received: up.received, zero: up.zero, lost: up.lost };
  }

  /** The last decode's Y, Cb, Cr planes as 8-bit I420, for tests and diagnostics (a GPU readback). */
  async readI420(): Promise<Uint8Array> {
    const d = this.device;
    const rb = d.createBuffer({ size: this.planes.size, usage: GPUBufferUsage.MAP_READ | GPUBufferUsage.COPY_DST });
    const enc = d.createCommandEncoder();
    enc.copyBufferToBuffer(this.planes, 0, rb, 0, this.planes.size);
    d.queue.submit([enc.finish()]);
    await rb.mapAsync(GPUMapMode.READ);
    const f = new Float32Array(rb.getMappedRange().slice(0));
    rb.destroy();
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
    for (const b of this.owned) b.destroy();
    this.payload?.destroy();
    this.texture.destroy();
  }
}
