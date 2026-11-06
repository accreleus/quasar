// PyroWave v1 decoder prototype in WebGPU (WGSL). Spike B00, accreleus/quasar#1049.
// Throwaway research code: decodes one 4:2:0 frame (dequant -> inverse DWT -> YCbCr planes)
// and draws it with a YCbCr->RGB render pass. Ported from upstream
// Themaister/pyrowave 7fe0e266 shaders/{wavelet_dequant.comp, idwt.comp, yuv2rgb.frag} (MIT).
'use strict';

const LEVELS = 5;

// ---------- bitstream ----------

function parsePWV1(buf) {
  const dv = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  if (dv.getUint32(0, true) !== 0x31565750) throw new Error('not a PWV1 file');
  const w0 = dv.getUint32(4, true), w1 = dv.getUint32(8, true);
  const hdr = {
    width: (w0 & 0x3fff) + 1, height: ((w0 >>> 14) & 0x3fff) + 1,
    chroma444: (w1 >>> 26) & 1, ycbcrRange: (w1 >>> 30) & 1, ycbcrTransform: (w1 >>> 29) & 1,
    chromaSiting: (w1 >>> 31) & 1, fpsNum: dv.getUint32(12, true), fpsDen: dv.getUint32(16, true),
  };
  const frames = [];
  for (let o = 24; o + 4 <= buf.byteLength;) {
    const n = dv.getUint32(o, true);
    frames.push(buf.subarray(o + 4, o + 4 + n));
    o += 4 + n;
  }
  return { hdr, frames };
}

// Geometry and block-index order, exactly as bitstream.md "Block index ordering".
function geometry(width, height) {
  const align = (v, a) => (v + a - 1) & ~(a - 1);
  const aw = Math.max(align(width, 32), 128), ah = Math.max(align(height, 32), 128);
  const bands = []; // in block-index order
  let blocks = 0, hiWords = 0, loWords = 0;
  const base = [[], [], []]; // base[comp][level][band] -> {lo, off}
  for (let c = 0; c < 3; c++) for (let l = 0; l < LEVELS; l++) base[c][l] = [];
  // allocate storage for every band incl. LL of each level (LL<4 is written by the iDWT)
  for (let c = 0; c < 3; c++) for (let l = 0; l < LEVELS; l++) {
    if (l === 0 && c !== 0) continue; // 4:2:0
    const bw = aw >> (l + 1), bh = ah >> (l + 1), lo = l >= 2;
    for (let b = 0; b < 4; b++) {
      if (lo) { base[c][l][b] = { lo: 1, off: loWords }; loWords += bw * bh; }
      else { base[c][l][b] = { lo: 0, off: hiWords }; hiWords += bw * bh; }
    }
  }
  for (let l = LEVELS - 1; l >= 0; l--) for (let c = 0; c < 3; c++) {
    if (l === 0 && c !== 0) continue;
    for (let b = (l === LEVELS - 1 ? 0 : 1); b < 4; b++) {
      const bw = aw >> (l + 1), bh = ah >> (l + 1);
      const bx = Math.ceil(bw / 32), by = Math.ceil(bh / 32);
      bands.push({ level: l, comp: c, band: b, blockOffset: blocks, blocksX: bx, blockCount: bx * by,
        width: bw, height: bh, lo: base[c][l][b].lo, off: base[c][l][b].off });
      blocks += bx * by;
    }
  }
  const planeY = aw * ah, planeC = (aw >> 1) * (ah >> 1);
  return { aw, ah, bands, totalBlocks: blocks, base, hiWords, loWords,
    planeOff: [0, planeY, planeY + planeC], planeStride: [aw, aw >> 1, aw >> 1], planeWords: planeY + 2 * planeC };
}

// One frame -> offsets[block_index] (u32 words into payload, ~0 = missing) + payload words.
function packFrame(frame, geo) {
  const dv = new DataView(frame.buffer, frame.byteOffset, frame.byteLength);
  const offsets = new Uint32Array(geo.totalBlocks).fill(0xffffffff);
  const words = new Uint32Array(Math.ceil(frame.byteLength / 4) + 8);
  let n = 0, received = 0, totalBlocks = -1;
  for (let o = 0; o + 8 <= frame.byteLength;) {
    const w0 = dv.getUint32(o, true), w1 = dv.getUint32(o + 4, true);
    if (w0 >>> 31) { // extended: start-of-frame header (8 bytes)
      if (((w1 >>> 24) & 3) !== 0) throw new Error('unknown extended code');
      totalBlocks = w1 & 0xffffff; o += 8; continue;
    }
    const pw = (w0 >>> 16) & 0xfff, bi = w1 >>> 8;
    if (pw < 2 || o + pw * 4 > frame.byteLength) throw new Error('bad packet at ' + o);
    if (bi < geo.totalBlocks && offsets[bi] === 0xffffffff) {
      offsets[bi] = n;
      for (let i = 0; i < pw; i++) words[n++] = dv.getUint32(o + 4 * i, true);
      received++;
    }
    o += pw * 4;
  }
  return { offsets, payload: words.subarray(0, Math.max(n + 8, 8)), received, totalBlocks };
}

