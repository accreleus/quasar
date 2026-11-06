// Runs in headless Chromium on WebGPU (vitest.webgpu.config.ts, `make test-webgpu`), not jsdom.
// Decodes every golden fixture (testdata/pyrowave) and holds it to the manifest's decoder_bar.
import { beforeAll, describe, expect, it } from "vitest";
import manifest from "../../../testdata/pyrowave/manifest.json";
import { parsePwv1 } from "./bitstream";
import { PyroWaveDecoder, pyroWaveDeviceFeatures } from "./decoder";

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

async function decoderFor(file: string) {
  const { header, frames } = parsePwv1(await fixture(file));
  return { decoder: await PyroWaveDecoder.create(device, header), frame: frames[0], header };
}

describe("PyroWaveDecoder on the golden fixtures", () => {
  const bar = manifest.decoder_bar;

  it.each(manifest.fixtures.map((f) => [f.name, f] as const))("%s meets the decoder bar", async (_name, f) => {
    const { decoder, frame } = await decoderFor(f.pwv1.file);
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

  it("draws BT.709 limited-range RGB into the texture", async () => {
    const f = manifest.fixtures.find((x) => x.name === "detail_200x114_hi")!;
    const { decoder, frame } = await decoderFor(f.pwv1.file);
    decoder.decode([frame], null);
    const yuv = await decoder.readI420();
    const { width: w, height: h } = f;
    const bpr = Math.ceil((w * 4) / 256) * 256;
    const rb = device.createBuffer({ size: bpr * h, usage: GPUBufferUsage.MAP_READ | GPUBufferUsage.COPY_DST });
    const enc = device.createCommandEncoder();
    enc.copyTextureToBuffer({ texture: decoder.texture }, { buffer: rb, bytesPerRow: bpr }, [w, h]);
    device.queue.submit([enc.finish()]);
    await rb.mapAsync(GPUMapMode.READ);
    const rgba = new Uint8Array(rb.getMappedRange().slice(0));
    rb.destroy();
    decoder.destroy();
    let worst = 0;
    for (let y = 0; y < h; y++)
      for (let x = 0; x < w; x++) {
        const want = expectedRgb(yuv, w, h, x, y);
        for (let c = 0; c < 3; c++) worst = Math.max(worst, Math.abs(rgba[y * bpr + 4 * x + c] - want[c]));
      }
    console.log(`pyrowave-rgb max error ${worst}`);
    expect(worst).toBeLessThanOrEqual(2);
  });
});
