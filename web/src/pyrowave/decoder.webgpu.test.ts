// Runs in headless Chromium on WebGPU (vitest.webgpu.config.ts, `make test-webgpu`), not jsdom.
// Decodes every golden fixture (testdata/pyrowave) and holds it to the manifest's decoder_bar.
// VITE_PYROWAVE_PATHS / VITE_PYROWAVE_SUBGROUP_MIN state what the adapter must offer (defaults:
// SwiftShader); a real-GPU run sets them, so a missing feature fails instead of narrowing the run.
import { afterEach, beforeAll, beforeEach, describe, expect, it } from "vitest";
import manifest from "../../../testdata/pyrowave/manifest.json";
import { blockLayout, parsePwv1 } from "./bitstream";
import { PyroWaveDecoder, PyroWaveError, pyroWaveDeviceFeatures, type DecoderPath } from "./decoder";

const expectedPaths = String(import.meta.env.VITE_PYROWAVE_PATHS ?? "subgroups-fp32").split(",") as DecoderPath[];
const expectedSubgroupMin = Number(import.meta.env.VITE_PYROWAVE_SUBGROUP_MIN ?? 4);

const urls: Record<string, string> = import.meta.glob("../../../testdata/pyrowave/*", { query: "?url", import: "default", eager: true });
async function fixture(file: string): Promise<Uint8Array> {
  const url = urls[`../../../testdata/pyrowave/${file}`];
  if (!url) throw new Error(`no fixture ${file}`);
  return new Uint8Array(await (await fetch(url)).arrayBuffer());
}

function planes(i420: Uint8Array, w: number, h: number): Uint8Array[] {
  const ys = w * h;
  const cs = (w >> 1) * (h >> 1);
  return [i420.subarray(0, ys), i420.subarray(ys, ys + cs), i420.subarray(ys + cs, ys + 2 * cs)];
}

function compare(a: Uint8Array, b: Uint8Array): { psnr: number; maxAbs: number } {
  let se = 0;
  let maxAbs = 0;
  for (let i = 0; i < a.length; i++) {
    const e = a[i] - b[i];
    se += e * e;
    maxAbs = Math.max(maxAbs, Math.abs(e));
  }
  return { psnr: se === 0 ? 99 : 10 * Math.log10((255 * 255 * a.length) / se), maxAbs };
}

let device: GPUDevice;
beforeAll(async () => {
  const adapter = await navigator.gpu?.requestAdapter();
  if (!adapter) throw new Error("no WebGPU adapter: run in Chromium with --enable-unsafe-webgpu");
  device = await adapter.requestDevice({ requiredFeatures: pyroWaveDeviceFeatures(adapter) });
});

// A WebGPU error is silent unless captured: every test must leave none.
beforeEach(() => device.pushErrorScope("validation"));
afterEach(async () => {
  expect((await device.popErrorScope())?.message ?? null).toBeNull();
});

async function decoderFor(file: string, forcePath?: DecoderPath) {
  const { header, frames } = parsePwv1(await fixture(file));
  return { decoder: await PyroWaveDecoder.create(device, header, { forcePath }), frame: frames[0], header };
}

async function rejection(p: Promise<unknown>): Promise<PyroWaveError> {
  const e = await p.then(
    () => null,
    (err: unknown) => err,
  );
  expect(e).toBeInstanceOf(PyroWaveError);
  return e as PyroWaveError;
}

it("runs on the adapter the run expects: decoder paths and subgroup size", () => {
  const offered = (["fast", "subgroups-fp32"] as const).filter((p) => device.features.has("subgroups") && (p !== "fast" || device.features.has("shader-f16")));
  console.log(`pyrowave-adapter ${device.adapterInfo.vendor}/${device.adapterInfo.architecture} paths=${offered.join(",")} subgroup=${device.adapterInfo.subgroupMinSize}-${device.adapterInfo.subgroupMaxSize}`);
  expect(offered).toEqual(expectedPaths);
  expect(device.adapterInfo.subgroupMinSize).toBe(expectedSubgroupMin);
});