// ---------- WGSL ----------

function dequantWGSL(sg, f16) {
  const HI = f16 ? 'f16' : 'f32';
  return `
${f16 ? 'enable f16;' : ''}
${sg ? 'enable subgroups;' : ''}
struct Band { block_offset: u32, blocks_x: u32, block_count: u32, out_base: u32,
              width: u32, height: u32, lo: u32, pad: u32 };
struct Params { num_bands: u32, wg_stride: u32, p0: u32, p1: u32, bands: array<Band, 64> };
@group(0) @binding(0) var<uniform> params: Params;
@group(0) @binding(1) var<storage, read> offsets: array<u32>;
@group(0) @binding(2) var<storage, read> payload: array<u32>;
@group(0) @binding(3) var<storage, read_write> coef_hi: array<${HI}>;
@group(0) @binding(4) var<storage, read_write> coef_lo: array<f32>;
@group(0) @binding(5) var<storage, read> block_band: array<u32>;

override SG_AT_LEAST_16: bool = false;

var<workgroup> sh_plane_off: array<u32, 16>;
var<workgroup> sh_sign_off: u32;
var<workgroup> sh_scan: array<u32, 128>;
var<workgroup> sh_sgid: atomic<u32>;

fn rd8(b: u32) -> u32 { return (payload[b >> 2u] >> ((b & 3u) * 8u)) & 0xffu; }
fn rd16(h: u32) -> u32 { return (payload[h >> 1u] >> ((h & 1u) * 16u)) & 0xffffu; }

fn decode_quant(q: u32) -> f32 {
  let e = 4 - i32(q >> 3u);
  let m = i32(q & 7u);
  return (1.0 / (8.0 * 1024.0 * 1024.0)) * f32((8 + m) * (1 << u32(20 + e)));
}

fn plane_bytes(cw: u32, qb: u32) -> u32 {
  let lsbs = cw & 0x5555u;
  var msbs = cw & 0xaaaau;
  msbs |= msbs >> 1u;
  return countOneBits(lsbs) + countOneBits(msbs) + qb * 8u;
}

@compute @workgroup_size(128)
fn main(@builtin(workgroup_id) wg: vec3u, @builtin(local_invocation_index) lii: u32
        ${sg ? ', @builtin(subgroup_invocation_id) sgi: u32, @builtin(subgroup_size) sgs: u32' : ''}) {
  let bi = wg.y * params.wg_stride + wg.x;
  if (bi >= params.p0) { return; } // p0 = total blocks
  let d = params.bands[block_band[bi]];

  // Linear lane index ordered by subgroup, like upstream's gl_SubgroupID * gl_SubgroupSize + id.
  ${sg ? `
  var mine = 0u;
  if (subgroupElect()) { mine = atomicAdd(&sh_sgid, 1u); }
  let sg_id = subgroupBroadcastFirst(mine);
  let li = sg_id * sgs + sgi;` : `
  let li = lii;`}

  let rel = bi - d.block_offset;
  let blk = vec2u(rel % d.blocks_x, rel / d.blocks_x);
  let sub = li & 7u;
  let bx = (li >> 3u) & 3u;
  let by = (li >> 5u) & 3u;
  let lb = by * 4u + bx;
  let coord = blk * 32u + 8u * vec2u(bx, by) + vec2u(4u * (sub >> 2u), 2u * (sub & 3u));

  let off = offsets[bi];
  var ballot = 0u;
  var qcode = 0u;
  if (off != 0xffffffffu) {
    ballot = payload[off] & 0xffffu;
    qcode = payload[off + 1u] & 0xffu;
  }
  let nset = countOneBits(ballot);
  let base = off * 4u + 8u + 3u * nset;

  // Byte cost of each 8x8 block -> where its magnitude planes start.
  var cost = 0u;
  if (li < 16u && extractBits(ballot, li, 1u) != 0u) {
    let lco = countOneBits(extractBits(ballot, 0u, li));
    cost = plane_bytes(rd16(off * 2u + 4u + lco), rd8(off * 4u + 8u + nset * 2u + lco) & 0xfu);
  }
  var incl: u32;
  ${sg ? `
  if (SG_AT_LEAST_16) {
    incl = subgroupInclusiveAdd(cost); // lanes 0..15 are all in subgroup 0
  } else {` : '{'}
    if (li < 16u) { sh_scan[li] = cost; }
    workgroupBarrier();
    for (var s = 1u; s < 16u; s <<= 1u) {
      var up = 0u;
      if (li < 16u && li >= s) { up = sh_scan[li - s]; }
      workgroupBarrier();
      if (li < 16u) { sh_scan[li] += up; }
      workgroupBarrier();
    }
    incl = sh_scan[min(li, 15u)];
    workgroupBarrier();
  }
  if (li < 16u) {
    sh_plane_off[li] = base + incl - cost;
    if (li == 15u) { sh_sign_off = 8u * (base + incl); }
  }
  workgroupBarrier();

  // Each lane decodes one 4x2 subblock (8 magnitudes, MSB plane first).
  var v: array<f32, 8>;
  var sig = 0u;
  if (extractBits(ballot, lb, 1u) != 0u) {
    let lco = countOneBits(extractBits(ballot, 0u, lb));
    let cw = rd16(off * 2u + 4u + lco);
    let cw2 = rd8(off * 4u + 8u + nset * 2u + lco);
    let qb = cw2 & 0xfu;
    if (cw != 0u) {
      let bit_off = 2u * sub;
      let lsbs = cw & 0x5555u;
      var msbs = cw & 0xaaaau;
      msbs |= msbs >> 1u;
      var bo = countOneBits(extractBits(lsbs, 0u, bit_off)) + countOneBits(extractBits(msbs, 0u, bit_off))
             + qb * sub + sh_plane_off[lb];
      let planes = qb + extractBits(cw, bit_off, 2u);
      var mag: array<u32, 8>;
      for (var q = i32(planes) - 1; q >= 0; q--) {
        let p = rd8(bo);
        bo++;
        for (var b = 0u; b < 8u; b++) { mag[b] |= ((p >> b) & 1u) << u32(q); }
      }
      let scale = decode_quant(qcode) * (f32((cw2 >> 4u) & 0xfu) / 8.0 + 0.25);
      for (var b = 0u; b < 8u; b++) {
        if (mag[b] != 0u) { v[b] = (f32(mag[b]) + 0.5) * scale; sig++; }
      }
    }
  }

  // Exclusive scan of significant counts over the 32x32 block, in lane order -> sign bit offset.
  var excl: u32;
  ${sg ? `
  {
    let nsg = 128u / sgs;
    let si = subgroupInclusiveAdd(sig);
    if (sgi == sgs - 1u) { sh_scan[sg_id] = si; }
    workgroupBarrier();
    if (li == 0u) { for (var i = 1u; i < nsg; i++) { sh_scan[i] += sh_scan[i - 1u]; } }
    workgroupBarrier();
    excl = si - sig;
    if (sg_id != 0u) { excl += sh_scan[sg_id - 1u]; }
  }` : `
  {
    sh_scan[li] = sig;
    workgroupBarrier();
    for (var s = 1u; s < 128u; s <<= 1u) {
      var up = 0u;
      if (li >= s) { up = sh_scan[li - s]; }
      workgroupBarrier();
      sh_scan[li] += up;
      workgroupBarrier();
    }
    excl = sh_scan[li] - sig;
  }`}

  let so = sh_sign_off + excl;
  var sw = payload[so / 32u];
  let m = so & 31u;
  if (m != 0u) { sw = (sw >> m) | (payload[so / 32u + 1u] << (32u - m)); }
  var sc = 0u;
  for (var b = 0u; b < 8u; b++) {
    if (v[b] != 0.0) {
      if (((sw >> sc) & 1u) != 0u) { v[b] = -v[b]; }
      sc++;
    }
  }

  // Element b sits at (b / 2, b % 2) inside the 4x2 subblock.
  for (var b = 0u; b < 8u; b++) {
    let p = coord + vec2u(b >> 1u, b & 1u);
    if (p.x < d.width && p.y < d.height) {
      let idx = d.out_base + p.y * d.width + p.x;
      if (d.lo != 0u) { coef_lo[idx] = v[b]; } else { coef_hi[idx] = ${HI}(v[b]); }
    }
  }
}`;
}

// iDWT for one level: four bands (bw x bh) -> one 2bw x 2bh image; workgroup_id.z = component.
// Each workgroup reconstructs a 32x32 output tile from a 40x40 interleaved tile (4-sample apron).
// Upstream's mirrored-repeat textureGather trick equals whole-sample symmetric extension of the
// interleaved signal, which is what mirror() does here with plain loads.
// Every lifting step runs across the whole tile in workgroup memory (no per-thread arrays: AMD's
// compiler spilled those to scratch and ran 25x slower).
function idwtWGSL(f16) {
  const HI = f16 ? 'f16' : 'f32';
  return `
${f16 ? 'enable f16;' : ''}
// mode: bit 0 = input in coef_lo; bits 1..2 = output 0 coef_hi, 1 coef_lo, 2 final plane.
struct C { in_base: vec4u, bw: u32, bh: u32, out_base: u32, mode: u32 };
@group(0) @binding(0) var<uniform> cs: array<C, 3>;
@group(0) @binding(1) var<storage, read_write> coef_hi: array<${HI}>;
@group(0) @binding(2) var<storage, read_write> coef_lo: array<f32>;
@group(0) @binding(3) var<storage, read_write> planes: array<f32>;

const ALPHA = -1.586134342059924;
const BETA = -0.052980118572961;
const GAMMA = 0.882911075530934;
const DELTA = 0.443506852043971;
const K = 1.230174104914001;
const INV_K = 1.0 / 1.230174104914001;
const T = 40u;   // tile incl. apron
const S = 41u;   // shared row stride

var<workgroup> sh: array<f32, ${40 * 41}>;
override SKIP_LIFT: bool = false; // diagnostics only (?skiplift=1): wrong output, isolates load/store cost

// Whole-sample symmetric extension. Indices further out than the 4-sample apron only feed
// outputs past the edge, which are discarded, so a clamp is enough there.
fn mirror(i: i32, n: i32) -> i32 {
  var m = abs(i);
  if (m >= n) { m = 2 * n - 2 - m; }
  return clamp(m, 0, n - 1);
}

fn load_in(i: u32, lo: bool) -> f32 {
  if (lo) { return coef_lo[i]; }
  return f32(coef_hi[i]);
}

// Vertical lifting step on rows i0, i0+2, ..., i1 (all 40 columns).
fn step_v(li: u32, i0: u32, i1: u32, c: f32) {
  let n = ((i1 - i0) / 2u + 1u) * T;
  for (var k = li; k < n; k += 64u) {
    let o = (i0 + 2u * (k / T)) * S + k % T;
    sh[o] -= c * (sh[o - S] + sh[o + S]);
  }
  workgroupBarrier();
}

// Horizontal lifting step on columns i0, i0+2, ..., i1 (rows 4..35).
fn step_h(li: u32, i0: u32, i1: u32, c: f32) {
  let nc = (i1 - i0) / 2u + 1u;
  for (var k = li; k < nc * 32u; k += 64u) {
    let o = (4u + (k & 31u)) * S + i0 + 2u * (k >> 5u);
    sh[o] -= c * (sh[o - 1u] + sh[o + 1u]);
  }
  workgroupBarrier();
}

@compute @workgroup_size(64)
fn main(@builtin(workgroup_id) wg: vec3u, @builtin(local_invocation_index) li: u32) {
  let p = cs[wg.z];
  let in_lo = (p.mode & 1u) != 0u;
  let out_kind = p.mode >> 1u;
  let nx = i32(2u * p.bw);
  let ny = i32(2u * p.bh);
  let ox = i32(wg.x) * 32 - 4;
  let oy = i32(wg.y) * 32 - 4;
  for (var k = li; k < T * T; k += 64u) {
    let r = k / T;
    let c = k % T;
    let gx = u32(mirror(ox + i32(c), nx));
    let gy = u32(mirror(oy + i32(r), ny));
    let band = (gx & 1u) | ((gy & 1u) << 1u); // 0 LL, 1 HL, 2 LH, 3 HH
    // Both passes' K / 1/K pre-scale folded in (low = K, high = 1/K per axis).
    let sc = select(INV_K, K, (r & 1u) == 0u) * select(INV_K, K, (c & 1u) == 0u);
    sh[r * S + c] = sc * load_in(p.in_base[band] + (gy >> 1u) * p.bw + (gx >> 1u), in_lo);
  }
  workgroupBarrier();

  // CDF 9/7 synthesis, vertical first (upstream's order), then horizontal.
  if (!SKIP_LIFT) {
  step_v(li, 2u, 38u, DELTA);
  step_v(li, 3u, 37u, GAMMA);
  step_v(li, 4u, 36u, BETA);
  step_v(li, 5u, 35u, ALPHA);
  step_h(li, 2u, 38u, DELTA);
  step_h(li, 3u, 37u, GAMMA);
  step_h(li, 4u, 36u, BETA);
  step_h(li, 5u, 35u, ALPHA);
  }

  let ow = 2u * p.bw;
  let oh = 2u * p.bh;
  for (var k = li; k < 1024u; k += 64u) {
    let x = wg.x * 32u + (k & 31u);
    let y = wg.y * 32u + (k >> 5u);
    if (x < ow && y < oh) {
      let v = sh[((k >> 5u) + 4u) * S + (k & 31u) + 4u];
      let o = p.out_base + y * ow + x;
      if (out_kind == 2u) {
        planes[o] = clamp(v + 0.5, 0.0, 1.0);
      } else if (out_kind == 1u) {
        coef_lo[o] = v;
      } else {
        coef_hi[o] = ${HI}(v);
      }
    }
  }
}`;
}

const yuvWGSL = `
struct P { aw: u32, w: u32, h: u32, limited: u32, off_cb: u32, off_cr: u32, cstride: u32, bt2020: u32 };
@group(0) @binding(0) var<uniform> p: P;
@group(0) @binding(1) var<storage, read> planes: array<f32>;

@vertex fn vs(@builtin(vertex_index) i: u32) -> @builtin(position) vec4f {
  let xy = vec2f(f32((i << 1u) & 2u), f32(i & 2u));
  return vec4f(xy * 2.0 - 1.0, 0.0, 1.0);
}

fn chroma(off: u32, x: f32, y: f32) -> f32 {
  // centre-sited 4:2:0, bilinear
  let cw = i32(p.cstride);
  let ch = i32((p.h + 1u) / 2u);
  let fx = x * 0.5 - 0.25;
  let fy = y * 0.5 - 0.25;
  let x0 = i32(floor(fx));
  let y0 = i32(floor(fy));
  let tx = fx - f32(x0);
  let ty = fy - f32(y0);
  let xa = u32(clamp(x0, 0, cw - 1)); let xb = u32(clamp(x0 + 1, 0, cw - 1));
  let ya = u32(clamp(y0, 0, ch - 1)); let yb = u32(clamp(y0 + 1, 0, ch - 1));
  let s = p.cstride;
  let a = mix(planes[off + ya * s + xa], planes[off + ya * s + xb], tx);
  let b = mix(planes[off + yb * s + xa], planes[off + yb * s + xb], tx);
  return mix(a, b, ty);
}

@fragment fn fs(@builtin(position) pos: vec4f) -> @location(0) vec4f {
  let x = u32(pos.x);
  let y = u32(pos.y);
  var Y = planes[y * p.aw + x];
  var cb = chroma(p.off_cb, pos.x, pos.y) - 128.0 / 255.0;
  var cr = chroma(p.off_cr, pos.x, pos.y) - 128.0 / 255.0;
  if (p.limited != 0u) {
    Y = clamp((Y - 16.0 / 255.0) * (255.0 / 219.0), 0.0, 1.0);
    cb = clamp(cb * (255.0 / 224.0), -0.5, 0.5);
    cr = clamp(cr * (255.0 / 224.0), -0.5, 0.5);
  }
  var m: mat3x3f;
  if (p.bt2020 != 0u) {
    m = mat3x3f(vec3f(1.0, 1.0, 1.0), vec3f(0.0, -0.202008 / 0.587, 1.772), vec3f(1.402, -0.419198 / 0.587, 0.0));
  } else {
    m = mat3x3f(vec3f(1.0, 1.0, 1.0), vec3f(0.0, -0.13397432 / 0.7152, 1.8556), vec3f(1.5748, -0.33480248 / 0.7152, 0.0));
  }
  return vec4f(clamp(m * vec3f(Y, cb, cr), vec3f(0.0), vec3f(1.0)), 1.0);
}`;

// ---------- decoder ----------

class PyroWaveDecoder {
  static async create(adapter, want) {
    const feats = [];
    const has = (f) => adapter.features.has(f);
    const sg = want.subgroups && has('subgroups');
    const f16 = want.f16 && has('shader-f16');
    if (sg) feats.push('subgroups');
    if (f16) feats.push('shader-f16');
    if (has('timestamp-query')) feats.push('timestamp-query');
    const device = await adapter.requestDevice({
      requiredFeatures: feats,
      requiredLimits: { maxStorageBufferBindingSize: adapter.limits.maxStorageBufferBindingSize,
                        maxBufferSize: adapter.limits.maxBufferSize },
    });
    const errors = [];
    device.addEventListener('uncapturederror', (e) => errors.push(e.error.message));
    return new PyroWaveDecoder(adapter, device, sg, f16, errors);
  }

  constructor(adapter, device, sg, f16, errors) {
    this.adapter = adapter; this.device = device; this.sg = sg; this.f16 = f16; this.errors = errors;
    this.timestamps = device.features.has('timestamp-query');
    const info = adapter.info || {};
    this.sgMin = info.subgroupMinSize || 0;
    this.path = (sg && f16) ? 'fast (subgroups + shader-f16)' : sg ? 'subgroups, fp32 storage'
      : f16 ? 'shared-memory scans, fp16 storage' : 'fallback (shared-memory scans, fp32)';
  }

  async compile(m) {
    const info = await m.getCompilationInfo();
    const errs = info.messages.filter((x) => x.type === 'error');
    if (errs.length) throw new Error('WGSL: ' + errs.map((e) => `${e.lineNum}:${e.linePos} ${e.message}`).join('\n'));
    return m;
  }

  release() {
    for (const k of ['coefHi', 'coefLo', 'planes', 'offsets', 'payload', 'blockBand', 'dqParams', 'rgbParams', 'rgbTarget', 'qs', 'qResolve', 'qRead']) {
      if (this[k]) { this[k].destroy(); this[k] = null; }
    }
    for (const s of this.idwt || []) s.ub.destroy();
    this.idwt = [];
  }

  async setup(hdr) {
    const d = this.device;
    this.release();
    if (hdr.chroma444) throw new Error('prototype handles 4:2:0 only');
    const geo = geometry(hdr.width, hdr.height);
    if (geo.bands.length > 64) throw new Error('too many bands');
    this.hdr = hdr; this.geo = geo;
    const S = GPUBufferUsage.STORAGE, C = GPUBufferUsage.COPY_SRC, D = GPUBufferUsage.COPY_DST, U = GPUBufferUsage.UNIFORM;
    const hiBytes = Math.ceil(geo.hiWords * (this.f16 ? 2 : 4) / 4) * 4;
    this.coefHi = d.createBuffer({ size: hiBytes, usage: S });
    this.coefLo = d.createBuffer({ size: geo.loWords * 4, usage: S });
    this.planes = d.createBuffer({ size: geo.planeWords * 4, usage: S | C });
    this.offsets = d.createBuffer({ size: geo.totalBlocks * 4, usage: S | D });
    this.payloadCap = 0;

    // dequant
    const dqMod = await this.compile(d.createShaderModule({ code: dequantWGSL(this.sg, this.f16) }));
    this.dqPipe = await d.createComputePipelineAsync({ layout: 'auto',
      compute: { module: dqMod, entryPoint: 'main', constants: this.sg ? { SG_AT_LEAST_16: this.sgMin >= 16 ? 1 : 0 } : {} } });
    const pu = new Uint32Array(4 + 64 * 8);
    this.wgStride = Math.min(geo.totalBlocks, 65535);
    pu[0] = geo.bands.length; pu[1] = this.wgStride; pu[2] = geo.totalBlocks;
    const bb = new Uint32Array(geo.totalBlocks);
    geo.bands.forEach((b, i) => bb.fill(i, b.blockOffset, b.blockOffset + b.blockCount));
    this.blockBand = d.createBuffer({ size: bb.byteLength, usage: S | D });
    d.queue.writeBuffer(this.blockBand, 0, bb);
    geo.bands.forEach((b, i) => pu.set([b.blockOffset, b.blocksX, b.blockCount, b.off, b.width, b.height, b.lo, 0], 4 + 8 * i));
    this.dqParams = d.createBuffer({ size: pu.byteLength, usage: U | D });
    d.queue.writeBuffer(this.dqParams, 0, pu);

    // idwt: one dispatch per (level, component)
    // One dispatch per level, z = component: WebGPU puts a barrier between dispatches, so fewer is faster.
    const st = { type: 'storage' }, csv = GPUShaderStage.COMPUTE;
    const idwtBGL = d.createBindGroupLayout({ entries: [{ binding: 0, visibility: csv, buffer: { type: 'uniform' } },
      { binding: 1, visibility: csv, buffer: st }, { binding: 2, visibility: csv, buffer: st }, { binding: 3, visibility: csv, buffer: st }] });
    const im = await this.compile(d.createShaderModule({ code: idwtWGSL(this.f16) }));
    const pipe = await d.createComputePipelineAsync({ layout: d.createPipelineLayout({ bindGroupLayouts: [idwtBGL] }),
      compute: { module: im, entryPoint: 'main',
        constants: { SKIP_LIFT: new URLSearchParams(location.search).get('skiplift') === '1' ? 1 : 0 } } });
    this.idwt = [];
    for (let l = LEVELS - 1; l >= 0; l--) {
      const bw = geo.aw >> (l + 1), bh = geo.ah >> (l + 1);
      const comps = l === 0 ? [0] : [0, 1, 2];
      const u = new Uint32Array(3 * 8);
      for (const c of comps) {
        const final = l === 0 || (c !== 0 && l === 1);
        const outKind = final ? 2 : (l - 1 >= 2 ? 1 : 0);
        const b = geo.base[c][l];
        u.set([b[0].off, b[1].off, b[2].off, b[3].off, bw, bh,
          final ? geo.planeOff[c] : geo.base[c][l - 1][0].off, (l >= 2 ? 1 : 0) | (outKind << 1)], 8 * c);
      }
      const ub = d.createBuffer({ size: u.byteLength, usage: U | D });
      d.queue.writeBuffer(ub, 0, u);
      const bg = d.createBindGroup({ layout: idwtBGL, entries: [
        { binding: 0, resource: { buffer: ub } }, { binding: 1, resource: { buffer: this.coefHi } },
        { binding: 2, resource: { buffer: this.coefLo } }, { binding: 3, resource: { buffer: this.planes } }] });
      this.idwt.push({ level: l, pipe, bg, ub, wx: Math.ceil(bw / 16), wy: Math.ceil(bh / 16), wz: comps.length });
    }

    // yuv -> rgb
    const ym = await this.compile(d.createShaderModule({ code: yuvWGSL }));
    this.format = navigator.gpu.getPreferredCanvasFormat();
    this.rgbPipe = await d.createRenderPipelineAsync({ layout: 'auto', vertex: { module: ym, entryPoint: 'vs' },
      fragment: { module: ym, entryPoint: 'fs', targets: [{ format: this.format }] }, primitive: { topology: 'triangle-list' } });
    const yu = new Uint32Array([geo.aw, hdr.width, hdr.height, hdr.ycbcrRange, geo.planeOff[1], geo.planeOff[2], geo.aw >> 1, hdr.ycbcrTransform]);
    this.rgbParams = d.createBuffer({ size: yu.byteLength, usage: U | D });
    d.queue.writeBuffer(this.rgbParams, 0, yu);
    this.rgbBG = d.createBindGroup({ layout: this.rgbPipe.getBindGroupLayout(0), entries: [
      { binding: 0, resource: { buffer: this.rgbParams } }, { binding: 1, resource: { buffer: this.planes } }] });
    this.rgbTarget = d.createTexture({ size: [hdr.width, hdr.height], format: this.format,
      usage: GPUTextureUsage.RENDER_ATTACHMENT | GPUTextureUsage.COPY_SRC });

    if (this.timestamps) {
      this.qs = d.createQuerySet({ type: 'timestamp', count: 64 });
      this.qResolve = d.createBuffer({ size: 64 * 8, usage: GPUBufferUsage.QUERY_RESOLVE | C });
      this.qRead = d.createBuffer({ size: 64 * 8, usage: GPUBufferUsage.MAP_READ | D });
    }
  }

  // CPU side: parse packets, upload offsets + payload. Returns ms spent.
  upload(frame) {
    const t0 = performance.now();
    const pk = packFrame(frame, this.geo);
    const d = this.device;
    if (pk.payload.byteLength > this.payloadCap) {
      this.payloadCap = Math.ceil(pk.payload.byteLength * 1.25 / 4) * 4;
      this.payload = d.createBuffer({ size: this.payloadCap, usage: GPUBufferUsage.STORAGE | GPUBufferUsage.COPY_DST });
      this.dqBG = d.createBindGroup({ layout: this.dqPipe.getBindGroupLayout(0), entries: [
        { binding: 0, resource: { buffer: this.dqParams } }, { binding: 1, resource: { buffer: this.offsets } },
        { binding: 2, resource: { buffer: this.payload } }, { binding: 3, resource: { buffer: this.coefHi } },
        { binding: 4, resource: { buffer: this.coefLo } }, { binding: 5, resource: { buffer: this.blockBand } }] });
    }
    d.queue.writeBuffer(this.offsets, 0, pk.offsets);
    d.queue.writeBuffer(this.payload, 0, pk.payload);
    this.packet = pk;
    return performance.now() - t0;
  }

  encDequant(pass) {
    pass.setPipeline(this.dqPipe);
    pass.setBindGroup(0, this.dqBG);
    const n = this.geo.totalBlocks;
    pass.dispatchWorkgroups(this.wgStride, Math.ceil(n / this.wgStride));
  }

  encIdwt(pass, level) {
    for (const s of this.idwt) {
      if (level !== undefined && s.level !== level) continue;
      pass.setPipeline(s.pipe);
      pass.setBindGroup(0, s.bg);
      pass.dispatchWorkgroups(s.wx, s.wy, s.wz);
    }
  }

  encRgb(enc, view, tsw, reps = 1) {
    const pass = enc.beginRenderPass({ colorAttachments: [{ view, loadOp: 'clear', storeOp: 'store',
      clearValue: [0, 0, 0, 1] }], timestampWrites: tsw });
    pass.setPipeline(this.rgbPipe);
    pass.setBindGroup(0, this.rgbBG);
    for (let i = 0; i < reps; i++) pass.draw(3);
    pass.end();
  }

  decode(view) {
    const enc = this.device.createCommandEncoder();
    const pass = enc.beginComputePass();
    this.encDequant(pass);
    this.encIdwt(pass);
    pass.end();
    if (view) this.encRgb(enc, view);
    this.device.queue.submit([enc.finish()]);
  }

  // GPU time per stage. Each stage runs `reps` times back to back inside one pass with a timestamp at
  // each end, so Chrome's 100 us timestamp quantisation is divided by reps.
  async time(reps) {
    if (!this.timestamps) return null;
    const d = this.device;
    const stages = [
      ['dequant', (p) => this.encDequant(p)],
      ...[4, 3, 2, 1, 0].map((l) => [`idwt L${l}`, (p) => this.encIdwt(p, l)]),
      ['idwt (all levels)', (p) => this.encIdwt(p)],
      ['decode (dequant + idwt)', (p) => { this.encDequant(p); this.encIdwt(p); }],
    ];
    const enc = d.createCommandEncoder();
    let q = 0;
    for (const [, fn] of stages) {
      const pass = enc.beginComputePass({ timestampWrites: { querySet: this.qs, beginningOfPassWriteIndex: q, endOfPassWriteIndex: q + 1 } });
      for (let i = 0; i < reps; i++) fn(pass);
      pass.end();
      q += 2;
    }
    this.encRgb(enc, this.rgbTarget.createView(), { querySet: this.qs, beginningOfPassWriteIndex: q, endOfPassWriteIndex: q + 1 }, reps);
    q += 2;
    enc.resolveQuerySet(this.qs, 0, q, this.qResolve, 0);
    enc.copyBufferToBuffer(this.qResolve, 0, this.qRead, 0, q * 8);
    d.queue.submit([enc.finish()]);
    await this.qRead.mapAsync(GPUMapMode.READ);
    const t = new BigUint64Array(this.qRead.getMappedRange().slice(0));
    this.qRead.unmap();
    const out = {};
    [...stages.map((s) => s[0]), 'yuv2rgb (render pass)'].forEach((name, i) => {
      out[name] = Number(t[2 * i + 1] - t[2 * i]) / reps / 1000; // us
    });
    return out;
  }

  // Wall-clock fallback when timestamp-query is missing: reps decodes, wait for the queue.
  async timeWall(reps) {
    const enc = this.device.createCommandEncoder();
    const pass = enc.beginComputePass();
    for (let i = 0; i < reps; i++) { this.encDequant(pass); this.encIdwt(pass); }
    pass.end();
    const t0 = performance.now();
    this.device.queue.submit([enc.finish()]);
    await this.device.queue.onSubmittedWorkDone();
    return (performance.now() - t0) * 1000 / reps;
  }

  // Preview only (not timed): YCbCr->RGB pass into an offscreen texture, read back as ImageData.
  async renderImageData() {
    const d = this.device, { width: w, height: h } = this.hdr;
    const bpr = Math.ceil(w * 4 / 256) * 256;
    const rb = d.createBuffer({ size: bpr * h, usage: GPUBufferUsage.MAP_READ | GPUBufferUsage.COPY_DST });
    const enc = d.createCommandEncoder();
    this.encRgb(enc, this.rgbTarget.createView());
    enc.copyTextureToBuffer({ texture: this.rgbTarget }, { buffer: rb, bytesPerRow: bpr }, [w, h]);
    d.queue.submit([enc.finish()]);
    await rb.mapAsync(GPUMapMode.READ);
    const src = new Uint8Array(rb.getMappedRange());
    const img = new ImageData(w, h);
    const bgra = this.format === 'bgra8unorm';
    for (let y = 0; y < h; y++) for (let x = 0; x < w; x++) {
      const i = y * bpr + 4 * x, o = 4 * (y * w + x);
      img.data[o] = src[i + (bgra ? 2 : 0)]; img.data[o + 1] = src[i + 1]; img.data[o + 2] = src[i + (bgra ? 0 : 2)]; img.data[o + 3] = 255;
    }
    rb.unmap(); rb.destroy();
    return img;
  }

  async readPlanes() {
    const d = this.device;
    const rb = d.createBuffer({ size: this.planes.size, usage: GPUBufferUsage.MAP_READ | GPUBufferUsage.COPY_DST });
    const enc = d.createCommandEncoder();
    enc.copyBufferToBuffer(this.planes, 0, rb, 0, this.planes.size);
    d.queue.submit([enc.finish()]);
    await rb.mapAsync(GPUMapMode.READ);
    const f = new Float32Array(rb.getMappedRange().slice(0));
    rb.unmap(); rb.destroy();
    // Quantise like an R8_UNORM store and crop to the picture.
    const { width: w, height: h } = this.hdr, g = this.geo;
    const out = [];
    for (let c = 0; c < 3; c++) {
      const pw = c ? w >> 1 : w, ph = c ? h >> 1 : h, st = g.planeStride[c], o = g.planeOff[c];
      const a = new Uint8Array(pw * ph);
      for (let y = 0; y < ph; y++) for (let x = 0; x < pw; x++) a[y * pw + x] = Math.round(Math.min(1, Math.max(0, f[o + y * st + x])) * 255);
      out.push(a);
    }
    return out;
  }
}

function psnr(a, b) {
  let s = 0;
  for (let i = 0; i < a.length; i++) { const e = a[i] - b[i]; s += e * e; }
  const mse = s / a.length;
  return mse === 0 ? Infinity : 10 * Math.log10(255 * 255 / mse);
}

// y4m (8-bit 4:2:0) -> first frame planes
function parseY4MFirstFrame(buf) {
  const nl = buf.indexOf(10);
  const hdr = new TextDecoder().decode(buf.subarray(0, nl)).split(' ');
  const w = +hdr.find((t) => t[0] === 'W').slice(1), h = +hdr.find((t) => t[0] === 'H').slice(1);
  let o = nl + 1;
  o = buf.indexOf(10, o) + 1; // FRAME line
  const ys = w * h, cs = (w >> 1) * (h >> 1);
  return { w, h, planes: [buf.subarray(o, o + ys), buf.subarray(o + ys, o + ys + cs), buf.subarray(o + ys + cs, o + ys + 2 * cs)] };
}

window.PyroWave = { parsePWV1, geometry, packFrame, PyroWaveDecoder, psnr, parseY4MFirstFrame, dequantWGSL, idwtWGSL };
