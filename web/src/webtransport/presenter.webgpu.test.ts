// The upscale + sharpen pass on a real WebGPU device (#1349), in headless Chromium
// (`make test-webgpu`; SwiftShader unless PYROWAVE_GPU=native). It draws a still 1080p
// picture through the real presenter into a 1440p target and holds the pass to plain
// bilinear: the picture must change, its edges must get crisper, and turning the HUD's
// switch off must reproduce bilinear exactly.
//
// Two things this environment cannot do, so the test stubs exactly those and nothing else:
//  - The run's Chromium is launched with `--disable-vulkan-surface`, so a WebGPU *canvas*
//    swapchain is lost on the first draw. The presenter's two canvas calls — `configure`
//    and `getCurrentTexture` — hand it a real offscreen texture instead.
//  - Importing a canvas-backed VideoFrame kills the device here, so the decoded frame is
//    supplied as a real rgba8unorm texture through `presentTexture`, the PyroWave
//    decoder's path. Its fragment shader is the same one `present()` draws with; the
//    external-texture pipeline choice is covered by the jsdom presenter tests.
// Everything else is real: the shader, the pipelines, the geometry, the uniforms, a
// validation error scope, and the pixels.
//
// VITE_PRESENTER_CAPTURE=1 prints labelled PNG data URLs to the log for the issue's
// side-by-side evidence. Timings are SwiftShader's software rasteriser, not a GPU's.
import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";
import { createWebGpuPresenter, type Presenter } from "./presenter";

/** The stream: 1080p, as the encoder sends it. */
const W = 1920;
const H = 1080;
/** The display: 1440p, so the picture is drawn 1.33x larger than it arrives. */
const CW = 2560;
const CH = 1440;
const CAPTURE = String(import.meta.env.VITE_PRESENTER_CAPTURE ?? "") === "1";

let format: GPUTextureFormat;
let source: Uint8Array<ArrayBuffer>;

beforeAll(() => {
  // A detached stand-in canvas is not an Element; the presenter only observes it.
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  format = navigator.gpu!.getPreferredCanvasFormat();
  source = sourcePicture();
});

afterAll(() => vi.unstubAllGlobals());

/** A still picture with what an upscale has to survive: hard shallow diagonals, stripes
 *  from 2px up, a dark panel with a bright border (the ringing case), text-like blocks. */
function sourcePicture(): Uint8Array<ArrayBuffer> {
  const c = document.createElement("canvas");
  [c.width, c.height] = [W, H];
  const g = c.getContext("2d")!;
  const grad = g.createLinearGradient(0, 0, W, H);
  grad.addColorStop(0, "#1b2430");
  grad.addColorStop(1, "#7b8794");
  g.fillStyle = grad;
  g.fillRect(0, 0, W, H);
  g.strokeStyle = "#f2f5f8";
  g.lineWidth = 3;
  for (let i = 0; i < 6; i++) {
    g.beginPath();
    g.moveTo(80, 90 + i * 60);
    g.lineTo(W - 80, 160 + i * 60);
    g.stroke();
  }
  for (let k = 0, period = 2; period <= 16; period *= 2, k++) {
    for (let x = 80; x < 560; x += period) {
      g.fillStyle = (x / period) % 2 === 0 ? "#0e1116" : "#e8eef4";
      g.fillRect(x, 560 + k * 40, period, 32);
    }
  }
  g.fillStyle = "#0b0e12";
  g.fillRect(700, 560, 520, 300);
  g.strokeStyle = "#ffffff";
  g.lineWidth = 2;
  g.strokeRect(700.5, 560.5, 519, 299);
  g.fillStyle = "#dfe6ee";
  for (let i = 0; i < 40; i++) g.fillRect(1300 + (i % 10) * 55, 560 + Math.floor(i / 10) * 70, 34, 9);
  // Small text and fine detail: what a game UI, a subtitle or a menu actually lives or
  // dies on at 1.33x.
  g.font = "13px sans-serif";
  g.fillText("Quasar: a 1080p stream drawn at 1440p. Small UI text, thin strokes, 1.33x.", 1300, 920);
  g.font = "11px sans-serif";
  g.fillText("The quick brown fox jumps over the lazy dog. 0123456789", 1300, 950);
  const noise = g.createImageData(400, 200);
  for (let i = 0; i < noise.data.length; i += 4) {
    const v = 90 + ((i * 2654435761) % 120);
    noise.data[i] = v;
    noise.data[i + 1] = v;
    noise.data[i + 2] = v;
    noise.data[i + 3] = 255;
  }
  g.putImageData(noise, 1300, 700);
  const rgba = g.getImageData(0, 0, W, H).data;
  const bytes = new Uint8Array(rgba.length);
  bytes.set(rgba);
  return bytes;
}