describe.each(expectedPaths)("PyroWaveDecoder on the golden fixtures, %s path", (path) => {
  const bar = manifest.decoder_bar;

  it.each(manifest.fixtures.map((f) => [f.name, f] as const))("%s meets the decoder bar", async (_name, f) => {
    const { decoder, frame } = await decoderFor(f.pwv1.file, path);
    expect(decoder.path).toBe(path);
    const stats = decoder.decode([frame], null);
    const got = planes(await decoder.readI420(), f.width, f.height);
    const want = planes(await fixture(f.ref.file), f.width, f.height);
    const result = ["Y", "Cb", "Cr"].map((plane, i) => ({ plane, ...compare(got[i], want[i]) }));
    console.log(`pyrowave-fixture ${f.name} path=${decoder.path} ${result.map((r) => `${r.plane}=${r.psnr.toFixed(2)}dB/max${r.maxAbs}`).join(" ")}`);
    decoder.destroy();
    expect(stats.received).toBe(f.blocks);
    for (const r of result) {
      expect(r.psnr, `${r.plane} PSNR`).toBeGreaterThanOrEqual(bar.min_psnr_db);
      expect(r.maxAbs, `${r.plane} max error`).toBeLessThanOrEqual(bar.max_abs_err);
    }
  });
});

describe("hostile input", () => {
  it.each([16384, 8192])("a %i-pixel square header is refused before anything is allocated", async (size) => {
    const e = await rejection(PyroWaveDecoder.create(device, { width: size, height: size, fullRange: false }));
    expect(e.reason).toBe("too_large");
  });

  it("a header larger than the negotiated resolution is refused", async () => {
    const e = await rejection(PyroWaveDecoder.create(device, { width: 1920, height: 1080, fullRange: false }, { maxPixels: 1280 * 720 }));
    expect(e.reason).toBe("too_large");
  });

  it.each([0x00, 0xff])("a block shorter than its header claims is concealed, whatever follows it (tail byte %i)", async (fill) => {
    const name = "detail_320x192_hi";
    const { decoder, frame } = await decoderFor(`${name}.pwv1`);
    decoder.decode([frame], null);
    const clean = await decoder.readI420();

    // Swap the 40th block for one announcing all 16 8x8 blocks with 8 bytes of room for them.
    const dv = new DataView(frame.buffer, frame.byteOffset, frame.byteLength);
    let o = 8;
    for (let i = 0; i < 40; i++) o += ((dv.getUint32(o, true) >>> 16) & 0xfff) * 4;
    const end = o + ((dv.getUint32(o, true) >>> 16) & 0xfff) * 4;
    const bad = new Uint8Array(16).fill(fill);
    const bdv = new DataView(bad.buffer);
    bdv.setUint32(0, (4 << 16) | 0xffff, true);
    bdv.setUint32(4, dv.getUint32(o + 4, true), true); // same block index and quant code
    const stats = decoder.decode([frame.subarray(0, o), bad, frame.subarray(end)], null);
    expect(stats.lost).toBe(1);
    expect(await decoder.readI420()).toEqual(clean); // concealed from the clean frame; nothing else moved
    decoder.destroy();
  });
});

describe("lifecycle", () => {
  it("a destroyed decoder refuses to decode", async () => {
    const { decoder, frame } = await decoderFor("flat_200x114_lo.pwv1");
    decoder.destroy();
    expect(() => decoder.decode([frame], null)).toThrow(expect.objectContaining({ reason: "destroyed" }));
  });

  it("a lost device makes decode and readback fail with device_lost", async () => {
    const adapter = (await navigator.gpu.requestAdapter())!; // an adapter makes one device
    const own = await adapter.requestDevice({ requiredFeatures: pyroWaveDeviceFeatures(adapter) });
    const { header, frames } = parsePwv1(await fixture("flat_200x114_lo.pwv1"));
    const decoder = await PyroWaveDecoder.create(own, header);
    decoder.decode([frames[0]], null);
    own.destroy();
    await own.lost;
    expect(() => decoder.decode([frames[0]], null)).toThrow(expect.objectContaining({ reason: "device_lost" }));
    expect((await rejection(decoder.readI420())).reason).toBe("device_lost");
  });

  it("a readback whose map fails rejects", async () => {
    const adapter = (await navigator.gpu.requestAdapter())!;
    const own = await adapter.requestDevice({ requiredFeatures: pyroWaveDeviceFeatures(adapter) });
    const { header, frames } = parsePwv1(await fixture("flat_200x114_lo.pwv1"));
    const decoder = await PyroWaveDecoder.create(own, header);
    decoder.decode([frames[0]], null);
    const read = decoder.readI420();
    own.destroy(); // before the map resolves, and before the lost promise flips the flag
    await expect(read).rejects.toThrow();
  });
});

describe("block concealment", () => {
  const f = manifest.fixtures.find((x) => x.name === "detail_320x192_hi")!;
  const n = 138; // block indices of a 320x192 frame

  it("a frame whose every block is lost shows the last frame again", async () => {
    const { decoder, frame } = await decoderFor(f.pwv1.file);
    decoder.decode([frame], null);
    const before = await decoder.readI420();
    const stats = decoder.decode([], new Uint8Array(n));
    expect(stats.lost).toBe(n);
    expect(await decoder.readI420()).toEqual(before);
    decoder.destroy();
  });

  it("a covered index with no block is zero, never the last frame's", async () => {
    const { decoder, frame } = await decoderFor(f.pwv1.file);
    decoder.decode([frame], null);
    decoder.decode([], new Uint8Array(n).fill(1));
    expect(new Set(await decoder.readI420())).toEqual(new Set([128])); // all-zero coefficients: mid grey
    decoder.destroy();
  });

  it("a new decoder (a new config_id) conceals a lost block as zero", async () => {
    const { decoder } = await decoderFor(f.pwv1.file);
    decoder.decode([], new Uint8Array(n));
    expect(new Set(await decoder.readI420())).toEqual(new Set([128]));
    decoder.destroy();
  });

  it("only the lost block is concealed: the received ones decode as sent", async () => {
    // Decode detail, then flat with block 0 lost: block 0 is level 4 Y LL (the DC of luma).
    const flat = parsePwv1(await fixture("flat_320x192_hi.pwv1")).frames[0];
    const lossy = await decoderFor(f.pwv1.file);
    lossy.decoder.decode([lossy.frame], null);
    const covered = new Uint8Array(n).fill(1);
    covered[0] = 0;
    // Bitstream order: the SOF, then block 0 first if it was sent.
    const dv = new DataView(flat.buffer, flat.byteOffset);
    expect(dv.getUint32(12, true) >>> 8).toBe(0);
    const block0Bytes = ((dv.getUint32(8, true) >>> 16) & 0xfff) * 4;
    const withoutBlock0 = [flat.subarray(0, 8), flat.subarray(8 + block0Bytes)];
    expect(lossy.decoder.decode(withoutBlock0, covered).lost).toBe(1);
    const [y, cb, cr] = planes(await lossy.decoder.readI420(), 320, 192);

    const clean = await decoderFor("flat_320x192_hi.pwv1");
    clean.decoder.decode([flat], null);
    const [cleanY, cleanCb, cleanCr] = planes(await clean.decoder.readI420(), 320, 192);
    expect(y).not.toEqual(cleanY); // luma DC came from the detail frame
    expect(cb).toEqual(cleanCb);
    expect(cr).toEqual(cleanCr);
    lossy.decoder.destroy();
    clean.decoder.destroy();
  });
});