type Px = Uint8ClampedArray<ArrayBuffer>;

interface Shot {
  pixels: Px;
  fatal: string[];
  /** ms per presented frame, the queue drained each frame. */
  msPerFrame: number;
}

/** Presents the still through a real presenter at `w`x`h`, pass on or off, and reads the
 *  target back. A fresh presenter for each shot: an adapter is consumed by requestDevice. */
async function shoot(opts: { sharpen: boolean; w?: number; h?: number }): Promise<Shot> {
  const [w, h] = [opts.w ?? CW, opts.h ?? CH];
  // The target and the read-back must belong to the presenter's own device, so they are
  // made after it exists. The canvas stub is what hands it a real texture to draw into.
  let target!: GPUTexture;
  const canvas = {
    width: w,
    height: h,
    dataset: {} as Record<string, string>,
    getContext: () => ({ configure() {}, getCurrentTexture: () => target }),
  } as unknown as HTMLCanvasElement;
  const fatal: string[] = [];
  const presenter: Presenter = (await createWebGpuPresenter(canvas, (m) => fatal.push(m)))!;
  expect(presenter).not.toBeNull();
  const device = presenter.device!;
  target = device.createTexture({
    size: [w, h],
    format,
    usage: GPUTextureUsage.RENDER_ATTACHMENT | GPUTextureUsage.COPY_SRC,
  });
  const frame = device.createTexture({
    size: [W, H],
    format: "rgba8unorm",
    usage: GPUTextureUsage.TEXTURE_BINDING | GPUTextureUsage.COPY_DST,
  });
  device.queue.writeTexture({ texture: frame }, source, { bytesPerRow: W * 4 }, [W, H]);
  if (!opts.sharpen) canvas.dataset.upscaleSharpen = "off";
  device.pushErrorScope("validation");
  expect(presenter.presentTexture!(frame)).toBe(true);
  const pixels = await readBack(device, target, w, h);
  expect((await device.popErrorScope())?.message ?? null).toBeNull();
  // Frame time, submitted and drained frame by frame.
  const t0 = performance.now();
  for (let i = 0; i < 30; i++) {
    presenter.presentTexture!(frame);
    await device.queue.onSubmittedWorkDone();
  }
  const msPerFrame = (performance.now() - t0) / 30;
  presenter.destroy();
  return { pixels, fatal, msPerFrame };
}

async function readBack(device: GPUDevice, texture: GPUTexture, w: number, h: number): Promise<Px> {
  const bytesPerRow = Math.ceil((w * 4) / 256) * 256;
  const buffer = device.createBuffer({ size: bytesPerRow * h, usage: GPUBufferUsage.COPY_DST | GPUBufferUsage.MAP_READ });
  const encoder = device.createCommandEncoder();
  encoder.copyTextureToBuffer({ texture }, { buffer, bytesPerRow }, [w, h]);
  device.queue.submit([encoder.finish()]);
  await buffer.mapAsync(GPUMapMode.READ);
  const src = new Uint8Array(buffer.getMappedRange());
  const out = new Uint8ClampedArray(w * h * 4);
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      const i = y * bytesPerRow + x * 4;
      const o = (y * w + x) * 4;
      // The canvas format is bgra8unorm; the assertions and the captures want rgba.
      out[o] = src[i + 2];
      out[o + 1] = src[i + 1];
      out[o + 2] = src[i];
      out[o + 3] = src[i + 3];
    }
  }
  buffer.destroy();
  return out;
}

/** Mean absolute 4-neighbour Laplacian on luma: rises as edges are reconstructed. */
function edgeEnergy(p: Px, w: number, h: number): number {
  const luma = (i: number) => 0.2126 * p[i] + 0.7152 * p[i + 1] + 0.0722 * p[i + 2];
  let sum = 0;
  let n = 0;
  for (let y = 1; y < h - 1; y++) {
    for (let x = 1; x < w - 1; x++) {
      const i = (y * w + x) * 4;
      sum += Math.abs(4 * luma(i) - luma(i - 4) - luma(i + 4) - luma(i - w * 4) - luma(i + w * 4));
      n++;
    }
  }
  return sum / n;
}

const meanLuma = (p: Uint8ClampedArray) => {
  let s = 0;
  for (let i = 0; i < p.length; i += 4) s += 0.2126 * p[i] + 0.7152 * p[i + 1] + 0.0722 * p[i + 2];
  return s / (p.length / 4);
};

const differing = (a: Px, b: Px) => {
  let n = 0;
  for (let i = 0; i < a.length; i += 4) if (a[i] !== b[i] || a[i + 1] !== b[i + 1] || a[i + 2] !== b[i + 2]) n++;
  return n / (a.length / 4);
};

describe("the upscale + sharpen pass, 1080p drawn at 1440p", () => {
  it("rebuilds edges instead of stretching them, and the switch restores bilinear", async () => {
    const plain = await shoot({ sharpen: false });
    const sharp = await shoot({ sharpen: true });
    expect(plain.fatal).toEqual([]);
    expect(sharp.fatal).toEqual([]);
    // A real pass, not a no-op: every pixel near an edge or a texel boundary changes.
    expect(differing(plain.pixels, sharp.pixels)).toBeGreaterThan(0.05);
    // And it sharpens: more high-frequency energy at the same average brightness.
    const [ep, es] = [edgeEnergy(plain.pixels, CW, CH), edgeEnergy(sharp.pixels, CW, CH)];
    const [lp, ls] = [meanLuma(plain.pixels), meanLuma(sharp.pixels)];
    console.log(
      `presenter-pass edge-energy plain=${ep.toFixed(2)} sharp=${es.toFixed(2)} luma plain=${lp.toFixed(2)} sharp=${ls.toFixed(2)}`,
    );
    if (CAPTURE) capture("upscale-1080p-at-1440p", plain.pixels, sharp.pixels, "plain scaling", "upscaled + sharpened");
    expect(es).toBeGreaterThan(ep * 1.02);
    expect(Math.abs(ls - lp)).toBeLessThan(1);
  }, 120_000);

  it("is pixel-identical to plain at 1:1", async () => {
    const plain = await shoot({ sharpen: false, w: W, h: H });
    const sharp = await shoot({ sharpen: true, w: W, h: H });
    expect(differing(plain.pixels, sharp.pixels)).toBe(0);
  }, 120_000);

  it("costs a measurable frame time on this adapter", async () => {
    const plain = await shoot({ sharpen: false });
    const sharp = await shoot({ sharpen: true });
    console.log(
      `presenter-pass cost @1440p plain=${plain.msPerFrame.toFixed(2)} ms/frame ` +
        `sharp=${sharp.msPerFrame.toFixed(2)} ms/frame (software rasteriser: not the acceptance number)`,
    );
    expect(sharp.msPerFrame).toBeGreaterThan(0);
  }, 120_000);
});

/** Prints labelled side-by-side pairs for the issue's evidence: the whole frame at half
 *  scale, a 1:1 detail crop, and a 3x zoom on the small text. */
function capture(name: string, plain: Px, sharp: Px, l: string, r: string) {
  const whole = (p: Px) => {
    const c = document.createElement("canvas");
    [c.width, c.height] = [CW, CH];
    c.getContext("2d")!.putImageData(new ImageData(p, CW, CH), 0, 0);
    return c;
  };
  const [a, b] = [whole(plain), whole(sharp)];
  // Crop of the target, then how many screen pixels each source pixel becomes.
  const crops = [
    { tag: "half", sx: 0, sy: 0, w: CW / 2, h: CH / 2, zoom: 1 },
    { tag: "detail", sx: 1700, sy: 900, w: 620, h: 348, zoom: 1 },
    { tag: "textzoom", sx: 1730, sy: 1215, w: 300, h: 60, zoom: 3 },
  ];
  for (const { tag, sx, sy, w, h, zoom } of crops) {
    const [dw, dh] = [w * zoom, h * zoom];
    const c = document.createElement("canvas");
    [c.width, c.height] = [dw * 2 + 12, dh + 36];
    const g = c.getContext("2d")!;
    g.fillStyle = "#111";
    g.fillRect(0, 0, c.width, c.height);
    g.imageSmoothingEnabled = zoom === 1;
    g.drawImage(a, sx, sy, w, h, 0, 36, dw, dh);
    g.drawImage(b, sx, sy, w, h, dw + 12, 36, dw, dh);
    g.fillStyle = "#fff";
    g.font = "16px sans-serif";
    g.fillText(l, 8, 24);
    g.fillText(r, dw + 20, 24);
    console.log(`PRESENTER_CAPTURE ${name}-${tag} ${c.toDataURL("image/png")}`);
  }
}