describe("YCbCr to RGB", () => {
  // BT.709 from its definition (Kr 0.2126, Kb 0.0722), limited range, centre-sited 4:2:0.
  function expectedRgb(i420: Uint8Array, w: number, h: number, x: number, y: number): number[] {
    const [Y, Cb, Cr] = planes(i420, w, h);
    const cw = w >> 1;
    const ch = h >> 1;
    const sample = (p: Uint8Array) => {
      const fx = (x + 0.5) / 2 - 0.5;
      const fy = (y + 0.5) / 2 - 0.5;
      const x0 = Math.floor(fx);
      const y0 = Math.floor(fy);
      const at = (i: number, j: number) => p[Math.min(Math.max(j, 0), ch - 1) * cw + Math.min(Math.max(i, 0), cw - 1)];
      const tx = fx - x0;
      const ty = fy - y0;
      return (at(x0, y0) * (1 - tx) + at(x0 + 1, y0) * tx) * (1 - ty) + (at(x0, y0 + 1) * (1 - tx) + at(x0 + 1, y0 + 1) * tx) * ty;
    };
    const yy = Math.min(Math.max((Y[y * w + x] - 16) / 219, 0), 1);
    const pb = Math.min(Math.max((sample(Cb) - 128) / 224, -0.5), 0.5);
    const pr = Math.min(Math.max((sample(Cr) - 128) / 224, -0.5), 0.5);
    const [kr, kb] = [0.2126, 0.0722];
    const r = yy + 2 * (1 - kr) * pr;
    const b = yy + 2 * (1 - kb) * pb;
    const g = (yy - kr * r - kb * b) / (1 - kr - kb);
    return [r, g, b].map((v) => Math.round(Math.min(Math.max(v, 0), 1) * 255));
  }

  async function worstRgbError(decoder: PyroWaveDecoder): Promise<number> {
    const yuv = await decoder.readI420();
    const { width: w, height: h } = decoder.header;
    const bpr = Math.ceil((w * 4) / 256) * 256;
    const rb = device.createBuffer({ size: bpr * h, usage: GPUBufferUsage.MAP_READ | GPUBufferUsage.COPY_DST });
    const enc = device.createCommandEncoder();
    enc.copyTextureToBuffer({ texture: decoder.texture }, { buffer: rb, bytesPerRow: bpr }, [w, h]);
    device.queue.submit([enc.finish()]);
    await rb.mapAsync(GPUMapMode.READ);
    const rgba = new Uint8Array(rb.getMappedRange().slice(0));
    rb.destroy();
    let worst = 0;
    for (let y = 0; y < h; y++)
      for (let x = 0; x < w; x++) {
        const want = expectedRgb(yuv, w, h, x, y);
        for (let c = 0; c < 3; c++) worst = Math.max(worst, Math.abs(rgba[y * bpr + 4 * x + c] - want[c]));
      }
    return worst;
  }

  it("draws BT.709 limited-range RGB into the texture", async () => {
    const { decoder, frame } = await decoderFor("detail_200x114_hi.pwv1");
    decoder.decode([frame], null);
    const worst = await worstRgbError(decoder);
    console.log(`pyrowave-rgb max error ${worst}`);
    decoder.destroy();
    expect(worst).toBeLessThanOrEqual(2);
  });

  it("samples chroma only inside the picture at the right edge", async () => {
    // 200 px wide: chroma is 100 columns, its plane 112. One hand-built level-1 Cb high-pass block
    // over chroma columns 96-111 makes column 100 (padding) differ sharply from column 99.
    const header = { width: 200, height: 114, fullRange: false };
    const band = blockLayout(200, 114).bands.find((b) => b.level === 1 && b.comp === 1 && b.band === 1)!;
    const block = new Uint8Array(28);
    const dv = new DataView(block.buffer);
    dv.setUint32(0, (7 << 16) | (1 << 2), true); // 7 words; ballot: 8x8 block (bx 2, by 0) = coefficient columns 48-55
    dv.setUint32(4, ((band.blockOffset + 1) << 8) | 0x38, true); // the band's second block; quant code 0x38
    dv.setUint16(8, 0x5555, true); // cw: one plane per 4x2 subblock (a zero cw is an all-zero 8x8 block)
    block[10] = 6 << 4; // qb 0, scale 6
    block.fill(0xff, 11, 19); // one plane per 4x2 subblock: every magnitude 1; sign bits (19-26) 0
    const decoder = await PyroWaveDecoder.create(device, header);
    expect(decoder.decode([block], null).received).toBe(1);
    const worst = await worstRgbError(decoder);
    decoder.destroy();
    expect(worst).toBeLessThanOrEqual(2);
  });
});
